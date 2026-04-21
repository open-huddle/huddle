package channel_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/open-huddle/huddle/apps/api/ent"
	entmembership "github.com/open-huddle/huddle/apps/api/ent/membership"
	"github.com/open-huddle/huddle/apps/api/internal/auth"
	"github.com/open-huddle/huddle/apps/api/internal/policy"
	"github.com/open-huddle/huddle/apps/api/internal/principal"
	"github.com/open-huddle/huddle/apps/api/internal/services/channel"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

type fixture struct {
	ctx    context.Context
	client *ent.Client
	svc    *channel.Service
	org    *ent.Organization
	alice  *ent.User // member of org
}

// newFixture sets up an org with Alice as a member. Tests that need an
// outsider (no membership) can call testutil.MakeUser themselves.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	client := testutil.NewClient(t)
	resolver := principal.NewResolver(client)
	engine := policy.NewRBAC(client)
	svc := channel.New(client, resolver, engine, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	org := testutil.MakeOrg(ctx, t, client, "acme")
	alice := testutil.MakeUser(ctx, t, client, "alice")
	testutil.MakeMember(ctx, t, client, alice, org, entmembership.RoleMember)

	return &fixture{ctx: ctx, client: client, svc: svc, org: org, alice: alice}
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

func TestCreate_InvalidArguments(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	for _, tc := range []struct {
		name string
		req  *huddlev1.ChannelServiceCreateRequest
	}{
		{"bad org id", &huddlev1.ChannelServiceCreateRequest{OrganizationId: "not-a-uuid", Name: "General", Slug: "general"}},
		{"empty name", &huddlev1.ChannelServiceCreateRequest{OrganizationId: f.org.ID.String(), Name: "", Slug: "general"}},
		{"empty slug", &huddlev1.ChannelServiceCreateRequest{OrganizationId: f.org.ID.String(), Name: "General", Slug: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Create(ctx, connect.NewRequest(tc.req))
			if got := connectCode(t, err); got != connect.CodeInvalidArgument {
				t.Fatalf("want InvalidArgument, got %s", got)
			}
		})
	}
}

func TestCreate_NoClaims_Unauthenticated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.svc.Create(f.ctx, connect.NewRequest(&huddlev1.ChannelServiceCreateRequest{
		OrganizationId: f.org.ID.String(),
		Name:           "General",
		Slug:           "general",
	}))
	if got := connectCode(t, err); got != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %s", got)
	}
}

func TestCreate_NonMember_PermissionDenied(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Bob is authenticated but not a member of Acme.
	bobCtx := callerCtx(f.ctx, "bob")
	_, err := f.svc.Create(bobCtx, connect.NewRequest(&huddlev1.ChannelServiceCreateRequest{
		OrganizationId: f.org.ID.String(),
		Name:           "General",
		Slug:           "general",
	}))
	if got := connectCode(t, err); got != connect.CodePermissionDenied {
		t.Fatalf("want PermissionDenied, got %s", got)
	}
}

func TestCreate_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	resp, err := f.svc.Create(ctx, connect.NewRequest(&huddlev1.ChannelServiceCreateRequest{
		OrganizationId: f.org.ID.String(),
		Name:           "General",
		Slug:           "general",
		Topic:          "everything",
		Description:    "catch-all channel",
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got := resp.Msg.Channel
	if got.Name != "General" || got.Slug != "general" || got.Topic != "everything" || got.Description != "catch-all channel" {
		t.Fatalf("unexpected channel: %+v", got)
	}
	if got.OrganizationId != f.org.ID.String() {
		t.Fatalf("want org id %s, got %s", f.org.ID, got.OrganizationId)
	}
	if got.CreatedById != f.alice.ID.String() {
		t.Fatalf("want created_by_id %s, got %s", f.alice.ID, got.CreatedById)
	}
}

// TODO(postgres-integration): per-org duplicate-slug returns CodeInternal on
// SQLite (pgconn-specific isUniqueViolation) and should be verified as
// CodeAlreadyExists on real Postgres.

func TestList_InvalidArgument(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	_, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.ChannelServiceListRequest{
		OrganizationId: "not-a-uuid",
	}))
	if got := connectCode(t, err); got != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %s", got)
	}
}

func TestList_NonMember_PermissionDenied(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Bob has no membership — List must return PermissionDenied, not an empty
	// list, so clients cannot use empty vs denied to detect org existence.
	bobCtx := callerCtx(f.ctx, "bob")
	_, err := f.svc.List(bobCtx, connect.NewRequest(&huddlev1.ChannelServiceListRequest{
		OrganizationId: f.org.ID.String(),
	}))
	if got := connectCode(t, err); got != connect.CodePermissionDenied {
		t.Fatalf("want PermissionDenied, got %s", got)
	}
}

func TestList_EmptyAndPopulated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	empty, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.ChannelServiceListRequest{
		OrganizationId: f.org.ID.String(),
	}))
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(empty.Msg.Channels) != 0 {
		t.Fatalf("want 0 channels, got %d", len(empty.Msg.Channels))
	}

	for _, slug := range []string{"general", "random", "dev"} {
		if _, err := f.svc.Create(ctx, connect.NewRequest(&huddlev1.ChannelServiceCreateRequest{
			OrganizationId: f.org.ID.String(),
			Name:           slug,
			Slug:           slug,
		})); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}

	populated, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.ChannelServiceListRequest{
		OrganizationId: f.org.ID.String(),
	}))
	if err != nil {
		t.Fatalf("list populated: %v", err)
	}
	got := make([]string, 0, len(populated.Msg.Channels))
	for _, c := range populated.Msg.Channels {
		got = append(got, c.Slug)
	}
	// List orders oldest first (created_at asc).
	want := []string{"general", "random", "dev"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}

func TestGet_InvalidArgument(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	_, err := f.svc.Get(ctx, connect.NewRequest(&huddlev1.ChannelServiceGetRequest{Id: "not-a-uuid"}))
	if got := connectCode(t, err); got != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %s", got)
	}
}

func TestGet_NotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	_, err := f.svc.Get(ctx, connect.NewRequest(&huddlev1.ChannelServiceGetRequest{Id: uuid.NewString()}))
	if got := connectCode(t, err); got != connect.CodeNotFound {
		t.Fatalf("want NotFound, got %s", got)
	}
}

func TestGet_NonMemberOfChannelOrg_PermissionDenied(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	aliceCtx := callerCtx(f.ctx, "alice")
	created, err := f.svc.Create(aliceCtx, connect.NewRequest(&huddlev1.ChannelServiceCreateRequest{
		OrganizationId: f.org.ID.String(),
		Name:           "General",
		Slug:           "general",
	}))
	if err != nil {
		t.Fatalf("seed channel: %v", err)
	}

	// Bob can authenticate but is not a member of Acme — he must not be able
	// to read its channels.
	bobCtx := callerCtx(f.ctx, "bob")
	_, err = f.svc.Get(bobCtx, connect.NewRequest(&huddlev1.ChannelServiceGetRequest{
		Id: created.Msg.Channel.Id,
	}))
	if got := connectCode(t, err); got != connect.CodePermissionDenied {
		t.Fatalf("want PermissionDenied, got %s", got)
	}
}

func TestGet_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	created, err := f.svc.Create(ctx, connect.NewRequest(&huddlev1.ChannelServiceCreateRequest{
		OrganizationId: f.org.ID.String(),
		Name:           "General",
		Slug:           "general",
		Topic:          "everything",
	}))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := f.svc.Get(ctx, connect.NewRequest(&huddlev1.ChannelServiceGetRequest{
		Id: created.Msg.Channel.Id,
	}))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Msg.Channel.Id != created.Msg.Channel.Id || got.Msg.Channel.Slug != "general" || got.Msg.Channel.Topic != "everything" {
		t.Fatalf("unexpected get response: %+v", got.Msg.Channel)
	}
	if got.Msg.Channel.CreatedById != f.alice.ID.String() {
		t.Fatalf("want created_by_id %s, got %s", f.alice.ID, got.Msg.Channel.CreatedById)
	}
}
