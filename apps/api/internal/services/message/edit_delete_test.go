package message_test

import (
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	entmembership "github.com/open-huddle/huddle/apps/api/ent/membership"
	"github.com/open-huddle/huddle/apps/api/ent/outboxevent"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// sendAsAlice is a tiny helper so edit/delete tests can set up a Message
// for Alice to mutate without recreating the full newFixture + Send
// preamble in every case.
func sendAsAlice(t *testing.T, f *fixture, body string, mentionIDs ...string) string {
	t.Helper()
	ctx := callerCtx(f.ctx, "alice")
	resp, err := f.svc.Send(ctx, connect.NewRequest(&huddlev1.MessageServiceSendRequest{
		ChannelId:      f.ch.ID.String(),
		Body:           body,
		MentionUserIds: mentionIDs,
	}))
	if err != nil {
		t.Fatalf("seed Send: %v", err)
	}
	return resp.Msg.Message.Id
}

// --- Edit ------------------------------------------------------------------

func TestEdit_AuthorUpdatesBodyAndStampsEditedAt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	msgID := sendAsAlice(t, f, "original")

	ctx := callerCtx(f.ctx, "alice")
	resp, err := f.svc.Edit(ctx, connect.NewRequest(&huddlev1.MessageServiceEditRequest{
		Id:   msgID,
		Body: "edited",
	}))
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if resp.Msg.Message.Body != "edited" {
		t.Errorf("body: want %q got %q", "edited", resp.Msg.Message.Body)
	}
	if resp.Msg.Message.EditedAt == nil {
		t.Error("edited_at should be set on the response")
	}

	// Outbox event lands with message.edited.
	events, err := f.client.OutboxEvent.Query().
		Where(outboxevent.EventTypeEQ("message.edited")).All(f.ctx)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 message.edited outbox event, got %d", len(events))
	}
}

func TestEdit_NonAuthorDenied(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Bob is a member but not the author.
	addMember(t, f, "bob")
	msgID := sendAsAlice(t, f, "alice wrote this")

	bobCtx := callerCtx(f.ctx, "bob")
	_, err := f.svc.Edit(bobCtx, connect.NewRequest(&huddlev1.MessageServiceEditRequest{
		Id:   msgID,
		Body: "bob's rewrite",
	}))
	if got, want := connectCode(t, err), connect.CodePermissionDenied; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}

func TestEdit_AdminCannotEditOthersMessage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Elevate bob to admin; admins can delete but not edit someone else's words.
	bobID := addMember(t, f, "bob")
	if _, err := f.client.Membership.Update().
		Where(entmembership.HasUserWith(), entmembership.RoleEQ(entmembership.RoleMember)).
		Save(f.ctx); err != nil {
		// ignore — we just use membership role inline below
		_ = bobID
	}
	// Actually just delete bob's current membership and recreate as admin.
	if _, err := f.client.Membership.Delete().Exec(f.ctx); err != nil {
		t.Fatalf("reset memberships: %v", err)
	}
	testutil.MakeMember(f.ctx, t, f.client, f.alice, f.org, entmembership.RoleMember)
	bob := testutil.MakeUser(f.ctx, t, f.client, "bob-admin-subject")
	// Re-seed bob as an admin directly.
	testutil.MakeMember(f.ctx, t, f.client, bob, f.org, entmembership.RoleAdmin)
	_ = bob

	msgID := sendAsAlice(t, f, "alice wrote this")

	adminCtx := callerCtx(f.ctx, "bob-admin-subject")
	_, err := f.svc.Edit(adminCtx, connect.NewRequest(&huddlev1.MessageServiceEditRequest{
		Id:   msgID,
		Body: "moderated",
	}))
	if got, want := connectCode(t, err), connect.CodePermissionDenied; got != want {
		t.Errorf("admin should not be able to edit others' messages; code: want %v got %v", want, got)
	}
}

func TestEdit_ReplacesMentionsWholesale(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	bobID := addMember(t, f, "bob")
	carolID := addMember(t, f, "carol")

	msgID := sendAsAlice(t, f, "initial", bobID)

	ctx := callerCtx(f.ctx, "alice")
	resp, err := f.svc.Edit(ctx, connect.NewRequest(&huddlev1.MessageServiceEditRequest{
		Id:             msgID,
		Body:           "now mentioning carol",
		MentionUserIds: []string{carolID},
	}))
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if len(resp.Msg.Message.MentionUserIds) != 1 || resp.Msg.Message.MentionUserIds[0] != carolID {
		t.Errorf("mentions after edit: want [%s] got %v", carolID, resp.Msg.Message.MentionUserIds)
	}
}

func TestEdit_MissingMessageIsNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")
	_, err := f.svc.Edit(ctx, connect.NewRequest(&huddlev1.MessageServiceEditRequest{
		Id:   uuid.New().String(),
		Body: "wherever",
	}))
	if got, want := connectCode(t, err), connect.CodeNotFound; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}

// --- Delete ----------------------------------------------------------------

func TestDelete_AuthorSoftDeletes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	msgID := sendAsAlice(t, f, "going away")

	ctx := callerCtx(f.ctx, "alice")
	if _, err := f.svc.Delete(ctx, connect.NewRequest(&huddlev1.MessageServiceDeleteRequest{
		Id: msgID,
	})); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// List hides it.
	resp, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.MessageServiceListRequest{
		ChannelId: f.ch.ID.String(),
	}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Msg.Messages) != 0 {
		t.Errorf("deleted message should be filtered out of List, got %d messages", len(resp.Msg.Messages))
	}

	// Row is still in the DB for audit.
	count, err := f.client.Message.Query().Count(f.ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("soft-delete should keep the row, got %d rows", count)
	}

	// Outbox event lands.
	events, err := f.client.OutboxEvent.Query().
		Where(outboxevent.EventTypeEQ("message.deleted")).All(f.ctx)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 message.deleted outbox event, got %d", len(events))
	}
}

func TestDelete_MemberCannotDeleteOthers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	addMember(t, f, "bob")
	msgID := sendAsAlice(t, f, "alice wrote")

	bobCtx := callerCtx(f.ctx, "bob")
	_, err := f.svc.Delete(bobCtx, connect.NewRequest(&huddlev1.MessageServiceDeleteRequest{
		Id: msgID,
	}))
	if got, want := connectCode(t, err), connect.CodePermissionDenied; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}

func TestDelete_AdminCanDeleteOthers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	msgID := sendAsAlice(t, f, "alice wrote")

	// Seed bob as admin directly.
	bob := testutil.MakeUser(f.ctx, t, f.client, "bob")
	testutil.MakeMember(f.ctx, t, f.client, bob, f.org, entmembership.RoleAdmin)

	ctx := callerCtx(f.ctx, "bob")
	if _, err := f.svc.Delete(ctx, connect.NewRequest(&huddlev1.MessageServiceDeleteRequest{
		Id: msgID,
	})); err != nil {
		t.Fatalf("admin Delete: %v", err)
	}
}

func TestDelete_MissingMessageIsNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")
	_, err := f.svc.Delete(ctx, connect.NewRequest(&huddlev1.MessageServiceDeleteRequest{
		Id: uuid.New().String(),
	}))
	if got, want := connectCode(t, err), connect.CodeNotFound; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}
