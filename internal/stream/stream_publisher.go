package stream

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/nats-io/nats.go"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// PublishOpt follows a published task. It is set up before the task is
// published, so it sees everything the task sends, and returns the wait that
// runs once the task is out and the stop that ends its subscriptions. The
// wait stops them itself; Publish calls stop when the wait never runs. A
// setup error stops the publish.
type PublishOpt func(nn *NATSStream, msg *message) (wait func(ctx context.Context) error, stop func(), err error)

// progressBuffer is how many progress texts can queue for a waiter. With
// ChanSubscribe a full channel makes NATS drop the new messages for that
// waiter and flag it as a slow consumer; the queued, older texts stay. The
// next report after the waiter catches up carries the current count again,
// so a drop only delays the line.
const progressBuffer = 64

// WaitForCompletion waits for the task's final state. Each onProgress gets
// the text of every progress report the command sends meanwhile, over core
// NATS, before the final state is returned.
func WaitForCompletion(onProgress ...func(text string)) PublishOpt {
	return func(nn *NATSStream, msg *message) (func(ctx context.Context) error, func(), error) {
		sub, err := nn.jt.SubscribeSync(msg.TaskStateTopic())
		if err != nil {
			return nil, nil, fmt.Errorf("error subscribing to jetstream: %w", err)
		}
		var progress chan *nats.Msg
		var progressSub *nats.Subscription
		if len(onProgress) > 0 && nn.nc != nil {
			progress = make(chan *nats.Msg, progressBuffer)
			progressSub, err = nn.nc.ChanSubscribe(msg.taskProgressTopic(), progress)
			if err == nil {
				err = nn.nc.Flush()
			}
			if err != nil {
				unsubscribe(sub, progressSub)
				return nil, nil, fmt.Errorf("error subscribing to progress: %w", err)
			}
		}
		var stopOnce sync.Once
		stop := func() { stopOnce.Do(func() { unsubscribe(sub, progressSub) }) }
		return func(ctx context.Context) error {
			defer stop()
			report := func(m *nats.Msg) {
				for _, onText := range onProgress {
					onText(string(m.Data))
				}
			}
			// drain reports the progress sent before the final state. The
			// flush is a round trip on this connection, so every progress
			// message the server routed before it is in the channel.
			drain := func() {
				if progress == nil {
					return
				}
				if err := nn.nc.Flush(); err != nil {
					log.Err(err).Msg("error flushing before the last progress")
				}
				for {
					select {
					case m := <-progress:
						report(m)
					default:
						return
					}
				}
			}

			states := make(chan *nats.Msg)
			stateErr := make(chan error, 1)
			stateCtx, stopStates := context.WithCancel(ctx)
			defer stopStates()
			go func() {
				for {
					stateMsg, err := sub.NextMsgWithContext(stateCtx)
					if err != nil {
						stateErr <- err
						return
					}
					select {
					case states <- stateMsg:
					case <-stateCtx.Done():
						return
					}
				}
			}()

			for {
				var stateMsg *nats.Msg
				select {
				case m := <-progress:
					report(m)
					continue
				case stateMsg = <-states:
				case err := <-stateErr:
					if errors.Is(err, context.Canceled) && ctx.Err() != nil {
						_, err := nn.jt.Publish(fmt.Sprintf(TOPIC_TASK_ABORT, msg.msg.Module, msg.msg.Component, msg.msg.Method, msg.msg.GetId()), nil)
						if err != nil {
							return fmt.Errorf("error publishing abort: %w", err)
						}
						return context.Canceled
					}
					return fmt.Errorf("error getting message: %w", err)
				}
				state := pb.State{}
				if err := proto.Unmarshal(stateMsg.Data, &state); err != nil {
					return fmt.Errorf("error unmarshalling message: %w", err)
				}
				if err := stateMsg.Ack(); err != nil {
					return fmt.Errorf("error acking message: %w", err)
				}

				switch state.State {
				case pb.State_STATE_COMPLETED:
					drain()
					if len(state.GetNotices()) > 0 {
						return &Notice{Lines: state.GetNotices()}
					}
					return nil
				case pb.State_STATE_ERROR:
					drain()
					if state.GetError() == "" {
						return errors.New("command failed")
					}
					return commandError(state.GetError())
				case pb.State_STATE_CANCELED:
					return fmt.Errorf("canceled")
				case pb.State_STATE_UNSPECIFIED, pb.State_STATE_CREATED,
					pb.State_STATE_PUBLISHED, pb.State_STATE_RECEIVED:
					// The task is not done yet, so keep waiting.
				default:
					return fmt.Errorf("unknown task state %d", state.State)
				}
			}
		}, stop, nil
	}
}

// unsubscribe ends each subscription that was made.
func unsubscribe(subs ...*nats.Subscription) {
	for _, sub := range subs {
		if sub == nil {
			continue
		}
		if err := sub.Unsubscribe(); err != nil {
			log.Err(err).Msg("error unsubscribing from nats")
		}
	}
}

func (nn *NATSStream) Publish(ctx context.Context, msg Message, opts ...PublishOpt) error {
	m, isMsg := msg.(*message)
	if !isMsg {
		return fmt.Errorf("invalid message")
	}
	payload, err := proto.Marshal(m.msg)
	if err != nil {
		return fmt.Errorf("error marshalling message: %w", err)
	}
	state := pb.State{State: pb.State_STATE_PUBLISHED, OccurredAt: timestamppb.Now()}
	statePayload, err := proto.Marshal(&state)
	if err != nil {
		return fmt.Errorf("error marshalling state: %w", err)
	}
	waits := make([]func(context.Context) error, 0, len(opts))
	stops := make([]func(), 0, len(opts))
	stopAll := func() {
		for _, stop := range stops {
			stop()
		}
	}
	for _, opt := range opts {
		wait, stop, err := opt(nn, m)
		if err != nil {
			stopAll()
			return err
		}
		waits = append(waits, wait)
		stops = append(stops, stop)
	}

	_, err = nn.jt.Publish(msg.TaskTopic(), payload)
	if err != nil {
		stopAll()
		return fmt.Errorf("error publishing message: %w", err)
	}

	_, err = nn.jt.Publish(msg.TaskStateTopic(), statePayload)
	if err != nil {
		stopAll()
		return fmt.Errorf("error publishing state: %w", err)
	}

	grp, _ := errgroup.WithContext(ctx)
	for _, wait := range waits {
		grp.Go(func() error {
			return wait(ctx)
		})
	}
	if err := grp.Wait(); err != nil {
		return err
	}
	return nil
}
