// Package events is the API's interface to the cluster event bus. Phase 2c
// uses it for realtime message fan-out; later phases (search indexing, audit
// log, notifications) will subscribe to the same subjects.
//
// The Publisher and Subscriber interfaces are deliberately narrow: handlers
// depend on these, not on the NATS client. A future swap (or a no-op stub
// for tests) fits behind the same shape.
package events

import (
	"context"

	"github.com/google/uuid"

	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// SubjectMessageCreated returns the NATS subject for a "message created"
// event on the given channel. Exposed so the outbox-writer and subscribers
// agree on the wire shape without duplicating the format string.
func SubjectMessageCreated(channelID uuid.UUID) string {
	return "huddle.messages.created." + channelID.String()
}

// SubjectMessageEdited returns the NATS subject for a "message edited"
// event on the given channel. Parallel shape to Created so future
// subscribers can fan in on the same channel prefix.
func SubjectMessageEdited(channelID uuid.UUID) string {
	return "huddle.messages.edited." + channelID.String()
}

// SubjectMessageDeleted returns the NATS subject for a "message deleted"
// event. The payload carries only the message id + channel id — there is
// no body to ship.
func SubjectMessageDeleted(channelID uuid.UUID) string {
	return "huddle.messages.deleted." + channelID.String()
}

// Publisher fans out domain events. Implementations should be safe for
// concurrent use.
//
// The caller owns serialization: the payload is the exact byte stream
// subscribers will decode. Keeps the interface narrow enough to swap NATS
// for Kafka / in-memory / a CDC bridge without touching domain code.
type Publisher interface {
	Publish(ctx context.Context, subject string, payload []byte) error
}

// Subscriber feeds events from the bus into per-call channels. The returned
// channel is closed when ctx is cancelled or the subscription is torn down
// (slow-consumer disconnect, broker restart). Callers do not unsubscribe
// explicitly — context cancellation is the lifecycle.
type Subscriber interface {
	// SubscribeMessages returns a channel of new messages for one chat
	// channel, starting at "now" (no replay). Older messages should be
	// fetched via MessageService.List.
	SubscribeMessages(ctx context.Context, channelID uuid.UUID) (<-chan *huddlev1.Message, error)
}
