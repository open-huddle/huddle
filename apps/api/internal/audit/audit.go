// Package audit turns outbox rows into compliance audit entries. It runs as
// a background consumer — polls OutboxEvent for rows that have no
// corresponding AuditEvent yet, mirrors the event shape into audit_events,
// and relies on the unique constraint on audit_events.outbox_event_id for
// idempotency across restarts.
//
// Audit is deliberately decoupled from NATS delivery: we log every event
// regardless of whether the broker succeeded, so compliance trails cannot be
// lost by a broker outage. Trades some freshness (poll cadence) for
// reliability.
package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/open-huddle/huddle/apps/api/ent"
	"github.com/open-huddle/huddle/apps/api/ent/outboxevent"
	"github.com/open-huddle/huddle/apps/api/internal/observability"
)

const (
	defaultInterval  = 2 * time.Second
	defaultBatchSize = 200
)

// Consumer mirrors OutboxEvent rows into AuditEvent.
type Consumer struct {
	client *ent.Client
	logger *slog.Logger

	interval  time.Duration
	batchSize int
	instr     *observability.WorkerInstr
}

type Option func(*Consumer)

func WithInterval(d time.Duration) Option {
	return func(c *Consumer) { c.interval = d }
}

func WithBatchSize(n int) Option {
	return func(c *Consumer) { c.batchSize = n }
}

// WithWorkerInstr wires per-tick + per-row spans and RED metrics. Optional;
// a nil instr (the test default) preserves full functional behavior with no
// observability side-effects.
func WithWorkerInstr(w *observability.WorkerInstr) Option {
	return func(c *Consumer) { c.instr = w }
}

func NewConsumer(client *ent.Client, logger *slog.Logger, opts ...Option) *Consumer {
	c := &Consumer{
		client:    client,
		logger:    logger,
		interval:  defaultInterval,
		batchSize: defaultBatchSize,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Run polls until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	if err := c.ConsumeBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
		c.logger.Warn("audit: initial drain", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.ConsumeBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
				c.logger.Warn("audit: consume batch", "err", err)
			}
		}
	}
}

// ConsumeBatch does one iteration. Exported so tests can drive the loop
// deterministically.
func (c *Consumer) ConsumeBatch(ctx context.Context) error {
	return c.instr.Tick(ctx, func(ctx context.Context) error {
		// "Outbox rows not yet audited" — ent translates this to NOT EXISTS
		// over the audit_event FK, which is index-backed on the unique
		// column.
		rows, err := c.client.OutboxEvent.Query().
			Where(outboxevent.Not(outboxevent.HasAuditEvent())).
			Order(ent.Asc(outboxevent.FieldCreatedAt)).
			Limit(c.batchSize).
			All(ctx)
		if err != nil {
			return fmt.Errorf("query un-audited: %w", err)
		}

		for _, row := range rows {
			rowCtx, end := c.instr.StartRow(ctx,
				attribute.String("outbox.id", row.ID.String()),
				attribute.String("event_type", row.EventType),
			)
			err := c.insertAuditEvent(rowCtx, row)
			if err != nil {
				// A concurrent consumer on another replica may have beaten us
				// to this row — the unique constraint on outbox_event_id
				// rejects the second write. Both "duplicate key" and genuine
				// errors land here; we log and move on. The next iteration
				// picks up anything that genuinely failed (it'll still lack
				// an audit row).
				c.logger.Warn("audit: insert", "err", err, "outbox_id", row.ID)
			}
			end(err)
		}
		return nil
	})
}

func (c *Consumer) insertAuditEvent(ctx context.Context, row *ent.OutboxEvent) error {
	create := c.client.AuditEvent.Create().
		SetOutboxEventID(row.ID).
		SetEventType(row.EventType).
		SetResourceType(row.ResourceType).
		SetResourceID(row.ResourceID).
		SetPayload(row.Payload)
	if row.ActorID != nil {
		create = create.SetActorID(*row.ActorID)
	}
	if row.OrganizationID != nil {
		create = create.SetOrganizationID(*row.OrganizationID)
	}
	return create.Exec(ctx)
}
