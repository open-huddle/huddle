package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Invitation represents an admin's offer to join an organization, pending
// acceptance by a specific email address. The row owns the one-time token
// state: token_hash is the lookup key at accept time (HMAC-SHA256 of the
// random token), and token_plaintext carries the not-yet-emailed secret
// that the invitations.Mailer worker reads, sends, and clears.
//
// The token_plaintext column exists at rest for at most one mailer tick
// (a few seconds in steady state) between InviteMember commit and the
// mailer stamping email_sent_at. See ADR-0013 for the compliance
// rationale and the rejected alternatives.
//
// Uniqueness: one pending (accepted_at IS NULL) invitation per
// (organization_id, email) is enforced at the handler level via upsert —
// a re-invite to the same pending email rotates the token and extends
// the expiry rather than creating a second row. The handler's check is
// authoritative; the (organization_id, email) index below is for lookup
// speed, not uniqueness.
type Invitation struct {
	ent.Schema
}

func (Invitation) Mixin() []ent.Mixin {
	return []ent.Mixin{UUIDMixin{}, TimeMixin{}}
}

func (Invitation) Fields() []ent.Field {
	return []ent.Field{
		// Invitee's email address. Handlers lowercase before insert so
		// email lookups and the (org, email) uniqueness rule are
		// case-insensitive.
		field.String("email").NotEmpty(),

		// Role the invitee will hold once they accept. Duplicates
		// Membership.role's enum values on purpose: ent doesn't share
		// enum types across schemas and the churn of keeping them in
		// sync is tiny.
		field.Enum("role").Values("owner", "admin", "member").Default("member"),

		// HMAC-SHA256(invites_secret, raw_token). Unique so accept-time
		// lookup is an index seek. NotEmpty guards against accidental
		// zero-hash inserts.
		field.Bytes("token_hash").NotEmpty().Unique(),

		// Plaintext token, held only until the mailer sends the email
		// then cleared to NULL. Required-at-insert (we always write
		// something here) but nullable post-send. Immutable from app
		// writes after creation; the only mutation is the clear-to-NULL
		// that the mailer performs.
		field.Bytes("token_plaintext").Optional().Nillable(),

		// When the invitation can no longer be accepted. Handler sets
		// this to created_at + invites.ttl (default 7d).
		field.Time("expires_at"),

		// Set by invitations.Mailer after email.Sender.Send succeeds.
		// NULL means "still pending send" — the mailer's polling filter.
		field.Time("email_sent_at").Optional().Nillable(),

		// Set by AcceptInvitation after the Membership row lands. NULL
		// means "still pending accept"; non-NULL rows are terminal.
		field.Time("accepted_at").Optional().Nillable(),
	}
}

func (Invitation) Edges() []ent.Edge {
	return []ent.Edge{
		// Required parent — every invitation belongs to exactly one org.
		edge.From("organization", Organization.Type).
			Ref("invitations").
			Required().
			Unique(),

		// Required author — the admin/owner who issued the invite.
		edge.From("invited_by", User.Type).
			Ref("invitations_sent").
			Required().
			Unique(),

		// Optional — set only on accept. Not Required so the column is
		// nullable; ent auto-generates the FK.
		edge.From("accepted_by", User.Type).
			Ref("invitations_accepted").
			Unique(),

		// Email send history — one per attempt, success or failure.
		edge.To("deliveries", EmailDelivery.Type),
	}
}

func (Invitation) Indexes() []ent.Index {
	return []ent.Index{
		// Lookup key for the handler's "find pending invite by (org, email)"
		// check. Not unique: accepted rows can pile up for the same (org,
		// email) pair over time, and the partial-unique "one pending at a
		// time" constraint is enforced in-handler, not at the DB.
		index.Edges("organization").Fields("email"),
	}
}
