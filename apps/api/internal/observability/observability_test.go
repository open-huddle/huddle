package observability_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/open-huddle/huddle/apps/api/internal/observability"
)

// TestInit_Disabled is the path every existing deployment takes until an
// operator opts in. Init must NOT dial the OTLP endpoint, NOT register
// any provider, and return a no-op shutdown the caller can defer
// without a nil check.
func TestInit_Disabled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	shutdown, err := observability.Init(context.Background(), observability.Config{
		Enabled: false,
		// Deliberately bogus endpoint — if Init dialed it, this would
		// either error or block. The disabled path must skip the dial.
		OTLPEndpoint: "127.0.0.1:1",
		ServiceName:  "huddle-api-test",
	}, logger)
	if err != nil {
		t.Fatalf("Init disabled: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init returned nil shutdown — caller's defer would panic")
	}
	// Shutdown should be cheap and never error in the disabled case.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Errorf("disabled shutdown returned %v, want nil", err)
	}
}

// TestInit_EnabledNoCollector verifies Init does not BLOCK at startup
// when the OTLP collector is unreachable. otlptracegrpc.New defaults to
// a non-blocking dial (the gRPC client retries connections in the
// background), so a missing collector during local-dev `make api-run`
// without `--profile observability` should be a tolerable warning, not
// a fatal startup failure.
func TestInit_EnabledNoCollector(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdown, err := observability.Init(ctx, observability.Config{
		Enabled: true,
		// 127.0.0.1:1 is closed on every machine — no collector here.
		OTLPEndpoint:   "127.0.0.1:1",
		OTLPInsecure:   true,
		ServiceName:    "huddle-api-test",
		ServiceVersion: "test",
	}, logger)
	if err != nil {
		t.Fatalf("Init enabled (no collector): %v", err)
	}
	defer func() {
		// Shutdown must not hang even if the exporter has nothing to flush
		// to. Bound it to 2s so a regression here surfaces as a test
		// timeout, not a hung process.
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer drainCancel()
		if err := shutdown(drainCtx); err != nil {
			// Shutdown errors here are cosmetic — the exporter's deadline
			// expired trying to flush. Log; don't fail.
			t.Logf("shutdown with no collector: %v (expected)", err)
		}
	}()
}
