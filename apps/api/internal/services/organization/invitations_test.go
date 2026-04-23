package organization_test

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"io"
	"log/slog"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/open-huddle/huddle/apps/api/ent"
	entinvitation "github.com/open-huddle/huddle/apps/api/ent/invitation"
	entmembership "github.com/open-huddle/huddle/apps/api/ent/membership"
	"github.com/open-huddle/huddle/apps/api/ent/outboxevent"
	"github.com/open-huddle/huddle/apps/api/internal/auth"
	"github.com/open-huddle/huddle/apps/api/internal/policy"
	"github.com/open-huddle/huddle/apps/api/internal/principal"
	"github.com/open-huddle/huddle/apps/api/internal/services/organization"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

type inviteFixture struct {
	ctx    context.Context
	client *ent.Client
	svc    *organization.Service
	owner  *ent.User
	org    *ent.Organization
}

// newInviteFixture seeds an org with Alice as its owner. Callers hit the
// invite flow via alice's callerCtx (defined in the shared organization
// test file).
func newInviteFixture(t *testing.T) *inviteFixture {
	t.Helper()
	client := testutil.NewClient(t)
	resolver := principal.NewResolver(client)
	engine := policy.NewRBAC(client)
	svc := organization.New(client, resolver, engine, testInviteConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	owner := testutil.MakeUser(ctx, t, client, "alice")
	org := testutil.MakeOrg(ctx, t, client, "acme")
	testutil.MakeMember(ctx, t, client, owner, org, entmembership.RoleOwner)
	return &inviteFixture{ctx: ctx, client: client, svc: svc, owner: owner, org: org}
}

func (f *inviteFixture) inviteReq(email, role string) *connect.Request[huddlev1.InviteMemberRequest] {
	return connect.NewRequest(&huddlev1.InviteMemberRequest{
		OrganizationId: f.org.ID.String(),
		Email:          email,
		Role:           role,
	})
}

// --- InviteMember ----------------------------------------------------------

func TestInviteMember_CreatesRowAndOutboxEvent(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	resp, err := f.svc.InviteMember(ctx, f.inviteReq("Bob@Example.Test", "member"))
	if err != nil {
		t.Fatalf("InviteMember: %v", err)
	}
	if resp.Msg.Invitation.Email != "bob@example.test" {
		t.Errorf("email should have been lowercased, got %q", resp.Msg.Invitation.Email)
	}
	if resp.Msg.Invitation.Role != "member" {
		t.Errorf("role: want member got %q", resp.Msg.Invitation.Role)
	}

	// Row exists with plaintext and hashed tokens set; email_sent_at nil.
	rows, err := f.client.Invitation.Query().All(f.ctx)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 invitation row, got %d", len(rows))
	}
	inv := rows[0]
	if inv.TokenPlaintext == nil || len(*inv.TokenPlaintext) == 0 {
		t.Error("token_plaintext should be set until the mailer sends")
	}
	if len(inv.TokenHash) == 0 {
		t.Error("token_hash should be set")
	}
	if inv.EmailSentAt != nil {
		t.Error("email_sent_at should be nil on fresh invite")
	}
	if inv.AcceptedAt != nil {
		t.Error("accepted_at should be nil on fresh invite")
	}

	// Outbox event landed with no token in the payload.
	events, err := f.client.OutboxEvent.Query().
		Where(outboxevent.EventTypeEQ("invitation.created")).
		All(f.ctx)
	if err != nil {
		t.Fatalf("list outbox events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 invitation.created outbox event, got %d", len(events))
	}
	// The payload is the Invitation proto; should not contain the raw
	// token bytes.
	payloadStr := string(events[0].Payload)
	if plain := *inv.TokenPlaintext; len(plain) > 0 && contains(payloadStr, string(plain)) {
		t.Error("outbox event payload contains the plaintext token — it must not")
	}
}

func TestInviteMember_ReinvitingReplacesToken(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	if _, err := f.svc.InviteMember(ctx, f.inviteReq("bob@example.test", "member")); err != nil {
		t.Fatalf("first InviteMember: %v", err)
	}
	first := firstInvite(f.ctx, t, f.client)

	if _, err := f.svc.InviteMember(ctx, f.inviteReq("bob@example.test", "member")); err != nil {
		t.Fatalf("second InviteMember: %v", err)
	}

	// Still exactly one row.
	count, err := f.client.Invitation.Query().Count(f.ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("re-invite should replace the row, got %d", count)
	}
	second := firstInvite(f.ctx, t, f.client)

	if first.ID != second.ID {
		t.Errorf("re-invite should keep the same row id")
	}
	if equalBytes(first.TokenHash, second.TokenHash) {
		t.Errorf("re-invite should rotate the token hash")
	}
	if first.ExpiresAt.Equal(second.ExpiresAt) {
		t.Errorf("re-invite should extend expires_at")
	}
}

func TestInviteMember_DeniedForNonAdmin(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)

	// Carol signs in but has no membership in the org. She must not be
	// able to invite.
	ctx := callerCtx(f.ctx, "carol")

	_, err := f.svc.InviteMember(ctx, f.inviteReq("dave@example.test", "member"))
	if got, want := connectCode(t, err), connect.CodePermissionDenied; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}

func TestInviteMember_InvalidEmail(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	for _, bad := range []string{"", "not-an-email", "   "} {
		_, err := f.svc.InviteMember(ctx, f.inviteReq(bad, "member"))
		if got := connectCode(t, err); got != connect.CodeInvalidArgument {
			t.Errorf("email %q: want InvalidArgument got %v", bad, got)
		}
	}
}

// --- AcceptInvitation ------------------------------------------------------

func TestAcceptInvitation_HappyPath(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	aliceCtx := callerCtx(f.ctx, "alice")

	if _, err := f.svc.InviteMember(aliceCtx, f.inviteReq("bob@example.test", "member")); err != nil {
		t.Fatalf("InviteMember: %v", err)
	}
	inv := firstInvite(f.ctx, t, f.client)
	plain := *inv.TokenPlaintext
	tokenStr := base64.RawURLEncoding.EncodeToString(plain)

	bobCtx := callerCtxWithEmail(f.ctx, "bob", "bob@example.test")
	resp, err := f.svc.AcceptInvitation(bobCtx, connect.NewRequest(&huddlev1.AcceptInvitationRequest{Token: tokenStr}))
	if err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
	if resp.Msg.Membership.OrganizationId != f.org.ID.String() {
		t.Errorf("membership org: want %s got %s", f.org.ID, resp.Msg.Membership.OrganizationId)
	}
	if resp.Msg.Membership.Role != "member" {
		t.Errorf("membership role: want member got %q", resp.Msg.Membership.Role)
	}

	// Invite row is terminal: accepted_at set, token_plaintext cleared.
	reloaded, err := f.client.Invitation.Get(f.ctx, inv.ID)
	if err != nil {
		t.Fatalf("reload invite: %v", err)
	}
	if reloaded.AcceptedAt == nil {
		t.Error("accepted_at should be set")
	}
	// ent maps NULL nullable-bytes columns back to a non-nil pointer to an
	// empty slice in some driver combinations — treat both as "cleared".
	// Memory: feedback_ent_empty_bytes_null explains the symmetric quirk
	// on write.
	if reloaded.TokenPlaintext != nil && len(*reloaded.TokenPlaintext) > 0 {
		t.Errorf("token_plaintext should be cleared on accept, got %d bytes", len(*reloaded.TokenPlaintext))
	}
}

func TestAcceptInvitation_RejectsEmailMismatch(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	aliceCtx := callerCtx(f.ctx, "alice")

	if _, err := f.svc.InviteMember(aliceCtx, f.inviteReq("bob@example.test", "member")); err != nil {
		t.Fatalf("InviteMember: %v", err)
	}
	tokenStr := base64.RawURLEncoding.EncodeToString(*firstInvite(f.ctx, t, f.client).TokenPlaintext)

	// Eve has the invite token (forwarded email) but is signed in as
	// eve@example.test, not bob. Must be denied.
	eveCtx := callerCtxWithEmail(f.ctx, "eve", "eve@example.test")
	_, err := f.svc.AcceptInvitation(eveCtx, connect.NewRequest(&huddlev1.AcceptInvitationRequest{Token: tokenStr}))
	if got, want := connectCode(t, err), connect.CodePermissionDenied; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}

func TestAcceptInvitation_RejectsExpired(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	aliceCtx := callerCtx(f.ctx, "alice")

	if _, err := f.svc.InviteMember(aliceCtx, f.inviteReq("bob@example.test", "member")); err != nil {
		t.Fatalf("InviteMember: %v", err)
	}
	inv := firstInvite(f.ctx, t, f.client)

	// Rewind expires_at into the past. Unauthenticated (not
	// PermissionDenied) because the token itself is no longer valid —
	// forwarded tokens shouldn't reveal who the original invitee was.
	if _, err := f.client.Invitation.UpdateOne(inv).
		SetExpiresAt(time.Now().Add(-time.Hour)).Save(f.ctx); err != nil {
		t.Fatalf("rewind expiry: %v", err)
	}
	tokenStr := base64.RawURLEncoding.EncodeToString(*inv.TokenPlaintext)
	bobCtx := callerCtxWithEmail(f.ctx, "bob", "bob@example.test")
	_, err := f.svc.AcceptInvitation(bobCtx, connect.NewRequest(&huddlev1.AcceptInvitationRequest{Token: tokenStr}))
	if got, want := connectCode(t, err), connect.CodeUnauthenticated; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}

func TestAcceptInvitation_RejectsAlreadyAccepted(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	aliceCtx := callerCtx(f.ctx, "alice")

	if _, err := f.svc.InviteMember(aliceCtx, f.inviteReq("bob@example.test", "member")); err != nil {
		t.Fatalf("InviteMember: %v", err)
	}
	tokenStr := base64.RawURLEncoding.EncodeToString(*firstInvite(f.ctx, t, f.client).TokenPlaintext)
	bobCtx := callerCtxWithEmail(f.ctx, "bob", "bob@example.test")

	if _, err := f.svc.AcceptInvitation(bobCtx, connect.NewRequest(&huddlev1.AcceptInvitationRequest{Token: tokenStr})); err != nil {
		t.Fatalf("first accept: %v", err)
	}

	// Second accept — the invite is terminal; token_plaintext has also
	// been cleared but the check we exercise here is accepted_at, which
	// trips before any plaintext reference.
	_, err := f.svc.AcceptInvitation(bobCtx, connect.NewRequest(&huddlev1.AcceptInvitationRequest{Token: tokenStr}))
	if got, want := connectCode(t, err), connect.CodeUnauthenticated; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}

func TestAcceptInvitation_RejectsUnknownToken(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	bobCtx := callerCtxWithEmail(f.ctx, "bob", "bob@example.test")

	// A well-formed but unknown token. Must not leak whether the token
	// exists at all; Unauthenticated is the generic code.
	bogus := make([]byte, 32)
	for i := range bogus {
		bogus[i] = byte(i)
	}
	_, err := f.svc.AcceptInvitation(bobCtx, connect.NewRequest(&huddlev1.AcceptInvitationRequest{
		Token: base64.RawURLEncoding.EncodeToString(bogus),
	}))
	if got, want := connectCode(t, err), connect.CodeUnauthenticated; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}

func TestAcceptInvitation_RejectsMalformedToken(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	bobCtx := callerCtxWithEmail(f.ctx, "bob", "bob@example.test")

	for _, bad := range []string{"", "???", "short"} {
		_, err := f.svc.AcceptInvitation(bobCtx, connect.NewRequest(&huddlev1.AcceptInvitationRequest{Token: bad}))
		switch bad {
		case "":
			if got := connectCode(t, err); got != connect.CodeInvalidArgument {
				t.Errorf("empty token: want InvalidArgument got %v", got)
			}
		default:
			if got := connectCode(t, err); got != connect.CodeUnauthenticated {
				t.Errorf("malformed token %q: want Unauthenticated got %v", bad, got)
			}
		}
	}
}

// --- helpers ---------------------------------------------------------------

// callerCtxWithEmail is callerCtx with a non-default email — needed for
// the accept-path tests that verify the invite's email matches the
// caller's claims (rather than the fixed subject+"@example.test" the
// shared callerCtx uses).
func callerCtxWithEmail(ctx context.Context, subject, email string) context.Context {
	return auth.WithClaims(ctx, auth.Claims{
		Subject:       subject,
		Email:         email,
		EmailVerified: true,
		Name:          subject,
	})
}

func firstInvite(ctx context.Context, t *testing.T, client *ent.Client) *ent.Invitation {
	t.Helper()
	rows, err := client.Invitation.Query().
		Order(ent.Desc(entinvitation.FieldCreatedAt)).
		Limit(1).
		All(ctx)
	if err != nil {
		t.Fatalf("query invites: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("expected at least 1 invitation row")
	}
	return rows[0]
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && bytesContains([]byte(haystack), []byte(needle))
}

func bytesContains(hay, needle []byte) bool {
	// Small local substring search — avoids pulling bytes/strings for
	// one assertion.
	if len(needle) > len(hay) {
		return false
	}
outer:
	for i := 0; i+len(needle) <= len(hay); i++ {
		for j := range needle {
			if hay[i+j] != needle[j] {
				continue outer
			}
		}
		return true
	}
	return false
}

func equalBytes(a, b []byte) bool { return hmac.Equal(a, b) }
