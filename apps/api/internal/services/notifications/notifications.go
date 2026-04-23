// Package notifications implements the NotificationService Connect
// handler. List returns the calling principal's notifications (unread
// only by default, cursor-paginated, newest first); MarkRead stamps
// read_at on a specific row, returning NotFound when the notification
// isn't owned by the caller.
//
// Authorization is ownership-by-recipient: every query is scoped by
// recipient_user_id == caller.ID, and every mutation verifies ownership
// before acting. There is no policy.Engine involvement — the data
// model itself is the authz boundary.
package notifications

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
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/open-huddle/huddle/apps/api/ent"
	entnotification "github.com/open-huddle/huddle/apps/api/ent/notification"
	"github.com/open-huddle/huddle/apps/api/internal/principal"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
	"github.com/open-huddle/huddle/gen/go/huddle/v1/huddlev1connect"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

var _ huddlev1connect.NotificationServiceHandler = (*Service)(nil)

// Service fulfills NotificationService.
type Service struct {
	client   *ent.Client
	resolver *principal.Resolver
	logger   *slog.Logger
}

func New(client *ent.Client, resolver *principal.Resolver, logger *slog.Logger) *Service {
	return &Service{client: client, resolver: resolver, logger: logger}
}

func (s *Service) List(ctx context.Context, req *connect.Request[huddlev1.NotificationServiceListRequest]) (*connect.Response[huddlev1.NotificationServiceListResponse], error) {
	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	limit := pageSize(int(req.Msg.Limit))

	q := s.client.Notification.Query().
		Where(entnotification.RecipientUserIDEQ(caller.ID)).
		Order(ent.Desc(entnotification.FieldCreatedAt), ent.Desc(entnotification.FieldID))
	if !req.Msg.IncludeRead {
		q = q.Where(entnotification.ReadAtIsNil())
	}

	if req.Msg.Before != "" {
		ts, id, err := decodeCursor(req.Msg.Before)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid cursor: %w", err))
		}
		q = q.Where(entnotification.Or(
			entnotification.CreatedAtLT(ts),
			entnotification.And(entnotification.CreatedAtEQ(ts), entnotification.IDLT(id)),
		))
	}

	rows, err := q.Limit(limit + 1).All(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list notifications: %w", err))
	}

	resp := &huddlev1.NotificationServiceListResponse{
		Notifications: make([]*huddlev1.Notification, 0, limit),
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	for _, n := range rows {
		resp.Notifications = append(resp.Notifications, toProto(n))
	}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		resp.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) MarkRead(ctx context.Context, req *connect.Request[huddlev1.NotificationServiceMarkReadRequest]) (*connect.Response[huddlev1.NotificationServiceMarkReadResponse], error) {
	id, err := uuid.Parse(req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid notification id"))
	}

	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	// Scope the lookup to the caller so a notification the caller doesn't
	// own returns NotFound — matches "don't confirm the id exists to
	// non-owners" semantics.
	n, err := s.client.Notification.Query().
		Where(
			entnotification.IDEQ(id),
			entnotification.RecipientUserIDEQ(caller.ID),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("notification %s not found", id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("lookup notification: %w", err))
	}

	// MarkRead is idempotent — a second call on an already-read row just
	// returns the current state without bumping read_at.
	if n.ReadAt == nil {
		updated, err := s.client.Notification.UpdateOne(n).
			SetReadAt(time.Now()).
			Save(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("mark read: %w", err))
		}
		n = updated
	}

	return connect.NewResponse(&huddlev1.NotificationServiceMarkReadResponse{
		Notification: toProto(n),
	}), nil
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

// Cursor encoding matches MessageService.List — base64url of
// "<unix_nanos>|<uuid>". Opaque to clients; keeps the (created_at, id)
// tuple ordering that drives the query's strict less-than predicate.

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

func toProto(n *ent.Notification) *huddlev1.Notification {
	out := &huddlev1.Notification{
		Id:              n.ID.String(),
		RecipientUserId: n.RecipientUserID.String(),
		Kind:            string(n.Kind),
		OrganizationId:  n.OrganizationID.String(),
		CreatedAt:       timestamppb.New(n.CreatedAt),
	}
	if n.MessageID != nil {
		out.MessageId = n.MessageID.String()
	}
	if n.ChannelID != nil {
		out.ChannelId = n.ChannelID.String()
	}
	if n.ReadAt != nil {
		out.ReadAt = timestamppb.New(*n.ReadAt)
	}
	return out
}
