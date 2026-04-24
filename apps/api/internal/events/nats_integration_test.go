//go:build integration

// Integration tests that exercise the real NATS JetStream wire path:
// a publish on `huddle.messages.<verb>.<channel>` must arrive at every
// SubscribeMessages call for that channel, with the correct
// MessageEventKind, and must not leak into subscribers for other
// channels. SQLite tests can't cover this — JetStream is its own
// service — so this file is the canonical place where the
// publish/subscribe contract is verified end-to-end.
//
// Run with `make test-integration` (no Docker needed for this file
// specifically — the embedded nats-server runs in-process — but the
// integration build tag is shared with the Postgres suite, which does
// need Docker).
package events_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/open-huddle/huddle/apps/api/internal/events"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// newBus brings up an embedded NATS server, opens an events.NATS client
// against it, and registers cleanup. The server's StoreDir is per-test
// so JetStream state does not leak between cases.
func newBus(ctx context.Context, t *testing.T) *events.NATS {
	t.Helper()
	url := testutil.NewEmbeddedNATS(t)
	bus, err := events.Open(ctx, url, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(bus.Close)
	return bus
}

func mustMarshalMessage(t *testing.T, m *huddlev1.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// recvWithin pulls the next event off the subscriber's channel or
// reports a timeout. Returns ok=false on timeout AND on a closed
// channel; tests distinguish via the e value (nil on close vs
// non-nil on timeout in flight). Most tests just check ok.
func recvWithin(t *testing.T, ch <-chan *events.MessageEvent, d time.Duration) (*events.MessageEvent, bool) {
	t.Helper()
	select {
	case e, ok := <-ch:
		return e, ok
	case <-time.After(d):
		return nil, false
	}
}

// TestNATS_SubscribeMessages_AllThreeKinds is the core Realtime v2
// guarantee: a single subscriber call sees creates, edits, and
// deletes for its channel — not just creates. This is what the
// `huddle.messages.*.<channel>` wildcard filter is for.
func TestNATS_SubscribeMessages_AllThreeKinds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := newBus(ctx, t)
	channelID := uuid.New()
	messageID := uuid.New()

	sub, err := bus.SubscribeMessages(ctx, channelID)
	if err != nil {
		t.Fatalf("SubscribeMessages: %v", err)
	}

	payload := mustMarshalMessage(t, &huddlev1.Message{
		Id:        messageID.String(),
		ChannelId: channelID.String(),
		Body:      "hello",
	})

	for _, subj := range []string{
		events.SubjectMessageCreated(channelID),
		events.SubjectMessageEdited(channelID),
		events.SubjectMessageDeleted(channelID),
	} {
		if err := bus.Publish(ctx, subj, payload); err != nil {
			t.Fatalf("publish %s: %v", subj, err)
		}
	}

	wantKinds := []events.MessageEventKind{
		events.MessageEventCreated,
		events.MessageEventEdited,
		events.MessageEventDeleted,
	}
	for i, want := range wantKinds {
		got, ok := recvWithin(t, sub, 5*time.Second)
		if !ok {
			t.Fatalf("event %d (kind %v): timeout — subscriber did not receive", i, want)
		}
		if got.Kind != want {
			t.Errorf("event %d: kind = %v, want %v", i, got.Kind, want)
		}
		if got.Message == nil {
			t.Errorf("event %d: nil Message", i)
			continue
		}
		if got.Message.Id != messageID.String() {
			t.Errorf("event %d: id = %q, want %q", i, got.Message.Id, messageID.String())
		}
	}

	// No fourth event should arrive — we published exactly three.
	if extra, ok := recvWithin(t, sub, 200*time.Millisecond); ok {
		t.Errorf("unexpected extra event after the three published: %+v", extra)
	}
}

// TestNATS_SubscribeMessages_FilterScopedToChannel verifies the
// JetStream consumer's FilterSubject actually scopes by channel: a
// subscriber on channel A must NOT receive events published for
// channel B, even on the same verb. A bug here would let every
// subscriber see every message in the deployment — exactly the leak
// channel auth is supposed to prevent.
func TestNATS_SubscribeMessages_FilterScopedToChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := newBus(ctx, t)
	chanA := uuid.New()
	chanB := uuid.New()

	sub, err := bus.SubscribeMessages(ctx, chanA)
	if err != nil {
		t.Fatalf("SubscribeMessages chanA: %v", err)
	}

	payloadA := mustMarshalMessage(t, &huddlev1.Message{
		Id:        uuid.NewString(),
		ChannelId: chanA.String(),
		Body:      "for A",
	})
	payloadB := mustMarshalMessage(t, &huddlev1.Message{
		Id:        uuid.NewString(),
		ChannelId: chanB.String(),
		Body:      "for B — must not arrive",
	})

	// Publish to B first; if the filter were broken, B's event would
	// be the first thing the subscriber sees and the assert below
	// would catch it.
	if err := bus.Publish(ctx, events.SubjectMessageCreated(chanB), payloadB); err != nil {
		t.Fatalf("publish B: %v", err)
	}
	if err := bus.Publish(ctx, events.SubjectMessageCreated(chanA), payloadA); err != nil {
		t.Fatalf("publish A: %v", err)
	}

	got, ok := recvWithin(t, sub, 5*time.Second)
	if !ok {
		t.Fatalf("timed out waiting for A's event")
	}
	if got.Message == nil || got.Message.Body != "for A" {
		t.Fatalf("first event was not A's: %+v — filter is broken", got.Message)
	}
	if extra, ok := recvWithin(t, sub, 200*time.Millisecond); ok {
		t.Errorf("subscriber for chan A also received: %+v — channel filter is broken", extra)
	}
}

// TestNATS_SubscribeMessages_DeliverNew_SkipsBacklog verifies the
// DeliverNewPolicy on the consumer: events published *before*
// SubscribeMessages returns must not be delivered to that subscriber.
// This is the contract MessageService.List backfills against — if
// the bus replays history, clients would see duplicates after a
// reconnect.
func TestNATS_SubscribeMessages_DeliverNew_SkipsBacklog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := newBus(ctx, t)
	channelID := uuid.New()

	backlog := mustMarshalMessage(t, &huddlev1.Message{
		Id:        uuid.NewString(),
		ChannelId: channelID.String(),
		Body:      "published before subscribe",
	})
	if err := bus.Publish(ctx, events.SubjectMessageCreated(channelID), backlog); err != nil {
		t.Fatalf("publish backlog: %v", err)
	}

	sub, err := bus.SubscribeMessages(ctx, channelID)
	if err != nil {
		t.Fatalf("SubscribeMessages: %v", err)
	}

	if got, ok := recvWithin(t, sub, 300*time.Millisecond); ok {
		t.Errorf("backlog leaked through DeliverNew: %+v", got)
	}

	// Sanity: a fresh post-subscribe event still arrives.
	fresh := mustMarshalMessage(t, &huddlev1.Message{
		Id:        uuid.NewString(),
		ChannelId: channelID.String(),
		Body:      "after subscribe",
	})
	if err := bus.Publish(ctx, events.SubjectMessageCreated(channelID), fresh); err != nil {
		t.Fatalf("publish fresh: %v", err)
	}
	got, ok := recvWithin(t, sub, 5*time.Second)
	if !ok {
		t.Fatalf("post-subscribe event: timeout")
	}
	if got.Message == nil || got.Message.Body != "after subscribe" {
		t.Errorf("got %+v, want body=\"after subscribe\"", got.Message)
	}
}

// TestNATS_SubscribeMessages_ContextCancelClosesChannel verifies the
// teardown contract from events.Subscriber: when the caller's context
// is cancelled, the returned channel must be closed (not just stop
// receiving). MessageService.Subscribe relies on this — its handler
// loop exits when the channel closes.
func TestNATS_SubscribeMessages_ContextCancelClosesChannel(t *testing.T) {
	bus := newBus(context.Background(), t)
	channelID := uuid.New()

	subCtx, cancel := context.WithCancel(context.Background())
	sub, err := bus.SubscribeMessages(subCtx, channelID)
	if err != nil {
		t.Fatalf("SubscribeMessages: %v", err)
	}
	cancel()

	// Drain anything in flight, then expect close. The teardown
	// goroutine in nats.go waits on ctx.Done before close(out), so
	// the close happens promptly after cancel.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-sub:
			if !ok {
				return // closed — pass
			}
			// In-flight event drained; keep looping for the close.
		case <-deadline:
			t.Fatal("subscriber channel did not close within 2s of context cancel")
		}
	}
}
