package search_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/open-huddle/huddle/apps/api/ent"
	entmembership "github.com/open-huddle/huddle/apps/api/ent/membership"
	"github.com/open-huddle/huddle/apps/api/internal/auth"
	"github.com/open-huddle/huddle/apps/api/internal/policy"
	"github.com/open-huddle/huddle/apps/api/internal/principal"
	internalsearch "github.com/open-huddle/huddle/apps/api/internal/search"
	searchsvc "github.com/open-huddle/huddle/apps/api/internal/services/search"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// fakeBackend is a minimal internalsearch.Client for handler tests.
type fakeBackend struct {
	mu         sync.Mutex
	lastQuery  internalsearch.MessageQuery
	nextResult internalsearch.MessageResult
	nextErr    error
}

func (f *fakeBackend) EnsureIndex(context.Context) error { return nil }
func (f *fakeBackend) IndexMessage(context.Context, internalsearch.MessageDoc) error {
	return nil
}
func (f *fakeBackend) DeleteMessage(context.Context, uuid.UUID) error { return nil }

func (f *fakeBackend) SearchMessages(_ context.Context, q internalsearch.MessageQuery) (internalsearch.MessageResult, error) {
	f.mu.Lock()
	f.lastQuery = q
	f.mu.Unlock()
	if f.nextErr != nil {
		return internalsearch.MessageResult{}, f.nextErr
	}
	return f.nextResult, nil
}

func (f *fakeBackend) query() internalsearch.MessageQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastQuery
}

type fixture struct {
	ctx     context.Context
	client  *ent.Client
	svc     *searchsvc.Service
	org     *ent.Organization
	alice   *ent.User
	backend *fakeBackend
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	client := testutil.NewClient(t)
	resolver := principal.NewResolver(client)
	engine := policy.NewRBAC(client)
	backend := &fakeBackend{}
	svc := searchsvc.New(resolver, engine, backend, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	org := testutil.MakeOrg(ctx, t, client, "acme")
	alice := testutil.MakeUser(ctx, t, client, "alice")
	testutil.MakeMember(ctx, t, client, alice, org, entmembership.RoleMember)

	return &fixture{ctx: ctx, client: client, svc: svc, org: org, alice: alice, backend: backend}
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

func TestSearchMessages_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	msgID := uuid.New()
	channelID := uuid.New()
	authorID := uuid.New()
	when := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	f.backend.nextResult = internalsearch.MessageResult{
		Hits: []internalsearch.MessageHit{{
			Doc: internalsearch.MessageDoc{
				ID:             msgID,
				ChannelID:      channelID,
				OrganizationID: f.org.ID,
				AuthorID:       authorID,
				Body:           "hello world",
				CreatedAt:      when,
			},
			Snippet:    "hello **world**",
			Score:      1.5,
			SortValues: []interface{}{1.5, when.Format(time.RFC3339Nano), msgID.String()},
		}},
		// NextCursor matches the hit's SortValues — handler should encode it.
		NextCursor: []interface{}{1.5, when.Format(time.RFC3339Nano), msgID.String()},
	}

	resp, err := f.svc.SearchMessages(ctx, connect.NewRequest(&huddlev1.SearchServiceSearchMessagesRequest{
		OrganizationId: f.org.ID.String(),
		Query:          "world",
	}))
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(resp.Msg.Hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(resp.Msg.Hits))
	}
	h := resp.Msg.Hits[0]
	if h.Id != msgID.String() || h.ChannelId != channelID.String() || h.AuthorId != authorID.String() {
		t.Errorf("hit ids: got id=%s channel=%s author=%s", h.Id, h.ChannelId, h.AuthorId)
	}
	if h.Snippet != "hello **world**" {
		t.Errorf("snippet: want %q got %q", "hello **world**", h.Snippet)
	}
	if h.Score != 1.5 {
		t.Errorf("score: want 1.5 got %v", h.Score)
	}
	if resp.Msg.NextCursor == "" {
		t.Fatal("next_cursor should be set when backend returns NextCursor")
	}
	// Cursor is opaque but must round-trip back to the same []interface{}.
	decoded, err := base64.RawURLEncoding.DecodeString(resp.Msg.NextCursor)
	if err != nil {
		t.Fatalf("cursor base64: %v", err)
	}
	var values []interface{}
	if err := json.Unmarshal(decoded, &values); err != nil {
		t.Fatalf("cursor json: %v", err)
	}
	if len(values) != 3 {
		t.Errorf("cursor tuple: want 3 values got %d (%v)", len(values), values)
	}

	// The backend got the right query shape — org filter, channel filter
	// not set (empty list), Alice's limit defaulted.
	q := f.backend.query()
	if q.OrganizationID != f.org.ID {
		t.Errorf("backend org_id: want %s got %s", f.org.ID, q.OrganizationID)
	}
	if q.Query != "world" {
		t.Errorf("backend query: want %q got %q", "world", q.Query)
	}
	if len(q.ChannelIDs) != 0 {
		t.Errorf("backend channel_ids: want empty got %v", q.ChannelIDs)
	}
	if q.Limit != 25 {
		t.Errorf("backend limit: want default 25 got %d", q.Limit)
	}
}

func TestSearchMessages_DeniedForNonMember(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Bob is not in the org; the resolver will upsert a user row on
	// Resolve, but policy.RBAC denies for no matching Membership.
	ctx := callerCtx(f.ctx, "bob")

	_, err := f.svc.SearchMessages(ctx, connect.NewRequest(&huddlev1.SearchServiceSearchMessagesRequest{
		OrganizationId: f.org.ID.String(),
		Query:          "anything",
	}))
	if got, want := connectCode(t, err), connect.CodePermissionDenied; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}

// TestSearchMessages_InvalidInputs covers the validation checks that run
// BEFORE authz. A bad org_id, empty query, or bad channel_id format should
// never reach the policy engine.
func TestSearchMessages_InvalidInputs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		req  *huddlev1.SearchServiceSearchMessagesRequest
		want connect.Code
	}{
		{
			name: "bad org_id",
			req:  &huddlev1.SearchServiceSearchMessagesRequest{OrganizationId: "not-a-uuid", Query: "x"},
			want: connect.CodeInvalidArgument,
		},
		{
			name: "empty query",
			req:  &huddlev1.SearchServiceSearchMessagesRequest{OrganizationId: uuid.New().String(), Query: ""},
			want: connect.CodeInvalidArgument,
		},
		{
			name: "bad channel_id",
			req:  &huddlev1.SearchServiceSearchMessagesRequest{OrganizationId: uuid.New().String(), Query: "x", ChannelIds: []string{"nope"}},
			want: connect.CodeInvalidArgument,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			ctx := callerCtx(f.ctx, "alice")
			_, err := f.svc.SearchMessages(ctx, connect.NewRequest(tc.req))
			if got := connectCode(t, err); got != tc.want {
				t.Errorf("code: want %v got %v", tc.want, got)
			}
		})
	}
}

// Cursor decode runs AFTER authz, so it needs a real membership to reach.
// Isolated into its own test because the fixture org ID cannot be baked
// into the shared table above.
func TestSearchMessages_MalformedCursor(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	_, err := f.svc.SearchMessages(ctx, connect.NewRequest(&huddlev1.SearchServiceSearchMessagesRequest{
		OrganizationId: f.org.ID.String(),
		Query:          "x",
		Cursor:         "!!!",
	}))
	if got, want := connectCode(t, err), connect.CodeInvalidArgument; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}

func TestSearchMessages_BackendFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")
	f.backend.nextErr = errors.New("opensearch down")

	_, err := f.svc.SearchMessages(ctx, connect.NewRequest(&huddlev1.SearchServiceSearchMessagesRequest{
		OrganizationId: f.org.ID.String(),
		Query:          "hi",
	}))
	if got, want := connectCode(t, err), connect.CodeUnavailable; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}

func TestSearchMessages_ClampsLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	_, err := f.svc.SearchMessages(ctx, connect.NewRequest(&huddlev1.SearchServiceSearchMessagesRequest{
		OrganizationId: f.org.ID.String(),
		Query:          "x",
		Limit:          1_000, // way above the 100 cap
	}))
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if got := f.backend.query().Limit; got != 100 {
		t.Errorf("limit should clamp to 100, got %d", got)
	}
}
