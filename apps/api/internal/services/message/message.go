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
	entmembership "github.com/open-huddle/huddle/apps/api/ent/membership"
	entmessage "github.com/open-huddle/huddle/apps/api/ent/message"
	entmessagemention "github.com/open-huddle/huddle/apps/api/ent/messagemention"
	entorganization "github.com/open-huddle/huddle/apps/api/ent/organization"
	entuser "github.com/open-huddle/huddle/apps/api/ent/user"
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

	// Validate mentions before opening the tx so obviously-invalid
	// requests never touch the DB. Returns a deduped set with the
	// sender's own id filtered out.
	mentionIDs, err := s.validateMentions(ctx, req.Msg.MentionUserIds, orgID, caller.ID)
	if err != nil {
		return nil, err
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

	// Persist mentions in the same tx — the join table is the source of
	// truth the notifications consumer + future "my mentions" UI read.
	for _, uid := range mentionIDs {
		if _, err := tx.MessageMention.Create().
			SetMessageID(m.ID).
			SetUserID(uid).
			Save(ctx); err != nil {
			_ = tx.Rollback()
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("insert mention: %w", err))
		}
	}

	out := toProto(m, mentionIDs)
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

// validateMentions parses raw id strings, rejects malformed UUIDs with
// InvalidArgument, de-dupes the set, drops the sender's own id (self-
// mentions are silently no-ops), and verifies every remaining user is a
// member of the channel's organization. A non-member id is
// InvalidArgument — the sender should not be able to ping strangers.
func (s *Service) validateMentions(ctx context.Context, raw []string, orgID, senderID uuid.UUID) ([]uuid.UUID, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[uuid.UUID]struct{}, len(raw))
	ids := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid mention_user_id %q", s))
		}
		if id == senderID {
			continue // self-mention is a no-op
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	// Bulk-check membership in one query: "do all mentioned user ids
	// belong to a Membership in this org?". Count mismatch = at least
	// one stranger.
	got, err := s.client.Membership.Query().
		Where(
			entmembership.HasOrganizationWith(entorganization.IDEQ(orgID)),
			entmembership.HasUserWith(entuser.IDIn(ids...)),
		).
		Count(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("validate mentions: %w", err))
	}
	if got != len(ids) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("one or more mentioned users are not members of this organization"))
	}
	return ids, nil
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

	events, err := s.subs.SubscribeMessages(ctx, channelID)
	if err != nil {
		s.logger.Error("subscribe", "err", err, "channel_id", channelID)
		return connect.NewError(connect.CodeInternal, errors.New("subscribe failed"))
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-events:
			if !ok {
				// Subscriber tore down (server shutdown or broker disconnect).
				// Returning nil ends the stream cleanly; clients will
				// reconnect.
				return nil
			}
			resp, err := buildSubscribeResponse(evt)
			if err != nil {
				s.logger.Warn("subscribe: drop malformed event",
					"err", err, "channel_id", channelID)
				continue
			}
			if err := stream.Send(resp); err != nil {
				// Most often a broken pipe — client went away. Returning the
				// error lets Connect map it to the right transport state.
				return err
			}
		}
	}
}

// buildSubscribeResponse maps a bus event to the oneof-carrying wire
// response. Unknown kinds are skipped (logged at the call site) rather
// than sent as empty — clients should never receive a response with no
// variant set.
func buildSubscribeResponse(evt *events.MessageEvent) (*huddlev1.MessageServiceSubscribeResponse, error) {
	if evt == nil || evt.Message == nil {
		return nil, errors.New("subscriber emitted nil message event")
	}
	switch evt.Kind {
	case events.MessageEventCreated:
		return &huddlev1.MessageServiceSubscribeResponse{
			Event: &huddlev1.MessageServiceSubscribeResponse_Created{
				Created: &huddlev1.MessageCreatedEvent{Message: evt.Message},
			},
		}, nil
	case events.MessageEventEdited:
		return &huddlev1.MessageServiceSubscribeResponse{
			Event: &huddlev1.MessageServiceSubscribeResponse_Edited{
				Edited: &huddlev1.MessageEditedEvent{Message: evt.Message},
			},
		}, nil
	case events.MessageEventDeleted:
		return &huddlev1.MessageServiceSubscribeResponse{
			Event: &huddlev1.MessageServiceSubscribeResponse_Deleted{
				Deleted: &huddlev1.MessageDeletedEvent{
					MessageId: evt.Message.Id,
					ChannelId: evt.Message.ChannelId,
				},
			},
		}, nil
	default:
		return nil, fmt.Errorf("unknown event kind: %v", evt.Kind)
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
		Where(
			entmessage.ChannelIDEQ(channelID),
			// Soft-deleted rows stay in the DB for audit but are hidden
			// from client-facing reads. Edit/Delete handlers own the
			// stamp; anything with deleted_at set is invisible here.
			entmessage.DeletedAtIsNil(),
		).
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
	mentionsByMsg, err := s.mentionsFor(ctx, rows)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load mentions: %w", err))
	}
	for _, m := range rows {
		resp.Messages = append(resp.Messages, toProto(m, mentionsByMsg[m.ID]))
	}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		resp.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return connect.NewResponse(resp), nil
}

// mentionsFor loads MessageMention rows for the given messages in a
// single query and groups them by message_id. The List handler uses
// this to hydrate the mention_user_ids field on each returned Message
// without an N+1 pattern. Returns an empty map when rows is empty so
// callers can range over the result unconditionally.
func (s *Service) mentionsFor(ctx context.Context, rows []*ent.Message) (map[uuid.UUID][]uuid.UUID, error) {
	out := make(map[uuid.UUID][]uuid.UUID, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, m := range rows {
		ids = append(ids, m.ID)
	}
	mentions, err := s.client.MessageMention.Query().
		Where(entmessagemention.MessageIDIn(ids...)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, mm := range mentions {
		out[mm.MessageID] = append(out[mm.MessageID], mm.UserID)
	}
	return out, nil
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

// toProto renders a Message for the wire, including the mention set.
// mentions may be nil (no @-mentions on this message); callers that
// have a list of just-written ids (Send) pass them directly, callers
// reading persisted messages (List / Subscribe) populate from the
// MessageMention join table.
func toProto(m *ent.Message, mentions []uuid.UUID) *huddlev1.Message {
	out := &huddlev1.Message{
		Id:        m.ID.String(),
		ChannelId: m.ChannelID.String(),
		AuthorId:  m.AuthorID.String(),
		Body:      m.Body,
		CreatedAt: timestamppb.New(m.CreatedAt),
	}
	if m.EditedAt != nil {
		out.EditedAt = timestamppb.New(*m.EditedAt)
	}
	if len(mentions) > 0 {
		out.MentionUserIds = make([]string, 0, len(mentions))
		for _, id := range mentions {
			out.MentionUserIds = append(out.MentionUserIds, id.String())
		}
	}
	return out
}
