package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AuditEvent is the compliance record of something that happened. It is the
// SOC 2 / HIPAA audit trail — not the operational queue (that's OutboxEvent).
// Separating the two lets their retention policies diverge: the outbox gets
// GC'd after the publishers have drained it; audit rows are kept for the
// retention window compliance asks for, untouched.
//
// Rows are written by the audit consumer by mirroring OutboxEvent fields.
// outbox_event_id is unique so re-running the consumer (restart, replay) is
// idempotent — the unique violation is the de-dup signal.
type AuditEvent struct {
	ent.Schema
}

func (AuditEvent) Mixin() []ent.Mixin {
	return []ent.Mixin{UUIDMixin{}}
}

func (AuditEvent) Fields() []ent.Field {
	return []ent.Field{
		field.Time("created_at").Default(time.Now).Immutable(),

		// Unique ref to the originating OutboxEvent. Enforces at-most-once
		// audit per outbox row even if the consumer replays. Backs the
		// outbox_event edge below.
		field.UUID("outbox_event_id", uuid.UUID{}).Unique().Immutable(),

		// Mirrored from the outbox event — denormalized on purpose so audit
		// queries never join back to a source table that might be deleted or
		// edited after the fact.
		field.String("event_type").NotEmpty().Immutable(),
		field.UUID("actor_id", uuid.UUID{}).Optional().Nillable().Immutable(),
		field.UUID("organization_id", uuid.UUID{}).Optional().Nillable().Immutable(),
		field.String("resource_type").NotEmpty().Immutable(),
		field.UUID("resource_id", uuid.UUID{}).Immutable(),
		field.Bytes("payload").Immutable(),
	}
}

func (AuditEvent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("outbox_event", OutboxEvent.Type).
			Ref("audit_event").
			Unique().
			Required().
			Immutable().
			Field("outbox_event_id"),
	}
}

func (AuditEvent) Indexes() []ent.Index {
	return []ent.Index{
		// "what did actor X do?" — the single most common audit query.
		index.Fields("actor_id", "created_at"),
		// "what happened in tenant X?" — org-scoped audit views.
		index.Fields("organization_id", "created_at"),
	}
}
