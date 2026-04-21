package message_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/open-huddle/huddle/apps/api/ent"
	entmembership "github.com/open-huddle/huddle/apps/api/ent/membership"
	"github.com/open-huddle/huddle/apps/api/internal/auth"
	"github.com/open-huddle/huddle/apps/api/internal/policy"
	"github.com/open-huddle/huddle/apps/api/internal/principal"
	"github.com/open-huddle/huddle/apps/api/internal/services/message"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// spyPublisher records PublishMessageCreated calls. fail, when non-nil, is
// returned on every call — exercises the best-effort swallow path in Send.
type spyPublisher struct {
	mu        sync.Mutex
	published []*huddlev1.Message
	fail      error
}

func (p *spyPublisher) PublishMessageCreated(_ context.Context, msg *huddlev1.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return p.fail
	}
	p.published = append(p.published, msg)
	return nil
}

func (p *spyPublisher) calls() []*huddlev1.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*huddlev1.Message, len(p.published))
	copy(out, p.published)
	return out
}

// nopSubscriber satisfies events.Subscriber for tests that don't exercise the
// streaming path. Returns a closed channel so any unexpected Subscribe call
// fails fast rather than blocks.
type nopSubscriber struct{}

func (nopSubscriber) SubscribeMessages(_ context.Context, _ uuid.UUID) (<-chan *huddlev1.Message, error) {
	ch := make(chan *huddlev1.Message)
	close(ch)
	return ch, nil
}

type fixture struct {
	ctx       context.Context
	client    *ent.Client
	svc       *message.Service
	publisher *spyPublisher
	org       *ent.Organization
	ch        *ent.Channel
	alice     *ent.User // member of org
}

// newFixture sets up: an org, a channel in it, Alice as a member of the org.
// Bob (non-member) can be added per-test via callerCtx + auto-upsert through
// the resolver.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	client := testutil.NewClient(t)
	resolver := principal.NewResolver(client)
	engine := policy.NewRBAC(client)
	pub := &spyPublisher{}
	svc := message.New(client, resolver, engine, pub, nopSubscriber{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	org := testutil.MakeOrg(ctx, t, client, "acme")
	alice := testutil.MakeUser(ctx, t, client, "alice")
	testutil.MakeMember(ctx, t, client, alice, org, entmembership.RoleMember)
	ch, err := client.Channel.Create().
		SetName("general").
		SetSlug("general").
		SetOrganization(org).
		SetCreatedBy(alice).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed channel: %v", err)
	}

	return &fixture{ctx: ctx, client: client, svc: svc, publisher: pub, org: org, ch: ch, alice: alice}
}

func callerCtx(ctx context.Context, subject string) context.Context {
	return auth.WithClaims(ctx, auth.Claims{
		Subject:       subject,
		Email:         subject + "@example.test",
		EmailVerified: true,
		Name:          subject,
	})
}

func connectCode(t *testing.T, err error) connect.Code {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error is not *connect.Error: %v", err)
	}
	return ce.Code()
}

// --- Send -------------------------------------------------------------------

func TestSend_InvalidArguments(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	for _, tc := range []struct {
		name string
		req  *huddlev1.MessageServiceSendRequest
	}{
		{"bad channel id", &huddlev1.MessageServiceSendRequest{ChannelId: "not-a-uuid", Body: "hi"}},
		{"empty body", &huddlev1.MessageServiceSendRequest{ChannelId: f.ch.ID.String(), Body: ""}},
		{"body too long", &huddlev1.MessageServiceSendRequest{ChannelId: f.ch.ID.String(), Body: strings.Repeat("x", 8193)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Send(ctx, connect.NewRequest(tc.req))
			if got := connectCode(t, err); got != connect.CodeInvalidArgument {
				t.Fatalf("want InvalidArgument, got %s", got)
			}
		})
	}
}

func TestSend_NoClaims_Unauthenticated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.svc.Send(f.ctx, connect.NewRequest(&huddlev1.MessageServiceSendRequest{
		ChannelId: f.ch.ID.String(),
		Body:      "hi",
	}))
	if got := connectCode(t, err); got != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %s", got)
	}
}

func TestSend_ChannelNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	_, err := f.svc.Send(ctx, connect.NewRequest(&huddlev1.MessageServiceSendRequest{
		ChannelId: uuid.NewString(),
		Body:      "hi",
	}))
	if got := connectCode(t, err); got != connect.CodeNotFound {
		t.Fatalf("want NotFound, got %s", got)
	}
}

func TestSend_NonMemberOfOrg_PermissionDenied(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "bob")

	_, err := f.svc.Send(ctx, connect.NewRequest(&huddlev1.MessageServiceSendRequest{
		ChannelId: f.ch.ID.String(),
		Body:      "hi",
	}))
	if got := connectCode(t, err); got != connect.CodePermissionDenied {
		t.Fatalf("want PermissionDenied, got %s", got)
	}
}

func TestSend_HappyPath_PersistsAndPublishes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	resp, err := f.svc.Send(ctx, connect.NewRequest(&huddlev1.MessageServiceSendRequest{
		ChannelId: f.ch.ID.String(),
		Body:      "hello world",
	}))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp.Msg.Message.Body != "hello world" || resp.Msg.Message.ChannelId != f.ch.ID.String() {
		t.Fatalf("unexpected message: %+v", resp.Msg.Message)
	}
	if resp.Msg.Message.AuthorId != f.alice.ID.String() {
		t.Fatalf("want author %s, got %s", f.alice.ID, resp.Msg.Message.AuthorId)
	}

	// DB row landed.
	count, err := f.client.Message.Query().Count(f.ctx)
	if err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if count != 1 {
		t.Fatalf("want 1 message persisted, got %d", count)
	}

	// Publisher was invoked with the same proto — subscribers get the live
	// copy without an extra DB hop.
	calls := f.publisher.calls()
	if len(calls) != 1 {
		t.Fatalf("want 1 publish call, got %d", len(calls))
	}
	if calls[0].Id != resp.Msg.Message.Id {
		t.Fatalf("publish id %s != response id %s", calls[0].Id, resp.Msg.Message.Id)
	}
}

func TestSend_PublishFailure_StillSucceeds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.publisher.fail = errors.New("simulated nats outage")
	ctx := callerCtx(f.ctx, "alice")

	// Best-effort publish: a broker outage must not roll back the database
	// write. Send returns 200; subscribers backfill via List.
	resp, err := f.svc.Send(ctx, connect.NewRequest(&huddlev1.MessageServiceSendRequest{
		ChannelId: f.ch.ID.String(),
		Body:      "hello",
	}))
	if err != nil {
		t.Fatalf("send should swallow publish error, got %v", err)
	}
	if resp.Msg.Message.Id == "" {
		t.Fatal("want persisted message id, got empty")
	}
	count, err := f.client.Message.Query().Count(f.ctx)
	if err != nil || count != 1 {
		t.Fatalf("want 1 persisted message after publish failure, got count=%d err=%v", count, err)
	}
}

// --- List -------------------------------------------------------------------

func TestList_InvalidArguments(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	_, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.MessageServiceListRequest{
		ChannelId: "not-a-uuid",
	}))
	if got := connectCode(t, err); got != connect.CodeInvalidArgument {
		t.Fatalf("bad channel id: want InvalidArgument, got %s", got)
	}

	_, err = f.svc.List(ctx, connect.NewRequest(&huddlev1.MessageServiceListRequest{
		ChannelId: f.ch.ID.String(),
		Before:    "not-base64!!!",
	}))
	if got := connectCode(t, err); got != connect.CodeInvalidArgument {
		t.Fatalf("bad cursor: want InvalidArgument, got %s", got)
	}
}

func TestList_NoClaims_Unauthenticated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.svc.List(f.ctx, connect.NewRequest(&huddlev1.MessageServiceListRequest{
		ChannelId: f.ch.ID.String(),
	}))
	if got := connectCode(t, err); got != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %s", got)
	}
}

func TestList_ChannelNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	_, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.MessageServiceListRequest{
		ChannelId: uuid.NewString(),
	}))
	if got := connectCode(t, err); got != connect.CodeNotFound {
		t.Fatalf("want NotFound, got %s", got)
	}
}

func TestList_NonMember_PermissionDenied(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "bob")

	_, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.MessageServiceListRequest{
		ChannelId: f.ch.ID.String(),
	}))
	if got := connectCode(t, err); got != connect.CodePermissionDenied {
		t.Fatalf("want PermissionDenied, got %s", got)
	}
}

func TestList_Empty(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	resp, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.MessageServiceListRequest{
		ChannelId: f.ch.ID.String(),
	}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp.Msg.Messages) != 0 || resp.Msg.NextCursor != "" {
		t.Fatalf("want empty list + empty cursor, got %+v", resp.Msg)
	}
}

func TestList_NewestFirst_UnderLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	sendAll(ctx, t, f, []string{"first", "second", "third"})

	resp, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.MessageServiceListRequest{
		ChannelId: f.ch.ID.String(),
	}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"third", "second", "first"} // newest first
	got := bodies(resp.Msg.Messages)
	if !equalSlice(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	if resp.Msg.NextCursor != "" {
		t.Fatalf("under limit: want empty cursor, got %q", resp.Msg.NextCursor)
	}
}

func TestList_CursorPagination(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	sendAll(ctx, t, f, []string{"one", "two", "three", "four", "five"})

	// Page 1: limit 3 → newest 3 + cursor.
	page1, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.MessageServiceListRequest{
		ChannelId: f.ch.ID.String(),
		Limit:     3,
	}))
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if got := bodies(page1.Msg.Messages); !equalSlice(got, []string{"five", "four", "three"}) {
		t.Fatalf("page 1: want [five four three], got %v", got)
	}
	if page1.Msg.NextCursor == "" {
		t.Fatal("page 1: want cursor, got empty")
	}

	// Page 2: using cursor → remaining 2, no cursor.
	page2, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.MessageServiceListRequest{
		ChannelId: f.ch.ID.String(),
		Limit:     3,
		Before:    page1.Msg.NextCursor,
	}))
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if got := bodies(page2.Msg.Messages); !equalSlice(got, []string{"two", "one"}) {
		t.Fatalf("page 2: want [two one], got %v", got)
	}
	if page2.Msg.NextCursor != "" {
		t.Fatalf("page 2: want empty cursor at end, got %q", page2.Msg.NextCursor)
	}
}

func TestList_DefaultAndClampedLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	// Seed 3 messages and verify zero limit falls back to default (50) — we
	// get all 3 without a cursor. Can't easily test the 200-max clamp without
	// seeding >200 messages; the pagination test above already proves the
	// limit-is-honoured path.
	sendAll(ctx, t, f, []string{"a", "b", "c"})

	zero, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.MessageServiceListRequest{
		ChannelId: f.ch.ID.String(),
		Limit:     0,
	}))
	if err != nil {
		t.Fatalf("limit=0: %v", err)
	}
	if len(zero.Msg.Messages) != 3 || zero.Msg.NextCursor != "" {
		t.Fatalf("limit=0: want 3 messages, no cursor, got %d msgs cursor=%q", len(zero.Msg.Messages), zero.Msg.NextCursor)
	}

	negative, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.MessageServiceListRequest{
		ChannelId: f.ch.ID.String(),
		Limit:     -10,
	}))
	if err != nil {
		t.Fatalf("limit=-10: %v", err)
	}
	if len(negative.Msg.Messages) != 3 {
		t.Fatalf("limit=-10 should fall back to default: want 3 messages, got %d", len(negative.Msg.Messages))
	}
}

// TODO(subscribe): exercise MessageService.Subscribe in a dedicated test PR
// once embedded nats-server/test + a ServerStream fake are available.

// --- helpers ----------------------------------------------------------------

func sendAll(ctx context.Context, t *testing.T, f *fixture, bodies []string) {
	t.Helper()
	for _, body := range bodies {
		if _, err := f.svc.Send(ctx, connect.NewRequest(&huddlev1.MessageServiceSendRequest{
			ChannelId: f.ch.ID.String(),
			Body:      body,
		})); err != nil {
			t.Fatalf("send %q: %v", body, err)
		}
	}
}

func bodies(msgs []*huddlev1.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Body)
	}
	return out
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
