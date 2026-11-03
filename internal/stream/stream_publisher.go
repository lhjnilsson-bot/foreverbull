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

// unrecordedBuffer holds the states reported over core NATS for a task. The
// worker reports one, the state that ends the task, and only when the stream
// refused it; Publish reports one when the stream refused the task itself,
// and then no worker runs it. The waiter returns on the first, so the
// channel never fills. If it did, NATS would drop the new message for this
// waiter, and the connection's error handler (logAsyncError) logs that.
const unrecordedBuffer = 4

// WaitForCompletion waits for the task's final state. Each onProgress gets
// the text of every progress report the command sends meanwhile, over core
// NATS, before the final state is returned.
func WaitForCompletion(onProgress ...func(text string)) PublishOpt {
	return func(nn *NATSStream, msg *message) (func(ctx context.Context) error, func(), error) {
		if nn.nc == nil {
			return nil, nil, errors.New("error following the task: no core NATS connection for unrecorded states")
		}
		sub, err := nn.jt.SubscribeSync(msg.TaskStateTopic())
		if err != nil {
			return nil, nil, fmt.Errorf("error subscribing to jetstream: %w", err)
		}
		// A state the stream refused comes over core NATS instead, so the
		// waiter ends even when the stream can't store the final state.
		unrecorded := make(chan *nats.Msg, unrecordedBuffer)
		unrecordedSub, err := nn.nc.ChanSubscribe(msg.taskUnrecordedTopic(), unrecorded)
		if err != nil {
			unsubscribe(sub)
			return nil, nil, fmt.Errorf("error subscribing to unrecorded states: %w", err)
		}
		var progress chan *nats.Msg
		var progressSub *nats.Subscription
		if len(onProgress) > 0 {
			progress = make(chan *nats.Msg, progressBuffer)
			progressSub, err = nn.nc.ChanSubscribe(msg.taskProgressTopic(), progress)
			if err != nil {
				unsubscribe(sub, unrecordedSub)
				return nil, nil, fmt.Errorf("error subscribing to progress: %w", err)
			}
		}
		if err := nn.nc.Flush(); err != nil {
			unsubscribe(sub, unrecordedSub, progressSub)
			return nil, nil, fmt.Errorf("error subscribing to progress and unrecorded states: %w", err)
		}
		var stopOnce sync.Once
		stop := func() { stopOnce.Do(func() { unsubscribe(sub, unrecordedSub, progressSub) }) }
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
				case m := <-unrecorded:
					state := pb.State{}
					if err := proto.Unmarshal(m.Data, &state); err != nil {
						return fmt.Errorf("error unmarshalling unrecorded state: %w", err)
					}
					if done, err := finalState(&state, drain); done {
						return err
					}
					continue
				case stateMsg = <-states:
				case err := <-stateErr:
					if errors.Is(err, context.Canceled) && ctx.Err() != nil {
						_, err := nn.jt.Publish(fmt.Sprintf(TOPIC_TASK_ABORT, msg.msg.Module, msg.msg.Component, msg.msg.Method, msg.msg.GetId()), nil)
						if err != nil {
							return publishError("error publishing abort", err)
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
				if done, err := finalState(&state, drain); done {
					return err
				}
			}
		}, stop, nil
	}
}

// finalState reports whether state ends the task, and the task's result
// when it does. drain runs before a COMPLETED or ERROR result is returned.
func finalState(state *pb.State, drain func()) (bool, error) {
	switch state.State {
	case pb.State_STATE_COMPLETED:
		drain()
		if len(state.GetNotices()) > 0 {
			return true, &Notice{Lines: state.GetNotices()}
		}
		return true, nil
	case pb.State_STATE_ERROR:
		drain()
		if state.GetError() == "" {
			return true, errors.New("command failed")
		}
		return true, commandError(state.GetError())
	case pb.State_STATE_CANCELED:
		return true, fmt.Errorf("canceled")
	case pb.State_STATE_UNSPECIFIED, pb.State_STATE_CREATED,
		pb.State_STATE_PUBLISHED, pb.State_STATE_RECEIVED:
		// The task is not done yet, so keep waiting.
		return false, nil
	default:
		return true, fmt.Errorf("unknown task state %d", state.State)
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

// publishError is a refused publish. A full stream is ErrStateNotRecorded,
// whose text is the plain cause, so the NATS error is logged here with what
// failed; any other error is wrapped as before.
func publishError(what string, err error) error {
	if streamFull(err) {
		log.Err(err).Msg(what + ": the stream is full")
		return notRecorded(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// reportRefusedTask sends the ERROR for a task that was never sent, or that
// the stream refused, over core NATS on TOPIC_TASK_UNRECORDED, where the
// state listener records it.
func (nn *NATSStream) reportRefusedTask(msg *message, refusal error) error {
	if nn.nc == nil {
		return errors.New("error reporting a refused task: no core NATS connection")
	}
	state := unrecordedState(&pb.State{State: pb.State_STATE_PUBLISHED}, refusal)
	payload, err := proto.Marshal(state)
	if err != nil {
		return fmt.Errorf("error marshalling the refused task's state: %w", err)
	}
	if err := nn.nc.Publish(msg.taskUnrecordedTopic(), payload); err != nil {
		return fmt.Errorf("error reporting a refused task: %w", err)
	}
	if err := nn.nc.Flush(); err != nil {
		return fmt.Errorf("error reporting a refused task: %w", err)
	}
	return nil
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

	// PUBLISHED goes before the task: when its publish fails, the task was
	// never sent and nothing runs that the caller was told failed. That
	// holds for any error, so the task is recorded as failed even when the
	// error leaves open whether PUBLISHED was stored, and the ERROR never
	// contradicts a later state.
	_, err = nn.jt.Publish(msg.TaskStateTopic(), statePayload)
	if err != nil {
		stopAll()
		pubErr := publishError("error publishing state", err)
		if reportErr := nn.reportRefusedTask(m, err); reportErr != nil {
			return errors.Join(pubErr, reportErr)
		}
		return pubErr
	}

	_, err = nn.jt.Publish(msg.TaskTopic(), payload)
	if err != nil {
		stopAll()
		pubErr := publishError("error publishing message", err)
		if !refused(err) {
			// The task may have been stored and may run, so no ERROR is
			// recorded for it.
			return pubErr
		}
		// PUBLISHED is stored but the task never will be. Record the task
		// as failed, over core NATS as a worker would, so its states end.
		if reportErr := nn.reportRefusedTask(m, err); reportErr != nil {
			return errors.Join(pubErr, reportErr)
		}
		return pubErr
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
