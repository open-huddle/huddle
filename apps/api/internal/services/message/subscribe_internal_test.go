package message

// Internal-package tests covering the buildSubscribeResponse dispatch
// that drives the oneof variant on MessageServiceSubscribeResponse.
// Living inside the package (vs message_test) so we can reach the
// unexported helper directly — the full end-to-end Connect streaming
// path is exercised by the integration test suite against a running
// NATS + API.

import (
	"testing"

	"github.com/google/uuid"

	"github.com/open-huddle/huddle/apps/api/internal/events"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

func TestBuildSubscribeResponse_Created(t *testing.T) {
	t.Parallel()
	msg := &huddlev1.Message{
		Id: uuid.New().String(), ChannelId: uuid.New().String(),
		AuthorId: uuid.New().String(), Body: "hi",
	}
	resp, err := buildSubscribeResponse(&events.MessageEvent{
		Kind: events.MessageEventCreated, Message: msg,
	})
	if err != nil {
		t.Fatalf("buildSubscribeResponse: %v", err)
	}
	created := resp.GetCreated()
	if created == nil {
		t.Fatalf("want Created variant, got %T", resp.GetEvent())
	}
	if created.Message.Body != "hi" {
		t.Errorf("body: want %q got %q", "hi", created.Message.Body)
	}
	if resp.GetEdited() != nil || resp.GetDeleted() != nil {
		t.Error("only the Created variant should be set")
	}
}

func TestBuildSubscribeResponse_Edited(t *testing.T) {
	t.Parallel()
	msg := &huddlev1.Message{
		Id: uuid.New().String(), ChannelId: uuid.New().String(),
		AuthorId: uuid.New().String(), Body: "hi (edited)",
	}
	resp, err := buildSubscribeResponse(&events.MessageEvent{
		Kind: events.MessageEventEdited, Message: msg,
	})
	if err != nil {
		t.Fatalf("buildSubscribeResponse: %v", err)
	}
	edited := resp.GetEdited()
	if edited == nil {
		t.Fatalf("want Edited variant, got %T", resp.GetEvent())
	}
	if edited.Message.Body != "hi (edited)" {
		t.Errorf("body: want %q got %q", "hi (edited)", edited.Message.Body)
	}
}

func TestBuildSubscribeResponse_Deleted(t *testing.T) {
	t.Parallel()
	msgID := uuid.New().String()
	chID := uuid.New().String()
	resp, err := buildSubscribeResponse(&events.MessageEvent{
		Kind:    events.MessageEventDeleted,
		Message: &huddlev1.Message{Id: msgID, ChannelId: chID},
	})
	if err != nil {
		t.Fatalf("buildSubscribeResponse: %v", err)
	}
	deleted := resp.GetDeleted()
	if deleted == nil {
		t.Fatalf("want Deleted variant, got %T", resp.GetEvent())
	}
	if deleted.MessageId != msgID {
		t.Errorf("message_id: want %s got %s", msgID, deleted.MessageId)
	}
	if deleted.ChannelId != chID {
		t.Errorf("channel_id: want %s got %s", chID, deleted.ChannelId)
	}
	// Delete responses carry ids only — no body rides along even though
	// the source MessageEvent has a full Message on it. That's the whole
	// point of distinguishing the wire shape.
}

func TestBuildSubscribeResponse_UnknownKindErrors(t *testing.T) {
	t.Parallel()
	_, err := buildSubscribeResponse(&events.MessageEvent{
		Kind:    events.MessageEventUnknown,
		Message: &huddlev1.Message{Id: uuid.New().String()},
	})
	if err == nil {
		t.Fatal("expected error for unknown kind; handler's caller logs and drops this")
	}
}

func TestBuildSubscribeResponse_NilMessageErrors(t *testing.T) {
	t.Parallel()
	_, err := buildSubscribeResponse(&events.MessageEvent{
		Kind: events.MessageEventCreated, Message: nil,
	})
	if err == nil {
		t.Fatal("expected error for nil message; signals a bug in the subscriber")
	}
}
