package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// NotificationPreference is a per-user, per-kind knob controlling whether
// the notifications.Mailer sends an email when a Notification of that
// kind lands. Stored "sparse" on purpose: the **absence** of a row means
// "use the default" (email enabled), which is the industry-standard
// opt-out shape for @-mentions. Users only create rows when they want to
// opt out; a reset-to-default is just a row delete.
//
// Per-kind (not per-channel) for MVP — matches ADR-0014's framing that
// preference granularity should land only when a second kind makes it
// meaningful. Per-channel and per-sender granularity are future PRs.
type NotificationPreference struct {
	ent.Schema
}

func (NotificationPreference) Mixin() []ent.Mixin {
	return []ent.Mixin{UUIDMixin{}, TimeMixin{}}
}

func (NotificationPreference) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}).Immutable(),

		// Keep the enum values aligned with Notification.kind — a kind
		// that can produce a Notification must be togglable here.
		field.Enum("kind").Values("mention").Immutable(),

		// When false, the mailer skips Notifications of this kind for
		// this user. Default true so a row created with no explicit
		// value still carries the industry-norm opt-in-to-email shape
		// (the only reason to have a row is to opt out, but defaulting
		// to true means a buggy insert doesn't silently silence someone).
		field.Bool("email_enabled").Default(true),
	}
}

func (NotificationPreference) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("notification_preferences").
			Required().
			Unique().
			Immutable().
			Field("user_id"),
	}
}

func (NotificationPreference) Indexes() []ent.Index {
	return []ent.Index{
		// One row per (user, kind). UPSERT on this key is how the
		// SetPreference handler enforces "no duplicates".
		index.Fields("user_id", "kind").Unique(),
	}
}
