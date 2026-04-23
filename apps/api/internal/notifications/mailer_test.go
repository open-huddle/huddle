package notifications_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-huddle/huddle/apps/api/ent"
	entmembership "github.com/open-huddle/huddle/apps/api/ent/membership"
	entnotification "github.com/open-huddle/huddle/apps/api/ent/notification"
	entnotificationpreference "github.com/open-huddle/huddle/apps/api/ent/notificationpreference"
	"github.com/open-huddle/huddle/apps/api/internal/email"
	"github.com/open-huddle/huddle/apps/api/internal/notifications"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
)

// spySender captures every Send call so tests can assert on the rendered
// Message, and can be told to return an error for failure-path coverage.
type spySender struct {
	mu        sync.Mutex
	calls     []email.Message
	returnErr error
}

func (s *spySender) Send(_ context.Context, msg email.Message) error {
	s.mu.Lock()
	s.calls = append(s.calls, msg)
	s.mu.Unlock()
	return s.returnErr
}

func (s *spySender) snapshot() []email.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]email.Message, len(s.calls))
	copy(out, s.calls)
	return out
}

// mailerFixture seeds the minimum shape the mailer needs: an org, a
// channel, two users (sender + recipient), and a Notification row
// targeting the recipient. Tests then drive SendBatch and assert on
// email + Notification row state.
type mailerFixture struct {
	ctx          context.Context
	client       *ent.Client
	sender       *spySender
	mailer       *notifications.Mailer
	senderUser   *ent.User
	recipient    *ent.User
	message      *ent.Message
	notification *ent.Notification
}

func newMailerFixture(t *testing.T) *mailerFixture {
	t.Helper()
	client := testutil.NewClient(t)
	spy := &spySender{}
	mailer := notifications.NewMailer(client, spy,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"noreply@huddle.test",
		"Huddle Test",
		"https://huddle.test",
	)

	ctx := context.Background()
	org := testutil.MakeOrg(ctx, t, client, "acme")
	alice := testutil.MakeUser(ctx, t, client, "alice")
	bob := testutil.MakeUser(ctx, t, client, "bob")
	testutil.MakeMember(ctx, t, client, alice, org, entmembership.RoleMember)
	testutil.MakeMember(ctx, t, client, bob, org, entmembership.RoleMember)
	channel, err := client.Channel.Create().
		SetName("general").SetSlug("general").
		SetOrganization(org).SetCreatedBy(alice).Save(ctx)
	if err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	msg, err := client.Message.Create().
		SetChannelID(channel.ID).SetAuthorID(alice.ID).
		SetBody("hey @bob, look at this long and interesting message").Save(ctx)
	if err != nil {
		t.Fatalf("seed message: %v", err)
	}
	n, err := client.Notification.Create().
		SetRecipientUserID(bob.ID).
		SetKind(entnotification.KindMention).
		SetMessageID(msg.ID).
		SetChannelID(channel.ID).
		SetOrganizationID(org.ID).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed notification: %v", err)
	}
	return &mailerFixture{
		ctx: ctx, client: client, sender: spy, mailer: mailer,
		senderUser: alice, recipient: bob, message: msg, notification: n,
	}
}

func TestSendBatch_DefaultOptInSendsEmail(t *testing.T) {
	t.Parallel()
	f := newMailerFixture(t)

	if err := f.mailer.SendBatch(f.ctx); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	calls := f.sender.snapshot()
	if len(calls) != 1 {
		t.Fatalf("want 1 email (no preference row = default enabled), got %d", len(calls))
	}
	if calls[0].ToAddress != f.recipient.Email {
		t.Errorf("to: want %s got %s", f.recipient.Email, calls[0].ToAddress)
	}
	if calls[0].Subject != "alice mentioned you in #general" {
		t.Errorf("subject: %q", calls[0].Subject)
	}
	if !contains(calls[0].BodyText, "hey @bob") {
		t.Errorf("body missing message excerpt; got:\n%s", calls[0].BodyText)
	}
	if !contains(calls[0].BodyText, "https://huddle.test/channels/"+f.message.ChannelID.String()+"#message-"+f.message.ID.String()) {
		t.Errorf("body missing deep-link URL; got:\n%s", calls[0].BodyText)
	}

	reloaded, err := f.client.Notification.Get(f.ctx, f.notification.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.EmailedAt == nil {
		t.Error("emailed_at should be stamped on success")
	}
}

func TestSendBatch_ExplicitOptOutSkips(t *testing.T) {
	t.Parallel()
	f := newMailerFixture(t)

	// Bob explicitly opts out of mention emails.
	if _, err := f.client.NotificationPreference.Create().
		SetUserID(f.recipient.ID).
		SetKind(entnotificationpreference.KindMention).
		SetEmailEnabled(false).
		Save(f.ctx); err != nil {
		t.Fatalf("seed preference: %v", err)
	}

	if err := f.mailer.SendBatch(f.ctx); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	if got := len(f.sender.snapshot()); got != 0 {
		t.Errorf("opt-out recipient should not receive email, got %d sends", got)
	}
	reloaded, err := f.client.Notification.Get(f.ctx, f.notification.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.EmailedAt != nil {
		t.Error("skipped notification should NOT be stamped — user could opt back in later")
	}
}

func TestSendBatch_ExplicitOptInStillSends(t *testing.T) {
	t.Parallel()
	f := newMailerFixture(t)

	// A preference row with email_enabled=true is behaviorally
	// equivalent to no row — verify the filter treats it that way.
	if _, err := f.client.NotificationPreference.Create().
		SetUserID(f.recipient.ID).
		SetKind(entnotificationpreference.KindMention).
		SetEmailEnabled(true).
		Save(f.ctx); err != nil {
		t.Fatalf("seed preference: %v", err)
	}

	if err := f.mailer.SendBatch(f.ctx); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if got := len(f.sender.snapshot()); got != 1 {
		t.Errorf("explicit opt-in should send, got %d", got)
	}
}

func TestSendBatch_TransientFailureRetries(t *testing.T) {
	t.Parallel()
	f := newMailerFixture(t)
	f.sender.returnErr = errors.New("smtp: connection refused")

	if err := f.mailer.SendBatch(f.ctx); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	afterFirst, err := f.client.Notification.Get(f.ctx, f.notification.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if afterFirst.EmailedAt != nil {
		t.Error("emailed_at must stay nil when Send returned an error — otherwise we skip the retry")
	}

	// Clear the error and tick again — the same notification should
	// get picked up on the second pass.
	f.sender.mu.Lock()
	f.sender.returnErr = nil
	f.sender.mu.Unlock()
	if err := f.mailer.SendBatch(f.ctx); err != nil {
		t.Fatalf("SendBatch (second): %v", err)
	}
	if got := len(f.sender.snapshot()); got != 2 {
		t.Errorf("want 2 total Send calls after retry, got %d", got)
	}
	afterSecond, err := f.client.Notification.Get(f.ctx, f.notification.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if afterSecond.EmailedAt == nil {
		t.Error("emailed_at should stamp after a successful retry")
	}
}

func TestSendBatch_SkipsAlreadyEmailed(t *testing.T) {
	t.Parallel()
	f := newMailerFixture(t)
	// Pre-stamp as if a prior tick had already emailed this row.
	if _, err := f.client.Notification.UpdateOne(f.notification).
		SetEmailedAt(time.Now()).Save(f.ctx); err != nil {
		t.Fatalf("pre-stamp: %v", err)
	}

	if err := f.mailer.SendBatch(f.ctx); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if got := len(f.sender.snapshot()); got != 0 {
		t.Errorf("already-emailed row should be skipped, got %d sends", got)
	}
}

// --- helpers ---------------------------------------------------------------

// keep uuid import exercised in case a future helper needs it.
var _ = uuid.Nil

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) > 0 && indexOf(haystack, needle) >= 0
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
