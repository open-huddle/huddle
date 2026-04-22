// Package outbox drains the transactional-outbox table (OutboxEvent) to the
// event bus. Domain handlers write outbox rows inside the same DB transaction
// as the originating mutation; this worker publishes them downstream and
// marks them done.
//
// Multi-replica safety: the publisher polls inside a transaction and uses
// `SELECT ... FOR UPDATE SKIP LOCKED` so two replicas drain disjoint rows.
// Per-row publish + stamp happens while the lock is held; commit releases
// ownership. Subscribers still key on message UUID, so a duplicate publish
// under pathological retry is safe, but the common case is now exactly-once.
// SQLite (unit tests) doesn't support the lock suffix; the publisher skips
// it on non-Postgres dialects — Postgres integration tests exercise the
// real path.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"

	"github.com/open-huddle/huddle/apps/api/ent"
	"github.com/open-huddle/huddle/apps/api/ent/outboxevent"
	"github.com/open-huddle/huddle/apps/api/internal/events"
)

const (
	defaultInterval  = time.Second
	defaultBatchSize = 100
)

// Publisher drains OutboxEvent rows to the bus.
type Publisher struct {
	client *ent.Client
	bus    events.Publisher
	logger *slog.Logger

	interval  time.Duration
	batchSize int
	// dialect gates FOR UPDATE SKIP LOCKED — only Postgres supports it.
	// Empty string (the default) means "don't apply the lock clause",
	// which is the right behavior for SQLite-backed unit tests.
	dialect string
}

// Option tunes Publisher; zero options yields production defaults.
type Option func(*Publisher)

// WithInterval sets the poll cadence. Shorter intervals reduce latency to
// subscribers at the cost of more idle queries.
func WithInterval(d time.Duration) Option {
	return func(p *Publisher) { p.interval = d }
}

// WithBatchSize caps how many rows one iteration may publish. Bounds the
// worst-case latency of a single loop body.
func WithBatchSize(n int) Option {
	return func(p *Publisher) { p.batchSize = n }
}

// WithDialect tells the publisher what SQL dialect it is running against.
// When set to dialect.Postgres, the poll query is suffixed with
// FOR UPDATE SKIP LOCKED so concurrent replicas drain disjoint rows. Other
// dialects skip the clause. Tests typically omit this option and exercise
// the unlocked path against SQLite.
func WithDialect(d string) Option {
	return func(p *Publisher) { p.dialect = d }
}

func NewPublisher(client *ent.Client, bus events.Publisher, logger *slog.Logger, opts ...Option) *Publisher {
	p := &Publisher{
		client:    client,
		bus:       bus,
		logger:    logger,
		interval:  defaultInterval,
		batchSize: defaultBatchSize,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Run polls until ctx is cancelled. Intended to run in its own goroutine.
// Errors inside a batch are logged and the loop continues — this is a
// background worker, not a request path.
func (p *Publisher) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	// Drain once immediately so startup doesn't wait an interval to pick up
	// anything enqueued while the worker was down.
	if err := p.PublishBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
		p.logger.Warn("outbox: initial drain", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.PublishBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
				p.logger.Warn("outbox: publish batch", "err", err)
			}
		}
	}
}

// PublishBatch does one drain iteration. Exported so tests can drive the
// loop deterministically.
//
// Runs inside a transaction so the SELECT can hold FOR UPDATE SKIP LOCKED
// on Postgres — other replicas skip our rows and pick up the next batch.
// The per-row publish runs while locks are held; commit releases them. On
// SQLite the lock clause is omitted and the tx is just a read-write wrapper.
func (p *Publisher) PublishBatch(ctx context.Context) error {
	tx, err := p.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	q := tx.OutboxEvent.Query().
		Where(outboxevent.PublishedAtIsNil()).
		Order(ent.Asc(outboxevent.FieldCreatedAt)).
		Limit(p.batchSize)
	if p.dialect == dialect.Postgres {
		q = q.ForUpdate(sql.WithLockAction(sql.SkipLocked))
	}
	rows, err := q.All(ctx)
	if err != nil {
		return fmt.Errorf("query unpublished: %w", err)
	}

	for _, row := range rows {
		// Publish errors are logged but don't abort the batch — a transient
		// broker failure on one row shouldn't stall the rest. The row stays
		// unpublished (its UPDATE never runs) and retries next iteration.
		if err := p.bus.Publish(ctx, row.Subject, row.Payload); err != nil {
			p.logger.Warn("outbox: publish", "err", err, "id", row.ID, "subject", row.Subject)
			continue
		}
		if err := tx.OutboxEvent.UpdateOneID(row.ID).
			SetPublishedAt(time.Now()).
			Exec(ctx); err != nil {
			// Failure to mark published is also non-fatal: the event *did*
			// reach NATS, and a re-publish on the next cycle is safe because
			// subscribers key on message UUID. Just warn loudly.
			p.logger.Warn("outbox: mark published", "err", err, "id", row.ID)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}
