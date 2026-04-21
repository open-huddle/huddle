// Package outbox drains the transactional-outbox table (OutboxEvent) to the
// event bus. Domain handlers write outbox rows inside the same DB transaction
// as the originating mutation; this worker publishes them downstream and
// marks them done.
//
// Single-replica dedup is fine for now: each API replica runs one Publisher;
// with multiple replicas the same row may publish twice. NATS subscribers are
// expected to be idempotent (they key on the message UUID). A proper fix —
// `SELECT ... FOR UPDATE SKIP LOCKED` or advisory-lock leader election — is a
// known follow-up; it's not in this PR because it's Postgres-specific and we
// don't have multi-replica deployments running yet.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

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
func (p *Publisher) PublishBatch(ctx context.Context) error {
	rows, err := p.client.OutboxEvent.Query().
		Where(outboxevent.PublishedAtIsNil()).
		Order(ent.Asc(outboxevent.FieldCreatedAt)).
		Limit(p.batchSize).
		All(ctx)
	if err != nil {
		return fmt.Errorf("query unpublished: %w", err)
	}

	for _, row := range rows {
		// Publish errors are logged but don't abort the batch — a transient
		// broker failure on one row shouldn't stall the rest. The row stays
		// unpublished and retries next iteration.
		if err := p.bus.Publish(ctx, row.Subject, row.Payload); err != nil {
			p.logger.Warn("outbox: publish", "err", err, "id", row.ID, "subject", row.Subject)
			continue
		}
		if err := p.client.OutboxEvent.UpdateOneID(row.ID).
			SetPublishedAt(time.Now()).
			Exec(ctx); err != nil {
			// Failure to mark published is also non-fatal: the event *did*
			// reach NATS, and a re-publish on the next cycle is safe because
			// subscribers key on message UUID. Just warn loudly.
			p.logger.Warn("outbox: mark published", "err", err, "id", row.ID)
		}
	}
	return nil
}
