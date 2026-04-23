package organization

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/open-huddle/huddle/apps/api/ent"
	entinvitation "github.com/open-huddle/huddle/apps/api/ent/invitation"
	entmembership "github.com/open-huddle/huddle/apps/api/ent/membership"
	"github.com/open-huddle/huddle/apps/api/ent/organization"
	"github.com/open-huddle/huddle/apps/api/internal/policy"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

const (
	// tokenBytes is the raw-entropy size of an invite token. 32 bytes
	// (256 bits) is both the output width of SHA-256 and comfortably
	// above what any brute-force attacker can enumerate.
	tokenBytes = 32

	// maxEmailLen is RFC 5321's limit on the local + domain parts of an
	// email address. Slightly generous to account for quoted-local
	// forms, but bounded so a giant payload can't blow up the row.
	maxEmailLen = 320
)

// InviteMember mints a single-use invitation token bound to (organization,
// email, role), stores its HMAC, and emits an outbox event so the mailer
// worker can deliver the token via email. Re-inviting the same pending
// email rotates the token + resets the expiry in place rather than
// creating a second row — the handler upsert is how we enforce the "one
// pending invite per (org, email)" invariant.
func (s *Service) InviteMember(ctx context.Context, req *connect.Request[huddlev1.InviteMemberRequest]) (*connect.Response[huddlev1.InviteMemberResponse], error) {
	orgID, err := uuid.Parse(req.Msg.OrganizationId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid organization_id"))
	}
	email, err := normalizeEmail(req.Msg.Email)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	role, err := parseInviteRole(req.Msg.Role)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	// Same authz surface as AddMember — inviting a user is a step on the
	// path to making them a member, and owner role requires caller-owner.
	if err := s.authz.Authorize(ctx, caller.ID, policy.ActionAddMember, policy.Resource{
		Type:           "organization",
		ID:             orgID,
		OrganizationID: orgID,
	}); err != nil {
		return nil, s.denyErr(err)
	}
	if role == entinvitation.RoleOwner {
		callerRole, lookupErr := s.callerRole(ctx, caller.ID, orgID)
		if lookupErr != nil {
			return nil, connect.NewError(connect.CodeInternal, lookupErr)
		}
		if callerRole != entmembership.RoleOwner {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners can invite at owner role"))
		}
	}

	// Generate a fresh token per invite. Re-inviting the same (org,
	// email) rotates the token — the prior token goes stale instantly
	// because its hash gets overwritten below.
	plain, hashed, err := mintToken(s.invites.HMACSecret)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("mint token: %w", err))
	}

	expiresAt := time.Now().Add(s.invites.TTL)

	// Fetch org once so we can set the edge on create AND serialize for the
	// outbox payload. Also surfaces a clean 404 if the org doesn't exist,
	// rather than letting the FK fail at commit time.
	org, err := s.client.Organization.Get(ctx, orgID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("organization %s not found", orgID))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get org: %w", err))
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("begin tx: %w", err))
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Look for an existing pending invite for (org, email). Accepted rows
	// are terminal — ignore them, and if the caller is trying to re-invite
	// someone who already joined, AddMember's UNIQUE(membership) handles
	// the dupe at the actual add-member step.
	existing, err := tx.Invitation.Query().
		Where(
			entinvitation.HasOrganizationWith(organization.IDEQ(orgID)),
			entinvitation.EmailEQ(email),
			entinvitation.AcceptedAtIsNil(),
		).
		First(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("lookup pending invite: %w", err))
	}

	var invite *ent.Invitation
	if existing != nil {
		invite, err = tx.Invitation.UpdateOne(existing).
			SetTokenHash(hashed).
			SetTokenPlaintext(plain).
			SetExpiresAt(expiresAt).
			SetRole(role).
			ClearEmailSentAt(). // force re-send on the new token
			Save(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update pending invite: %w", err))
		}
	} else {
		invite, err = tx.Invitation.Create().
			SetEmail(email).
			SetRole(role).
			SetTokenHash(hashed).
			SetTokenPlaintext(plain).
			SetExpiresAt(expiresAt).
			SetOrganization(org).
			SetInvitedBy(caller).
			Save(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create invite: %w", err))
		}
	}

	// Emit an outbox event — NO plaintext token in the payload. The
	// mailer reads token_plaintext off the Invitation row directly; the
	// outbox event is for audit and future consumers that want to know
	// an invitation was issued. Payload is the Invitation proto without
	// the secret.
	payload, err := marshalInvitationEvent(invite, orgID, caller.ID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("marshal invite event: %w", err))
	}
	if _, err := tx.OutboxEvent.Create().
		SetAggregateType("invitation").
		SetAggregateID(invite.ID).
		SetEventType("invitation.created").
		SetSubject("huddle.invitations.created." + invite.ID.String()).
		SetPayload(payload).
		SetActorID(caller.ID).
		SetOrganizationID(orgID).
		SetResourceType("invitation").
		SetResourceID(invite.ID).
		Save(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("enqueue invite event: %w", err))
	}

	if err := tx.Commit(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	committed = true

	return connect.NewResponse(&huddlev1.InviteMemberResponse{
		Invitation: invitationToProto(invite, orgID, caller.ID),
	}), nil
}

// AcceptInvitation consumes a token out of an invitation email and
// creates the backing Membership row. The token itself is the gate — the
// caller must additionally be authenticated (so the created Membership
// binds to a real User), and their claims' email must match the email on
// the invitation (so forwarding the invite email to a third party can't
// silently hijack the slot).
func (s *Service) AcceptInvitation(ctx context.Context, req *connect.Request[huddlev1.AcceptInvitationRequest]) (*connect.Response[huddlev1.AcceptInvitationResponse], error) {
	if req.Msg.Token == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("token is required"))
	}
	rawToken, err := base64.RawURLEncoding.DecodeString(req.Msg.Token)
	if err != nil || len(rawToken) != tokenBytes {
		// Do NOT leak which part is wrong; an attacker doesn't need help.
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invitation token is invalid"))
	}
	hashed := hashToken(s.invites.HMACSecret, rawToken)

	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("begin tx: %w", err))
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	invite, err := tx.Invitation.Query().
		Where(entinvitation.TokenHashEQ(hashed)).
		WithOrganization().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invitation token is invalid"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("lookup invite: %w", err))
	}
	if invite.AcceptedAt != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invitation has already been accepted"))
	}
	if time.Now().After(invite.ExpiresAt) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invitation has expired"))
	}
	if !strings.EqualFold(caller.Email, invite.Email) {
		// Surfaced as PermissionDenied, not Unauthenticated, because the
		// caller IS authenticated — they just aren't the right person for
		// this token. Logged so an operator reviewing invite failures can
		// spot forwarding attempts.
		s.logger.Warn("invitation: email mismatch",
			"invite_id", invite.ID,
			"invite_email", invite.Email,
			"caller_email", caller.Email,
		)
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("invitation was issued to a different email"))
	}

	orgID := invite.Edges.Organization.ID

	// Create the membership. UNIQUE(user, org) handles the "user was added
	// through AddMember while the invite was pending" race cleanly —
	// second path returns AlreadyExists, which the caller can treat as
	// success for the accept flow if they want.
	m, err := tx.Membership.Create().
		SetUser(caller).
		SetOrganization(invite.Edges.Organization).
		SetRole(entmembership.Role(invite.Role)).
		Save(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("already a member of this organization"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create membership: %w", err))
	}

	if _, err := tx.Invitation.UpdateOne(invite).
		SetAcceptedAt(time.Now()).
		SetAcceptedBy(caller).
		ClearTokenPlaintext(). // redundant once the mailer has sent, but safe to clear on accept too
		Save(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("mark invite accepted: %w", err))
	}

	if err := tx.Commit(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	committed = true

	return connect.NewResponse(&huddlev1.AcceptInvitationResponse{
		Membership: membershipToProto(m, caller.ID, orgID),
	}), nil
}

// normalizeEmail validates shape and lowercases the address. Lowercasing
// is what makes the (org, email) uniqueness check behave case-insensitively
// without a functional index.
func normalizeEmail(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("email is required")
	}
	if len(trimmed) > maxEmailLen {
		return "", fmt.Errorf("email exceeds %d characters", maxEmailLen)
	}
	addr, err := mail.ParseAddress(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid email: %w", err)
	}
	return strings.ToLower(addr.Address), nil
}

// parseInviteRole is the invitation.Role equivalent of parseRole
// (organization.go). Both enums share the same string set; we declare
// them separately so ent doesn't couple the two tables.
func parseInviteRole(s string) (entinvitation.Role, error) {
	switch entinvitation.Role(s) {
	case entinvitation.RoleOwner:
		return entinvitation.RoleOwner, nil
	case entinvitation.RoleAdmin:
		return entinvitation.RoleAdmin, nil
	case entinvitation.RoleMember, "":
		return entinvitation.RoleMember, nil
	default:
		return "", fmt.Errorf("invalid role %q", s)
	}
}

// mintToken returns a fresh random token + its HMAC hash. The returned
// plaintext goes into Invitation.token_plaintext (read by the mailer),
// the hash goes into Invitation.token_hash (read by accept lookup).
func mintToken(secret []byte) ([]byte, []byte, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return nil, nil, fmt.Errorf("read random: %w", err)
	}
	return buf, hashToken(secret, buf), nil
}

// hashToken is HMAC-SHA256(secret, token). HMAC because:
//   - we want fast verify at accept time (no bcrypt-style work factor)
//   - the token is already 32B of crypto randomness, so slow hashing
//     adds no brute-force resistance
//   - the secret lives in server config, not alongside the hash in the
//     DB, so a DB dump alone is insufficient to recompute token→hash
func hashToken(secret, token []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(token)
	return m.Sum(nil)
}

func invitationToProto(inv *ent.Invitation, orgID, inviterID uuid.UUID) *huddlev1.Invitation {
	return &huddlev1.Invitation{
		Id:              inv.ID.String(),
		OrganizationId:  orgID.String(),
		Email:           inv.Email,
		Role:            string(inv.Role),
		InvitedByUserId: inviterID.String(),
		ExpiresAt:       timestamppb.New(inv.ExpiresAt),
		CreatedAt:       timestamppb.New(inv.CreatedAt),
	}
}

// marshalInvitationEvent serializes the Invitation proto with NO token
// fields — the outbox event is for audit + future consumers, not the
// mailer. This keeps the token out of audit_events (which has its own
// long retention) and out of any search / notifications projection.
func marshalInvitationEvent(inv *ent.Invitation, orgID, inviterID uuid.UUID) ([]byte, error) {
	return proto.Marshal(invitationToProto(inv, orgID, inviterID))
}
