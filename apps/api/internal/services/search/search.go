// Package search implements the SearchService Connect handler. It parses
// and authorizes a SearchMessages request, delegates the query to a
// search.Client (today: OpenSearch), and translates the backend result
// back into proto hits.
//
// Authorization is organization-scoped: the caller must be a member of the
// organization they are searching in. The handler does NOT filter the list
// of channel_ids against the caller's read rights today — every member of
// an org can read every channel in it. When private channels land, the
// handler will intersect request.channel_ids with the caller's visible
// channel set before passing it down.
package search

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/open-huddle/huddle/apps/api/internal/policy"
	"github.com/open-huddle/huddle/apps/api/internal/principal"
	internalsearch "github.com/open-huddle/huddle/apps/api/internal/search"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
	"github.com/open-huddle/huddle/gen/go/huddle/v1/huddlev1connect"
)

const (
	defaultLimit = 25
	maxLimit     = 100
)

var _ huddlev1connect.SearchServiceHandler = (*Service)(nil)

// Service wires SearchService to the search backend. It holds only the
// dependencies the handler needs; the backend itself is constructed in
// main.go so tests can drop in an in-memory fake.
type Service struct {
	resolver *principal.Resolver
	authz    policy.Engine
	backend  internalsearch.Client
	logger   *slog.Logger
}

func New(resolver *principal.Resolver, authz policy.Engine, backend internalsearch.Client, logger *slog.Logger) *Service {
	return &Service{
		resolver: resolver,
		authz:    authz,
		backend:  backend,
		logger:   logger,
	}
}

func (s *Service) SearchMessages(ctx context.Context, req *connect.Request[huddlev1.SearchServiceSearchMessagesRequest]) (*connect.Response[huddlev1.SearchServiceSearchMessagesResponse], error) {
	orgID, err := uuid.Parse(req.Msg.OrganizationId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid organization_id"))
	}
	if req.Msg.Query == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("query is required"))
	}

	channelIDs := make([]uuid.UUID, 0, len(req.Msg.ChannelIds))
	for _, raw := range req.Msg.ChannelIds {
		cid, err := uuid.Parse(raw)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid channel_id %q", raw))
		}
		channelIDs = append(channelIDs, cid)
	}

	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	if err := s.authz.Authorize(ctx, caller.ID, policy.ActionReadMessage, policy.Resource{
		Type:           "message",
		OrganizationID: orgID,
	}); err != nil {
		return nil, s.denyErr(err)
	}

	cursor, err := decodeCursor(req.Msg.Cursor)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid cursor: %w", err))
	}

	q := internalsearch.MessageQuery{
		OrganizationID: orgID,
		Query:          req.Msg.Query,
		ChannelIDs:     channelIDs,
		Limit:          clampLimit(int(req.Msg.Limit)),
		CursorValues:   cursor,
	}
	if req.Msg.After != nil {
		q.After = req.Msg.After.AsTime()
	}
	if req.Msg.Before != nil {
		q.Before = req.Msg.Before.AsTime()
	}

	result, err := s.backend.SearchMessages(ctx, q)
	if err != nil {
		s.logger.Error("search backend", "err", err, "org_id", orgID)
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("search unavailable"))
	}

	resp := &huddlev1.SearchServiceSearchMessagesResponse{
		Hits: make([]*huddlev1.MessageHit, 0, len(result.Hits)),
	}
	for _, h := range result.Hits {
		resp.Hits = append(resp.Hits, &huddlev1.MessageHit{
			Id:        h.Doc.ID.String(),
			ChannelId: h.Doc.ChannelID.String(),
			AuthorId:  h.Doc.AuthorID.String(),
			CreatedAt: timestamppb.New(h.Doc.CreatedAt),
			Snippet:   h.Snippet,
			Score:     h.Score,
		})
	}
	if len(result.NextCursor) > 0 {
		encoded, err := encodeCursor(result.NextCursor)
		if err != nil {
			s.logger.Error("encode cursor", "err", err)
			return nil, connect.NewError(connect.CodeInternal, errors.New("encode cursor"))
		}
		resp.NextCursor = encoded
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) denyErr(err error) error {
	if errors.Is(err, policy.ErrDenied) {
		return connect.NewError(connect.CodePermissionDenied, err)
	}
	s.logger.Error("policy lookup failed", "err", err)
	return connect.NewError(connect.CodeInternal, errors.New("authorization lookup failed"))
}

func clampLimit(req int) int {
	if req <= 0 {
		return defaultLimit
	}
	if req > maxLimit {
		return maxLimit
	}
	return req
}

// Cursor encoding: the OpenSearch search_after tuple, JSON-encoded then
// base64url-wrapped. Opaque to clients. Keeping it JSON (rather than a
// custom pipe-delimited format) means we don't have to care about the
// concrete type of each sort key as the mapping evolves.

func encodeCursor(values []interface{}) (string, error) {
	raw, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeCursor(s string) ([]interface{}, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	var values []interface{}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return values, nil
}
