package policy

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-huddle/huddle/apps/api/ent"
	"github.com/open-huddle/huddle/apps/api/ent/membership"
	"github.com/open-huddle/huddle/apps/api/ent/organization"
	"github.com/open-huddle/huddle/apps/api/ent/user"
)

// Engine is the surface handlers depend on. Authorize returns nil on allow,
// ErrDenied (wrapped with detail) on deny, and other errors when the lookup
// itself fails — handlers must distinguish these to choose the right gRPC
// code (PermissionDenied vs Internal).
type Engine interface {
	Authorize(ctx context.Context, principalID uuid.UUID, action Action, resource Resource) error
}

// RBAC implements Engine by looking up the principal's membership row and
// matching against a static role→permission map. A future swap to OPA / Cedar
// keeps the same Engine contract.
type RBAC struct {
	client *ent.Client
}

func NewRBAC(client *ent.Client) *RBAC {
	return &RBAC{client: client}
}

// Permissions per role. Owner ⊇ Admin ⊇ Member. Adding a new action means
// listing the lowest role that may perform it; do not duplicate across rows.
var permissions = map[Action]map[membership.Role]bool{
	ActionReadOrganization: {membership.RoleMember: true, membership.RoleAdmin: true, membership.RoleOwner: true},
	ActionAddMember:        {membership.RoleAdmin: true, membership.RoleOwner: true},
}

func (r *RBAC) Authorize(ctx context.Context, principalID uuid.UUID, action Action, resource Resource) error {
	switch action {
	case ActionCreateOrganization:
		// Any authenticated user can found an organization. No resource lookup.
		return nil
	case ActionReadOrganization, ActionAddMember:
		if resource.OrganizationID == uuid.Nil {
			return fmt.Errorf("%w: %s requires an organization id", ErrDenied, action)
		}
		role, err := r.lookupRole(ctx, principalID, resource.OrganizationID)
		if err != nil {
			return err
		}
		if !permissions[action][role] {
			return fmt.Errorf("%w: role %q cannot %s", ErrDenied, role, action)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown action %q", ErrDenied, action)
	}
}

// lookupRole returns the principal's role in the organization, or ErrDenied
// if no membership exists. Lookup failures (DB unreachable, etc.) bubble up
// untyped so handlers can map them to CodeInternal.
func (r *RBAC) lookupRole(ctx context.Context, principalID, orgID uuid.UUID) (membership.Role, error) {
	m, err := r.client.Membership.Query().
		Where(
			membership.HasUserWith(user.IDEQ(principalID)),
			membership.HasOrganizationWith(organization.IDEQ(orgID)),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return "", fmt.Errorf("%w: not a member of organization %s", ErrDenied, orgID)
		}
		return "", fmt.Errorf("policy: lookup membership: %w", err)
	}
	return m.Role, nil
}
