package stream_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/suite"
	"go.uber.org/fx"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type StreamModuleTest struct {
	suite.Suite

	conn       *pgxpool.Pool
	repository *stream.Repository
	jt         nats.JetStreamContext

	app *fx.App
}

func (test *StreamModuleTest) SetupSuite() {
	test_helper.SetupEnvironment(test.T(), &test_helper.Containers{
		NATS:     true,
		Postgres: true,
	})
}

func (test *StreamModuleTest) SetupTest() {
	conn, err := pgxpool.New(context.Background(), environment.GetPostgresURL())
	test.Require().NoError(err)
	test.conn = conn
	test.repository = stream.NewRepository(conn)

	natsConn, err := nats.Connect(environment.GetNATSURL())
	test.Require().NoError(err)

	jt, err := natsConn.JetStream()
	test.Require().NoError(err)
	test.jt = jt

	test.startApp()
}

func (test *StreamModuleTest) startApp() {
	test.app = fx.New(
		fx.Provide(
			func() *pgxpool.Pool {
				return test.conn
			},
		),
		stream.Module,
	)
	test.Require().NoError(test.app.Start(context.Background()))
}

func (test *StreamModuleTest) TearDownTest() {
	test.Require().NoError(test.app.Stop(context.Background()))
}

func TestStreamModule(t *testing.T) {
	suite.Run(t, new(StreamModuleTest))
}

func (test *StreamModuleTest) TestModule() {
	message, err := stream.NewMessage("test", "test", "test", nil)
	test.Require().NoError(err)

	test.Require().NoError(test.repository.CreateMessage(context.Background(), message.GetMsg()))

	state := &pb.State{
		State:      pb.State_STATE_CREATED,
		OccurredAt: timestamppb.Now(),
	}
	payload, err := proto.Marshal(state)
	test.Require().NoError(err)

	_, err = test.jt.Publish(message.TaskStateTopic(), payload)
	test.Require().NoError(err)
	time.Sleep(time.Second)

	msg, states, err := test.repository.GetMessage(context.Background(), message.GetMsg().Id)
	test.Require().NoError(err)
	test.Require().NotNil(msg)
	test.Require().Len(states, 1)
	test.Require().Equal(pb.State_STATE_CREATED, states[0].State)
}

func (test *StreamModuleTest) TestModulePrunesExpiredStatesAtStart() {
	test.Require().NoError(test.app.Stop(context.Background()))

	expired := time.Now().Add(-stream.StateRetention - time.Minute)
	test.Require().NoError(test.repository.CreateMessageState(context.Background(), "expired", &pb.State{
		State:      pb.State_STATE_COMPLETED,
		OccurredAt: timestamppb.New(expired),
	}))
	test.Require().NoError(test.repository.CreateMessageState(context.Background(), "kept", &pb.State{
		State:      pb.State_STATE_COMPLETED,
		OccurredAt: timestamppb.Now(),
	}))

	test.startApp()
	count := func(messageID string) int {
		var n int
		test.Require().NoError(test.conn.QueryRow(context.Background(),
			`SELECT count(*) FROM message_states WHERE message_id=$1`, messageID).Scan(&n))
		return n
	}
	test.Eventually(func() bool { return count("expired") == 0 },
		5*time.Second, 50*time.Millisecond, "a row past the stream retention is pruned")
	test.Equal(1, count("kept"))
}
