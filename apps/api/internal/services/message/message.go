package message

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/open-huddle/huddle/apps/api/ent"
	entchannel "github.com/open-huddle/huddle/apps/api/ent/channel"
	entmessage "github.com/open-huddle/huddle/apps/api/ent/message"
	"github.com/open-huddle/huddle/apps/api/internal/events"
	"github.com/open-huddle/huddle/apps/api/internal/policy"
	"github.com/open-huddle/huddle/apps/api/internal/principal"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
	"github.com/open-huddle/huddle/gen/go/huddle/v1/huddlev1connect"
)

// Pagination tunables. Centralised so the proto comment stays a single
// source of truth.
const (
	defaultPageSize = 50
	maxPageSize     = 200
	maxBodyBytes    = 8192
)

var _ huddlev1connect.MessageServiceHandler = (*Service)(nil)

// Service handles sending and listing messages. Authorization is per-channel:
// a member of the channel's organization may both read and send. Edit and
// delete are intentionally not in this service yet.
//
// Send persists messages and their outbox entries in one transaction — the
// outbox publisher worker (internal/outbox) drains to NATS out of band. The
// Subscriber dependency remains for the streaming Subscribe RPC.
type Service struct {
	client   *ent.Client
	resolver *principal.Resolver
	authz    policy.Engine
	subs     events.Subscriber
	logger   *slog.Logger
}

func New(client *ent.Client, resolver *principal.Resolver, authz policy.Engine, subs events.Subscriber, logger *slog.Logger) *Service {
	return &Service{
		client:   client,
		resolver: resolver,
		authz:    authz,
		subs:     subs,
		logger:   logger,
	}
}

func (s *Service) Send(ctx context.Context, req *connect.Request[huddlev1.MessageServiceSendRequest]) (*connect.Response[huddlev1.MessageServiceSendResponse], error) {
	channelID, err := uuid.Parse(req.Msg.ChannelId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid channel_id"))
	}
	body := req.Msg.Body
	if len(body) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("body is required"))
	}
	if len(body) > maxBodyBytes {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("body exceeds %d bytes", maxBodyBytes))
	}

	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	orgID, err := s.channelOrg(ctx, channelID)
	if err != nil {
		return nil, err
	}

	if err := s.authz.Authorize(ctx, caller.ID, policy.ActionSendMessage, policy.Resource{
		Type:           "message",
		OrganizationID: orgID,
	}); err != nil {
		return nil, s.denyErr(err)
	}

	// Persist the message and the outbox entry in one transaction so a broker
	// outage cannot silently drop events anymore — the outbox publisher
	// worker retries from the database (see ADR-0007). The old best-effort
	// direct publish is gone; downstream consumers (realtime Subscribe,
	// audit, future CDC) all read off the outbox instead.
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("begin tx: %w", err))
	}

	m, err := tx.Message.Create().
		SetChannelID(channelID).
		SetAuthorID(caller.ID).
		SetBody(body).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("send message: %w", err))
	}

	out := toProto(m)
	payload, err := proto.Marshal(out)
	if err != nil {
		_ = tx.Rollback()
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("marshal payload: %w", err))
	}

	if _, err := tx.OutboxEvent.Create().
		SetAggregateType("message").
		SetAggregateID(m.ID).
		SetEventType("message.created").
		SetSubject(events.SubjectMessageCreated(channelID)).
		SetPayload(payload).
		SetActorID(caller.ID).
		SetOrganizationID(orgID).
		SetResourceType("message").
		SetResourceID(m.ID).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("outbox: %w", err))
	}

	if err := tx.Commit(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}

	return connect.NewResponse(&huddlev1.MessageServiceSendResponse{
		Message: out,
	}), nil
}

func (s *Service) Subscribe(ctx context.Context, req *connect.Request[huddlev1.MessageServiceSubscribeRequest], stream *connect.ServerStream[huddlev1.MessageServiceSubscribeResponse]) error {
	channelID, err := uuid.Parse(req.Msg.ChannelId)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid channel_id"))
	}

	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return connect.NewError(connect.CodeUnauthenticated, err)
	}

	orgID, err := s.channelOrg(ctx, channelID)
	if err != nil {
		return err
	}

	if err := s.authz.Authorize(ctx, caller.ID, policy.ActionReadMessage, policy.Resource{
		Type:           "message",
		OrganizationID: orgID,
	}); err != nil {
		return s.denyErr(err)
	}

	msgs, err := s.subs.SubscribeMessages(ctx, channelID)
	if err != nil {
		s.logger.Error("subscribe", "err", err, "channel_id", channelID)
		return connect.NewError(connect.CodeInternal, errors.New("subscribe failed"))
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case m, ok := <-msgs:
			if !ok {
				// Subscriber tore down (server shutdown or broker disconnect).
				// Returning nil ends the stream cleanly; clients will
				// reconnect.
				return nil
			}
			if err := stream.Send(&huddlev1.MessageServiceSubscribeResponse{Message: m}); err != nil {
				// Most often a broken pipe — client went away. Returning the
				// error lets Connect map it to the right transport state.
				return err
			}
		}
	}
}

func (s *Service) List(ctx context.Context, req *connect.Request[huddlev1.MessageServiceListRequest]) (*connect.Response[huddlev1.MessageServiceListResponse], error) {
	channelID, err := uuid.Parse(req.Msg.ChannelId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid channel_id"))
	}
	limit := pageSize(int(req.Msg.Limit))

	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	orgID, err := s.channelOrg(ctx, channelID)
	if err != nil {
		return nil, err
	}

	if err := s.authz.Authorize(ctx, caller.ID, policy.ActionReadMessage, policy.Resource{
		Type:           "message",
		OrganizationID: orgID,
	}); err != nil {
		return nil, s.denyErr(err)
	}

	q := s.client.Message.Query().
		Where(entmessage.ChannelIDEQ(channelID)).
		Order(ent.Desc(entmessage.FieldCreatedAt), ent.Desc(entmessage.FieldID))

	if req.Msg.Before != "" {
		ts, id, err := decodeCursor(req.Msg.Before)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid cursor: %w", err))
		}
		// Strict less-than on (created_at, id) — same column order as the index.
		q = q.Where(entmessage.Or(
			entmessage.CreatedAtLT(ts),
			entmessage.And(entmessage.CreatedAtEQ(ts), entmessage.IDLT(id)),
		))
	}

	// Fetch one extra row to detect "more available" without a second query.
	rows, err := q.Limit(limit + 1).All(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list messages: %w", err))
	}

	resp := &huddlev1.MessageServiceListResponse{
		Messages: make([]*huddlev1.Message, 0, limit),
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	for _, m := range rows {
		resp.Messages = append(resp.Messages, toProto(m))
	}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		resp.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return connect.NewResponse(resp), nil
}

// channelOrg looks up a channel's organization for authz. Maps not-found to
// CodeNotFound so a non-existent channel and an inaccessible one are
// distinguishable from the client's perspective (we already revealed the ID
// exists by accepting it; a 404 here is honest).
func (s *Service) channelOrg(ctx context.Context, channelID uuid.UUID) (uuid.UUID, error) {
	ch, err := s.client.Channel.Query().
		Where(entchannel.IDEQ(channelID)).
		WithOrganization().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return uuid.Nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("channel %s not found", channelID))
		}
		return uuid.Nil, connect.NewError(connect.CodeInternal, fmt.Errorf("lookup channel: %w", err))
	}
	if ch.Edges.Organization == nil {
		// Should be impossible — Channel.organization is Required.
		return uuid.Nil, connect.NewError(connect.CodeInternal, errors.New("channel has no organization"))
	}
	return ch.Edges.Organization.ID, nil
}

func (s *Service) denyErr(err error) error {
	if errors.Is(err, policy.ErrDenied) {
		return connect.NewError(connect.CodePermissionDenied, err)
	}
	s.logger.Error("policy lookup failed", "err", err)
	return connect.NewError(connect.CodeInternal, errors.New("authorization lookup failed"))
}

func pageSize(req int) int {
	if req <= 0 {
		return defaultPageSize
	}
	if req > maxPageSize {
		return maxPageSize
	}
	return req
}

// Cursor encoding: base64url("<unix_nanos>|<uuid>"). Opaque to clients.
// Keeping the format simple makes it diagnosable in logs without losing
// the strict ordering needed for the (created_at, id) tuple.

func encodeCursor(t time.Time, id uuid.UUID) string {
	raw := fmt.Sprintf("%d|%s", t.UnixNano(), id.String())
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("base64: %w", err)
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, errors.New("malformed cursor")
	}
	var nanos int64
	if _, err := fmt.Sscanf(parts[0], "%d", &nanos); err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("timestamp: %w", err)
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("uuid: %w", err)
	}
	return time.Unix(0, nanos), id, nil
}

func toProto(m *ent.Message) *huddlev1.Message {
	return &huddlev1.Message{
		Id:        m.ID.String(),
		ChannelId: m.ChannelID.String(),
		AuthorId:  m.AuthorID.String(),
		Body:      m.Body,
		CreatedAt: timestamppb.New(m.CreatedAt),
	}
}
