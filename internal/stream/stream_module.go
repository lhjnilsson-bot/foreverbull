package stream

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/nats-io/nats.go"
	"github.com/rs/zerolog/log"
	"go.uber.org/fx"
	"google.golang.org/protobuf/proto"
)

type stateSubscriber struct {
	repository *Repository
	jt         nats.JetStreamContext
	nc         *nats.Conn

	sub           *nats.Subscription
	unrecordedSub *nats.Subscription
}

// msgHandler records a task state. It runs on a NATS callback with no
// caller to return to, so every failure is logged at error level with the
// subject that names the message.
func (s *stateSubscriber) msgHandler(msg *nats.Msg) {
	state := &pb.State{}
	err := proto.Unmarshal(msg.Data, state)
	if err != nil {
		log.Err(err).Str("subject", msg.Subject).Msg("error unmarshalling message state")
		return
	}

	messageID, err := stateMessageID(msg.Subject)
	if err != nil {
		log.Err(err).Msg("error recording message state")
		return
	}
	err = s.repository.CreateMessageState(context.Background(), messageID, state)
	if err != nil {
		log.Err(err).Str("subject", msg.Subject).Str("id", messageID).Msg("error creating message state")
	}
}

// unrecordedHandler records a state a worker reported over core NATS
// because the stream refused it, so message_states still names how the
// task ended. Like msgHandler it has no caller, so every failure is logged
// at error level with the subject.
func (s *stateSubscriber) unrecordedHandler(msg *nats.Msg) {
	state := &pb.State{}
	if err := proto.Unmarshal(msg.Data, state); err != nil {
		log.Err(err).Str("subject", msg.Subject).Msg("error unmarshalling unrecorded state")
		return
	}
	messageID, err := unrecordedMessageID(msg.Subject)
	if err != nil {
		log.Err(err).Msg("error recording unrecorded state")
		return
	}
	if err := s.repository.CreateMessageState(context.Background(), messageID, state); err != nil {
		log.Err(err).Str("subject", msg.Subject).Str("id", messageID).Msg("error creating unrecorded message state")
	}
}

// unrecordedMessageID reads the message id from a TOPIC_TASK_UNRECORDED
// subject, foreverbull-unrecorded.<module>.<component>.<method>.<id>.
func unrecordedMessageID(subject string) (string, error) {
	parts := strings.Split(subject, ".")
	if len(parts) != 5 || parts[0] != "foreverbull-unrecorded" || parts[4] == "" {
		return "", fmt.Errorf("unrecorded subject %q isn't foreverbull-unrecorded.<module>.<component>.<method>.<id>", subject)
	}
	return parts[4], nil
}

// unrecordedQueue is the queue group of the state listeners' unrecorded
// subscriptions.
const unrecordedQueue = "foreverbull-state-listener"

// startUnrecorded subscribes to the states workers report over core NATS.
// The flush makes the subscription live before Start returns.
func (s *stateSubscriber) startUnrecorded() error {
	if s.nc == nil {
		return errors.New("error subscribing to unrecorded states: no core NATS connection")
	}
	// A queue group, so with more than one broker one of them records each
	// state, as the durable consumer does for the stream's states.
	sub, err := s.nc.QueueSubscribe("foreverbull-unrecorded.*.*.*.*", unrecordedQueue, s.unrecordedHandler)
	if err != nil {
		return fmt.Errorf("error subscribing to unrecorded states: %w", err)
	}
	if err := s.nc.Flush(); err != nil {
		return errors.Join(fmt.Errorf("error subscribing to unrecorded states: %w", err), sub.Unsubscribe())
	}
	s.unrecordedSub = sub
	return nil
}

// stateMessageID reads the message id from a TOPIC_TASK_STATE subject,
// foreverbull.<module>.<component>.<method>.<id>.state.
func stateMessageID(subject string) (string, error) {
	parts := strings.Split(subject, ".")
	if len(parts) != 6 || parts[0] != "foreverbull" || parts[5] != "state" || parts[4] == "" {
		return "", fmt.Errorf("state subject %q isn't foreverbull.<module>.<component>.<method>.<id>.state", subject)
	}
	return parts[4], nil
}

func (s *stateSubscriber) Start(ctx context.Context) error {
	var err error

	opts := []nats.SubOpt{
		nats.MaxDeliver(1),
		nats.Durable("foreverbull-state-listener"),
	}
	deliverPolicy := environment.GetNATSDeliveryPolicy()

	switch deliverPolicy {
	case "all":
		opts = append(opts, nats.DeliverAll())
	case "last":
		opts = append(opts, nats.DeliverLast())
	default:
		return fmt.Errorf("unknown delivery policy: %s", environment.GetNATSDeliveryPolicy())
	}

	if err := s.startUnrecorded(); err != nil {
		return err
	}
	s.sub, err = s.jt.Subscribe("foreverbull.*.*.*.*.state", s.msgHandler, opts...)
	if err != nil {
		return errors.Join(fmt.Errorf("error subscribing to jetstream: %w", err), s.unrecordedSub.Unsubscribe())
	}
	return nil
}

func (s *stateSubscriber) Stop(ctx context.Context) error {
	var errs []error
	if err := s.sub.Unsubscribe(); err != nil {
		errs = append(errs, fmt.Errorf("error unsubscribing from jetstream for orchestration: %w", err))
	}
	if err := s.unrecordedSub.Unsubscribe(); err != nil {
		errs = append(errs, fmt.Errorf("error unsubscribing from unrecorded states: %w", err))
	}
	return errors.Join(errs...)
}

var Module = fx.Options(
	fx.Provide(
		func() (Stream, error) {
			return NewNATSStream()
		},
		func(_ Stream, conn *pgxpool.Pool) (*stateSubscriber, error) {
			ctx := context.Background()
			repo := NewRepository(conn)
			if err := CreateTables(ctx, conn); err != nil {
				return nil, fmt.Errorf("error creating tables: %w", err)
			}
			natsConn, err := nats.Connect(environment.GetNATSURL(), natsOptions()...)
			if err != nil {
				return nil, fmt.Errorf("error connecting to nats: %w", err)
			}

			jt, err := natsConn.JetStream()
			if err != nil {
				return nil, fmt.Errorf("error connecting to jetstream: %w", err)
			}
			return &stateSubscriber{repository: repo, jt: jt, nc: natsConn}, nil
		},
	),
	fx.Invoke(
		func(lc fx.Lifecycle, sub *stateSubscriber) error {
			lc.Append(
				fx.Hook{
					OnStart: func(ctx context.Context) error {
						return sub.Start(ctx)
					},
					OnStop: func(ctx context.Context) error {
						return sub.Stop(ctx)
					},
				},
			)
			return nil
		},
	),
)
