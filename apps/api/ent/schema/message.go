package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type Message struct {
	ent.Schema
}

func (Message) Mixin() []ent.Mixin {
	return []ent.Mixin{UUIDMixin{}, TimeMixin{}}
}

func (Message) Fields() []ent.Field {
	return []ent.Field{
		// Explicit FK fields rather than Ent's auto-generated edge columns.
		// This lets us put channel_id first in the cursor index — critical
		// for "messages in this channel, newest-first" being an index seek
		// rather than a scan-and-filter at scale.
		field.UUID("channel_id", uuid.UUID{}),
		field.UUID("author_id", uuid.UUID{}),

		// body is opaque text — clients render it as Markdown. The server does
		// not parse, format, or strip syntax. Length capped at 8 KiB to prevent
		// abuse of the row, the audit trail, and downstream search indexers.
		field.Text("body").NotEmpty().MaxLen(8192),
	}
}

func (Message) Edges() []ent.Edge {
	return []ent.Edge{
		// Required parent — every message belongs to exactly one channel.
		edge.From("channel", Channel.Type).
			Ref("messages").
			Required().
			Unique().
			Field("channel_id"),
		// Required author — every message has exactly one author.
		// Intentionally NOT Optional / SET NULL: deleting a user with messages
		// must be a conscious operation (scrub or reassign first), so the
		// audit trail can never silently lose attribution.
		edge.From("author", User.Type).
			Ref("messages").
			Required().
			Unique().
			Field("author_id"),
	}
}

func (Message) Indexes() []ent.Index {
	return []ent.Index{
		// (channel_id, created_at) is the cursor-pagination working set:
		// "newest messages in this channel" and "older than (created_at, id)".
		// Leading column is the channel filter so Postgres seeks rather than
		// scans-and-filters when a busy channel grows.
		index.Fields("channel_id", "created_at"),
	}
}
