package stream

import (
	"context"
	"errors"
	"fmt"

	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/nats-io/nats.go"
)

const (
	TOPIC_TASK       = "foreverbull.%s.%s.%s"
	TOPIC_TASK_ABORT = "foreverbull.%s.%s.%s.%s.abort"
	TOPIC_TASK_STATE = "foreverbull.%s.%s.%s.%s.state"
	// TOPIC_TASK_PROGRESS is core NATS, outside the foreverbull stream's
	// foreverbull.> subjects: progress is not stored on JetStream and gets
	// no message_states row. A waiter that isn't listening misses it.
	TOPIC_TASK_PROGRESS = "foreverbull-progress.%s.%s.%s.%s"
)

// StreamName is the shared JetStream stream every broker domain publishes
// tasks, task states, aborts, and events on.
const StreamName = "foreverbull"

// StreamConfig is the foreverbull stream's configuration.
func StreamConfig() *nats.StreamConfig {
	return &nats.StreamConfig{
		Name:     StreamName,
		Subjects: []string{"foreverbull.>"},
	}
}

// streamManager is the part of nats.JetStreamContext that ensureStream uses.
type streamManager interface {
	StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error)
	AddStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error)
	UpdateStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error)
}

// ensureStream creates the foreverbull stream, or updates an existing one to
// StreamConfig. AddStream alone refuses a stream that exists with another
// configuration, so a deployment created by an older broker would keep its
// old settings; UpdateStream gives it the current ones. A change the server
// can't apply in place fails startup with the server's reason.
func ensureStream(js streamManager, cfg *nats.StreamConfig) error {
	_, err := js.StreamInfo(cfg.Name)
	if errors.Is(err, nats.ErrStreamNotFound) {
		if _, err := js.AddStream(cfg); err != nil {
			return fmt.Errorf("error creating stream %s: %w", cfg.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("error reading stream %s: %w", cfg.Name, err)
	}
	if _, err := js.UpdateStream(cfg); err != nil {
		return fmt.Errorf("error updating stream %s: %w", cfg.Name, err)
	}
	return nil
}

type Stream interface {
	Publish(ctx context.Context, message Message, opts ...PublishOpt) error
	RunOrchestration(ctx context.Context, orchestration *pb.Orchestration) <-chan *pb.Orchestration_Step
	NewSubscriber(module string, dc DependencyContainer) (Subscriber, error)
}

type Subscriber interface {
	Subscribe(component, method string, cb func(context.Context, Message) error) error
	Unsubscribe() error
}

type NATSStream struct {
	jt nats.JetStreamContext
	// nc carries progress over core NATS.
	nc *nats.Conn
}

// NATSConnOptions sends the broker user when NATS_USER is set.
// An empty user stays anonymous so unit tests and a local server keep working.
func NATSConnOptions() []nats.Option {
	user := environment.GetNATSUser()
	if user == "" {
		return nil
	}
	return []nats.Option{nats.UserInfo(user, environment.GetNATSPassword())}
}

func NewNATSStream() (Stream, error) {
	natsConnect, err := nats.Connect(environment.GetNATSURL(), NATSConnOptions()...)
	if err != nil {
		return nil, fmt.Errorf("error connecting to nats: %w", err)
	}

	jetstream, err := natsConnect.JetStream()
	if err != nil {
		return nil, fmt.Errorf("error connecting to jetstream: %w", err)
	}

	if err := ensureStream(jetstream, StreamConfig()); err != nil {
		return nil, err
	}

	return &NATSStream{
		jt: jetstream,
		nc: natsConnect,
	}, nil
}

func (nn *NATSStream) NewSubscriber(module string, dc DependencyContainer) (Subscriber, error) {
	return &NatsSubscriber{
		module: module,
		jt:     nn.jt,
		nc:     nn.nc,
		deps:   dc.(*dependencyContainer),
	}, nil
}
