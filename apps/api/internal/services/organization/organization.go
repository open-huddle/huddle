package organization

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/open-huddle/huddle/apps/api/ent"
	entmembership "github.com/open-huddle/huddle/apps/api/ent/membership"
	"github.com/open-huddle/huddle/apps/api/ent/organization"
	"github.com/open-huddle/huddle/apps/api/ent/user"
	"github.com/open-huddle/huddle/apps/api/internal/policy"
	"github.com/open-huddle/huddle/apps/api/internal/principal"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
	"github.com/open-huddle/huddle/gen/go/huddle/v1/huddlev1connect"
)

var _ huddlev1connect.OrganizationServiceHandler = (*Service)(nil)

// InviteConfig carries the knobs InviteMember and AcceptInvitation need.
// Kept as a struct rather than scattered arguments so New stays readable
// as the invite surface grows.
type InviteConfig struct {
	// HMACSecret is the key used to hash tokens at rest. Must be non-empty.
	// Rotation invalidates every outstanding pending invitation.
	HMACSecret []byte
	// LinkBaseURL is the public URL the invite email points at. The
	// mailer appends `?token=<token>`.
	LinkBaseURL string
	// TTL is the lifetime of a freshly-created invitation. Re-inviting an
	// existing pending row extends the clock by TTL from the moment of
	// re-invite.
	TTL time.Duration
}

// Service handles organization CRUD, membership management, and the email
// invite flow. Authorization happens inside each handler via the
// policy.Engine — the auth interceptor only attaches claims; it does not
// know what action is being performed.
type Service struct {
	client   *ent.Client
	resolver *principal.Resolver
	authz    policy.Engine
	invites  InviteConfig
	logger   *slog.Logger
}

func New(client *ent.Client, resolver *principal.Resolver, authz policy.Engine, invites InviteConfig, logger *slog.Logger) *Service {
	return &Service{
		client:   client,
		resolver: resolver,
		authz:    authz,
		invites:  invites,
		logger:   logger,
	}
}

func (s *Service) Create(ctx context.Context, req *connect.Request[huddlev1.CreateRequest]) (*connect.Response[huddlev1.CreateResponse], error) {
	if req.Msg.Name == "" || req.Msg.Slug == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name and slug are required"))
	}

	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	if err := s.authz.Authorize(ctx, caller.ID, policy.ActionCreateOrganization, policy.Resource{Type: "organization"}); err != nil {
		return nil, s.denyErr(err)
	}

	// Create the organization and the founder's owner-membership in one
	// transaction. A half-created org with no owner would be unrecoverable.
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("begin tx: %w", err))
	}

	org, err := tx.Organization.Create().
		SetName(req.Msg.Name).
		SetSlug(req.Msg.Slug).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		// Slug uniqueness collision is a client error, not internal.
		if isUniqueViolation(err) {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("slug %q already in use", req.Msg.Slug))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create org: %w", err))
	}

	if _, err := tx.Membership.Create().
		SetUser(caller).
		SetOrganization(org).
		SetRole(entmembership.RoleOwner).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create owner membership: %w", err))
	}

	if err := tx.Commit(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}

	return connect.NewResponse(&huddlev1.CreateResponse{
		Organization: orgToProto(org),
	}), nil
}

func (s *Service) List(ctx context.Context, _ *connect.Request[huddlev1.ListRequest]) (*connect.Response[huddlev1.ListResponse], error) {
	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	// List is membership-scoped at the query level — there is no separate
	// authz check because the query itself is the scoping mechanism. The
	// principle: never return rows the caller can't see.
	orgs, err := s.client.Organization.Query().
		Where(organization.HasMembershipsWith(entmembership.HasUserWith(user.IDEQ(caller.ID)))).
		Order(ent.Asc(organization.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list orgs: %w", err))
	}

	resp := &huddlev1.ListResponse{
		Organizations: make([]*huddlev1.Organization, 0, len(orgs)),
	}
	for _, o := range orgs {
		resp.Organizations = append(resp.Organizations, orgToProto(o))
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) AddMember(ctx context.Context, req *connect.Request[huddlev1.AddMemberRequest]) (*connect.Response[huddlev1.AddMemberResponse], error) {
	orgID, err := uuid.Parse(req.Msg.OrganizationId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid organization_id"))
	}
	targetID, err := uuid.Parse(req.Msg.UserId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid user_id"))
	}
	role, err := parseRole(req.Msg.Role)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	if err := s.authz.Authorize(ctx, caller.ID, policy.ActionAddMember, policy.Resource{
		Type:           "organization",
		ID:             orgID,
		OrganizationID: orgID,
	}); err != nil {
		return nil, s.denyErr(err)
	}

	// Granting owner is restricted further: only existing owners can mint a
	// new owner. The RBAC engine treats AddMember uniformly, so we layer this
	// rule here rather than expand the policy table.
	if role == entmembership.RoleOwner {
		callerRole, lookupErr := s.callerRole(ctx, caller.ID, orgID)
		if lookupErr != nil {
			return nil, connect.NewError(connect.CodeInternal, lookupErr)
		}
		if callerRole != entmembership.RoleOwner {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners can grant the owner role"))
		}
	}

	target, err := s.client.User.Get(ctx, targetID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("user %s not found", targetID))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get user: %w", err))
	}
	org, err := s.client.Organization.Get(ctx, orgID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("organization %s not found", orgID))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get org: %w", err))
	}

	m, err := s.client.Membership.Create().
		SetUser(target).
		SetOrganization(org).
		SetRole(role).
		Save(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("user is already a member"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("add member: %w", err))
	}

	return connect.NewResponse(&huddlev1.AddMemberResponse{
		Membership: membershipToProto(m, target.ID, org.ID),
	}), nil
}

func (s *Service) callerRole(ctx context.Context, principalID, orgID uuid.UUID) (entmembership.Role, error) {
	m, err := s.client.Membership.Query().
		Where(
			entmembership.HasUserWith(user.IDEQ(principalID)),
			entmembership.HasOrganizationWith(organization.IDEQ(orgID)),
		).
		Only(ctx)
	if err != nil {
		return "", fmt.Errorf("lookup caller role: %w", err)
	}
	return m.Role, nil
}

// denyErr converts a policy error into the right Connect error. ErrDenied
// maps to PermissionDenied; anything else is a lookup failure (Internal).
// Handlers wrap this as `return nil, s.denyErr(err)`.
func (s *Service) denyErr(err error) error {
	if errors.Is(err, policy.ErrDenied) {
		return connect.NewError(connect.CodePermissionDenied, err)
	}
	s.logger.Error("policy lookup failed", "err", err)
	return connect.NewError(connect.CodeInternal, errors.New("authorization lookup failed"))
}

func parseRole(s string) (entmembership.Role, error) {
	switch entmembership.Role(s) {
	case entmembership.RoleOwner:
		return entmembership.RoleOwner, nil
	case entmembership.RoleAdmin:
		return entmembership.RoleAdmin, nil
	case entmembership.RoleMember, "":
		return entmembership.RoleMember, nil
	default:
		return "", fmt.Errorf("invalid role %q (allowed: owner, admin, member)", s)
	}
}

func orgToProto(o *ent.Organization) *huddlev1.Organization {
	return &huddlev1.Organization{
		Id:        o.ID.String(),
		Name:      o.Name,
		Slug:      o.Slug,
		CreatedAt: timestamppb.New(o.CreatedAt),
	}
}

func membershipToProto(m *ent.Membership, userID, orgID uuid.UUID) *huddlev1.Membership {
	return &huddlev1.Membership{
		Id:             m.ID.String(),
		UserId:         userID.String(),
		OrganizationId: orgID.String(),
		Role:           string(m.Role),
		CreatedAt:      timestamppb.New(m.CreatedAt),
	}
}
