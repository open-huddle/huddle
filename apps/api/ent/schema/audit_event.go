package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AuditEvent is the compliance record of something that happened. It is the
// SOC 2 / HIPAA audit trail — not the operational queue (that's OutboxEvent).
// Separating the two lets their retention policies diverge: the outbox gets
// GC'd once the publishers have drained and the indexer has projected it;
// audit rows are kept for the retention window compliance asks for,
// untouched.
//
// Rows are written by the audit consumer by mirroring OutboxEvent fields.
// outbox_event_id is UNIQUE so re-running the consumer (restart, replay) is
// idempotent — the unique violation is the dedup signal. It is *nullable*
// because the outbox GC worker deletes the source row after retention,
// which SET NULLs the reference here. The denormalized fields below are
// the source of truth for compliance queries post-GC; the FK is purely an
// insert-time guard.
type AuditEvent struct {
	ent.Schema
}

func (AuditEvent) Mixin() []ent.Mixin {
	return []ent.Mixin{UUIDMixin{}}
}

func (AuditEvent) Fields() []ent.Field {
	return []ent.Field{
		field.Time("created_at").Default(time.Now).Immutable(),

		// Unique ref to the originating OutboxEvent. NULL after the outbox
		// row is GC'd — Postgres does that via ON DELETE SET NULL on the
		// FK below. Immutable from the app's side: the consumer sets it
		// once and never updates it; the DB's SET NULL bypasses the ORM.
		// UNIQUE on a nullable column is fine in Postgres (multiple NULLs
		// do not collide), so the at-insert dedup guarantee survives.
		field.UUID("outbox_event_id", uuid.UUID{}).
			Unique().
			Optional().
			Nillable().
			Immutable(),

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
		// Not Required — the FK column can go from a value to NULL under
		// the outbox GC worker. Immutable because the app never updates
		// the column; the transition to NULL is a DB-level cascade, not
		// an ent-emitted UPDATE. entsql SetNull annotation drives the
		// ON DELETE action in the generated migration.
		edge.From("outbox_event", OutboxEvent.Type).
			Ref("audit_event").
			Unique().
			Immutable().
			Field("outbox_event_id").
			Annotations(entsql.OnDelete(entsql.SetNull)),
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
