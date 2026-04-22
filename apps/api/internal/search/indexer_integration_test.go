//go:build integration

// Integration test for the search indexer's SKIP LOCKED concurrent-claim
// behavior. Same shape as the outbox publisher integration test — two
// indexer goroutines running against a real Postgres must split the un-
// indexed row set between them, without either indexing a row twice.
//
// Run with `make test-integration` (Docker required).
package search_test

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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/open-huddle/huddle/apps/api/ent"
	"github.com/open-huddle/huddle/apps/api/ent/outboxevent"
	"github.com/open-huddle/huddle/apps/api/internal/search"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// slowFakeSearch mirrors outbox_integration_test.slowSpyBus: IndexMessage
// blocks on a shared release channel so both indexer goroutines can be
// parked inside their respective transactions at the same time. Without
// the block, one goroutine would race to completion before the other even
// started — a pass would be indistinguishable from a serial drain.
//
// arrivals is a buffered channel of size == expected concurrent arrivals;
// the first N parks signal, subsequent parks drop the signal silently.
type slowFakeSearch struct {
	mu       sync.Mutex
	calls    []uuid.UUID // outbox event IDs, in index order
	release  chan struct{}
	arrivals chan struct{}
}

func (f *slowFakeSearch) EnsureIndex(context.Context) error { return nil }
func (f *slowFakeSearch) SearchMessages(context.Context, search.MessageQuery) (search.MessageResult, error) {
	return search.MessageResult{}, nil
}
func (f *slowFakeSearch) IndexMessage(_ context.Context, outboxID uuid.UUID, _ search.MessageDoc) error {
	select {
	case f.arrivals <- struct{}{}:
	default:
	}
	<-f.release
	f.mu.Lock()
	f.calls = append(f.calls, outboxID)
	f.mu.Unlock()
	return nil
}
func (f *slowFakeSearch) seen() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]uuid.UUID(nil), f.calls...)
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// seedMessageRow writes an outbox row with a valid Message protobuf
// payload so the indexer's decode path succeeds.
func seedIntegrationMessageRow(ctx context.Context, t *testing.T, client *ent.Client) *ent.OutboxEvent {
	t.Helper()
	msgID := uuid.New()
	channelID := uuid.New()
	authorID := uuid.New()
	orgID := uuid.New()

	payload, err := proto.Marshal(&huddlev1.Message{
		Id:        msgID.String(),
		ChannelId: channelID.String(),
		AuthorId:  authorID.String(),
		Body:      "hello",
		CreatedAt: timestamppb.New(time.Now()),
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	row, err := client.OutboxEvent.Create().
		SetAggregateType("message").
		SetAggregateID(msgID).
		SetEventType("message.created").
		SetSubject("huddle.messages.created." + channelID.String()).
		SetPayload(payload).
		SetActorID(authorID).
		SetOrganizationID(orgID).
		SetResourceType("message").
		SetResourceID(msgID).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	return row
}

// TestIndexBatch_ConcurrentClaim_DisjointRows matches the outbox
// publisher's concurrent-claim test: two indexers, small batches, a slow
// backend, and the assertion that every un-indexed row is indexed by
// exactly one indexer (no dup, no loss).
func TestIndexBatch_ConcurrentClaim_DisjointRows(t *testing.T) {
	ctx := context.Background()
	client := testutil.NewPostgresClient(t)

	const totalRows = 20
	for range totalRows {
		seedIntegrationMessageRow(ctx, t, client)
	}

	const batchSize = 5
	backend := &slowFakeSearch{
		release:  make(chan struct{}),
		arrivals: make(chan struct{}, 2),
	}
	idxA := search.NewIndexer(client, backend,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		search.WithIndexerBatchSize(batchSize),
		search.WithIndexerDialect(dialect.Postgres),
	)
	idxB := search.NewIndexer(client, backend,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		search.WithIndexerBatchSize(batchSize),
		search.WithIndexerDialect(dialect.Postgres),
	)

	var wg sync.WaitGroup
	drain := func(idx *search.Indexer) {
		defer wg.Done()
		drainCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		for {
			if err := idx.IndexBatch(drainCtx); err != nil {
				t.Errorf("IndexBatch: %v", err)
				return
			}
			remaining, err := client.OutboxEvent.Query().
				Where(
					outboxevent.IndexedAtIsNil(),
					outboxevent.EventTypeEQ("message.created"),
				).Count(drainCtx)
			if err != nil {
				t.Errorf("count: %v", err)
				return
			}
			if remaining == 0 {
				return
			}
		}
	}

	// Expect each indexer to park inside IndexMessage for its first row.
	// That's the moment both txs are open — disjointness has to hold then.
	wg.Add(2)
	go drain(idxA)
	go drain(idxB)

	deadline := time.After(15 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-backend.arrivals:
		case <-deadline:
			t.Fatal("indexers never both arrived inside IndexMessage — SKIP LOCKED may have deadlocked instead of skipping")
		}
	}

	close(backend.release)
	wg.Wait()

	got := backend.seen()
	if len(got) != totalRows {
		t.Fatalf("expected exactly %d index calls (no loss, no dup), got %d", totalRows, len(got))
	}
	seen := make(map[uuid.UUID]int, len(got))
	for _, id := range got {
		seen[id]++
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("outbox %s indexed %d times (want 1) — SKIP LOCKED failed to prevent double-claim", id, count)
		}
	}

	// All rows stamped indexed_at.
	remaining, err := client.OutboxEvent.Query().
		Where(
			outboxevent.IndexedAtIsNil(),
			outboxevent.EventTypeEQ("message.created"),
		).Count(ctx)
	if err != nil {
		t.Fatalf("count un-indexed: %v", err)
	}
	if remaining != 0 {
		t.Errorf("want 0 un-indexed rows after drain, got %d", remaining)
	}
}
