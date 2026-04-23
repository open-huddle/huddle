package message_test

import (
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	entmembership "github.com/open-huddle/huddle/apps/api/ent/membership"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// Alice is set up as a member by newFixture. addMember helps these tests
// introduce additional org members so Alice can mention them.
func addMember(t *testing.T, f *fixture, subject string) string {
	t.Helper()
	u := testutil.MakeUser(f.ctx, t, f.client, subject)
	testutil.MakeMember(f.ctx, t, f.client, u, f.org, entmembership.RoleMember)
	return u.ID.String()
}

func TestSend_AcceptsMentionsOfOrgMembers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	bobID := addMember(t, f, "bob")
	carolID := addMember(t, f, "carol")

	resp, err := f.svc.Send(ctx, connect.NewRequest(&huddlev1.MessageServiceSendRequest{
		ChannelId:      f.ch.ID.String(),
		Body:           "hey @bob and @carol",
		MentionUserIds: []string{bobID, carolID},
	}))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// The proto response carries the mention set as-sent.
	got := resp.Msg.Message.MentionUserIds
	if len(got) != 2 {
		t.Fatalf("want 2 mentions on the response, got %d", len(got))
	}
	seen := map[string]bool{got[0]: true, got[1]: true}
	if !seen[bobID] || !seen[carolID] {
		t.Errorf("response mentions missing expected ids: %v", got)
	}

	// MessageMention rows landed in the same tx.
	msgID := uuid.MustParse(resp.Msg.Message.Id)
	count, err := f.client.Message.Query().
		Where().
		QueryMentions().
		Where().
		Count(f.ctx)
	if err != nil {
		t.Fatalf("count mentions: %v", err)
	}
	if count != 2 {
		t.Fatalf("want 2 MessageMention rows, got %d", count)
	}

	// And the outbox event's payload carries the mentions so the
	// notifications consumer sees them.
	events, err := f.client.OutboxEvent.Query().All(f.ctx)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 outbox event, got %d", len(events))
	}
	if events[0].AggregateID != msgID {
		t.Errorf("outbox aggregate_id should match new message id")
	}
}

func TestSend_DropsSelfMention(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	resp, err := f.svc.Send(ctx, connect.NewRequest(&huddlev1.MessageServiceSendRequest{
		ChannelId:      f.ch.ID.String(),
		Body:           "talking to myself",
		MentionUserIds: []string{f.alice.ID.String()},
	}))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(resp.Msg.Message.MentionUserIds) != 0 {
		t.Errorf("self-mention should be silently dropped, got %v", resp.Msg.Message.MentionUserIds)
	}
}

func TestSend_DedupesRepeatedMention(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")
	bobID := addMember(t, f, "bob")

	resp, err := f.svc.Send(ctx, connect.NewRequest(&huddlev1.MessageServiceSendRequest{
		ChannelId:      f.ch.ID.String(),
		Body:           "bob bob bob",
		MentionUserIds: []string{bobID, bobID, bobID},
	}))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(resp.Msg.Message.MentionUserIds) != 1 {
		t.Fatalf("duplicates should collapse to one mention, got %v", resp.Msg.Message.MentionUserIds)
	}
}

func TestSend_RejectsNonMemberMention(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	stranger := uuid.New().String()
	_, err := f.svc.Send(ctx, connect.NewRequest(&huddlev1.MessageServiceSendRequest{
		ChannelId:      f.ch.ID.String(),
		Body:           "stranger danger",
		MentionUserIds: []string{stranger},
	}))
	if got, want := connectCode(t, err), connect.CodeInvalidArgument; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}

func TestSend_RejectsMalformedMentionID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	_, err := f.svc.Send(ctx, connect.NewRequest(&huddlev1.MessageServiceSendRequest{
		ChannelId:      f.ch.ID.String(),
		Body:           "hi",
		MentionUserIds: []string{"not-a-uuid"},
	}))
	if got, want := connectCode(t, err), connect.CodeInvalidArgument; got != want {
		t.Errorf("code: want %v got %v", want, got)
	}
}

func TestList_HydratesMentions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")
	bobID := addMember(t, f, "bob")

	if _, err := f.svc.Send(ctx, connect.NewRequest(&huddlev1.MessageServiceSendRequest{
		ChannelId:      f.ch.ID.String(),
		Body:           "@bob",
		MentionUserIds: []string{bobID},
	})); err != nil {
		t.Fatalf("Send: %v", err)
	}

	resp, err := f.svc.List(ctx, connect.NewRequest(&huddlev1.MessageServiceListRequest{
		ChannelId: f.ch.ID.String(),
	}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Msg.Messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(resp.Msg.Messages))
	}
	got := resp.Msg.Messages[0].MentionUserIds
	if len(got) != 1 || got[0] != bobID {
		t.Errorf("List did not hydrate mentions: got %v", got)
	}
}
