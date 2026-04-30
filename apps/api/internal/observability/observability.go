// Package observability owns the OpenTelemetry SDK lifecycle for apps/api.
// Init builds resource + tracer + meter providers and registers them as the
// process globals; downstream code uses otel.Tracer / otel.Meter without
// having to thread the providers through every package.
//
// Slice A scope (ADR-0019): traces and metrics over OTLP/gRPC; logs stay on
// slog/stdout so the collector ingests them via Alloy/promtail later. The
// SDK is wired but app-code-level spans and metrics are deferred to Slice B
// — this package only sets up the framework instrumentation (HTTP server,
// Connect interceptor, database/sql wrapper) so you get zero-effort
// per-RPC and per-query spans on day one.
package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config carries the knobs Init reads. Mirrors the user-facing
// config.Observability struct; kept separate so this package has no
// dependency on the config package (which would create an import cycle
// the moment cmd/api/main.go wires both together).
type Config struct {
	// Enabled gates everything. When false, Init wires no exporters,
	// returns a no-op Shutdown, and leaves the global tracer/meter at
	// their default no-op implementations. Existing deployments stay
	// silent until an operator opts in.
	Enabled bool

	// OTLPEndpoint is the host:port of the OTLP/gRPC receiver. With the
	// `observability` compose profile this is `localhost:4317` (the
	// grafana/otel-lgtm container's collector port).
	OTLPEndpoint string

	// OTLPInsecure disables TLS on the OTLP transport. True for the
	// dev compose stack; false for any real deployment, where the
	// collector should sit behind mTLS via the service mesh.
	OTLPInsecure bool

	// ServiceName is the `service.name` resource attribute every span
	// and metric is tagged with. Defaults to "huddle-api".
	ServiceName string

	// ServiceVersion is the `service.version` resource attribute. The
	// `cfg.Version` build var is the natural source.
	ServiceVersion string
}

// Init wires the OTel SDK. Returns a Shutdown function the caller MUST
// invoke at process exit — span/metric batches are flushed there. If
// Enabled is false, Init returns a no-op Shutdown and no error; callers
// don't have to branch on the flag.
//
// The shutdown timeout the caller passes governs how long we wait for
// in-flight exports to drain. 5–10s is a reasonable upper bound; longer
// just delays SIGTERM-driven shutdown.
func Init(ctx context.Context, cfg Config, logger *slog.Logger) (Shutdown, error) {
	if !cfg.Enabled {
		logger.Info("observability disabled — set HUDDLE_OBSERVABILITY_ENABLED=true to opt in")
		return noopShutdown, nil
	}

	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	tp, err := buildTracerProvider(ctx, cfg, res)
	if err != nil {
		return nil, fmt.Errorf("build tracer provider: %w", err)
	}
	otel.SetTracerProvider(tp)

	mp, err := buildMeterProvider(ctx, cfg, res)
	if err != nil {
		// Tear the tracer provider down too — we don't want to leak a
		// half-initialized SDK on the failure path.
		_ = tp.Shutdown(ctx)
		return nil, fmt.Errorf("build meter provider: %w", err)
	}
	otel.SetMeterProvider(mp)

	// Trace context + baggage propagation. The W3C TraceContext header
	// (`traceparent`) is what `otelhttp` reads from incoming requests
	// and writes on outgoing ones; baggage is the user-data sibling.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	logger.Info("observability initialized",
		"otlp_endpoint", cfg.OTLPEndpoint,
		"otlp_insecure", cfg.OTLPInsecure,
		"service_name", cfg.ServiceName,
		"service_version", cfg.ServiceVersion)

	return func(ctx context.Context) error {
		// Shut both providers down. Errors from one don't stop the other —
		// we want to flush as much as possible.
		var errs []error
		if err := tp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("tracer provider shutdown: %w", err))
		}
		if err := mp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("meter provider shutdown: %w", err))
		}
		return errors.Join(errs...)
	}, nil
}

// Shutdown is the function returned by Init. Always non-nil; the
// disabled-observability path returns a no-op so callers don't need a
// nil check.
type Shutdown func(context.Context) error

func noopShutdown(_ context.Context) error { return nil }

// HTTPHandler wraps an http.Handler with otelhttp. The operation name
// "huddle-api" appears as the parent span name for every request; child
// spans (Connect handlers, DB queries) hang off it. Safe to call when
// observability is disabled — otelhttp uses the global no-op tracer.
func HTTPHandler(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "huddle-api")
}

// ConnectInterceptor returns the Connect interceptor that emits a span
// per RPC and records server-side metrics. Add it ahead of the auth
// interceptor so spans cover the auth check and any downstream work.
//
// Returns a slice (rather than a single connect.Interceptor) for forward
// compatibility — otelconnect's API has historically expanded what ships
// in the default constructor, and we want a single seam to update.
//
// otelconnect.NewInterceptor returns an error only when invalid options
// are passed; we pass none, so the error is structurally unreachable.
// Panicking on the impossible case is louder than silently substituting
// a no-op — if the otelconnect API changes such that the default args
// can fail, we want the process to refuse to start, not silently lose
// RPC instrumentation.
func ConnectInterceptor() []connect.Interceptor {
	interceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		panic(fmt.Sprintf("otelconnect.NewInterceptor with default options: %v", err))
	}
	return []connect.Interceptor{interceptor}
}

// buildResource gathers the attributes attached to every span and metric.
// service.name / service.version are mandatory; we add a handful of
// SDK-derived attrs (process.* / host.*) automatically via resource.Default.
func buildResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithFromEnv(),   // honors OTEL_RESOURCE_ATTRIBUTES env override
		resource.WithProcess(),   // process.* attrs
		resource.WithOS(),        // os.* attrs
		resource.WithContainer(), // container.* attrs (cgroup-derived)
		resource.WithHost(),      // host.* attrs
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
	)
}

func buildTracerProvider(ctx context.Context, cfg Config, res *resource.Resource) (*sdktrace.TracerProvider, error) {
	// One-shot exporter creation — connect dial is non-blocking, real
	// failures surface on first export.
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint)}
	if cfg.OTLPInsecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exporter, err := otlptrace.New(ctx, otlptracegrpc.NewClient(opts...))
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		// Batched span processor — bounded queue, periodic flush.
		// Defaults are fine: 5s batch, 2048 queue size, 512 max batch.
		sdktrace.WithBatcher(exporter),
	), nil
}

func buildMeterProvider(ctx context.Context, cfg Config, res *resource.Resource) (*metric.MeterProvider, error) {
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint)}
	if cfg.OTLPInsecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	exporter, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}
	return metric.NewMeterProvider(
		metric.WithResource(res),
		// Periodic reader — 30s default in OTel-Go is fine; tighter
		// would just spam the collector for no gain on a process
		// that's not throughput-sensitive.
		metric.WithReader(metric.NewPeriodicReader(exporter,
			metric.WithInterval(30*time.Second),
		)),
	), nil
}
