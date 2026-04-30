package observability

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// PipelineMetrics owns the CDC-pipeline metric instruments — Slice C of
// ADR-0019. These complement the per-worker RED metrics from Slice B
// with the cross-component signals that answer "is the pipeline
// healthy?": end-to-end Send→Subscribe latency and outbox depth per
// consumer column.
//
// Replication-slot lag (the other load-bearing CDC signal) is collected
// by the postgres_exporter sidecar and shipped through the Prometheus
// receiver in the OTel collector — no Go-side instrument needed.
type PipelineMetrics struct {
	endToEndLatency metric.Float64Histogram
	depthGauge      metric.Int64ObservableGauge
	meter           metric.Meter
}

// NewPipelineMetrics builds the instrument set. Call after Init has
// set the global meter provider; when observability is disabled the
// global is the no-op default and the resulting instruments are
// zero-cost.
func NewPipelineMetrics() (*PipelineMetrics, error) {
	m := otel.Meter(workerInstrumentationScope)

	latency, err := m.Float64Histogram("huddle.pipeline.send_to_subscribe_seconds",
		metric.WithDescription("End-to-end latency from Send (outbox row created) to the Subscribe handler dispatching the corresponding NATS message."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("send_to_subscribe histogram: %w", err)
	}

	depth, err := m.Int64ObservableGauge("huddle.outbox.depth",
		metric.WithDescription("Outbox rows that have not yet been processed by the named consumer."),
		metric.WithUnit("{row}"),
	)
	if err != nil {
		return nil, fmt.Errorf("outbox.depth gauge: %w", err)
	}

	return &PipelineMetrics{
		endToEndLatency: latency,
		depthGauge:      depth,
		meter:           m,
	}, nil
}

// RecordSendToSubscribe stamps one observation of the end-to-end
// latency. `kind` distinguishes message.created / edited / deleted so
// the dashboard can surface per-verb percentiles. `latency` is the
// wall time from the outbox row's creation to dispatch; negative or
// extreme values (clock skew, replay) are dropped to keep the
// histogram interpretable.
//
// A nil receiver no-ops, mirroring the rest of the observability
// package.
func (p *PipelineMetrics) RecordSendToSubscribe(ctx context.Context, kind string, latency time.Duration) {
	if p == nil {
		return
	}
	seconds := latency.Seconds()
	// 24h cap is a sanity gate, not a sampling decision — anything
	// beyond is a clock-skew or replay artifact (NATS `MaxAge` is 24h
	// per events.NATS), and reporting it would skew percentiles.
	if seconds < 0 || seconds > 24*60*60 {
		return
	}
	p.endToEndLatency.Record(ctx, seconds, metric.WithAttributes(
		attribute.String("message.kind", kind),
	))
}

// OutboxDepthQuerier is the seam pipeline.go uses to read depth from
// the database. The production caller wires the *sql.DB; tests pass a
// stub that returns canned counts.
type OutboxDepthQuerier interface {
	OutboxDepth(ctx context.Context) (OutboxDepth, error)
}

// OutboxDepth is the per-consumer un-processed-row count, materialized
// from a single SQL query.
type OutboxDepth struct {
	Unpublished int64 // rows with published_at IS NULL — Debezium hasn't reached them
	Unindexed   int64 // rows with indexed_at IS NULL — search.Indexer hasn't reached them
	Unnotified  int64 // rows with notified_at IS NULL — notifications.Consumer hasn't reached them
	Unaudited   int64 // rows without a corresponding audit_events row
}

// Registration is what RegisterOutboxDepth returns. Wraps OTel's
// metric.Registration so the no-op observability-disabled path can hand
// back a value the caller can Unregister unconditionally; OTel's own
// Registration interface is unimplementable outside its package.
type Registration interface {
	Unregister() error
}

// RegisterOutboxDepth wires the outbox.depth gauge to a querier the
// OTel SDK invokes once per collection interval. Returns a Registration
// the caller defers Unregister on; the disabled-observability path
// returns a no-op so callers don't have to branch on it.
func (p *PipelineMetrics) RegisterOutboxDepth(querier OutboxDepthQuerier) (Registration, error) {
	if p == nil {
		return &pipelineRegistration{}, nil
	}
	if querier == nil {
		return nil, errors.New("nil outbox depth querier")
	}
	inner, err := p.meter.RegisterCallback(
		func(ctx context.Context, obs metric.Observer) error {
			depth, err := querier.OutboxDepth(ctx)
			if err != nil {
				// Don't propagate — a transient DB hiccup at scrape time
				// shouldn't poison every other instrument's collection.
				// The next scrape retries.
				return nil //nolint:nilerr // intentional swallow; see comment
			}
			obs.ObserveInt64(p.depthGauge, depth.Unpublished, metric.WithAttributes(attribute.String("consumer", "publisher")))
			obs.ObserveInt64(p.depthGauge, depth.Unindexed, metric.WithAttributes(attribute.String("consumer", "indexer")))
			obs.ObserveInt64(p.depthGauge, depth.Unnotified, metric.WithAttributes(attribute.String("consumer", "notifications")))
			obs.ObserveInt64(p.depthGauge, depth.Unaudited, metric.WithAttributes(attribute.String("consumer", "audit")))
			return nil
		},
		p.depthGauge,
	)
	if err != nil {
		return nil, err
	}
	return &pipelineRegistration{inner: inner}, nil
}

// pipelineRegistration adapts OTel's Registration to our nil-safe
// Registration interface. A zero value (no inner) Unregisters cleanly.
type pipelineRegistration struct {
	inner metric.Registration
}

func (r *pipelineRegistration) Unregister() error {
	if r == nil || r.inner == nil {
		return nil
	}
	return r.inner.Unregister()
}

// SQLOutboxDepthQuerier is the production OutboxDepthQuerier — runs a
// single aggregate query against the supplied *sql.DB.
type SQLOutboxDepthQuerier struct {
	db *sql.DB
}

// NewSQLOutboxDepthQuerier wires the production querier.
func NewSQLOutboxDepthQuerier(db *sql.DB) *SQLOutboxDepthQuerier {
	return &SQLOutboxDepthQuerier{db: db}
}

// outboxDepthQuery is the single round-trip used to materialize the
// gauge. FILTER clauses are evaluated in one scan; the NOT EXISTS
// subquery for audit lag uses the index on audit_events.outbox_event_id.
const outboxDepthQuery = `
SELECT
  count(*) FILTER (WHERE o.published_at IS NULL),
  count(*) FILTER (WHERE o.indexed_at IS NULL),
  count(*) FILTER (WHERE o.notified_at IS NULL),
  count(*) FILTER (WHERE NOT EXISTS (
    SELECT 1 FROM audit_events a WHERE a.outbox_event_id = o.id
  ))
FROM outbox_events o
`

// OutboxDepth runs the aggregate query and returns the four counts.
func (q *SQLOutboxDepthQuerier) OutboxDepth(ctx context.Context) (OutboxDepth, error) {
	row := q.db.QueryRowContext(ctx, outboxDepthQuery)
	var d OutboxDepth
	if err := row.Scan(&d.Unpublished, &d.Unindexed, &d.Unnotified, &d.Unaudited); err != nil {
		return OutboxDepth{}, fmt.Errorf("outbox depth query: %w", err)
	}
	return d, nil
}
