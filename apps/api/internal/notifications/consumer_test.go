package notifications_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/open-huddle/huddle/apps/api/ent"
	entnotification "github.com/open-huddle/huddle/apps/api/ent/notification"
	"github.com/open-huddle/huddle/apps/api/ent/outboxevent"
	"github.com/open-huddle/huddle/apps/api/internal/notifications"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// newConsumer builds a notifications.Consumer against a fresh in-memory
// SQLite client. Tests drive ConsumeBatch directly — the Run loop is a
// thin ticker wrapper covered by integration tests.
func newConsumer(t *testing.T) (*ent.Client, *notifications.Consumer) {
	t.Helper()
	client := testutil.NewClient(t)
	c := notifications.NewConsumer(client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return client, c
}

// seedMessageEvent inserts an outbox row that mirrors what
// MessageService.Send writes — message.created with a protobuf payload
// carrying the mention set. It also creates real User + Organization
// rows so the FK constraints on Notification are satisfied when the
// consumer fans out. Returns the outbox event id and the ids of the
// mentioned users (which the test needs to assert Notification rows
// were created for).
func seedMessageEvent(ctx context.Context, t *testing.T, client *ent.Client, mentionCount int) (uuid.UUID, uuid.UUID, []uuid.UUID) {
	t.Helper()

	// Real Organization + Channel + Message + User rows so every FK
	// the consumer writes through (Notification.organization/channel/
	// message/recipient) is satisfied. The outbox_event denormalized
	// columns need real User + Organization too.
	org, err := client.Organization.Create().
		SetName("Acme").SetSlug(uuid.New().String()).Save(ctx)
	if err != nil {
		t.Fatalf("seed org: %v", err)
	}
	author, err := client.User.Create().
		SetSubject("author-" + uuid.NewString()).
		SetEmail("author@example.test").
		SetDisplayName("Author").Save(ctx)
	if err != nil {
		t.Fatalf("seed author: %v", err)
	}
	channel, err := client.Channel.Create().
		SetName("general").SetSlug("general-" + uuid.NewString()[:8]).
		SetOrganization(org).SetCreatedBy(author).Save(ctx)
	if err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	message, err := client.Message.Create().
		SetChannelID(channel.ID).SetAuthorID(author.ID).
		SetBody("hello").Save(ctx)
	if err != nil {
		t.Fatalf("seed message: %v", err)
	}
	msgID := message.ID
	channelID := channel.ID

	mentions := make([]string, 0, mentionCount)
	mentionUUIDs := make([]uuid.UUID, 0, mentionCount)
	for i := 0; i < mentionCount; i++ {
		u, err := client.User.Create().
			SetSubject("mention-" + uuid.NewString()).
			SetEmail("mention@example.test").
			SetDisplayName("Mentionee").Save(ctx)
		if err != nil {
			t.Fatalf("seed mention user: %v", err)
		}
		mentions = append(mentions, u.ID.String())
		mentionUUIDs = append(mentionUUIDs, u.ID)
	}

	payload, err := proto.Marshal(&huddlev1.Message{
		Id:             msgID.String(),
		ChannelId:      channelID.String(),
		AuthorId:       author.ID.String(),
		Body:           "hello @team",
		CreatedAt:      timestamppb.New(time.Now()),
		MentionUserIds: mentions,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	row, err := client.OutboxEvent.Create().
		SetAggregateType("message").
		SetAggregateID(msgID).
		SetEventType("message.created").
		SetSubject("huddle.messages.created." + channelID.String()).
		SetPayload(payload).
		SetActorID(author.ID).
		SetOrganizationID(org.ID).
		SetResourceType("message").
		SetResourceID(msgID).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	return row.ID, org.ID, mentionUUIDs
}

func TestConsumeBatch_CreatesNotificationPerMention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, c := newConsumer(t)

	outboxID, orgID, recipients := seedMessageEvent(ctx, t, client, 3)

	if err := c.ConsumeBatch(ctx); err != nil {
		t.Fatalf("ConsumeBatch: %v", err)
	}

	// One Notification per mention, all with kind=mention and the
	// right organization scope.
	notes, err := client.Notification.Query().All(ctx)
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if len(notes) != len(recipients) {
		t.Fatalf("want %d notifications, got %d", len(recipients), len(notes))
	}
	for _, n := range notes {
		if n.Kind != entnotification.KindMention {
			t.Errorf("kind: want mention got %q", n.Kind)
		}
		if n.OrganizationID != orgID {
			t.Errorf("organization_id: want %s got %s", orgID, n.OrganizationID)
		}
		if n.ReadAt != nil {
			t.Errorf("fresh notification should be unread")
		}
	}

	// The outbox row is stamped notified_at so outbox.GC can proceed.
	reloaded, err := client.OutboxEvent.Get(ctx, outboxID)
	if err != nil {
		t.Fatalf("reload outbox: %v", err)
	}
	if reloaded.NotifiedAt == nil {
		t.Error("notified_at should be stamped after successful fan-out")
	}
}

func TestConsumeBatch_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, c := newConsumer(t)

	seedMessageEvent(ctx, t, client, 2)

	for i := 0; i < 2; i++ {
		if err := c.ConsumeBatch(ctx); err != nil {
			t.Fatalf("drain %d: %v", i+1, err)
		}
	}

	// Exactly 2 notifications — the UNIQUE(recipient, message, kind)
	// constraint on Notification is the dedup, and notified_at keeps
	// the second drain from finding the outbox row at all.
	count, err := client.Notification.Query().Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("want 2 notifications after two drains, got %d", count)
	}
}

func TestConsumeBatch_StampsNonMessageEventsWithoutWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, c := newConsumer(t)

	// A future channel.created row should be stamped but not fan out
	// notifications.
	aggID := uuid.New()
	row, err := client.OutboxEvent.Create().
		SetAggregateType("channel").
		SetAggregateID(aggID).
		SetEventType("channel.created").
		SetSubject("huddle.channels.created." + aggID.String()).
		SetPayload([]byte("irrelevant")).
		SetResourceType("channel").
		SetResourceID(aggID).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed channel outbox: %v", err)
	}

	if err := c.ConsumeBatch(ctx); err != nil {
		t.Fatalf("ConsumeBatch: %v", err)
	}

	if got, err := client.Notification.Query().Count(ctx); err != nil || got != 0 {
		t.Errorf("non-message events should not create notifications; got %d (err=%v)", got, err)
	}
	reloaded, err := client.OutboxEvent.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("reload outbox: %v", err)
	}
	if reloaded.NotifiedAt == nil {
		t.Error("non-message event should still be stamped so GC can proceed")
	}
}

func TestConsumeBatch_SkipsAlreadyNotified(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, c := newConsumer(t)

	outboxID, _, _ := seedMessageEvent(ctx, t, client, 1)
	// Pre-stamp as if a prior tick had already processed it.
	if err := client.OutboxEvent.UpdateOneID(outboxID).
		SetNotifiedAt(time.Now()).Exec(ctx); err != nil {
		t.Fatalf("pre-stamp notified_at: %v", err)
	}

	if err := c.ConsumeBatch(ctx); err != nil {
		t.Fatalf("ConsumeBatch: %v", err)
	}

	// No Notifications should have been created — the poll predicate
	// filters notified_at IS NULL.
	if got, err := client.Notification.Query().Count(ctx); err != nil || got != 0 {
		t.Errorf("pre-stamped row should not produce notifications; got %d (err=%v)", got, err)
	}

	// Sanity: the outbox row is still there (this consumer doesn't
	// delete; GC does).
	if _, err := client.OutboxEvent.Query().
		Where(outboxevent.IDEQ(outboxID)).
		Only(ctx); err != nil {
		t.Errorf("outbox row should still exist: %v", err)
	}
}

func TestConsumeBatch_MalformedPayloadIsStamped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, c := newConsumer(t)

	aggID := uuid.New()
	orgID := uuid.New()
	row, err := client.OutboxEvent.Create().
		SetAggregateType("message").
		SetAggregateID(aggID).
		SetEventType("message.created").
		SetSubject("huddle.messages.created.x").
		SetPayload([]byte{0xff, 0xff, 0xff}).
		SetOrganizationID(orgID).
		SetResourceType("message").
		SetResourceID(aggID).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed bad outbox: %v", err)
	}

	if err := c.ConsumeBatch(ctx); err != nil {
		t.Fatalf("ConsumeBatch: %v", err)
	}

	reloaded, err := client.OutboxEvent.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.NotifiedAt == nil {
		t.Error("malformed payload should still stamp notified_at so the worker doesn't hot-loop")
	}
}
