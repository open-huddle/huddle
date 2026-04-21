package channel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/open-huddle/huddle/apps/api/ent"
	entchannel "github.com/open-huddle/huddle/apps/api/ent/channel"
	"github.com/open-huddle/huddle/apps/api/ent/organization"
	"github.com/open-huddle/huddle/apps/api/internal/policy"
	"github.com/open-huddle/huddle/apps/api/internal/principal"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
	"github.com/open-huddle/huddle/gen/go/huddle/v1/huddlev1connect"
)

var _ huddlev1connect.ChannelServiceHandler = (*Service)(nil)

// Service handles channel CRUD. Per-organization slug uniqueness is enforced
// by the schema; the handler maps that to CodeAlreadyExists rather than
// CodeInternal so clients get an actionable error.
type Service struct {
	client   *ent.Client
	resolver *principal.Resolver
	authz    policy.Engine
	logger   *slog.Logger
}

func New(client *ent.Client, resolver *principal.Resolver, authz policy.Engine, logger *slog.Logger) *Service {
	return &Service{client: client, resolver: resolver, authz: authz, logger: logger}
}

func (s *Service) Create(ctx context.Context, req *connect.Request[huddlev1.ChannelServiceCreateRequest]) (*connect.Response[huddlev1.ChannelServiceCreateResponse], error) {
	if req.Msg.Name == "" || req.Msg.Slug == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name and slug are required"))
	}
	orgID, err := uuid.Parse(req.Msg.OrganizationId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid organization_id"))
	}

	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	if err := s.authz.Authorize(ctx, caller.ID, policy.ActionCreateChannel, policy.Resource{
		Type:           "channel",
		OrganizationID: orgID,
	}); err != nil {
		return nil, s.denyErr(err)
	}

	ch, err := s.client.Channel.Create().
		SetName(req.Msg.Name).
		SetSlug(req.Msg.Slug).
		SetTopic(req.Msg.Topic).
		SetDescription(req.Msg.Description).
		SetOrganizationID(orgID).
		SetCreatedByID(caller.ID).
		Save(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("slug %q already in use in this organization", req.Msg.Slug))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create channel: %w", err))
	}

	return connect.NewResponse(&huddlev1.ChannelServiceCreateResponse{
		Channel: toProto(ch, orgID, caller.ID),
	}), nil
}

func (s *Service) List(ctx context.Context, req *connect.Request[huddlev1.ChannelServiceListRequest]) (*connect.Response[huddlev1.ChannelServiceListResponse], error) {
	orgID, err := uuid.Parse(req.Msg.OrganizationId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid organization_id"))
	}

	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	// Fail-closed: a non-member asking for the channel list of an org gets
	// 403, not an empty result. The latter would silently hide whether the
	// org exists at all, but it would also mean the client cannot tell
	// "no channels yet" from "you have no access" — the explicit denial is
	// kinder to honest clients.
	if err := s.authz.Authorize(ctx, caller.ID, policy.ActionReadChannel, policy.Resource{
		Type:           "channel",
		OrganizationID: orgID,
	}); err != nil {
		return nil, s.denyErr(err)
	}

	rows, err := s.client.Channel.Query().
		Where(entchannel.HasOrganizationWith(organization.IDEQ(orgID))).
		WithCreatedBy().
		Order(ent.Asc(entchannel.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list channels: %w", err))
	}

	resp := &huddlev1.ChannelServiceListResponse{
		Channels: make([]*huddlev1.Channel, 0, len(rows)),
	}
	for _, ch := range rows {
		resp.Channels = append(resp.Channels, toProto(ch, orgID, createdByID(ch)))
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) Get(ctx context.Context, req *connect.Request[huddlev1.ChannelServiceGetRequest]) (*connect.Response[huddlev1.ChannelServiceGetResponse], error) {
	id, err := uuid.Parse(req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid id"))
	}

	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	ch, err := s.client.Channel.Query().
		Where(entchannel.IDEQ(id)).
		WithOrganization().
		WithCreatedBy().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("channel %s not found", id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get channel: %w", err))
	}

	orgID := ch.Edges.Organization.ID
	if err := s.authz.Authorize(ctx, caller.ID, policy.ActionReadChannel, policy.Resource{
		Type:           "channel",
		ID:             ch.ID,
		OrganizationID: orgID,
	}); err != nil {
		return nil, s.denyErr(err)
	}

	return connect.NewResponse(&huddlev1.ChannelServiceGetResponse{
		Channel: toProto(ch, orgID, createdByID(ch)),
	}), nil
}

// denyErr maps policy errors to Connect statuses. ErrDenied → PermissionDenied,
// anything else → Internal.
func (s *Service) denyErr(err error) error {
	if errors.Is(err, policy.ErrDenied) {
		return connect.NewError(connect.CodePermissionDenied, err)
	}
	s.logger.Error("policy lookup failed", "err", err)
	return connect.NewError(connect.CodeInternal, errors.New("authorization lookup failed"))
}

func toProto(ch *ent.Channel, orgID, createdBy uuid.UUID) *huddlev1.Channel {
	out := &huddlev1.Channel{
		Id:             ch.ID.String(),
		OrganizationId: orgID.String(),
		Name:           ch.Name,
		Slug:           ch.Slug,
		Topic:          ch.Topic,
		Description:    ch.Description,
		CreatedAt:      timestamppb.New(ch.CreatedAt),
	}
	if createdBy != uuid.Nil {
		out.CreatedById = createdBy.String()
	}
	return out
}

// createdByID prefers the eagerly loaded edge; falls back to uuid.Nil when
// the creator was deleted (FK is ON DELETE SET NULL by the migration).
func createdByID(ch *ent.Channel) uuid.UUID {
	if ch.Edges.CreatedBy != nil {
		return ch.Edges.CreatedBy.ID
	}
	return uuid.Nil
}
