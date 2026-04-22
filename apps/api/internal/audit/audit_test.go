package audit_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/open-huddle/huddle/apps/api/ent"
	"github.com/open-huddle/huddle/apps/api/ent/auditevent"
	"github.com/open-huddle/huddle/apps/api/internal/audit"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
)

func seedOutbox(ctx context.Context, t *testing.T, client *ent.Client, opts outboxOpts) *ent.OutboxEvent {
	t.Helper()
	create := client.OutboxEvent.Create().
		SetAggregateType(opts.aggregateType).
		SetAggregateID(opts.aggregateID).
		SetEventType(opts.eventType).
		SetSubject(opts.subject).
		SetPayload(opts.payload).
		SetResourceType(opts.resourceType).
		SetResourceID(opts.resourceID)
	if opts.actorID != uuid.Nil {
		create = create.SetActorID(opts.actorID)
	}
	if opts.organizationID != uuid.Nil {
		create = create.SetOrganizationID(opts.organizationID)
	}
	row, err := create.Save(ctx)
	if err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	return row
}

type outboxOpts struct {
	aggregateType  string
	aggregateID    uuid.UUID
	eventType      string
	subject        string
	payload        []byte
	actorID        uuid.UUID
	organizationID uuid.UUID
	resourceType   string
	resourceID     uuid.UUID
}

func newConsumer(t *testing.T) (*ent.Client, *audit.Consumer) {
	t.Helper()
	client := testutil.NewClient(t)
	return client, audit.NewConsumer(client, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestConsumeBatch_MirrorsOutboxFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, cons := newConsumer(t)

	actorID := uuid.New()
	orgID := uuid.New()
	resourceID := uuid.New()
	row := seedOutbox(ctx, t, client, outboxOpts{
		aggregateType:  "message",
		aggregateID:    resourceID,
		eventType:      "message.created",
		subject:        "huddle.messages.created.x",
		payload:        []byte("proto-bytes"),
		actorID:        actorID,
		organizationID: orgID,
		resourceType:   "message",
		resourceID:     resourceID,
	})

	if err := cons.ConsumeBatch(ctx); err != nil {
		t.Fatalf("ConsumeBatch: %v", err)
	}

	audits, err := client.AuditEvent.Query().All(ctx)
	if err != nil {
		t.Fatalf("query audits: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("want 1 audit row, got %d", len(audits))
	}
	a := audits[0]
	if a.OutboxEventID == nil || *a.OutboxEventID != row.ID {
		t.Errorf("outbox_event_id: want %s got %v", row.ID, a.OutboxEventID)
	}
	if a.EventType != "message.created" {
		t.Errorf("event_type: want message.created got %s", a.EventType)
	}
	if a.ResourceType != "message" {
		t.Errorf("resource_type: want message got %s", a.ResourceType)
	}
	if a.ResourceID != resourceID {
		t.Errorf("resource_id: want %s got %s", resourceID, a.ResourceID)
	}
	if a.ActorID == nil || *a.ActorID != actorID {
		t.Errorf("actor_id: want %s got %v", actorID, a.ActorID)
	}
	if a.OrganizationID == nil || *a.OrganizationID != orgID {
		t.Errorf("organization_id: want %s got %v", orgID, a.OrganizationID)
	}
	if string(a.Payload) != "proto-bytes" {
		t.Errorf("payload: want %q got %q", "proto-bytes", string(a.Payload))
	}
}

func TestConsumeBatch_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, cons := newConsumer(t)

	seedOutbox(ctx, t, client, outboxOpts{
		aggregateType: "message", aggregateID: uuid.New(),
		eventType: "message.created", subject: "s", payload: []byte("p"),
		resourceType: "message", resourceID: uuid.New(),
	})

	// Two drains in a row must leave exactly one audit row. The unique
	// constraint on audit_events.outbox_event_id is the dedup mechanism.
	if err := cons.ConsumeBatch(ctx); err != nil {
		t.Fatalf("drain 1: %v", err)
	}
	if err := cons.ConsumeBatch(ctx); err != nil {
		t.Fatalf("drain 2: %v", err)
	}

	count, err := client.AuditEvent.Query().Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("want 1 audit row after 2 drains, got %d", count)
	}
}

func TestConsumeBatch_SkipsAlreadyAudited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, cons := newConsumer(t)

	// Two outbox rows; one already has an audit entry.
	oldRow := seedOutbox(ctx, t, client, outboxOpts{
		aggregateType: "message", aggregateID: uuid.New(),
		eventType: "message.created", subject: "s-old", payload: []byte("old"),
		resourceType: "message", resourceID: uuid.New(),
	})
	if _, err := client.AuditEvent.Create().
		SetOutboxEventID(oldRow.ID).
		SetEventType(oldRow.EventType).
		SetResourceType(oldRow.ResourceType).
		SetResourceID(oldRow.ResourceID).
		SetPayload(oldRow.Payload).
		Save(ctx); err != nil {
		t.Fatalf("pre-seed audit: %v", err)
	}

	newRow := seedOutbox(ctx, t, client, outboxOpts{
		aggregateType: "message", aggregateID: uuid.New(),
		eventType: "message.created", subject: "s-new", payload: []byte("new"),
		resourceType: "message", resourceID: uuid.New(),
	})

	if err := cons.ConsumeBatch(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// We should now have exactly 2 audit rows: the pre-seeded one and the
	// one the consumer just created for the new outbox row.
	count, err := client.AuditEvent.Query().Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("want 2 audit rows, got %d", count)
	}

	// Verify the new row is linked to newRow, not oldRow.
	newAudit, err := client.AuditEvent.Query().
		Where(auditevent.OutboxEventIDEQ(newRow.ID)).
		Only(ctx)
	if err != nil {
		t.Fatalf("lookup new audit: %v", err)
	}
	if newAudit.ResourceID != newRow.ResourceID {
		t.Errorf("new audit resource_id mismatch: got %s want %s", newAudit.ResourceID, newRow.ResourceID)
	}
}

func TestConsumeBatch_HandlesNilActorAndOrg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, cons := newConsumer(t)

	// System-originated event: no actor, no org. Payload must be non-empty —
	// the DB enforces NOT NULL on the column; ent stores an empty slice as
	// NULL and the insert would fail.
	seedOutbox(ctx, t, client, outboxOpts{
		aggregateType: "system", aggregateID: uuid.New(),
		eventType: "system.boot", subject: "huddle.system.boot", payload: []byte("boot"),
		resourceType: "system", resourceID: uuid.New(),
		// actorID, organizationID intentionally zero → skipped by seedOutbox
	})

	if err := cons.ConsumeBatch(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	a, err := client.AuditEvent.Query().Only(ctx)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if a.ActorID != nil {
		t.Errorf("want nil actor_id, got %v", a.ActorID)
	}
	if a.OrganizationID != nil {
		t.Errorf("want nil organization_id, got %v", a.OrganizationID)
	}
}
