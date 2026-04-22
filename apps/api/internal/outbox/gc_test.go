package outbox_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-huddle/huddle/apps/api/ent"
	"github.com/open-huddle/huddle/apps/api/ent/auditevent"
	"github.com/open-huddle/huddle/apps/api/ent/outboxevent"
	"github.com/open-huddle/huddle/apps/api/internal/outbox"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
)

// stampOpts controls which markers are written on a seeded outbox row so
// tests can exercise each partial-stamp combination.
type stampOpts struct {
	createdAt   time.Time
	published   bool
	indexed     bool
	withAudit   bool
	publishedAt time.Time // defaults to createdAt when zero
	indexedAt   time.Time // defaults to createdAt when zero
}

// seedStampedOutbox inserts an outbox row at the given createdAt, then
// stamps published_at / indexed_at and creates a sibling AuditEvent as
// requested. Returns the outbox row id so tests can assert on it.
func seedStampedOutbox(ctx context.Context, t *testing.T, client *ent.Client, opts stampOpts) uuid.UUID {
	t.Helper()

	resourceID := uuid.New()
	row, err := client.OutboxEvent.Create().
		SetAggregateType("message").
		SetAggregateID(resourceID).
		SetEventType("message.created").
		SetSubject("huddle.messages.created." + resourceID.String()).
		SetPayload([]byte("proto-bytes")).
		SetResourceType("message").
		SetResourceID(resourceID).
		SetCreatedAt(opts.createdAt).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed outbox: %v", err)
	}

	if opts.published {
		ts := opts.publishedAt
		if ts.IsZero() {
			ts = opts.createdAt
		}
		if err := client.OutboxEvent.UpdateOneID(row.ID).SetPublishedAt(ts).Exec(ctx); err != nil {
			t.Fatalf("stamp published_at: %v", err)
		}
	}
	if opts.indexed {
		ts := opts.indexedAt
		if ts.IsZero() {
			ts = opts.createdAt
		}
		if err := client.OutboxEvent.UpdateOneID(row.ID).SetIndexedAt(ts).Exec(ctx); err != nil {
			t.Fatalf("stamp indexed_at: %v", err)
		}
	}
	if opts.withAudit {
		if _, err := client.AuditEvent.Create().
			SetOutboxEventID(row.ID).
			SetEventType(row.EventType).
			SetResourceType(row.ResourceType).
			SetResourceID(row.ResourceID).
			SetPayload(row.Payload).
			Save(ctx); err != nil {
			t.Fatalf("seed audit sibling: %v", err)
		}
	}
	return row.ID
}

func newGC(t *testing.T, retention time.Duration) (*ent.Client, *outbox.GC) {
	t.Helper()
	client := testutil.NewClient(t)
	gc := outbox.NewGC(client, slog.New(slog.NewTextHandler(io.Discard, nil)),
		outbox.WithGCRetention(retention),
	)
	return client, gc
}

func TestGC_DeletesOnlyFullyStampedAgedRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	retention := time.Hour
	client, gc := newGC(t, retention)

	aged := time.Now().Add(-2 * retention)
	fresh := time.Now().Add(-retention / 2)

	// The one row that should be deleted: fully stamped AND aged past retention.
	victim := seedStampedOutbox(ctx, t, client, stampOpts{
		createdAt: aged,
		published: true,
		indexed:   true,
		withAudit: true,
	})

	// Every other permutation: must survive.
	survivors := []uuid.UUID{
		// No stamps at all.
		seedStampedOutbox(ctx, t, client, stampOpts{createdAt: aged}),
		// Only published.
		seedStampedOutbox(ctx, t, client, stampOpts{createdAt: aged, published: true}),
		// Only indexed.
		seedStampedOutbox(ctx, t, client, stampOpts{createdAt: aged, indexed: true}),
		// Published + indexed but no audit sibling.
		seedStampedOutbox(ctx, t, client, stampOpts{createdAt: aged, published: true, indexed: true}),
		// Published + audited but not indexed.
		seedStampedOutbox(ctx, t, client, stampOpts{createdAt: aged, published: true, withAudit: true}),
		// Indexed + audited but not published.
		seedStampedOutbox(ctx, t, client, stampOpts{createdAt: aged, indexed: true, withAudit: true}),
		// Fully stamped but fresh — retention not yet elapsed.
		seedStampedOutbox(ctx, t, client, stampOpts{
			createdAt: fresh, published: true, indexed: true, withAudit: true,
		}),
	}

	n, err := gc.DeleteBatch(ctx)
	if err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 deletion, got %d", n)
	}

	if _, err := client.OutboxEvent.Get(ctx, victim); !ent.IsNotFound(err) {
		t.Errorf("victim row %s should be deleted; got err=%v", victim, err)
	}
	for _, id := range survivors {
		if _, err := client.OutboxEvent.Get(ctx, id); err != nil {
			t.Errorf("survivor row %s should still exist; got err=%v", id, err)
		}
	}
}

func TestGC_FKSetNullLeavesAuditRowIntact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	retention := time.Hour
	client, gc := newGC(t, retention)

	aged := time.Now().Add(-2 * retention)
	victim := seedStampedOutbox(ctx, t, client, stampOpts{
		createdAt: aged,
		published: true,
		indexed:   true,
		withAudit: true,
	})

	// Snapshot the denormalized audit fields before GC — they must survive
	// the outbox delete unchanged.
	before, err := client.AuditEvent.Query().
		Where(auditevent.OutboxEventIDEQ(victim)).
		Only(ctx)
	if err != nil {
		t.Fatalf("pre-gc audit lookup: %v", err)
	}

	if _, err := gc.DeleteBatch(ctx); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}

	// Same audit row by primary key, not by the now-null FK.
	after, err := client.AuditEvent.Get(ctx, before.ID)
	if err != nil {
		t.Fatalf("post-gc audit lookup: %v", err)
	}
	if after.OutboxEventID != nil {
		t.Errorf("outbox_event_id should be NULL after FK cascade, got %v", after.OutboxEventID)
	}
	if after.EventType != before.EventType || after.ResourceID != before.ResourceID {
		t.Errorf("denormalized fields lost on cascade: before=%+v after=%+v", before, after)
	}
}

func TestGC_UniqueConstraintAllowsMultipleNullFKs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	retention := time.Hour
	client, gc := newGC(t, retention)

	aged := time.Now().Add(-2 * retention)
	// Two fully-stamped aged rows — after GC, both audit siblings should
	// have NULL outbox_event_id without tripping the UNIQUE constraint.
	seedStampedOutbox(ctx, t, client, stampOpts{
		createdAt: aged, published: true, indexed: true, withAudit: true,
	})
	seedStampedOutbox(ctx, t, client, stampOpts{
		createdAt: aged, published: true, indexed: true, withAudit: true,
	})

	n, err := gc.DeleteBatch(ctx)
	if err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 deletions, got %d", n)
	}

	// Both audit rows exist with NULL outbox_event_id.
	audits, err := client.AuditEvent.Query().All(ctx)
	if err != nil {
		t.Fatalf("query audits: %v", err)
	}
	if len(audits) != 2 {
		t.Fatalf("want 2 audit rows post-GC, got %d", len(audits))
	}
	for _, a := range audits {
		if a.OutboxEventID != nil {
			t.Errorf("audit %s should have NULL outbox_event_id, got %v", a.ID, a.OutboxEventID)
		}
	}
}

func TestGC_RespectsBatchSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	retention := time.Hour
	client := testutil.NewClient(t)
	gc := outbox.NewGC(client, slog.New(slog.NewTextHandler(io.Discard, nil)),
		outbox.WithGCRetention(retention),
		outbox.WithGCBatchSize(3),
	)

	aged := time.Now().Add(-2 * retention)
	// 5 eligible rows; batch cap is 3, so one sweep drains 3 of them.
	for range 5 {
		seedStampedOutbox(ctx, t, client, stampOpts{
			createdAt: aged, published: true, indexed: true, withAudit: true,
		})
	}

	n, err := gc.DeleteBatch(ctx)
	if err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	if n != 3 {
		t.Fatalf("want 3 deletions under batch cap, got %d", n)
	}

	remaining, err := client.OutboxEvent.Query().Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 2 {
		t.Fatalf("want 2 rows remaining, got %d", remaining)
	}

	// A second sweep drains the rest.
	n2, err := gc.DeleteBatch(ctx)
	if err != nil {
		t.Fatalf("DeleteBatch second pass: %v", err)
	}
	if n2 != 2 {
		t.Fatalf("want 2 deletions on second pass, got %d", n2)
	}
}

func TestGC_EmptyOutboxIsNoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, gc := newGC(t, time.Hour)

	n, err := gc.DeleteBatch(ctx)
	if err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	if n != 0 {
		t.Fatalf("want 0 deletions on empty outbox, got %d", n)
	}
}

// Sanity check: the GC predicate uses the same field accessors the
// publisher and indexer use. If either column got renamed, this test
// would fail to compile — handy canary.
func TestGC_PredicateUsesPublishedAndIndexedColumns(t *testing.T) {
	t.Parallel()
	_ = outboxevent.PublishedAtNotNil
	_ = outboxevent.IndexedAtNotNil
	_ = outboxevent.HasAuditEvent
	_ = outboxevent.CreatedAtLT
}
