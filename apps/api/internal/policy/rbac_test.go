package policy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	entmembership "github.com/open-huddle/huddle/apps/api/ent/membership"
	"github.com/open-huddle/huddle/apps/api/internal/policy"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
)

func TestRBAC_CreateOrganization_AlwaysAllowed(t *testing.T) {
	t.Parallel()

	client := testutil.NewClient(t)
	engine := policy.NewRBAC(client)
	ctx := context.Background()

	// No membership seeded — anyone authenticated can found an org.
	if err := engine.Authorize(ctx, uuid.New(), policy.ActionCreateOrganization, policy.Resource{Type: "organization"}); err != nil {
		t.Fatalf("expected allow for CreateOrganization, got %v", err)
	}
}

// TestRBAC_RoleMatrix exercises every (action, role) combination that the
// permissions map mentions, plus the non-member case. One test matrix covers
// the full public decision table, so adding a new action/role forces a change
// here.
func TestRBAC_RoleMatrix(t *testing.T) {
	t.Parallel()

	client := testutil.NewClient(t)
	engine := policy.NewRBAC(client)
	ctx := context.Background()

	org := testutil.MakeOrg(ctx, t, client, "acme")
	owner := testutil.MakeUser(ctx, t, client, "alice-owner")
	admin := testutil.MakeUser(ctx, t, client, "bob-admin")
	member := testutil.MakeUser(ctx, t, client, "carol-member")
	outsider := testutil.MakeUser(ctx, t, client, "dave-outsider")

	testutil.MakeMember(ctx, t, client, owner, org, entmembership.RoleOwner)
	testutil.MakeMember(ctx, t, client, admin, org, entmembership.RoleAdmin)
	testutil.MakeMember(ctx, t, client, member, org, entmembership.RoleMember)
	// Outsider deliberately has no membership row.

	type allow struct{ owner, admin, member, outsider bool }
	cases := []struct {
		name   string
		action policy.Action
		want   allow
	}{
		{"ReadOrganization", policy.ActionReadOrganization, allow{true, true, true, false}},
		{"AddMember", policy.ActionAddMember, allow{true, true, false, false}},
		{"CreateChannel", policy.ActionCreateChannel, allow{true, true, true, false}},
		{"ReadChannel", policy.ActionReadChannel, allow{true, true, true, false}},
		{"SendMessage", policy.ActionSendMessage, allow{true, true, true, false}},
		{"ReadMessage", policy.ActionReadMessage, allow{true, true, true, false}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, actor := range []struct {
				label string
				id    uuid.UUID
				want  bool
			}{
				{"owner", owner.ID, tc.want.owner},
				{"admin", admin.ID, tc.want.admin},
				{"member", member.ID, tc.want.member},
				{"outsider", outsider.ID, tc.want.outsider},
			} {
				err := engine.Authorize(ctx, actor.id, tc.action, policy.Resource{
					Type:           "organization",
					OrganizationID: org.ID,
				})
				got := err == nil
				if got != actor.want {
					t.Errorf("%s %s: got allow=%v err=%v, want allow=%v", actor.label, tc.action, got, err, actor.want)
				}
				// Denials must wrap ErrDenied so callers can distinguish from
				// lookup failures. Internal errors would become 500s, not 403s.
				if !actor.want && !errors.Is(err, policy.ErrDenied) {
					t.Errorf("%s %s: err %v is not ErrDenied", actor.label, tc.action, err)
				}
			}
		})
	}
}

func TestRBAC_MissingOrganizationID(t *testing.T) {
	t.Parallel()

	client := testutil.NewClient(t)
	engine := policy.NewRBAC(client)
	ctx := context.Background()

	// A zero OrganizationID on an org-scoped action is a programming error in
	// the handler — RBAC denies rather than silently succeeding.
	err := engine.Authorize(ctx, uuid.New(), policy.ActionAddMember, policy.Resource{Type: "organization"})
	if !errors.Is(err, policy.ErrDenied) {
		t.Fatalf("want ErrDenied for missing org id, got %v", err)
	}
}

func TestRBAC_UnknownAction(t *testing.T) {
	t.Parallel()

	client := testutil.NewClient(t)
	engine := policy.NewRBAC(client)
	ctx := context.Background()

	err := engine.Authorize(ctx, uuid.New(), policy.Action("bogus:action"), policy.Resource{
		Type:           "organization",
		OrganizationID: uuid.New(),
	})
	if !errors.Is(err, policy.ErrDenied) {
		t.Fatalf("want ErrDenied for unknown action, got %v", err)
	}
}
