package stream_test

import (
	"testing"
	"time"

	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func jetStream(t *testing.T) nats.JetStreamContext {
	t.Helper()
	natsConnect, err := nats.Connect(environment.GetNATSURL())
	require.NoError(t, err)
	t.Cleanup(natsConnect.Close)
	js, err := natsConnect.JetStream()
	require.NoError(t, err)
	return js
}

// requireCurrentConfig checks the live foreverbull stream against
// StreamConfig.
func requireCurrentConfig(t *testing.T, js nats.JetStreamContext) {
	t.Helper()
	want := stream.StreamConfig()
	info, err := js.StreamInfo(stream.StreamName)
	require.NoError(t, err)
	require.Equal(t, want.Subjects, info.Config.Subjects)
	require.Equal(t, want.MaxAge, info.Config.MaxAge)
	wantBytes := want.MaxBytes
	if wantBytes == 0 {
		wantBytes = -1 // the server stores an unset byte limit as -1
	}
	require.Equal(t, wantBytes, info.Config.MaxBytes)
	require.Equal(t, want.Discard, info.Config.Discard)
}

func TestNewNATSStreamUpdatesAStreamWithAnotherConfig(t *testing.T) {
	test_helper.SetupEnvironment(t, &test_helper.Containers{NATS: true})
	js := jetStream(t)

	_, err := stream.NewNATSStream()
	require.NoError(t, err)

	// A configuration no broker uses, as left by another version. Its
	// limits are looser than any real one, so the shared test stream loses
	// nothing other tests are using.
	drifted := stream.StreamConfig()
	drifted.MaxAge = 48 * time.Hour
	drifted.MaxBytes = -1
	drifted.Discard = nats.DiscardOld
	_, err = js.UpdateStream(drifted)
	require.NoError(t, err)

	_, err = stream.NewNATSStream()
	require.NoError(t, err)
	requireCurrentConfig(t, js)
}
