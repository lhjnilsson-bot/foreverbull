package stream

import (
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

// A command whose event can't be published fails instead of completing.
func TestPublishEventFails(t *testing.T) {
	err := publishEvent(failingPublisher{err: errors.New("stream full")}, "backtest", "session", "run", &pb.Message{Id: "m-1"})
	require.EqualError(t, err, "error publishing event: stream full")
	require.ErrorIs(t, err, ErrServicesUnreachable)
	require.EqualError(t, stateError(err), "foreverbull's services couldn't reach each other")
}
