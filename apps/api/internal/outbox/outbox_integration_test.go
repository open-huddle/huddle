//go:build integration

// Integration tests that exercise FOR UPDATE SKIP LOCKED against real
// Postgres. SQLite has no equivalent, so this file is the only place the
// multi-replica claim semantics are actually verified.
//
// Run with `make test-integration` (Docker required).
package outbox_test

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"

	"github.com/open-huddle/huddle/apps/api/ent"
	"github.com/open-huddle/huddle/apps/api/ent/outboxevent"
	"github.com/open-huddle/huddle/apps/api/internal/outbox"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
)

// slowSpyBus records each publish and deliberately blocks on a shared
// release channel. Holding two concurrent PublishBatch goroutines inside
// their respective transactions is what forces the SKIP LOCKED path to
// actually engage — if one goroutine commits before the other even starts
// its SELECT, the test degenerates to "two sequential drains" which
// doesn't prove anything.
//
// arrivals is a buffered channel: the first `cap(arrivals)` publishes
// signal on it; later publishes (same or subsequent batches) drop the
// signal via a non-blocking send. The test waits for as many signals as
// it expects distinct goroutines to park, which proves both txs are
// simultaneously holding their claims before anyone releases.
type slowSpyBus struct {
	mu       sync.Mutex
	calls    []string // subjects, in publish order
	release  chan struct{}
	arrivals chan struct{}
}

func (b *slowSpyBus) Publish(_ context.Context, subject string, _ []byte) error {
	select {
	case b.arrivals <- struct{}{}:
	default:
	}
	<-b.release // wait for the test to unblock us
	b.mu.Lock()
	b.calls = append(b.calls, subject)
	b.mu.Unlock()
	return nil
}

func (b *slowSpyBus) subjects() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.calls))
	copy(out, b.calls)
	sort.Strings(out)
	return out
}

// seedOutboxRow writes a minimal un-published outbox row and returns it.
// Helper lives here (not in the shared test file) because the unit suite
// already has its own seedOutbox with a different signature.
func seedIntegrationOutboxRow(ctx context.Context, t *testing.T, client *ent.Client, i int) *ent.OutboxEvent {
	t.Helper()
	resourceID := uuid.New()
	row, err := client.OutboxEvent.Create().
		SetAggregateType("message").
		SetAggregateID(resourceID).
		SetEventType("message.created").
		SetSubject("huddle.messages.created.row-" + resourceID.String()).
		SetPayload([]byte{0x01}).
		SetResourceType("message").
		SetResourceID(resourceID).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed outbox row %d: %v", i, err)
	}
	return row
}

// TestPublishBatch_ConcurrentClaim_DisjointRows is the real proof of the
// multi-replica claim design: two goroutines calling PublishBatch at the
// same time, with small batches and a slow bus, must split the row set
// between them — no row should be published twice, and together they
// must cover every row.
func TestPublishBatch_ConcurrentClaim_DisjointRows(t *testing.T) {
	ctx := context.Background()
	client := testutil.NewPostgresClient(t)

	// Seed enough rows that several batches per goroutine are required.
	const totalRows = 20
	for i := range totalRows {
		seedIntegrationOutboxRow(ctx, t, client, i)
	}

	// Two publishers sharing the bus. Small batch size + slow publish keeps
	// overlap visible: by the time goroutine B starts its SELECT, goroutine
	// A is already holding its rows locked. SKIP LOCKED is what makes B's
	// SELECT return the *next* rows instead of the same ones.
	const batchSize = 5
	bus := &slowSpyBus{
		release:  make(chan struct{}),
		arrivals: make(chan struct{}, 2), // buffer == expected concurrent arrivals
	}
	pubA := outbox.NewPublisher(client, bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		outbox.WithBatchSize(batchSize),
		outbox.WithDialect(dialect.Postgres),
	)
	pubB := outbox.NewPublisher(client, bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		outbox.WithBatchSize(batchSize),
		outbox.WithDialect(dialect.Postgres),
	)

	// Drive both publishers concurrently. Each goroutine runs PublishBatch
	// until the table is empty; because the bus blocks on `release`,
	// progress only happens once the test releases.
	var wg sync.WaitGroup
	drain := func(p *outbox.Publisher) {
		defer wg.Done()
		drainCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		for {
			if err := p.PublishBatch(drainCtx); err != nil {
				t.Errorf("PublishBatch: %v", err)
				return
			}
			remaining, err := client.OutboxEvent.Query().
				Where(outboxevent.PublishedAtIsNil()).Count(drainCtx)
			if err != nil {
				t.Errorf("count: %v", err)
				return
			}
			if remaining == 0 {
				return
			}
		}
	}

	// First wave: each publisher opens a tx, SELECTs up to batchSize rows,
	// calls Publish *on its first row*, which blocks. Wait for one arrival
	// per goroutine — that proves both SELECTs returned non-empty disjoint
	// slices instead of one of them blocking on the other's lock.
	wg.Add(2)
	go drain(pubA)
	go drain(pubB)

	deadline := time.After(15 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-bus.arrivals:
		case <-deadline:
			t.Fatal("publishers never both arrived inside Publish — SKIP LOCKED may have deadlocked instead of skipping")
		}
	}

	// Release everyone. The goroutines commit their stamp, go back for the
	// next batch, and so on until the table drains. The bus's mutex serializes
	// the append, so we can inspect it once both goroutines exit.
	close(bus.release)
	wg.Wait()

	got := bus.subjects()
	if len(got) != totalRows {
		t.Fatalf("expected exactly %d publishes (no loss, no dup), got %d", totalRows, len(got))
	}
	// Verify no duplicate subjects — the core SKIP LOCKED guarantee.
	seen := make(map[string]int, len(got))
	for _, s := range got {
		seen[s]++
	}
	for subject, count := range seen {
		if count != 1 {
			t.Errorf("subject %s published %d times (want 1) — SKIP LOCKED failed to prevent double-claim", subject, count)
		}
	}

	// Sanity: all rows are stamped published_at.
	unpub, err := client.OutboxEvent.Query().
		Where(outboxevent.PublishedAtIsNil()).Count(ctx)
	if err != nil {
		t.Fatalf("count unpublished: %v", err)
	}
	if unpub != 0 {
		t.Errorf("want 0 unpublished rows after drain, got %d", unpub)
	}
}
