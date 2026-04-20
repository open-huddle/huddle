// Package policy decides whether an authenticated principal may perform an
// action on a resource. It is intentionally a thin abstraction: an interface
// + a concrete RBAC implementation that reads from Membership.role. A future
// implementation could front OPA without changing handler call sites.
//
// Authorization belongs at the handler level, not in the auth interceptor —
// only handlers know the resource ID being acted on. The interceptor stays
// concerned with token verification; handlers ask this package whether to
// proceed.
package policy

import (
	"errors"

	"github.com/google/uuid"
)

// Action names a verb-on-a-resource. Strings are stable identifiers because
// they appear in audit logs and (eventually) external policy bundles.
type Action string

const (
	ActionCreateOrganization Action = "organization:create"
	ActionReadOrganization   Action = "organization:read"
	ActionAddMember          Action = "organization:add_member"

	ActionCreateChannel Action = "channel:create"
	ActionReadChannel   Action = "channel:read"
)

// Resource names what an action targets. Type and OrganizationID together
// scope the check; ID is optional and used when the check needs the specific
// row (e.g. AddMember to a specific org).
type Resource struct {
	Type           string    // "organization", "channel", "message", ...
	ID             uuid.UUID // resource UUID (zero when not applicable)
	OrganizationID uuid.UUID // tenant scoping; zero only for tenant-creation actions
}

// ErrDenied is returned when the principal lacks permission for an action.
// Handlers should map it to connect.CodePermissionDenied. Wrap-checked with
// errors.Is so callers can distinguish "denied" from "lookup failed".
var ErrDenied = errors.New("policy: denied")
