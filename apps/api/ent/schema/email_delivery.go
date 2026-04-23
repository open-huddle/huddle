package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// EmailDelivery records one attempt to send an invitation email. Every
// attempt — success or failure — appends a row; the mailer retries failed
// invitations on the next tick by polling Invitation (not this table).
//
// Retention is deliberately longer than the Invitation itself so operators
// can answer "did we actually email this person?" long after the invite
// expires. The log shape is also the skeleton the broader notifications
// consumer will reuse for non-invite emails.
type EmailDelivery struct {
	ent.Schema
}

func (EmailDelivery) Mixin() []ent.Mixin {
	return []ent.Mixin{UUIDMixin{}}
}

func (EmailDelivery) Fields() []ent.Field {
	return []ent.Field{
		field.Time("created_at").Default(time.Now).Immutable(),

		// Outcome. "sent" = email.Sender.Send returned nil; "failed" =
		// any error path. We do NOT distinguish transient vs permanent
		// failure here — the mailer retries everything with email_sent_at
		// still NULL, and the operator reads failure_reason to triage.
		field.Enum("status").Values("sent", "failed").Immutable(),

		// Populated only on status=failed. Opaque string from the
		// email.Sender implementation; do NOT parse — it's for humans.
		field.String("failure_reason").Optional().Immutable(),

		// Denormalized recipient email at send time so an audit query
		// doesn't have to join back to an Invitation row whose token_*
		// fields have since been cleared (or whose email might have been
		// re-cased by a later re-invite).
		field.String("recipient").NotEmpty().Immutable(),
	}
}

func (EmailDelivery) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("invitation", Invitation.Type).
			Ref("deliveries").
			Required().
			Unique().
			Immutable(),
	}
}

func (EmailDelivery) Indexes() []ent.Index {
	return []ent.Index{
		// "Most recent delivery attempt for this invitation" — the shape
		// the mailer uses when building its EmailDelivery query for
		// an individual invite.
		index.Edges("invitation").Fields("created_at"),
	}
}
