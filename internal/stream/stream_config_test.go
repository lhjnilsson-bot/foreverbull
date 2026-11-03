package stream

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// fakeStreams records which stream calls ensureStream makes.
type fakeStreams struct {
	infoErr   error
	addErr    error
	updateErr error

	added   *nats.StreamConfig
	updated *nats.StreamConfig
}

func (f *fakeStreams) StreamInfo(string, ...nats.JSOpt) (*nats.StreamInfo, error) {
	if f.infoErr != nil {
		return nil, f.infoErr
	}
	return &nats.StreamInfo{}, nil
}

func (f *fakeStreams) AddStream(cfg *nats.StreamConfig, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	f.added = cfg
	return &nats.StreamInfo{Config: *cfg}, f.addErr
}

func (f *fakeStreams) UpdateStream(cfg *nats.StreamConfig, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	f.updated = cfg
	return &nats.StreamInfo{Config: *cfg}, f.updateErr
}

func TestEnsureStreamCreatesAMissingStream(t *testing.T) {
	js := &fakeStreams{infoErr: nats.ErrStreamNotFound}
	require.NoError(t, ensureStream(js, StreamConfig()))
	require.Equal(t, StreamConfig(), js.added)
	require.Nil(t, js.updated)
}

func TestEnsureStreamUpdatesAnExistingStream(t *testing.T) {
	js := &fakeStreams{}
	require.NoError(t, ensureStream(js, StreamConfig()))
	require.Equal(t, StreamConfig(), js.updated)
	require.Nil(t, js.added)
}

func TestEnsureStreamFailsWhenTheUpdateIsRefused(t *testing.T) {
	refused := errors.New("stream configuration update can not change subjects")
	js := &fakeStreams{updateErr: refused}
	err := ensureStream(js, StreamConfig())
	require.ErrorIs(t, err, refused)
	require.ErrorContains(t, err, "error updating stream foreverbull")
}

func TestEnsureStreamFailsWhenTheCreateIsRefused(t *testing.T) {
	refused := errors.New("insufficient resources")
	js := &fakeStreams{infoErr: nats.ErrStreamNotFound, addErr: refused}
	err := ensureStream(js, StreamConfig())
	require.ErrorIs(t, err, refused)
	require.ErrorContains(t, err, "error creating stream foreverbull")
}

func TestEnsureStreamFailsWhenTheStreamCantBeRead(t *testing.T) {
	unreachable := errors.New("nats: timeout")
	js := &fakeStreams{infoErr: unreachable}
	err := ensureStream(js, StreamConfig())
	require.ErrorIs(t, err, unreachable)
	require.Nil(t, js.added)
	require.Nil(t, js.updated)
}
