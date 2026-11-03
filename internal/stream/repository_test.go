package stream_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/stretchr/testify/suite"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type RepositoryTest struct {
	suite.Suite

	db         *pgxpool.Pool
	repository *stream.Repository
}

func (test *RepositoryTest) SetupTest() {
	test_helper.SetupEnvironment(test.T(), &test_helper.Containers{Postgres: true})

	var err error
	test.db, err = pgxpool.New(context.TODO(), environment.GetPostgresURL())
	test.Require().NoError(err)
	test.repository = stream.NewRepository(test.db)
}

func (test *RepositoryTest) TearDownTest() {
}

func (test *RepositoryTest) SetupSubTest() {
	test.Require().NoError(stream.RecreateTables(context.TODO(), test.db))
}

func TestRepository(t *testing.T) {
	suite.Run(t, new(RepositoryTest))
}

func (test *RepositoryTest) TestCreateMessage() {
	test.Run("Create", func() {
		msg := pb.Message{
			Id:        "id",
			Module:    "module",
			Component: "component",
			Method:    "method",
			Payload:   []byte("{'key': 'value'}"),
		}
		err := test.repository.CreateMessage(context.TODO(), &msg)
		test.Require().NoError(err)
	})
	test.Run("CreateDuplicate", func() {
		msg := pb.Message{
			Id:        "id",
			Module:    "module",
			Component: "component",
			Method:    "method",
			Payload:   []byte("payload"),
		}
		err := test.repository.CreateMessage(context.TODO(), &msg)
		test.Require().NoError(err)

		err = test.repository.CreateMessage(context.TODO(), &msg)
		test.Require().Error(err)
	})
}

func (test *RepositoryTest) TestCreateMessageState() {
	createMessage := func() {
		msg := pb.Message{
			Id:        "id",
			Module:    "module",
			Component: "component",
			Method:    "method",
			Payload:   []byte("payload"),
		}
		err := test.repository.CreateMessage(context.TODO(), &msg)
		test.Require().NoError(err)
	}
	test.Run("Create", func() {
		createMessage()

		err := test.repository.CreateMessageState(context.TODO(), "id", &pb.State{
			State: pb.State_STATE_CREATED,
		})
		test.Require().NoError(err)
	})
}

func (test *RepositoryTest) TestGetMessage() {
	createMessage := func() {
		msg := pb.Message{
			Id:        "id",
			Module:    "module",
			Component: "component",
			Method:    "method",
			Payload:   []byte("payload"),
		}
		err := test.repository.CreateMessage(context.TODO(), &msg)
		test.Require().NoError(err)
	}
	createMessageState := func(state *pb.State) {
		err := test.repository.CreateMessageState(context.TODO(), "id", state)
		test.Require().NoError(err)
	}
	test.Run("GetMessage", func() {
		createMessage()
		createMessageState(&pb.State{
			State:      pb.State_STATE_CREATED,
			OccurredAt: timestamppb.Now(),
		})

		msg, states, err := test.repository.GetMessage(context.TODO(), "id")
		test.Require().NoError(err)

		test.Require().NotNil(msg)
		test.Require().Len(states, 1)
		test.Require().Equal(pb.State_STATE_CREATED, states[0].State)
	})
	test.Run("GetMessageNotFound", func() {
		msg, states, err := test.repository.GetMessage(context.TODO(), "id")
		test.Require().Error(err)
		test.Require().Nil(msg)
		test.Require().Empty(states)
	})
	test.Run("MultipleStates", func() {
		createMessage()
		createMessageState(&pb.State{
			State:      pb.State_STATE_PUBLISHED,
			OccurredAt: timestamppb.Now(),
		})
		createMessageState(&pb.State{
			State:      pb.State_STATE_RECEIVED,
			OccurredAt: timestamppb.Now(),
		})
		createMessageState(&pb.State{
			State:      pb.State_STATE_ERROR,
			Error:      func() *string { s := "error"; return &s }(),
			OccurredAt: timestamppb.Now(),
		})

		msg, states, err := test.repository.GetMessage(context.TODO(), "id")
		test.Require().NoError(err)
		test.Require().NotNil(msg)
		test.Require().Len(states, 3)
		test.Require().Equal(pb.State_STATE_ERROR, states[0].State)
		test.Require().Equal(pb.State_STATE_RECEIVED, states[1].State)
		test.Require().Equal(pb.State_STATE_PUBLISHED, states[2].State)
		test.Require().Equal("error", *states[0].Error)
	})
}

func (test *RepositoryTest) TestDeleteStatesBefore() {
	now := time.Now()
	createState := func(messageID string, occurredAt time.Time) {
		err := test.repository.CreateMessageState(context.TODO(), messageID, &pb.State{
			State:      pb.State_STATE_COMPLETED,
			OccurredAt: timestamppb.New(occurredAt),
		})
		test.Require().NoError(err)
	}
	countStates := func(messageID string) int {
		var n int
		err := test.db.QueryRow(context.TODO(),
			`SELECT count(*) FROM message_states WHERE message_id=$1`, messageID).Scan(&n)
		test.Require().NoError(err)
		return n
	}
	test.Run("DeletesOnlyOlderRows", func() {
		createState("old", now.Add(-25*time.Hour))
		createState("old", now.Add(-48*time.Hour))
		createState("new", now.Add(-23*time.Hour))
		createState("new", now)

		deleted, err := test.repository.DeleteStatesBefore(context.TODO(), now.Add(-24*time.Hour), 100)
		test.Require().NoError(err)
		test.Equal(int64(2), deleted)
		test.Zero(countStates("old"))
		test.Equal(2, countStates("new"))
	})
	test.Run("DeletesAtMostTheLimitOldestFirst", func() {
		createState("oldest", now.Add(-72*time.Hour))
		createState("older", now.Add(-48*time.Hour))
		createState("old", now.Add(-25*time.Hour))

		deleted, err := test.repository.DeleteStatesBefore(context.TODO(), now.Add(-24*time.Hour), 2)
		test.Require().NoError(err)
		test.Equal(int64(2), deleted)
		test.Zero(countStates("oldest"))
		test.Zero(countStates("older"))
		test.Equal(1, countStates("old"))
	})
	test.Run("RefusesANonPositiveLimit", func() {
		_, err := test.repository.DeleteStatesBefore(context.TODO(), now, 0)
		test.Require().ErrorContains(err, "delete limit must be above zero")
	})
	test.Run("FailsWithoutTheTable", func() {
		_, err := test.db.Exec(context.TODO(), `DROP TABLE message_states`)
		test.Require().NoError(err)
		_, err = test.repository.DeleteStatesBefore(context.TODO(), now, 10)
		test.Require().ErrorContains(err, "failed to delete message states before")
	})
}

func (test *RepositoryTest) occurredAtIndexValid() (bool, bool) {
	var valid bool
	err := test.db.QueryRow(context.TODO(),
		`SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = 'message_states_occurred_at_idx'`).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false
	}
	test.Require().NoError(err)
	return true, valid
}

func (test *RepositoryTest) TestOccurredAtIndex() {
	test.Run("CreatedAndKept", func() {
		exists, valid := test.occurredAtIndexValid()
		test.Require().True(exists)
		test.Require().True(valid)
		test.Require().NoError(stream.CreateTables(context.TODO(), test.db))
		exists, valid = test.occurredAtIndexValid()
		test.Require().True(exists)
		test.Require().True(valid)
	})
	test.Run("InvalidRebuilt", func() {
		// What an interrupted CREATE INDEX CONCURRENTLY leaves behind.
		_, err := test.db.Exec(context.TODO(),
			`UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'message_states_occurred_at_idx'::regclass`)
		test.Require().NoError(err)
		exists, valid := test.occurredAtIndexValid()
		test.Require().True(exists)
		test.Require().False(valid)

		test.Require().NoError(stream.CreateTables(context.TODO(), test.db))
		exists, valid = test.occurredAtIndexValid()
		test.Require().True(exists)
		test.Require().True(valid, "an invalid index must be dropped and built again")
	})
}

func (test *RepositoryTest) TestCreateTablesAtOnce() {
	test.Run("TwoBrokersStartTogether", func() {
		// With the index gone, both starts build it.
		_, err := test.db.Exec(context.TODO(), `DROP INDEX message_states_occurred_at_idx`)
		test.Require().NoError(err)
		for i := range 200 {
			_, err := test.db.Exec(context.TODO(),
				`INSERT INTO message_states (message_id, state, occurred_at) SELECT 'm-' || g, 1, now() FROM generate_series(1, 500) g`)
			test.Require().NoError(err, "seed %d", i)
		}

		const starts = 2
		errs := make(chan error, starts)
		release := make(chan struct{})
		for range starts {
			go func() {
				<-release
				errs <- stream.CreateTables(context.TODO(), test.db)
			}()
		}
		close(release)
		for range starts {
			test.Require().NoError(<-errs)
		}
		exists, valid := test.occurredAtIndexValid()
		test.Require().True(exists)
		test.Require().True(valid)
	})
}

func (test *RepositoryTest) TestCreateTablesSchemaLock() {
	test.Run("WaitsForTheHolderAndReleases", func() {
		holder, err := test.db.Acquire(context.TODO())
		test.Require().NoError(err)
		defer holder.Release()
		var key int64
		test.Require().NoError(holder.QueryRow(context.TODO(), `SELECT x'666231313673'::bigint`).Scan(&key))
		_, err = holder.Exec(context.TODO(), `SELECT pg_advisory_lock($1)`, key)
		test.Require().NoError(err)

		ctx, cancel := context.WithTimeout(context.TODO(), 300*time.Millisecond)
		defer cancel()
		err = stream.CreateTables(ctx, test.db)
		test.Require().ErrorContains(err, "failed to take the schema lock: another broker holds it")

		_, err = holder.Exec(context.TODO(), `SELECT pg_advisory_unlock($1)`, key)
		test.Require().NoError(err)
		test.Require().NoError(stream.CreateTables(context.TODO(), test.db))

		var locked bool
		test.Require().NoError(holder.QueryRow(context.TODO(), `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked))
		test.Require().True(locked, "CreateTables must release the lock")
		_, err = holder.Exec(context.TODO(), `SELECT pg_advisory_unlock($1)`, key)
		test.Require().NoError(err)
	})
}
