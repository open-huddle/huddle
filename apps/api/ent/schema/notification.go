package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Notification is a per-user in-app record of "something happened you
// might want to see" — for MVP that's just @-mentions in messages, but
// the kind enum is extensible so DM pings, thread replies, reactions,
// etc. can be piped through the same entity as those features land.
//
// The row is the source of truth for the in-app notifications inbox.
// When the email side lands (follow-up PR), a separate Mailer will
// read this table — Notification stays authoritative, email_sent_at
// lives on a column we add at that point. Same pattern as the
// invitations.Mailer / Invitation.email_sent_at split.
//
// Idempotency: UNIQUE(recipient, message, kind) prevents duplicate
// notifications for the same (recipient, message, kind) tuple. The
// notifications.Consumer relies on this for at-most-once semantics
// under restart / SKIP LOCKED concurrent claim.
type Notification struct {
	ent.Schema
}

func (Notification) Mixin() []ent.Mixin {
	return []ent.Mixin{UUIDMixin{}, TimeMixin{}}
}

func (Notification) Fields() []ent.Field {
	return []ent.Field{
		// The user this notification is for. Indexed below for the
		// "my unread notifications" query.
		field.UUID("recipient_user_id", uuid.UUID{}).Immutable(),

		// What kind of thing happened. Enum now rather than free string
		// so adding a new kind forces a schema migration (which forces
		// the consumer and the UI to handle it explicitly).
		field.Enum("kind").Values("mention").Immutable(),

		// Source message the notification fires on. Optional so a
		// future "system notification" kind (welcome email, org
		// announcement) can skip it.
		field.UUID("message_id", uuid.UUID{}).Optional().Nillable().Immutable(),

		// Channel the message was sent in. Denormalized (as in outbox
		// + audit) so list queries don't have to join through Message
		// just to know where to link the notification.
		field.UUID("channel_id", uuid.UUID{}).Optional().Nillable().Immutable(),

		// Tenant scope. Required: every Notification belongs to exactly
		// one organization. Indexed for the admin-ish "notifications in
		// this org" query path a future compliance export would use.
		field.UUID("organization_id", uuid.UUID{}).Immutable(),

		// Set when the recipient marks the notification as read via
		// NotificationService.MarkRead. NULL means unread — the default
		// filter for List.
		field.Time("read_at").Optional().Nillable(),

		// What event produced this notification. message_created
		// triggers email delivery; message_edited is in-app-only
		// (notifications.Mailer filters on this). Immutable because a
		// notification's provenance is fixed at creation.
		field.Enum("source").
			Values("message_created", "message_edited").
			Default("message_created").
			Immutable(),

		// Set when notifications.Mailer has dispatched the email. NULL
		// means "still pending send" — the mailer's polling filter.
		// Same pattern as Invitation.email_sent_at.
		field.Time("emailed_at").Optional().Nillable(),
	}
}

func (Notification) Edges() []ent.Edge {
	return []ent.Edge{
		// Required — every notification targets exactly one user.
		edge.From("recipient", User.Type).
			Ref("notifications").
			Required().
			Unique().
			Immutable().
			Field("recipient_user_id"),

		// Optional — a message-targeted notification FK's back for
		// display, but the notification survives message deletion
		// (cascade SET NULL via ent default when the edge is optional).
		edge.From("message", Message.Type).
			Ref("notifications").
			Unique().
			Immutable().
			Field("message_id"),

		edge.From("channel", Channel.Type).
			Ref("notifications").
			Unique().
			Immutable().
			Field("channel_id"),

		edge.From("organization", Organization.Type).
			Ref("notifications").
			Required().
			Unique().
			Immutable().
			Field("organization_id"),
	}
}

func (Notification) Indexes() []ent.Index {
	return []ent.Index{
		// "My unread notifications, newest first." Postgres can pick
		// this up as an index-only scan on the working set.
		index.Fields("recipient_user_id", "read_at", "created_at"),

		// Idempotency guard: at most one Notification per
		// (recipient, message, kind). UNIQUE on nullable message_id is
		// fine in Postgres (multiple NULLs don't collide); for system
		// notifications (no message) this lets many coexist, which is
		// the right behavior.
		index.Fields("recipient_user_id", "message_id", "kind").Unique(),

		// Tenant-scoped query key for future compliance exports.
		index.Fields("organization_id", "created_at"),
	}
}
