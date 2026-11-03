package stream

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/nats-io/nats.go"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type NatsSubscriber struct {
	module string

	jt   nats.JetStreamContext
	nc   *nats.Conn
	subs []*nats.Subscription

	deps *dependencyContainer
}

func (ns *NatsSubscriber) Subscribe(component, method string, cb func(context.Context, Message) error) error {
	// publishState sets Error only for err; notices ride in their own field
	// so a completed state never carries an error.
	publishState := func(msg *message, state pb.State_State, err error, notices ...string) error {
		s := pb.State{State: state, OccurredAt: timestamppb.Now(), Notices: notices}
		if err != nil {
			errStr := err.Error()
			s.Error = &errStr
		}
		payload, err := proto.Marshal(&s)
		if err != nil {
			return fmt.Errorf("error marshalling state: %w", err)
		}

		s2 := pb.State{}
		err = proto.Unmarshal(payload, &s2)
		if err != nil {
			return fmt.Errorf("error unmarshalling state: %w", err)
		}
		_, err = ns.jt.Publish(fmt.Sprintf(TOPIC_TASK_STATE, ns.module, msg.msg.Component, msg.msg.Method, msg.msg.GetId()), payload)
		if err != nil {
			pubErr := fmt.Errorf("error publishing state %s: %w", state, err)
			if !refused(err) {
				// A lost ack, a deadline, or a dropped connection: the
				// server may have stored the state, so nothing that could
				// contradict it is sent.
				return &unconfirmedError{err: pubErr}
			}
			// The stream refused the state, so nobody following the stream
			// sees it. Report it over core NATS so the task still ends.
			if reportErr := ns.reportUnrecorded(msg, &s2, err); reportErr != nil {
				return errors.Join(pubErr, reportErr)
			}
			return pubErr
		}
		return nil
	}

	jtCb := func(natsMsg *nats.Msg) {
		msg := &message{
			msg:                 &pb.Message{},
			dependencyContainer: ns.deps,
		}
		err := proto.Unmarshal(natsMsg.Data, msg.msg)
		if err != nil {
			log.Err(err).Any("data", string(natsMsg.Data)).Msg("error unmarshalling message")
			return
		}
		err = publishState(msg, pb.State_STATE_RECEIVED, nil)
		var unconfirmed *unconfirmedError
		switch {
		case errors.As(err, &unconfirmed):
			// RECEIVED may be stored. Stopping here would leave the waiter
			// on a task that never ends, so the command runs; the waiter
			// doesn't need RECEIVED to see the final state.
			log.Err(err).Str("id", msg.msg.GetId()).Msg("RECEIVED may not be stored; running the command")
		case err != nil:
			log.Err(err).Str("id", msg.msg.GetId()).Msg("error publishing state")
			return
		}
		ctx := context.Background()
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		sub, err := ns.jt.Subscribe(fmt.Sprintf(TOPIC_TASK_ABORT, ns.module, msg.msg.Component, msg.msg.Method, msg.msg.GetId()), func(natsMsg *nats.Msg) {
			log.Info().Msg("aborting")
			cancel()
		})
		if err != nil {
			// Without the abort subscription the command can't be cancelled,
			// so it isn't run. The sender is waiting on RECEIVED; tell it.
			err = Unreachable(fmt.Errorf("error subscribing to abort: %w", err))
			log.Err(err).Str("id", msg.msg.GetId()).Msg("error starting command")
			if pubErr := publishState(msg, pb.State_STATE_ERROR, stateError(err)); pubErr != nil {
				log.Err(pubErr).Str("id", msg.msg.GetId()).Msg("error publishing state")
			}
			return
		}
		defer func() {
			if err := sub.Unsubscribe(); err != nil {
				log.Err(err).Msg("error unsubscribing from jetstream")
			}
		}()
		defer func() {
			if r := recover(); r != nil {
				fmt.Println(string(debug.Stack())) // nolint: forbidigo
				log.Err(fmt.Errorf("panic: %v", r)).Stack().Str("component", component).Str("method", method).Msg("panic in command subscriber")
				if err := publishState(msg, pb.State_STATE_ERROR, fmt.Errorf("panic: %v", r)); err != nil {
					log.Err(err).Msg("error publishing state")
				}
			}
		}()
		// Progress goes over core NATS, not as a task state: it isn't kept
		// on the JetStream stream and gets no message_states row.
		progressTopic := fmt.Sprintf(TOPIC_TASK_PROGRESS, ns.module, msg.msg.Component, msg.msg.Method, msg.msg.GetId())
		ctx = WithProgress(ctx, func(text string) {
			if ns.nc == nil {
				return
			}
			if err := ns.nc.Publish(progressTopic, []byte(text)); err != nil {
				log.Err(err).Msg("error publishing progress")
			}
		})
		err = cb(ctx, msg)
		var notices []string
		var notice *Notice
		if errors.As(err, &notice) {
			// A notice is a completed command; its lines ride on the state.
			notices = notice.Lines
			err = nil
		}
		if err == nil {
			// The event goes out before COMPLETED, so a failed publish is
			// reported as ERROR rather than after the sender saw success.
			err = publishEvent(ns.jt, ns.module, component, method, msg.msg)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				log.Info().Msg("command canceled")
				if err := publishState(msg, pb.State_STATE_CANCELED, nil); err != nil {
					log.Err(err).Str("id", msg.msg.GetId()).Msg("error publishing state")
				}
				return
			}
			log.Err(err).Msg("error executing command")
			if err := publishState(msg, pb.State_STATE_ERROR, stateError(err)); err != nil {
				log.Err(err).Str("id", msg.msg.GetId()).Msg("error publishing state")
			}
			return
		}
		if err := publishState(msg, pb.State_STATE_COMPLETED, nil, notices...); err != nil {
			// The callback has no caller to return to; the sender times out
			// waiting on a final state.
			log.Err(err).Str("id", msg.msg.GetId()).Msg("error publishing state")
			return
		}
		log.Info().Msg("command completed successfully")
	}

	topic := fmt.Sprintf(TOPIC_TASK, ns.module, component, method)
	name := strings.ReplaceAll(topic, ".", "-")
	consumerCfg := nats.ConsumerConfig{
		Name:           name,
		Durable:        name,
		AckPolicy:      nats.AckNonePolicy,
		DeliverSubject: name,
		FilterSubject:  topic,
	}
	_, err := ns.jt.AddConsumer("foreverbull", &consumerCfg)
	if err != nil {
		return fmt.Errorf("error creating consumer: %w", err)
	}

	opts := []nats.SubOpt{
		nats.Durable(name),
	}

	/* Unsure if this is needed
	deliverPolicy := environment.GetNATSDeliveryPolicy()
	switch deliverPolicy {
	case "all":
		fmt.Println("ALL")
		opts = append(opts, nats.DeliverAll())
	case "last":
		fmt.Println("LAST")
		opts = append(opts, nats.DeliverLast())
	default:
		return fmt.Errorf("unknown delivery policy: %s", environment.GetNATSDeliveryPolicy())
	}
	*/

	sub, err := ns.jt.Subscribe(fmt.Sprintf(TOPIC_TASK, ns.module, component, method), jtCb, opts...)
	if err != nil {
		return fmt.Errorf("error subscribing to jetstream: %w", err)
	}

	ns.subs = append(ns.subs, sub)
	return nil
}

func (ns *NatsSubscriber) Unsubscribe() error {
	for _, sub := range ns.subs {
		if !sub.IsValid() {
			continue
		}

		err := sub.Drain()
		if err != nil {
			return fmt.Errorf("error draining subscription: %w", err)
		}
	}

	return nil
}

// unconfirmedError is a state publish whose outcome is unknown: the
// stream may have stored it. Nothing is reported for it over core NATS.
type unconfirmedError struct{ err error }

func (e *unconfirmedError) Error() string {
	return e.err.Error() + " (the stream may have stored it)"
}
func (e *unconfirmedError) Unwrap() error { return e.err }

// unrecordedState is the state a worker reports over core NATS when the
// stream refused state with refusal. A final ERROR or CANCELED keeps its
// state and cause. Any other state becomes ERROR: a task whose RECEIVED or
// COMPLETED wasn't stored can't be followed to its end on the stream, so it
// fails with a named cause. The cause is ErrStateNotRecorded for a full
// stream and ErrServicesUnreachable for any other refusal. The NATS error
// itself goes only to the log.
func unrecordedState(state *pb.State, refusal error) *pb.State {
	if state.GetState() == pb.State_STATE_ERROR || state.GetState() == pb.State_STATE_CANCELED {
		return state
	}
	cause := ErrServicesUnreachable.Error()
	if streamFull(refusal) {
		cause = ErrStateNotRecorded.Error()
	}
	return &pb.State{State: pb.State_STATE_ERROR, OccurredAt: timestamppb.Now(), Error: &cause}
}

// reportUnrecorded sends the unrecordedState of state and the stream's
// refusal over core NATS on
// TOPIC_TASK_UNRECORDED, where WaitForCompletion and the state listener
// read it. Core NATS doesn't go through the stream, so it works when the
// stream refuses publishes. The flush makes a broken connection an error
// here rather than a message lost in a buffer.
func (ns *NatsSubscriber) reportUnrecorded(msg *message, state *pb.State, refusal error) error {
	if ns.nc == nil {
		return errors.New("error reporting an unrecorded state: no core NATS connection")
	}
	payload, err := proto.Marshal(unrecordedState(state, refusal))
	if err != nil {
		return fmt.Errorf("error marshalling unrecorded state: %w", err)
	}
	topic := fmt.Sprintf(TOPIC_TASK_UNRECORDED, ns.module, msg.msg.Component, msg.msg.Method, msg.msg.GetId())
	if err := ns.nc.Publish(topic, payload); err != nil {
		return fmt.Errorf("error reporting an unrecorded state: %w", err)
	}
	if err := ns.nc.Flush(); err != nil {
		return fmt.Errorf("error reporting an unrecorded state: %w", err)
	}
	return nil
}

type eventPublisher interface {
	Publish(subj string, data []byte, opts ...nats.PubOpt) (*nats.PubAck, error)
}

// publishEvent announces a completed command on its event subject. Only a
// definite refusal (refused) fails the command: a full stream with
// ErrStateNotRecorded, any other with ErrServicesUnreachable.
func publishEvent(js eventPublisher, module, component, method string, msg *pb.Message) error {
	payload, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("error marshalling event: %w", err)
	}
	subject := fmt.Sprintf("foreverbull.%s.%s.%s.event", module, component, method)
	if _, err := js.Publish(subject, payload); err != nil {
		switch {
		case streamFull(err):
			return notRecorded(fmt.Errorf("error publishing event: %w", err))
		case refused(err):
			return Unreachable(fmt.Errorf("error publishing event: %w", err))
		default:
			// A lost ack, a deadline, or a dropped connection: the stream
			// may have stored the event, so the command isn't failed for
			// it. Nothing in the broker subscribes to events, so a lost
			// one leaves no task waiting.
			log.Err(err).Str("subject", subject).Str("id", msg.GetId()).
				Msg("event may not be stored; completing the command")
		}
	}
	return nil
}
