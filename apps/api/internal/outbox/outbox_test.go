package outbox_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/open-huddle/huddle/apps/api/ent"
	"github.com/open-huddle/huddle/apps/api/ent/outboxevent"
	"github.com/open-huddle/huddle/apps/api/internal/outbox"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
)

// spyBus records publish calls and can be told to fail. Satisfies
// events.Publisher via the narrow Publish(subject, payload) shape.
type spyBus struct {
	mu       sync.Mutex
	calls    []call
	failOnce error // returned on the next call, then cleared
}

type call struct {
	subject string
	payload []byte
}

func (b *spyBus) Publish(_ context.Context, subject string, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failOnce != nil {
		err := b.failOnce
		b.failOnce = nil
		return err
	}
	// copy the slice so later mutations from the caller can't confuse
	// assertions.
	p := make([]byte, len(payload))
	copy(p, payload)
	b.calls = append(b.calls, call{subject: subject, payload: p})
	return nil
}

func (b *spyBus) copyCalls() []call {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]call, len(b.calls))
	copy(out, b.calls)
	return out
}

func seedOutbox(ctx context.Context, t *testing.T, client *ent.Client, subject string, payload []byte) *ent.OutboxEvent {
	t.Helper()
	row, err := client.OutboxEvent.Create().
		SetAggregateType("message").
		SetAggregateID(uuid.New()).
		SetEventType("message.created").
		SetSubject(subject).
		SetPayload(payload).
		SetResourceType("message").
		SetResourceID(uuid.New()).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	return row
}

func newPublisher(t *testing.T) (*ent.Client, *spyBus, *outbox.Publisher) {
	t.Helper()
	client := testutil.NewClient(t)
	bus := &spyBus{}
	pub := outbox.NewPublisher(client, bus, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return client, bus, pub
}

func TestPublishBatch_PublishesAndMarksPublished(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, bus, pub := newPublisher(t)

	row := seedOutbox(ctx, t, client, "huddle.messages.created.chan-a", []byte("payload-a"))

	if err := pub.PublishBatch(ctx); err != nil {
		t.Fatalf("PublishBatch: %v", err)
	}

	// Bus saw exactly one call with the seeded subject + payload.
	calls := bus.copyCalls()
	if len(calls) != 1 {
		t.Fatalf("want 1 publish, got %d", len(calls))
	}
	if calls[0].subject != "huddle.messages.created.chan-a" || string(calls[0].payload) != "payload-a" {
		t.Fatalf("unexpected call: %+v", calls[0])
	}

	// Row is now marked published_at.
	refreshed, err := client.OutboxEvent.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if refreshed.PublishedAt == nil {
		t.Fatal("want published_at set, still nil")
	}
}

func TestPublishBatch_DoesNotRepublishAlreadyPublished(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, bus, pub := newPublisher(t)

	seedOutbox(ctx, t, client, "huddle.messages.created.x", []byte("one"))

	// First drain publishes the row.
	if err := pub.PublishBatch(ctx); err != nil {
		t.Fatalf("drain 1: %v", err)
	}
	if got := len(bus.copyCalls()); got != 1 {
		t.Fatalf("want 1 call after first drain, got %d", got)
	}

	// Second drain must not re-publish — the row is already marked.
	if err := pub.PublishBatch(ctx); err != nil {
		t.Fatalf("drain 2: %v", err)
	}
	if got := len(bus.copyCalls()); got != 1 {
		t.Fatalf("want still 1 call after second drain, got %d", got)
	}
}

func TestPublishBatch_PublishFailure_LeavesRowPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, bus, pub := newPublisher(t)

	row := seedOutbox(ctx, t, client, "huddle.messages.created.x", []byte("one"))

	// First attempt fails — row must stay unpublished so the next cycle retries.
	bus.failOnce = errors.New("simulated broker outage")
	if err := pub.PublishBatch(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	refreshed, err := client.OutboxEvent.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if refreshed.PublishedAt != nil {
		t.Fatalf("publish failed but row is marked published_at=%v", refreshed.PublishedAt)
	}

	// Next cycle succeeds.
	if err := pub.PublishBatch(ctx); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := len(bus.copyCalls()); got != 1 {
		t.Fatalf("want 1 successful publish after retry, got %d", got)
	}
	refreshed, err = client.OutboxEvent.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("reload after retry: %v", err)
	}
	if refreshed.PublishedAt == nil {
		t.Fatal("retry succeeded but row still not marked published")
	}
}

func TestPublishBatch_RespectsBatchSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := testutil.NewClient(t)
	bus := &spyBus{}
	// Batch size 2 against 5 seeded rows: one drain = 2 publishes.
	pub := outbox.NewPublisher(client, bus, slog.New(slog.NewTextHandler(io.Discard, nil)), outbox.WithBatchSize(2))

	for i := 0; i < 5; i++ {
		seedOutbox(ctx, t, client, "huddle.messages.created.x", []byte{byte(i)})
	}

	if err := pub.PublishBatch(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := len(bus.copyCalls()); got != 2 {
		t.Fatalf("want 2 publishes per batch, got %d", got)
	}

	// Unpublished count should be 5 - 2 = 3.
	remaining, err := client.OutboxEvent.Query().Where(outboxevent.PublishedAtIsNil()).Count(ctx)
	if err != nil {
		t.Fatalf("count unpublished: %v", err)
	}
	if remaining != 3 {
		t.Fatalf("want 3 rows still pending, got %d", remaining)
	}
}
