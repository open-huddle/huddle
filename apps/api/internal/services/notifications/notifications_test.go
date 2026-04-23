package notifications_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/open-huddle/huddle/apps/api/ent"
	entnotification "github.com/open-huddle/huddle/apps/api/ent/notification"
	"github.com/open-huddle/huddle/apps/api/internal/auth"
	"github.com/open-huddle/huddle/apps/api/internal/principal"
	notificationsvc "github.com/open-huddle/huddle/apps/api/internal/services/notifications"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

type fixture struct {
	ctx    context.Context
	client *ent.Client
	svc    *notificationsvc.Service
	alice  *ent.User
	bob    *ent.User
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	client := testutil.NewClient(t)
	resolver := principal.NewResolver(client)
	svc := notificationsvc.New(client, resolver, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	alice := testutil.MakeUser(ctx, t, client, "alice")
	bob := testutil.MakeUser(ctx, t, client, "bob")
	return &fixture{ctx: ctx, client: client, svc: svc, alice: alice, bob: bob}
}

func callerCtx(ctx context.Context, subject string) context.Context {
	return auth.WithClaims(ctx, auth.Claims{
		Subject:       subject,
		Email:         subject + "@example.test",
		EmailVerified: true,
		Name:          subject,
	})
}

func connectCode(t *testing.T, err error) connect.Code {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error is not *connect.Error: %v", err)
	}
	return ce.Code()
}

// seedNotification writes one Notification row for the given recipient.
// Organization is real because the ent edge is Required — the FK fires
// at insert time. Message / Channel are nil (the edges are Optional) so
// we don't have to drag in the full graph just to test the inbox.
func seedNotification(ctx context.Context, t *testing.T, client *ent.Client, recipient *ent.User) *ent.Notification {
	t.Helper()
	org, err := client.Organization.Create().
		SetName("Acme").SetSlug(uuid.New().String()).Save(ctx)
	if err != nil {
		t.Fatalf("seed org: %v", err)
	}
	n, err := client.Notification.Create().
		SetRecipientUserID(recipient.ID).
		SetKind(entnotification.KindMention).
		SetOrganizationID(org.ID).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed notification: %v", err)
	}
	return n
}

func TestList_ReturnsCallerOwnedUnread(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	// Two for alice (unread), one for bob — caller shouldn't see bob's.
	seedNotification(f.ctx, t, f.client, f.alice)
	seedNotification(f.ctx, t, f.client, f.alice)
	seedNotification(f.ctx, t, f.client, f.bob)

	resp, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.NotificationServiceListRequest{}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Msg.Notifications) != 2 {
		t.Fatalf("want 2 notifications (alice's unread), got %d", len(resp.Msg.Notifications))
	}
	for _, n := range resp.Msg.Notifications {
		if n.RecipientUserId != f.alice.ID.String() {
			t.Errorf("cross-user leak: got notification for %s", n.RecipientUserId)
		}
		if n.ReadAt != nil {
			t.Errorf("default List should return unread only; got a read one")
		}
	}
}

func TestList_IncludeReadReturnsEverything(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	unread := seedNotification(f.ctx, t, f.client, f.alice)
	read := seedNotification(f.ctx, t, f.client, f.alice)
	// Mark one as read so the default filter would skip it.
	if _, err := f.svc.MarkRead(ctx, connect.NewRequest(&huddlev1.NotificationServiceMarkReadRequest{
		Id: read.ID.String(),
	})); err != nil {
		t.Fatalf("pre-mark read: %v", err)
	}

	resp, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.NotificationServiceListRequest{
		IncludeRead: true,
	}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Msg.Notifications) != 2 {
		t.Fatalf("want 2 notifications (read + unread), got %d", len(resp.Msg.Notifications))
	}
	seen := map[string]bool{}
	for _, n := range resp.Msg.Notifications {
		seen[n.Id] = true
	}
	if !seen[unread.ID.String()] || !seen[read.ID.String()] {
		t.Errorf("both unread %s and read %s should appear", unread.ID, read.ID)
	}
}

func TestMarkRead_StampsReadAt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")
	n := seedNotification(f.ctx, t, f.client, f.alice)

	resp, err := f.svc.MarkRead(ctx, connect.NewRequest(&huddlev1.NotificationServiceMarkReadRequest{
		Id: n.ID.String(),
	}))
	if err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if resp.Msg.Notification.ReadAt == nil {
		t.Error("response should carry the stamped read_at")
	}

	reloaded, err := f.client.Notification.Get(f.ctx, n.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.ReadAt == nil {
		t.Error("persisted row should have read_at set")
	}
}

func TestMarkRead_IdempotentOnAlreadyRead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")
	n := seedNotification(f.ctx, t, f.client, f.alice)

	first, err := f.svc.MarkRead(ctx, connect.NewRequest(&huddlev1.NotificationServiceMarkReadRequest{
		Id: n.ID.String(),
	}))
	if err != nil {
		t.Fatalf("first MarkRead: %v", err)
	}
	firstTS := first.Msg.Notification.ReadAt.AsTime()

	second, err := f.svc.MarkRead(ctx, connect.NewRequest(&huddlev1.NotificationServiceMarkReadRequest{
		Id: n.ID.String(),
	}))
	if err != nil {
		t.Fatalf("second MarkRead: %v", err)
	}
	if !second.Msg.Notification.ReadAt.AsTime().Equal(firstTS) {
		t.Error("second MarkRead should be a no-op; read_at must not change")
	}
}

// MarkRead on a notification belonging to another user returns NotFound
// rather than Forbidden — we don't want to confirm the id exists to
// non-owners.
func TestMarkRead_NotFoundForOthersNotification(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// The notification belongs to bob; alice tries to mark it read.
	n := seedNotification(f.ctx, t, f.client, f.bob)
	aliceCtx := callerCtx(f.ctx, "alice")

	_, err := f.svc.MarkRead(aliceCtx, connect.NewRequest(&huddlev1.NotificationServiceMarkReadRequest{
		Id: n.ID.String(),
	}))
	if got, want := connectCode(t, err), connect.CodeNotFound; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}

func TestMarkRead_InvalidIdIsInvalidArgument(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	_, err := f.svc.MarkRead(ctx, connect.NewRequest(&huddlev1.NotificationServiceMarkReadRequest{
		Id: "not-a-uuid",
	}))
	if got, want := connectCode(t, err), connect.CodeInvalidArgument; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}
