package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const workerInstrumentationScope = "github.com/open-huddle/huddle/apps/api/internal/observability"

// WorkerMetrics holds the metric instruments every background worker
// reports to. One per process; share across workers by passing the same
// pointer to NewWorkerInstr. The single-instrument-with-attributes shape
// (rather than one counter per worker) is what lets Grafana queries say
// `sum by (worker)` without enumerating six metric names.
type WorkerMetrics struct {
	rowsProcessed metric.Int64Counter
	errors        metric.Int64Counter
	tickDuration  metric.Float64Histogram
}

// NewWorkerMetrics builds the shared instruments. Call after Init has
// set the global meter provider; when observability is disabled the
// global is the no-op default and the resulting instruments are
// zero-cost.
func NewWorkerMetrics() (*WorkerMetrics, error) {
	m := otel.Meter(workerInstrumentationScope)
	rows, err := m.Int64Counter("huddle.worker.rows_processed",
		metric.WithDescription("Rows processed by background workers."),
		metric.WithUnit("{row}"),
	)
	if err != nil {
		return nil, fmt.Errorf("rows_processed counter: %w", err)
	}
	errs, err := m.Int64Counter("huddle.worker.errors",
		metric.WithDescription("Errors recorded during background worker execution."),
		metric.WithUnit("{error}"),
	)
	if err != nil {
		return nil, fmt.Errorf("errors counter: %w", err)
	}
	dur, err := m.Float64Histogram("huddle.worker.tick_duration_seconds",
		metric.WithDescription("Duration of one tick of a background worker."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("tick_duration histogram: %w", err)
	}
	return &WorkerMetrics{rowsProcessed: rows, errors: errs, tickDuration: dur}, nil
}

// WorkerInstr is the per-worker handle. All methods are nil-safe — a
// worker constructed without an instr (typically a unit test) sees
// no-op spans and metrics with full functional behavior preserved.
type WorkerInstr struct {
	name    string
	tracer  trace.Tracer
	metrics *WorkerMetrics
}

// NewWorkerInstr returns an instrumentation handle for the named worker.
// `name` becomes the `worker` attribute on every metric and the prefix
// of every span name (e.g. `audit.tick`, `audit.row`).
func NewWorkerInstr(name string, metrics *WorkerMetrics) *WorkerInstr {
	return &WorkerInstr{
		name:    name,
		tracer:  otel.Tracer(workerInstrumentationScope),
		metrics: metrics,
	}
}

// Tick wraps a single iteration of a background worker. Records the
// duration histogram, opens a span "<worker>.tick", and increments the
// errors counter (scope=tick) when fn returns a non-nil error.
//
// A nil receiver runs fn directly — useful for tests that don't wire
// instrumentation.
func (w *WorkerInstr) Tick(ctx context.Context, fn func(ctx context.Context) error) error {
	if w == nil {
		return fn(ctx)
	}
	start := time.Now()
	ctx, span := w.tracer.Start(ctx, w.name+".tick")
	defer span.End()

	err := fn(ctx)

	w.metrics.tickDuration.Record(ctx, time.Since(start).Seconds(),
		metric.WithAttributes(attribute.String("worker", w.name)))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		w.metrics.errors.Add(ctx, 1, metric.WithAttributes(
			attribute.String("worker", w.name),
			attribute.String("scope", "tick"),
		))
	}
	return err
}

// StartRow opens a per-row span and returns the spanned context plus an
// end function. The end function MUST be called; pass the row's error
// (or nil) so the span is annotated and metrics recorded. A nil
// receiver returns ctx unchanged and a no-op end.
func (w *WorkerInstr) StartRow(ctx context.Context, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	if w == nil {
		return ctx, func(error) {}
	}
	ctx, span := w.tracer.Start(ctx, w.name+".row", trace.WithAttributes(attrs...))
	return ctx, func(err error) {
		defer span.End()
		w.metrics.rowsProcessed.Add(ctx, 1,
			metric.WithAttributes(attribute.String("worker", w.name)))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			w.metrics.errors.Add(ctx, 1, metric.WithAttributes(
				attribute.String("worker", w.name),
				attribute.String("scope", "row"),
			))
		}
	}
}

// AddRows increments rows_processed by n. For workers that operate in
// bulk (outbox.GC's single DELETE) where per-row spans don't apply.
// Negative or zero n is a no-op.
func (w *WorkerInstr) AddRows(ctx context.Context, n int) {
	if w == nil || n <= 0 {
		return
	}
	w.metrics.rowsProcessed.Add(ctx, int64(n),
		metric.WithAttributes(attribute.String("worker", w.name)))
}
