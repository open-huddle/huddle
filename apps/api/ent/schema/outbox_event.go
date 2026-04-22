package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// OutboxEvent is the transactional-outbox row that stands between the domain
// write and downstream delivery (NATS publish, audit log, future CDC). Every
// row is written inside the same DB transaction as the domain mutation it
// describes — the broker outage that used to drop best-effort publishes (see
// ADR-0007) cannot silently lose events anymore.
//
// Denormalized actor_id / organization_id / resource_* columns let consumers
// like the audit log work without re-reading the aggregate — keeps consumer
// logic simple and immune to later edits on the source row.
type OutboxEvent struct {
	ent.Schema
}

func (OutboxEvent) Mixin() []ent.Mixin {
	// Not TimeMixin: an outbox entry is immutable, so updated_at would just
	// be noise. created_at is declared directly below.
	return []ent.Mixin{UUIDMixin{}}
}

func (OutboxEvent) Fields() []ent.Field {
	return []ent.Field{
		field.Time("created_at").Default(time.Now).Immutable(),

		// "message" / "channel" / "organization" — the aggregate that emitted
		// the event. Strings, not an enum, so adding a new aggregate type does
		// not require a schema migration.
		field.String("aggregate_type").NotEmpty().Immutable(),
		field.UUID("aggregate_id", uuid.UUID{}).Immutable(),

		// "message.created", "channel.created", "member.added", ... Verb on
		// the aggregate. Same stability requirements as audit logs.
		field.String("event_type").NotEmpty().Immutable(),

		// NATS subject this event publishes to. Precomputed at write time so
		// the publisher worker has no dispatch logic — it reads the subject
		// off the row and publishes the payload verbatim.
		field.String("subject").NotEmpty().Immutable(),

		// Protobuf-serialized event body. Typed bytes keep the wire format
		// identical to what subscribers already decode.
		field.Bytes("payload").Immutable(),

		// Denormalized context for consumers (audit, notifications, search).
		// Nullable because some future events may be system-originated or
		// tenant-less (e.g. global admin ops).
		field.UUID("actor_id", uuid.UUID{}).Optional().Nillable().Immutable(),
		field.UUID("organization_id", uuid.UUID{}).Optional().Nillable().Immutable(),

		// resource_type / resource_id are what the audit log and future
		// observability surfaces index on — "show me everything that happened
		// to this message".
		field.String("resource_type").NotEmpty().Immutable(),
		field.UUID("resource_id", uuid.UUID{}).Immutable(),

		// Set when the NATS publish succeeded. Nil means "pending" — the
		// publisher worker's polling filter.
		field.Time("published_at").Optional().Nillable(),

		// Set when the OpenSearch indexer has written the doc. Nil means
		// "not yet indexed" — the search.Indexer worker's polling filter.
		// Separate from published_at because the publisher and the indexer
		// fail (and succeed) independently; one row may be published but
		// not indexed, or vice versa. Eventually also a precondition for
		// outbox GC: a row is only safe to trim once it is published,
		// audited, AND indexed.
		field.Time("indexed_at").Optional().Nillable(),
	}
}

func (OutboxEvent) Edges() []ent.Edge {
	return []ent.Edge{
		// 1:1. Unique on the parent side makes ent generate a UNIQUE
		// constraint on audit_events.outbox_event_id so duplicate audit rows
		// for the same outbox event fail at the DB, not just at runtime.
		edge.To("audit_event", AuditEvent.Type).Unique(),
	}
}

func (OutboxEvent) Indexes() []ent.Index {
	return []ent.Index{
		// Worker poll key: "WHERE published_at IS NULL ORDER BY created_at".
		// Leading column is nullable, which lets Postgres skip already-
		// published rows cheaply as the table grows.
		index.Fields("published_at", "created_at"),
		// Same shape as published_at — lets search.Indexer scan only the
		// un-indexed backlog on every tick as the table grows.
		index.Fields("indexed_at", "created_at"),
		// Audit-consumer cursor + general chronological queries.
		index.Fields("created_at", "id"),
	}
}
