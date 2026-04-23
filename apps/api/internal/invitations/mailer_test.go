package invitations_test

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
	entemaildelivery "github.com/open-huddle/huddle/apps/api/ent/emaildelivery"
	entinvitation "github.com/open-huddle/huddle/apps/api/ent/invitation"
	entmembership "github.com/open-huddle/huddle/apps/api/ent/membership"
	"github.com/open-huddle/huddle/apps/api/internal/email"
	"github.com/open-huddle/huddle/apps/api/internal/invitations"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
)

// spySender records every Send call so tests can assert on the rendered
// Message, and can be toggled to return an error for failure-path
// coverage. Satisfies email.Sender.
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

// seedPendingInvite creates an Invitation row in the state InviteMember
// leaves behind — token set, email_sent_at nil — so the mailer has
// something to process.
func seedPendingInvite(ctx context.Context, t *testing.T, client *ent.Client, email string) *ent.Invitation {
	t.Helper()
	user := testutil.MakeUser(ctx, t, client, "alice")
	org := testutil.MakeOrg(ctx, t, client, "acme")
	testutil.MakeMember(ctx, t, client, user, org, entmembership.RoleOwner)

	tokenPlain := []byte{0x01, 0x02, 0x03, 0x04}
	tokenHash := []byte{0xaa, 0xbb, 0xcc, 0xdd}

	inv, err := client.Invitation.Create().
		SetEmail(email).
		SetRole(entinvitation.RoleMember).
		SetTokenHash(tokenHash).
		SetTokenPlaintext(tokenPlain).
		SetExpiresAt(time.Now().Add(24 * time.Hour)).
		SetOrganization(org).
		SetInvitedBy(user).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed invitation: %v", err)
	}
	return inv
}

func newMailer(t *testing.T, sender email.Sender) (*ent.Client, *invitations.Mailer) {
	t.Helper()
	client := testutil.NewClient(t)
	m := invitations.NewMailer(client, sender,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"noreply@open-huddle.test",
		"Open Huddle Test",
		"http://localhost:5173/accept-invite",
	)
	return client, m
}

func TestSendBatch_SendsPendingInviteAndStamps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sender := &spySender{}
	client, m := newMailer(t, sender)

	inv := seedPendingInvite(ctx, t, client, "bob@example.test")

	if err := m.SendBatch(ctx); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	calls := sender.snapshot()
	if len(calls) != 1 {
		t.Fatalf("want 1 Send call, got %d", len(calls))
	}
	if calls[0].ToAddress != "bob@example.test" {
		t.Errorf("to: want bob@example.test got %q", calls[0].ToAddress)
	}
	if !containsString(calls[0].BodyText, "http://localhost:5173/accept-invite?token=") {
		t.Errorf("body missing accept URL with token; got:\n%s", calls[0].BodyText)
	}

	reloaded, err := client.Invitation.Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("reload invite: %v", err)
	}
	if reloaded.EmailSentAt == nil {
		t.Error("email_sent_at should be stamped on success")
	}
	// Token plaintext cleared post-send; ent may present NULL as nil or
	// as a pointer to an empty slice depending on the driver — both are
	// "cleared" for our purposes.
	if reloaded.TokenPlaintext != nil && len(*reloaded.TokenPlaintext) > 0 {
		t.Errorf("token_plaintext should be cleared post-send, got %d bytes", len(*reloaded.TokenPlaintext))
	}

	// Delivery row recorded with status=sent.
	deliveries, err := client.EmailDelivery.Query().All(ctx)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("want 1 EmailDelivery, got %d", len(deliveries))
	}
	if deliveries[0].Status != entemaildelivery.StatusSent {
		t.Errorf("status: want sent got %q", deliveries[0].Status)
	}
	if deliveries[0].Recipient != "bob@example.test" {
		t.Errorf("recipient: want bob@example.test got %q", deliveries[0].Recipient)
	}
}

func TestSendBatch_RetriesOnTransportFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sender := &spySender{returnErr: errors.New("smtp: connection refused")}
	client, m := newMailer(t, sender)

	inv := seedPendingInvite(ctx, t, client, "bob@example.test")

	// First tick fails.
	if err := m.SendBatch(ctx); err != nil {
		t.Fatalf("SendBatch (first): %v", err)
	}
	afterFirst, err := client.Invitation.Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("reload (first): %v", err)
	}
	if afterFirst.EmailSentAt != nil {
		t.Error("email_sent_at must stay nil when Sender returned an error — otherwise we skip the retry")
	}
	deliveries, err := client.EmailDelivery.Query().All(ctx)
	if err != nil {
		t.Fatalf("list deliveries (first): %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != entemaildelivery.StatusFailed {
		t.Fatalf("want 1 failed EmailDelivery, got %+v", deliveries)
	}
	if !containsString(deliveries[0].FailureReason, "connection refused") {
		t.Errorf("failure_reason should carry the sender error, got %q", deliveries[0].FailureReason)
	}

	// Second tick succeeds — same invite, one more call.
	sender.mu.Lock()
	sender.returnErr = nil
	sender.mu.Unlock()
	if err := m.SendBatch(ctx); err != nil {
		t.Fatalf("SendBatch (second): %v", err)
	}
	if len(sender.snapshot()) != 2 {
		t.Errorf("want 2 total Send calls after retry, got %d", len(sender.snapshot()))
	}
	afterSecond, err := client.Invitation.Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("reload (second): %v", err)
	}
	if afterSecond.EmailSentAt == nil {
		t.Error("email_sent_at should stamp after a successful retry")
	}

	// Two delivery rows: failed then sent.
	deliveries, err = client.EmailDelivery.Query().
		Order(ent.Asc(entemaildelivery.FieldCreatedAt)).All(ctx)
	if err != nil {
		t.Fatalf("list deliveries (second): %v", err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("want 2 EmailDelivery rows after retry, got %d", len(deliveries))
	}
	if deliveries[0].Status != entemaildelivery.StatusFailed {
		t.Errorf("first delivery should be failed, got %q", deliveries[0].Status)
	}
	if deliveries[1].Status != entemaildelivery.StatusSent {
		t.Errorf("second delivery should be sent, got %q", deliveries[1].Status)
	}
}

func TestSendBatch_SkipsExpiredInvites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sender := &spySender{}
	client, m := newMailer(t, sender)

	inv := seedPendingInvite(ctx, t, client, "bob@example.test")
	// Rewind expires_at into the past — mailer must skip.
	if _, err := client.Invitation.UpdateOne(inv).
		SetExpiresAt(time.Now().Add(-time.Hour)).Save(ctx); err != nil {
		t.Fatalf("rewind expiry: %v", err)
	}

	if err := m.SendBatch(ctx); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if got := len(sender.snapshot()); got != 0 {
		t.Errorf("expected 0 sends for an expired invite, got %d", got)
	}
}

func TestSendBatch_SkipsAcceptedInvites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sender := &spySender{}
	client, m := newMailer(t, sender)

	inv := seedPendingInvite(ctx, t, client, "bob@example.test")
	if _, err := client.Invitation.UpdateOne(inv).
		SetAcceptedAt(time.Now()).Save(ctx); err != nil {
		t.Fatalf("mark accepted: %v", err)
	}

	if err := m.SendBatch(ctx); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if got := len(sender.snapshot()); got != 0 {
		t.Errorf("expected 0 sends for an accepted invite, got %d", got)
	}
}

// Keep the uuid import honest — helpers use it indirectly through ent.
var _ = uuid.Nil

func containsString(hay, needle string) bool {
	return len(needle) > 0 && len(hay) >= len(needle) && indexOf(hay, needle) >= 0
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
