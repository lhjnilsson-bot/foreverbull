package stream

import (
	"context"
	"errors"
	"testing"

	"github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

type failingPublisher struct{ err error }

func (p failingPublisher) Publish(string, []byte, ...nats.PubOpt) (*nats.PubAck, error) {
	return nil, p.err
}

// A command whose event the stream refuses fails instead of completing.
func TestPublishEventFails(t *testing.T) {
	err := publishEvent(failingPublisher{err: nats.ErrNoStreamResponse}, "backtest", "session", "run", &pb.Message{Id: "m-1"})
	require.EqualError(t, err, "error publishing event: nats: no response from stream")
	require.ErrorIs(t, err, ErrServicesUnreachable)
	require.EqualError(t, stateError(err), "foreverbull's services couldn't reach each other")
}

// An event whose publish may have been stored doesn't fail the command.
func TestPublishEventThatMayBeStoredCompletes(t *testing.T) {
	for _, err := range []error{nats.ErrTimeout, context.DeadlineExceeded, nats.ErrConnectionClosed, errors.New("broken pipe")} {
		require.NoError(t, publishEvent(failingPublisher{err: err}, "backtest", "session", "run", &pb.Message{Id: "m-1"}), "%v", err)
	}
}

func TestPublishEventOnAFullStreamNamesTheFullQueue(t *testing.T) {
	err := publishEvent(failingPublisher{err: errStreamFull}, "backtest", "session", "run", &pb.Message{Id: "m-1"})
	require.ErrorIs(t, err, ErrStateNotRecorded)
	require.EqualError(t, stateError(err), "foreverbull's task queue is full, so it couldn't record the task's progress")
}
