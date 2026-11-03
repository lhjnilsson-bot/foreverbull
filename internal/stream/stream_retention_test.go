package stream_test

import (
	"fmt"
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
	require.Equal(t, want.MaxBytes, info.Config.MaxBytes)
	require.Equal(t, int64(-1), info.Config.MaxMsgsPerSubject)
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

func TestStreamConfigKeepsMessagesPastTheLongestTask(t *testing.T) {
	longest, err := time.ParseDuration(environment.FinanceTaskTimeLimitDefault)
	require.NoError(t, err)

	cfg := stream.StreamConfig()
	require.Equal(t, stream.StreamName, cfg.Name)
	require.Equal(t, []string{"foreverbull.>"}, cfg.Subjects)
	require.Equal(t, 24*time.Hour, cfg.MaxAge)
	require.GreaterOrEqual(t, cfg.MaxAge, 4*longest,
		"a task's states must outlive the longest task with a clear margin")
	require.Equal(t, int64(512<<20), cfg.MaxBytes)
	require.Equal(t, nats.DiscardNew, cfg.Discard,
		"a full stream must refuse a publish, not drop a running task's states")
	require.Zero(t, cfg.MaxMsgsPerSubject,
		"WaitForCompletion reads a task's whole state history")
}

func TestNewNATSStreamGivesAnOlderStreamTheLimits(t *testing.T) {
	test_helper.SetupEnvironment(t, &test_helper.Containers{NATS: true})
	js := jetStream(t)

	_, err := stream.NewNATSStream()
	require.NoError(t, err)

	// The stream as earlier brokers created it, with no limits.
	_, err = js.UpdateStream(&nats.StreamConfig{
		Name:     stream.StreamName,
		Subjects: []string{"foreverbull.>"},
	})
	require.NoError(t, err)
	info, err := js.StreamInfo(stream.StreamName)
	require.NoError(t, err)
	require.Zero(t, info.Config.MaxAge)

	_, err = stream.NewNATSStream()
	require.NoError(t, err)
	requireCurrentConfig(t, js)
}

// retentionTestStream is StreamConfig on its own subjects, with the age and
// size shrunk so the server's enforcement shows within a test.
func retentionTestStream(t *testing.T, js nats.JetStreamContext, maxAge time.Duration, maxBytes int64) string {
	t.Helper()
	cfg := stream.StreamConfig()
	cfg.Name = fmt.Sprintf("foreverbull-retention-%d", time.Now().UnixNano())
	cfg.Subjects = []string{cfg.Name + ".>"}
	cfg.MaxAge = maxAge
	cfg.MaxBytes = maxBytes
	_, err := js.AddStream(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, js.DeleteStream(cfg.Name)) })
	return cfg.Name
}

func TestStreamRemovesMessagesOlderThanMaxAge(t *testing.T) {
	test_helper.SetupEnvironment(t, &test_helper.Containers{NATS: true})
	js := jetStream(t)
	name := retentionTestStream(t, js, time.Second, stream.MaxBytes)

	_, err := js.Publish(name+".a.b.c.id.state", []byte("old"))
	require.NoError(t, err)
	info, err := js.StreamInfo(name)
	require.NoError(t, err)
	require.Equal(t, uint64(1), info.State.Msgs)

	require.Eventually(t, func() bool {
		info, err := js.StreamInfo(name)
		return err == nil && info.State.Msgs == 0
	}, 10*time.Second, 100*time.Millisecond, "a message past MaxAge must be removed")
}

func TestStreamRefusesAPublishPastMaxBytes(t *testing.T) {
	test_helper.SetupEnvironment(t, &test_helper.Containers{NATS: true})
	js := jetStream(t)
	name := retentionTestStream(t, js, stream.MaxAge, 1024)

	_, err := js.Publish(name+".a.b.c.first.state", make([]byte, 600))
	require.NoError(t, err)
	_, err = js.Publish(name+".a.b.c.second.state", make([]byte, 600))
	require.Error(t, err, "a full stream must refuse the publish")

	info, err := js.StreamInfo(name)
	require.NoError(t, err)
	require.Equal(t, uint64(1), info.State.Msgs, "the stored message must be kept")
}
