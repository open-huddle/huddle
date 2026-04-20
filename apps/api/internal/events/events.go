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

// Publisher fans out domain events. Implementations should be safe for
// concurrent use.
type Publisher interface {
	// PublishMessageCreated emits a "message created" event for the channel.
	// The call is best-effort: a failed publish must not roll back the
	// originating database write. Phase 2c relies on the database as the
	// source of truth and treats the bus as a fast path for live clients;
	// later phases (Debezium CDC) will deliver durable replay.
	PublishMessageCreated(ctx context.Context, msg *huddlev1.Message) error
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
