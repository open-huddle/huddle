package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// MessageMention is the explicit join table between Message and User
// for @-mentions. Declaring it as a separate entity (rather than a
// JSON array on Message) is what lets us index "messages mentioning
// user X" cheaply — the query path the notifications consumer and
// any future "my mentions" UI both hit.
//
// Mentions are immutable. The Send handler writes the full set in the
// same transaction as the Message row; editing a message (ADR-0005
// deferred) would require a separate design call about whether edits
// can add/remove mentions or whether the original set stays as the
// audit shape.
type MessageMention struct {
	ent.Schema
}

func (MessageMention) Mixin() []ent.Mixin {
	return []ent.Mixin{UUIDMixin{}}
}

func (MessageMention) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("message_id", uuid.UUID{}).Immutable(),
		field.UUID("user_id", uuid.UUID{}).Immutable(),
	}
}

func (MessageMention) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("message", Message.Type).
			Ref("mentions").
			Required().
			Unique().
			Immutable().
			Field("message_id"),
		edge.From("user", User.Type).
			Ref("mentioned_in").
			Required().
			Unique().
			Immutable().
			Field("user_id"),
	}
}

func (MessageMention) Indexes() []ent.Index {
	return []ent.Index{
		// One mention row per (message, user). Double-mentioning the
		// same person in one message is a no-op.
		index.Fields("message_id", "user_id").Unique(),
		// "Messages that mention user X" — the reverse lookup the
		// future "my mentions" UI uses.
		index.Fields("user_id", "message_id"),
	}
}
