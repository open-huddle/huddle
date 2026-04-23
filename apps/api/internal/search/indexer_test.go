package search_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/open-huddle/huddle/apps/api/ent"
	"github.com/open-huddle/huddle/apps/api/internal/search"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// fakeClient records IndexMessage calls so tests can assert the doc shape
// and drive failure modes. Search and EnsureIndex are no-ops here — the
// indexer only calls IndexMessage.
type fakeClient struct {
	mu      sync.Mutex
	calls   []fakeCall
	indexFn func(outboxID uuid.UUID, doc search.MessageDoc) error
}

type fakeCall struct {
	outboxID uuid.UUID
	doc      search.MessageDoc
}

func (f *fakeClient) EnsureIndex(context.Context) error { return nil }
func (f *fakeClient) SearchMessages(context.Context, search.MessageQuery) (search.MessageResult, error) {
	return search.MessageResult{}, nil
}

func (f *fakeClient) IndexMessage(_ context.Context, outboxID uuid.UUID, doc search.MessageDoc) error {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{outboxID: outboxID, doc: doc})
	f.mu.Unlock()
	if f.indexFn != nil {
		return f.indexFn(outboxID, doc)
	}
	return nil
}

func (f *fakeClient) snapshot() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

func newIndexer(t *testing.T) (*ent.Client, *fakeClient, *search.Indexer) {
	t.Helper()
	client := testutil.NewClient(t)
	fc := &fakeClient{}
	idx := search.NewIndexer(client, fc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return client, fc, idx
}

// seedMessageOutbox writes an OutboxEvent with a message.created payload.
// body and createdAt drive the decoded MessageDoc the test asserts on.
func seedMessageOutbox(ctx context.Context, t *testing.T, client *ent.Client, body string, createdAt time.Time) *ent.OutboxEvent {
	t.Helper()

	msgID := uuid.New()
	channelID := uuid.New()
	authorID := uuid.New()
	orgID := uuid.New()

	payload, err := proto.Marshal(&huddlev1.Message{
		Id:        msgID.String(),
		ChannelId: channelID.String(),
		AuthorId:  authorID.String(),
		Body:      body,
		CreatedAt: timestamppb.New(createdAt),
	})
	if err != nil {
		t.Fatalf("marshal message payload: %v", err)
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

func TestIndexBatch_IndexesAndStampsIndexedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, fc, idx := newIndexer(t)

	when := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	row := seedMessageOutbox(ctx, t, client, "hello **world**", when)

	if err := idx.IndexBatch(ctx); err != nil {
		t.Fatalf("IndexBatch: %v", err)
	}

	calls := fc.snapshot()
	if len(calls) != 1 {
		t.Fatalf("want 1 IndexMessage call, got %d", len(calls))
	}
	if calls[0].outboxID != row.ID {
		t.Errorf("outbox id: want %s got %s", row.ID, calls[0].outboxID)
	}
	if calls[0].doc.Body != "hello **world**" {
		t.Errorf("body: want %q got %q", "hello **world**", calls[0].doc.Body)
	}
	if !calls[0].doc.CreatedAt.Equal(when) {
		t.Errorf("created_at: want %s got %s", when, calls[0].doc.CreatedAt)
	}
	if calls[0].doc.OrganizationID == uuid.Nil {
		t.Errorf("organization_id should carry the outbox row's org, got nil")
	}

	got, err := client.OutboxEvent.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("reload outbox row: %v", err)
	}
	if got.IndexedAt == nil {
		t.Errorf("indexed_at should be stamped")
	}
}

func TestIndexBatch_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, fc, idx := newIndexer(t)

	seedMessageOutbox(ctx, t, client, "once", time.Now())

	if err := idx.IndexBatch(ctx); err != nil {
		t.Fatalf("drain 1: %v", err)
	}
	if err := idx.IndexBatch(ctx); err != nil {
		t.Fatalf("drain 2: %v", err)
	}

	calls := fc.snapshot()
	// Second drain should find no un-indexed rows because the first stamped
	// indexed_at — exactly one IndexMessage call total.
	if len(calls) != 1 {
		t.Fatalf("want 1 IndexMessage call after 2 drains, got %d", len(calls))
	}
}

// Non-message outbox rows (channel.created, invitation.created, future
// event types) must not cause an OpenSearch write — but they must have
// indexed_at stamped so outbox.GC can eventually trim them. Without the
// stamp, non-message rows would live in the outbox forever regardless of
// their publish + audit state.
func TestIndexBatch_StampsNonMessageEventsWithoutIndexing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, fc, idx := newIndexer(t)

	aggID := uuid.New()
	channelRow, err := client.OutboxEvent.Create().
		SetAggregateType("channel").
		SetAggregateID(aggID).
		SetEventType("channel.created").
		SetSubject("huddle.channels.created." + aggID.String()).
		SetPayload([]byte("irrelevant")).
		SetResourceType("channel").
		SetResourceID(aggID).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed channel outbox: %v", err)
	}

	msgRow := seedMessageOutbox(ctx, t, client, "take me", time.Now())

	if err := idx.IndexBatch(ctx); err != nil {
		t.Fatalf("IndexBatch: %v", err)
	}

	// Only the message.created row reaches the search backend.
	calls := fc.snapshot()
	if len(calls) != 1 {
		t.Fatalf("want 1 IndexMessage call, got %d", len(calls))
	}
	if calls[0].outboxID != msgRow.ID {
		t.Errorf("indexed the wrong row: want %s got %s", msgRow.ID, calls[0].outboxID)
	}

	// Both rows get their indexed_at stamped — the channel.created row
	// because the indexer explicitly marks non-message events as
	// "evaluated, nothing to do," the message row because IndexMessage
	// succeeded.
	reloadedChannel, err := client.OutboxEvent.Get(ctx, channelRow.ID)
	if err != nil {
		t.Fatalf("reload channel row: %v", err)
	}
	if reloadedChannel.IndexedAt == nil {
		t.Errorf("channel.created row should have indexed_at stamped (so GC can trim)")
	}
	reloadedMsg, err := client.OutboxEvent.Get(ctx, msgRow.ID)
	if err != nil {
		t.Fatalf("reload message row: %v", err)
	}
	if reloadedMsg.IndexedAt == nil {
		t.Errorf("message.created row should have indexed_at stamped")
	}
}

func TestIndexBatch_TransientBackendErrorLeavesRowUnindexed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, fc, idx := newIndexer(t)
	fc.indexFn = func(uuid.UUID, search.MessageDoc) error {
		return errors.New("opensearch down")
	}

	row := seedMessageOutbox(ctx, t, client, "hello", time.Now())

	if err := idx.IndexBatch(ctx); err != nil {
		t.Fatalf("IndexBatch: %v", err)
	}

	got, err := client.OutboxEvent.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.IndexedAt != nil {
		t.Errorf("indexed_at should NOT be stamped after a backend failure")
	}
}

func TestIndexBatch_MalformedPayloadIsStampedToAvoidHotLoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, fc, idx := newIndexer(t)

	// Garbage bytes that don't parse as a Message proto. The indexer must
	// stamp indexed_at so the bad row doesn't block the worker forever.
	orgID := uuid.New()
	aggID := uuid.New()
	row, err := client.OutboxEvent.Create().
		SetAggregateType("message").
		SetAggregateID(aggID).
		SetEventType("message.created").
		SetSubject("huddle.messages.created.x").
		SetPayload([]byte{0xff, 0xff, 0xff, 0xff}).
		SetOrganizationID(orgID).
		SetResourceType("message").
		SetResourceID(aggID).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed outbox: %v", err)
	}

	if err := idx.IndexBatch(ctx); err != nil {
		t.Fatalf("IndexBatch: %v", err)
	}

	if got := fc.snapshot(); len(got) != 0 {
		t.Errorf("IndexMessage should not be called for a malformed payload, got %d calls", len(got))
	}

	reloaded, err := client.OutboxEvent.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.IndexedAt == nil {
		t.Errorf("indexed_at should be stamped so the bad row doesn't hot-loop")
	}
}
