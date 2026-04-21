package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// Stream + subject layout. Versioning the subject prefix (`huddle.`) lets a
// future v2 of the schema coexist on the same NATS cluster.
const (
	streamName            = "messages"
	subjectMessageCreated = "huddle.messages.created"
	subjectAllMessages    = "huddle.messages.>"
)

// NATS implements Publisher and Subscriber on top of JetStream.
type NATS struct {
	conn   *nats.Conn
	js     jetstream.JetStream
	logger *slog.Logger
}

// Open dials NATS, ensures the messages stream exists, and returns a NATS
// client ready for use. The stream is created idempotently — bumping
// retention here on a new release will update an existing deployment in
// place rather than fail.
func Open(ctx context.Context, url string, logger *slog.Logger) (*NATS, error) {
	conn, err := nats.Connect(url,
		nats.Name("huddle-api"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}

	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("jetstream client: %w", err)
	}

	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        streamName,
		Description: "Domain events for messages (Phase 2c realtime; future audit / search / notifications consumers).",
		Subjects:    []string{subjectAllMessages},
		Storage:     jetstream.FileStorage,
		// Bounded retention — JetStream is the live fast-path, not the
		// durable record. The DB is authoritative; clients backfill via
		// MessageService.List on reconnect.
		Retention: jetstream.LimitsPolicy,
		MaxAge:    24 * time.Hour,
		MaxMsgs:   100_000,
		Discard:   jetstream.DiscardOld,
	}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("create stream %q: %w", streamName, err)
	}

	return &NATS{conn: conn, js: js, logger: logger}, nil
}

// Close drains in-flight messages and shuts the connection down.
func (n *NATS) Close() {
	if err := n.conn.Drain(); err != nil {
		n.logger.Warn("nats drain", "err", err)
	}
}

func (n *NATS) PublishMessageCreated(ctx context.Context, msg *huddlev1.Message) error {
	if msg == nil || msg.ChannelId == "" {
		return errors.New("publish: message missing channel_id")
	}
	payload, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	subject := fmt.Sprintf("%s.%s", subjectMessageCreated, msg.ChannelId)
	if _, err := n.js.Publish(ctx, subject, payload); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

func (n *NATS) SubscribeMessages(ctx context.Context, channelID uuid.UUID) (<-chan *huddlev1.Message, error) {
	subject := fmt.Sprintf("%s.%s", subjectMessageCreated, channelID)

	// Ephemeral consumer — no client-side state, no need to track position.
	// DeliverNew means "from now"; MessageService.List backfills history.
	cons, err := n.js.CreateConsumer(ctx, streamName, jetstream.ConsumerConfig{
		FilterSubject: subject,
		DeliverPolicy: jetstream.DeliverNewPolicy,
		AckPolicy:     jetstream.AckNonePolicy,
		// If the subscriber disconnects, the consumer self-removes after
		// this window — keeps server resources bounded if a client drops.
		InactiveThreshold: 30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("create consumer: %w", err)
	}

	// Buffered slightly so a brief blip in the websocket write does not
	// stall the NATS callback. JetStream pull-based delivery handles real
	// backpressure beyond that.
	out := make(chan *huddlev1.Message, 64)

	consumeCtx, err := cons.Consume(func(jmsg jetstream.Msg) {
		m := &huddlev1.Message{}
		if err := proto.Unmarshal(jmsg.Data(), m); err != nil {
			n.logger.Warn("subscriber: unmarshal", "err", err, "subject", jmsg.Subject())
			return
		}
		select {
		case out <- m:
		case <-ctx.Done():
		}
	})
	if err != nil {
		return nil, fmt.Errorf("consume: %w", err)
	}

	// One goroutine owns teardown: when the caller's context ends (client
	// disconnect or server shutdown), stop the JetStream consumer and close
	// the channel so the handler's range loop exits cleanly.
	go func() {
		<-ctx.Done()
		consumeCtx.Stop()
		close(out)
	}()

	return out, nil
}
