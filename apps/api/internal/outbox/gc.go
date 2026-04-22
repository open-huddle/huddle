package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/open-huddle/huddle/apps/api/ent"
	"github.com/open-huddle/huddle/apps/api/ent/outboxevent"
)

// GC trims the outbox table. A row is eligible for deletion only when all
// three downstream markers are set:
//
//   - published_at IS NOT NULL      (outbox.Publisher reached NATS)
//   - indexed_at IS NOT NULL        (search.Indexer wrote the projection)
//   - HasAuditEvent                 (audit.Consumer mirrored the row)
//
// ...and the row is older than the retention window. The ordering between
// GC and the new consumers is structural: a consumer that lands later must
// also stamp its own column (or sibling row) before GC considers the event
// done. See ADR-0011.
//
// Deletion triggers the ON DELETE SET NULL on audit_events.outbox_event_id
// — the audit row survives with its denormalized fields intact, just
// without its pointer back to the (now-deleted) outbox row. That is the
// schema's way of saying "audit outlives outbox".
type GC struct {
	client *ent.Client
	logger *slog.Logger

	retention time.Duration
	interval  time.Duration
	batchSize int
}

// GCOption configures a GC; zero options yields production defaults.
type GCOption func(*GC)

// WithGCRetention sets the minimum age before a fully-stamped row is
// eligible for deletion. Must be > 0 — a zero retention would delete rows
// the same tick they were stamped.
func WithGCRetention(d time.Duration) GCOption {
	return func(g *GC) { g.retention = d }
}

// WithGCInterval sets the GC poll cadence. Much looser than publisher or
// indexer cadence — GC is housekeeping, not a hot path.
func WithGCInterval(d time.Duration) GCOption {
	return func(g *GC) { g.interval = d }
}

// WithGCBatchSize caps rows deleted per tick. Bounds worst-case lock
// footprint on a single DELETE; large backlogs drain across several ticks.
func WithGCBatchSize(n int) GCOption {
	return func(g *GC) { g.batchSize = n }
}

const (
	defaultGCRetention = 24 * time.Hour
	defaultGCInterval  = 5 * time.Minute
	defaultGCBatchSize = 500
)

// NewGC wires a GC worker against the given ent client. Operators tune
// retention via config; interval and batch size are usually fine at their
// defaults.
func NewGC(client *ent.Client, logger *slog.Logger, opts ...GCOption) *GC {
	g := &GC{
		client:    client,
		logger:    logger,
		retention: defaultGCRetention,
		interval:  defaultGCInterval,
		batchSize: defaultGCBatchSize,
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Run polls until ctx is cancelled. Errors inside a batch are logged and
// the loop continues — GC is background housekeeping, never a request
// path.
func (g *GC) Run(ctx context.Context) {
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()

	// Drain once on startup so a long-running publisher outage that left
	// retention-aged rows behind gets cleaned up without waiting a full
	// interval. Safe — the predicate is self-gating.
	if _, err := g.DeleteBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
		g.logger.Warn("outbox-gc: initial sweep", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := g.DeleteBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
				g.logger.Warn("outbox-gc: sweep", "err", err)
			}
		}
	}
}

// DeleteBatch does one sweep. Returns the number of rows deleted so tests
// (and, later, observability) can reason about progress.
//
// Postgres has no DELETE ... LIMIT, so the worker runs a two-step:
// collect up to batchSize eligible IDs in one query, then DELETE WHERE id
// IN (...). Ent's bulk Delete becomes a single round-trip on both
// Postgres and SQLite, and the FK's ON DELETE SET NULL handles the audit
// row fixup server-side.
func (g *GC) DeleteBatch(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-g.retention)

	ids, err := g.client.OutboxEvent.Query().
		Where(
			outboxevent.PublishedAtNotNil(),
			outboxevent.IndexedAtNotNil(),
			outboxevent.HasAuditEvent(),
			outboxevent.CreatedAtLT(cutoff),
		).
		Order(ent.Asc(outboxevent.FieldCreatedAt)).
		Limit(g.batchSize).
		IDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("collect eligible ids: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	deleted, err := g.client.OutboxEvent.Delete().
		Where(outboxevent.IDIn(ids...)).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete %d rows: %w", len(ids), err)
	}
	if deleted != len(ids) {
		// Only log; a row may have been deleted by a concurrent GC on
		// another replica between the select and the delete. That is the
		// design — the eventual consistency between replicas is safe
		// because the predicate only matches fully-stamped, retention-aged
		// rows.
		g.logger.Info("outbox-gc: partial delete",
			"eligible", len(ids), "deleted", deleted)
	}
	return deleted, nil
}
