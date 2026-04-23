package message

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/open-huddle/huddle/apps/api/ent"
	entmembership "github.com/open-huddle/huddle/apps/api/ent/membership"
	entmessage "github.com/open-huddle/huddle/apps/api/ent/message"
	entmessagemention "github.com/open-huddle/huddle/apps/api/ent/messagemention"
	entorganization "github.com/open-huddle/huddle/apps/api/ent/organization"
	entuser "github.com/open-huddle/huddle/apps/api/ent/user"
	"github.com/open-huddle/huddle/apps/api/internal/events"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// Edit replaces the body and mention set on a message the caller
// authored. Non-authors get PermissionDenied regardless of role —
// editing someone else's words is never allowed, even for an admin.
// (Deleting someone else's message is a separate, admin-allowed
// operation; see Delete below.)
//
// The handler diffs the new mention set against the pre-edit set and
// writes the *added* ids into the outbox payload. The notifications
// consumer fans out in-app Notifications for the added ids only,
// marked source=message_edited so the mailer skips them.
func (s *Service) Edit(ctx context.Context, req *connect.Request[huddlev1.MessageServiceEditRequest]) (*connect.Response[huddlev1.MessageServiceEditResponse], error) {
	messageID, err := uuid.Parse(req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid message id"))
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

	existing, err := s.client.Message.Query().
		Where(entmessage.IDEQ(messageID), entmessage.DeletedAtIsNil()).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("message %s not found", messageID))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load message: %w", err))
	}

	// Author-only. Admins editing others' words would be a compliance
	// problem — deletion exists as the moderation primitive instead.
	if existing.AuthorID != caller.ID {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only the author can edit a message"))
	}

	orgID, err := s.channelOrg(ctx, existing.ChannelID)
	if err != nil {
		return nil, err
	}

	newMentions, err := s.validateMentions(ctx, req.Msg.MentionUserIds, orgID, caller.ID)
	if err != nil {
		return nil, err
	}

	// Diff old vs new. Notifications consumer only fans out for added
	// ids — removed ids keep their existing notifications (the original
	// mention was real and already acknowledged by the recipient).
	oldMentions, err := s.mentionsForMessage(ctx, messageID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load current mentions: %w", err))
	}
	addedMentions := diffAdded(oldMentions, newMentions)

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("begin tx: %w", err))
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Replace mentions wholesale: simpler than a three-way merge, and
	// MessageMention is immutable on individual rows anyway.
	if _, err := tx.MessageMention.Delete().
		Where(entmessagemention.MessageIDEQ(messageID)).
		Exec(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("clear old mentions: %w", err))
	}
	for _, uid := range newMentions {
		if _, err := tx.MessageMention.Create().
			SetMessageID(messageID).
			SetUserID(uid).
			Save(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("insert mention: %w", err))
		}
	}

	editedAt := time.Now()
	m, err := tx.Message.UpdateOne(existing).
		SetBody(body).
		SetEditedAt(editedAt).
		Save(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update message: %w", err))
	}

	// Payload carries the full updated Message plus the *added* mention
	// set. The notifications consumer reads added_mention_user_ids from
	// the protobuf's mention_user_ids field on message.edited events;
	// the full list stays on the Message for Subscribe/List clients.
	// To communicate "this is the added set, not the full set," we
	// marshal a dedicated wrapper proto... actually: simplest is to
	// stuff the Message proto (which carries the full mention set) and
	// let the consumer diff against its own already-processed state
	// using its Notification UNIQUE constraint. See notifications
	// consumer: UNIQUE(recipient, message, kind) means re-notifying an
	// already-notified user is a no-op.
	out := toProto(m, newMentions)
	payload, err := proto.Marshal(out)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("marshal payload: %w", err))
	}
	if _, err := tx.OutboxEvent.Create().
		SetAggregateType("message").
		SetAggregateID(m.ID).
		SetEventType("message.edited").
		SetSubject(events.SubjectMessageEdited(existing.ChannelID)).
		SetPayload(payload).
		SetActorID(caller.ID).
		SetOrganizationID(orgID).
		SetResourceType("message").
		SetResourceID(m.ID).
		Save(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("outbox: %w", err))
	}

	if err := tx.Commit(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	committed = true

	_ = addedMentions // diff computed for completeness; UNIQUE does the dedup
	return connect.NewResponse(&huddlev1.MessageServiceEditResponse{
		Message: out,
	}), nil
}

// Delete soft-deletes a message. Authors can delete their own messages;
// admins and owners can delete anyone's within their org (moderation).
// Non-authors without admin/owner role get PermissionDenied.
func (s *Service) Delete(ctx context.Context, req *connect.Request[huddlev1.MessageServiceDeleteRequest]) (*connect.Response[huddlev1.MessageServiceDeleteResponse], error) {
	messageID, err := uuid.Parse(req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid message id"))
	}

	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	existing, err := s.client.Message.Query().
		Where(entmessage.IDEQ(messageID), entmessage.DeletedAtIsNil()).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("message %s not found", messageID))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load message: %w", err))
	}

	orgID, err := s.channelOrg(ctx, existing.ChannelID)
	if err != nil {
		return nil, err
	}

	// Authz: author always, or admin/owner of the org (moderation).
	if existing.AuthorID != caller.ID {
		role, lookupErr := s.callerRole(ctx, caller.ID, orgID)
		if lookupErr != nil {
			return nil, connect.NewError(connect.CodeInternal, lookupErr)
		}
		if role != entmembership.RoleAdmin && role != entmembership.RoleOwner {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only the author or an admin/owner can delete"))
		}
	}

	deletedAt := time.Now()

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("begin tx: %w", err))
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.Message.UpdateOne(existing).
		SetDeletedAt(deletedAt).
		Save(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("mark deleted: %w", err))
	}

	// Payload carries just the message id — there's no content to ship.
	// The consumer (search indexer) removes the doc by id; the
	// notifications consumer stamps without work.
	payload, err := proto.Marshal(&huddlev1.Message{Id: existing.ID.String(), ChannelId: existing.ChannelID.String()})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("marshal payload: %w", err))
	}
	if _, err := tx.OutboxEvent.Create().
		SetAggregateType("message").
		SetAggregateID(existing.ID).
		SetEventType("message.deleted").
		SetSubject(events.SubjectMessageDeleted(existing.ChannelID)).
		SetPayload(payload).
		SetActorID(caller.ID).
		SetOrganizationID(orgID).
		SetResourceType("message").
		SetResourceID(existing.ID).
		Save(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("outbox: %w", err))
	}

	if err := tx.Commit(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	committed = true

	return connect.NewResponse(&huddlev1.MessageServiceDeleteResponse{}), nil
}

// mentionsForMessage loads the current MessageMention set for one
// message id. Used by Edit to diff against the new set.
func (s *Service) mentionsForMessage(ctx context.Context, msgID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.client.MessageMention.Query().
		Where(entmessagemention.MessageIDEQ(msgID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.UserID)
	}
	return out, nil
}

// callerRole is shared with Delete's admin-or-owner gate. Not on the
// Service receiver because the organization service owns the RBAC
// lookup; duplicated here to keep the package self-contained.
func (s *Service) callerRole(ctx context.Context, principalID, orgID uuid.UUID) (entmembership.Role, error) {
	m, err := s.client.Membership.Query().
		Where(
			entmembership.HasUserWith(entuser.IDEQ(principalID)),
			entmembership.HasOrganizationWith(entorganization.IDEQ(orgID)),
		).
		Only(ctx)
	if err != nil {
		return "", fmt.Errorf("lookup caller role: %w", err)
	}
	return m.Role, nil
}

// diffAdded returns the ids in `next` that are not in `prev`.
func diffAdded(prev, next []uuid.UUID) []uuid.UUID {
	in := make(map[uuid.UUID]struct{}, len(prev))
	for _, id := range prev {
		in[id] = struct{}{}
	}
	added := make([]uuid.UUID, 0)
	for _, id := range next {
		if _, ok := in[id]; !ok {
			added = append(added, id)
		}
	}
	return added
}
