package organization_test

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
	"github.com/open-huddle/huddle/apps/api/internal/services/organization"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// fixture bundles the collaborators OrganizationService needs, wired up with
// the real RBAC engine and resolver against a per-test SQLite database.
type fixture struct {
	ctx    context.Context
	client *ent.Client
	svc    *organization.Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	client := testutil.NewClient(t)
	resolver := principal.NewResolver(client)
	engine := policy.NewRBAC(client)
	svc := organization.New(client, resolver, engine, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &fixture{ctx: context.Background(), client: client, svc: svc}
}

// callerCtx attaches synthetic OIDC claims for subject. The resolver will
// upsert the ent.User row on first access, matching production behaviour.
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

func TestCreate_NoClaims_Unauthenticated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.svc.Create(f.ctx, connect.NewRequest(&huddlev1.CreateRequest{Name: "Acme", Slug: "acme"}))
	if got := connectCode(t, err); got != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %s", got)
	}
}

func TestCreate_EmptyFields_InvalidArgument(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	for _, tc := range []struct {
		name string
		req  *huddlev1.CreateRequest
	}{
		{"empty name", &huddlev1.CreateRequest{Name: "", Slug: "acme"}},
		{"empty slug", &huddlev1.CreateRequest{Name: "Acme", Slug: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Create(ctx, connect.NewRequest(tc.req))
			if got := connectCode(t, err); got != connect.CodeInvalidArgument {
				t.Fatalf("want InvalidArgument, got %s", got)
			}
		})
	}
}

func TestCreate_HappyPath_FounderBecomesOwner(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	resp, err := f.svc.Create(ctx, connect.NewRequest(&huddlev1.CreateRequest{Name: "Acme", Slug: "acme"}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got := resp.Msg.Organization
	if got.Name != "Acme" || got.Slug != "acme" {
		t.Fatalf("unexpected org in response: %+v", got)
	}

	// Owner membership must have landed in the same tx. Otherwise the caller
	// would have a dangling organization that no one owns — unrecoverable.
	orgID := testutil.MustParseUUID(t, got.Id)
	mems, err := f.client.Membership.Query().All(f.ctx)
	if err != nil {
		t.Fatalf("list memberships: %v", err)
	}
	if len(mems) != 1 {
		t.Fatalf("want 1 membership, got %d", len(mems))
	}
	if mems[0].Role != entmembership.RoleOwner {
		t.Fatalf("want owner role, got %q", mems[0].Role)
	}
	org, err := f.client.Organization.Get(f.ctx, orgID)
	if err != nil {
		t.Fatalf("get org: %v", err)
	}
	if org.Slug != "acme" {
		t.Fatalf("want slug acme, got %q", org.Slug)
	}
}

// TODO(postgres-integration): the duplicate-slug case returns CodeInternal on
// SQLite because isUniqueViolation inspects a pgconn-specific error code.
// Cover CodeAlreadyExists in a Postgres integration test (testcontainers)
// when that layer lands.

func TestList_NoClaims_Unauthenticated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.svc.List(f.ctx, connect.NewRequest(&huddlev1.ListRequest{}))
	if got := connectCode(t, err); got != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %s", got)
	}
}

func TestList_NoMemberships_Empty(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	resp, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.ListRequest{}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp.Msg.Organizations) != 0 {
		t.Fatalf("want empty list, got %d orgs", len(resp.Msg.Organizations))
	}
}

func TestList_MembershipScoped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Alice founds two orgs; Bob founds one. Both should see only their own
	// — the query scoping is the authz mechanism for List.
	aliceCtx := callerCtx(f.ctx, "alice")
	if _, err := f.svc.Create(aliceCtx, connect.NewRequest(&huddlev1.CreateRequest{Name: "Acme", Slug: "acme"})); err != nil {
		t.Fatalf("alice create acme: %v", err)
	}
	if _, err := f.svc.Create(aliceCtx, connect.NewRequest(&huddlev1.CreateRequest{Name: "Beta", Slug: "beta"})); err != nil {
		t.Fatalf("alice create beta: %v", err)
	}
	bobCtx := callerCtx(f.ctx, "bob")
	if _, err := f.svc.Create(bobCtx, connect.NewRequest(&huddlev1.CreateRequest{Name: "Gamma", Slug: "gamma"})); err != nil {
		t.Fatalf("bob create gamma: %v", err)
	}

	aliceList, err := f.svc.List(aliceCtx, connect.NewRequest(&huddlev1.ListRequest{}))
	if err != nil {
		t.Fatalf("alice list: %v", err)
	}
	if got := slugs(aliceList.Msg.Organizations); !equalSet(got, []string{"acme", "beta"}) {
		t.Fatalf("alice: want [acme beta], got %v", got)
	}

	bobList, err := f.svc.List(bobCtx, connect.NewRequest(&huddlev1.ListRequest{}))
	if err != nil {
		t.Fatalf("bob list: %v", err)
	}
	if got := slugs(bobList.Msg.Organizations); !equalSet(got, []string{"gamma"}) {
		t.Fatalf("bob: want [gamma], got %v", got)
	}
}

func TestAddMember_InvalidArguments(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	validUUID := uuid.NewString()
	for _, tc := range []struct {
		name string
		req  *huddlev1.AddMemberRequest
	}{
		{"bad org id", &huddlev1.AddMemberRequest{OrganizationId: "not-a-uuid", UserId: validUUID, Role: "member"}},
		{"bad user id", &huddlev1.AddMemberRequest{OrganizationId: validUUID, UserId: "not-a-uuid", Role: "member"}},
		{"bad role", &huddlev1.AddMemberRequest{OrganizationId: validUUID, UserId: validUUID, Role: "god"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.AddMember(ctx, connect.NewRequest(tc.req))
			if got := connectCode(t, err); got != connect.CodeInvalidArgument {
				t.Fatalf("want InvalidArgument, got %s", got)
			}
		})
	}
}

func TestAddMember_NonMemberCaller_PermissionDenied(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Alice creates Acme; Bob (non-member) tries to add Carol.
	aliceCtx := callerCtx(f.ctx, "alice")
	created, err := f.svc.Create(aliceCtx, connect.NewRequest(&huddlev1.CreateRequest{Name: "Acme", Slug: "acme"}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	orgID := created.Msg.Organization.Id

	carol := testutil.MakeUser(f.ctx, t, f.client, "carol")

	bobCtx := callerCtx(f.ctx, "bob")
	// Bob needs a user row so resolver.Resolve doesn't short-circuit before
	// the authz check. List() is the lightest way to materialize his row.
	if _, err := f.svc.List(bobCtx, connect.NewRequest(&huddlev1.ListRequest{})); err != nil {
		t.Fatalf("bob list to materialize user: %v", err)
	}

	_, err = f.svc.AddMember(bobCtx, connect.NewRequest(&huddlev1.AddMemberRequest{
		OrganizationId: orgID,
		UserId:         carol.ID.String(),
		Role:           "member",
	}))
	if got := connectCode(t, err); got != connect.CodePermissionDenied {
		t.Fatalf("want PermissionDenied, got %s", got)
	}
}

func TestAddMember_MemberCaller_PermissionDenied(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Alice owns Acme and invites Bob as a plain member. Bob then tries to
	// AddMember and should be denied — member role can't invite.
	aliceCtx := callerCtx(f.ctx, "alice")
	created, _ := f.svc.Create(aliceCtx, connect.NewRequest(&huddlev1.CreateRequest{Name: "Acme", Slug: "acme"}))
	orgID := created.Msg.Organization.Id

	bob := testutil.MakeUser(f.ctx, t, f.client, "bob")
	if _, err := f.svc.AddMember(aliceCtx, connect.NewRequest(&huddlev1.AddMemberRequest{
		OrganizationId: orgID,
		UserId:         bob.ID.String(),
		Role:           "member",
	})); err != nil {
		t.Fatalf("alice add bob as member: %v", err)
	}

	carol := testutil.MakeUser(f.ctx, t, f.client, "carol")
	bobCtx := callerCtx(f.ctx, "bob")

	_, err := f.svc.AddMember(bobCtx, connect.NewRequest(&huddlev1.AddMemberRequest{
		OrganizationId: orgID,
		UserId:         carol.ID.String(),
		Role:           "member",
	}))
	if got := connectCode(t, err); got != connect.CodePermissionDenied {
		t.Fatalf("want PermissionDenied, got %s", got)
	}
}

func TestAddMember_AdminGrantingOwner_Denied(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Alice is owner; she promotes Bob to admin. Bob then tries to grant Carol
	// the owner role. Admin can call AddMember but only owners mint owners.
	aliceCtx := callerCtx(f.ctx, "alice")
	created, _ := f.svc.Create(aliceCtx, connect.NewRequest(&huddlev1.CreateRequest{Name: "Acme", Slug: "acme"}))
	orgID := created.Msg.Organization.Id

	bob := testutil.MakeUser(f.ctx, t, f.client, "bob")
	if _, err := f.svc.AddMember(aliceCtx, connect.NewRequest(&huddlev1.AddMemberRequest{
		OrganizationId: orgID,
		UserId:         bob.ID.String(),
		Role:           "admin",
	})); err != nil {
		t.Fatalf("alice add bob as admin: %v", err)
	}

	carol := testutil.MakeUser(f.ctx, t, f.client, "carol")
	bobCtx := callerCtx(f.ctx, "bob")

	_, err := f.svc.AddMember(bobCtx, connect.NewRequest(&huddlev1.AddMemberRequest{
		OrganizationId: orgID,
		UserId:         carol.ID.String(),
		Role:           "owner",
	}))
	if got := connectCode(t, err); got != connect.CodePermissionDenied {
		t.Fatalf("want PermissionDenied for admin→owner, got %s", got)
	}
}

func TestAddMember_OwnerGrantingOwner_Succeeds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	aliceCtx := callerCtx(f.ctx, "alice")
	created, _ := f.svc.Create(aliceCtx, connect.NewRequest(&huddlev1.CreateRequest{Name: "Acme", Slug: "acme"}))
	orgID := created.Msg.Organization.Id

	bob := testutil.MakeUser(f.ctx, t, f.client, "bob")
	resp, err := f.svc.AddMember(aliceCtx, connect.NewRequest(&huddlev1.AddMemberRequest{
		OrganizationId: orgID,
		UserId:         bob.ID.String(),
		Role:           "owner",
	}))
	if err != nil {
		t.Fatalf("owner→owner: %v", err)
	}
	if resp.Msg.Membership.Role != "owner" {
		t.Fatalf("want owner, got %q", resp.Msg.Membership.Role)
	}
}

func TestAddMember_AdminAddingMember_Succeeds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	aliceCtx := callerCtx(f.ctx, "alice")
	created, _ := f.svc.Create(aliceCtx, connect.NewRequest(&huddlev1.CreateRequest{Name: "Acme", Slug: "acme"}))
	orgID := created.Msg.Organization.Id

	bob := testutil.MakeUser(f.ctx, t, f.client, "bob")
	if _, err := f.svc.AddMember(aliceCtx, connect.NewRequest(&huddlev1.AddMemberRequest{
		OrganizationId: orgID,
		UserId:         bob.ID.String(),
		Role:           "admin",
	})); err != nil {
		t.Fatalf("alice add bob as admin: %v", err)
	}

	carol := testutil.MakeUser(f.ctx, t, f.client, "carol")
	bobCtx := callerCtx(f.ctx, "bob")
	resp, err := f.svc.AddMember(bobCtx, connect.NewRequest(&huddlev1.AddMemberRequest{
		OrganizationId: orgID,
		UserId:         carol.ID.String(),
		Role:           "member",
	}))
	if err != nil {
		t.Fatalf("admin→member: %v", err)
	}
	if resp.Msg.Membership.Role != "member" {
		t.Fatalf("want member, got %q", resp.Msg.Membership.Role)
	}
}

func TestAddMember_TargetNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	aliceCtx := callerCtx(f.ctx, "alice")
	created, _ := f.svc.Create(aliceCtx, connect.NewRequest(&huddlev1.CreateRequest{Name: "Acme", Slug: "acme"}))
	orgID := created.Msg.Organization.Id

	_, err := f.svc.AddMember(aliceCtx, connect.NewRequest(&huddlev1.AddMemberRequest{
		OrganizationId: orgID,
		UserId:         uuid.NewString(), // random, not in users table
		Role:           "member",
	}))
	if got := connectCode(t, err); got != connect.CodeNotFound {
		t.Fatalf("want NotFound for missing user, got %s", got)
	}
}

func TestAddMember_OrgNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Alice exists (via materialization below), but the org id is bogus.
	aliceCtx := callerCtx(f.ctx, "alice")
	if _, err := f.svc.List(aliceCtx, connect.NewRequest(&huddlev1.ListRequest{})); err != nil {
		t.Fatalf("materialize alice: %v", err)
	}
	bob := testutil.MakeUser(f.ctx, t, f.client, "bob")

	// With no membership on the bogus org, RBAC denies before we get to the
	// NotFound check. That's correct: an outsider asking about a nonexistent
	// org shouldn't learn it doesn't exist.
	_, err := f.svc.AddMember(aliceCtx, connect.NewRequest(&huddlev1.AddMemberRequest{
		OrganizationId: uuid.NewString(),
		UserId:         bob.ID.String(),
		Role:           "member",
	}))
	if got := connectCode(t, err); got != connect.CodePermissionDenied {
		t.Fatalf("want PermissionDenied for unknown org (outsider cannot probe existence), got %s", got)
	}
}

func slugs(orgs []*huddlev1.Organization) []string {
	out := make([]string, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, o.Slug)
	}
	return out
}

// equalSet reports set equality without ordering requirements.
func equalSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, v := range seen {
		if v != 0 {
			return false
		}
	}
	return true
}
