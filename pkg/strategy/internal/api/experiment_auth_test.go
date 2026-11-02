package api

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	service "github.com/lhjnilsson/foreverbull/pkg/pb/service"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy/service"
	"github.com/lhjnilsson/foreverbull/pkg/strategy/internal/repository"
	ss "github.com/lhjnilsson/foreverbull/pkg/strategy/internal/stream"
)

func TestCreateExperimentRequiresTriggerOnTheBacktest(t *testing.T) {
	t.Setenv(environment.AuthEnabled, "true")
	test_helper.SetupEnvironment(t, &test_helper.Containers{Postgres: true})
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, environment.GetPostgresURL())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, repository.Recreate(ctx, pool))
	require.NoError(t, auth.Migrate(ctx, pool))

	owner := insertExperimentAccount(t, ctx, pool, auth.RoleAdmin, "owner")
	caller := insertExperimentAccount(t, ctx, pool, auth.RoleEditor, "caller")
	require.NoError(t, repository.MigrateStrategyOwner(ctx, pool, owner))

	var strategyID string
	err = pool.QueryRow(ctx, `
		INSERT INTO strategy (name, owner_id) VALUES ('mean-rev', $1) RETURNING id`, owner).Scan(&strategyID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `ALTER TABLE backtest ADD COLUMN IF NOT EXISTS id text`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `ALTER TABLE backtest ADD COLUMN IF NOT EXISTS owner_id text`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO backtest (name, benchmark, id, owner_id) VALUES ('spy', 'SPY', 'bt-spy', $1)`, owner)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO domain_grant (domain_kind, domain_id, grantee_id, level)
		VALUES ('strategy', $1, $2, 'editor'), ('backtest', 'bt-spy', $2, 'viewer')`, strategyID, caller)
	require.NoError(t, err)

	server := &StrategyServer{pgx: pool}
	call := experimentStream{ctx: auth.WithAccount(ctx, auth.Account{ID: caller, Role: auth.RoleEditor})}
	req := &servicePb.CreateExperimentRequest{StrategyName: "mean-rev", BacktestName: "spy"}
	err = server.createExperimentAuthed(req, call)
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	_, err = pool.Exec(ctx, `
		UPDATE domain_grant SET level = 'editor'
		WHERE domain_kind = 'backtest' AND domain_id = 'bt-spy' AND grantee_id = $1`, caller)
	require.NoError(t, err)
	err = server.createExperimentAuthed(req, call)
	require.NotEqual(t, codes.PermissionDenied, status.Code(err))
}

func insertExperimentAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, role, username string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx, `INSERT INTO account (role) VALUES ($1) RETURNING id`, role).Scan(&id)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO account_password (account_id, username, password_hash)
		VALUES ($1, $2, 'hash')`, id, username)
	require.NoError(t, err)
	return id
}

type experimentStream struct {
	ctx context.Context
}

func (s experimentStream) Context() context.Context { return s.ctx }
func (s experimentStream) Send(*servicePb.CreateExperimentResponse) error {
	return nil
}
func (s experimentStream) SetHeader(metadata.MD) error  { return nil }
func (s experimentStream) SendHeader(metadata.MD) error { return nil }
func (s experimentStream) SetTrailer(metadata.MD)       {}
func (s experimentStream) SendMsg(any) error            { return nil }
func (s experimentStream) RecvMsg(any) error            { return io.EOF }

var _ grpc.ServerStreamingServer[servicePb.CreateExperimentResponse] = experimentStream{}

// authedStrategy seeds, with auth on, a strategy and a READY version owned
// by an admin, and a backtest 'spy' the admin owns. It returns the server
// context's account and the version.
func authedStrategy(t *testing.T) (*pgxpool.Pool, auth.Account, *pb.Version) {
	t.Helper()
	t.Setenv(environment.AuthEnabled, "true")
	test_helper.SetupEnvironment(t, &test_helper.Containers{Postgres: true})
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, environment.GetPostgresURL())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, repository.Recreate(ctx, pool))
	require.NoError(t, auth.Migrate(ctx, pool))

	owner := insertExperimentAccount(t, ctx, pool, auth.RoleAdmin, "owner")
	require.NoError(t, repository.MigrateStrategyOwner(ctx, pool, owner))
	var serviceID string
	err = pool.QueryRow(ctx, `INSERT INTO service (image, status, owner_id) VALUES ('algo:local', $1, $2) RETURNING id`,
		int(service.Service_Status_READY), owner).Scan(&serviceID)
	require.NoError(t, err)
	var versionID string
	err = pool.QueryRow(ctx, `
		WITH s AS (INSERT INTO strategy (name, owner_id) VALUES ('mean-rev', $1) RETURNING id)
		INSERT INTO strategy_version (strategy_id, rev, service_image, service_id)
		SELECT id, 1, 'algo:local', $2 FROM s RETURNING id`, owner, serviceID).Scan(&versionID)
	require.NoError(t, err)
	version, err := (&repository.Version{Conn: pool}).Get(ctx, versionID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `ALTER TABLE backtest ADD COLUMN IF NOT EXISTS id text`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `ALTER TABLE backtest ADD COLUMN IF NOT EXISTS owner_id text`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO backtest (name, benchmark, id, owner_id) VALUES ('spy', 'SPY', 'bt-spy', $1)`, owner)
	require.NoError(t, err)
	// The backtest module's ownership migration keys experiments on backtest_id.
	_, err = pool.Exec(ctx, `ALTER TABLE strategy_experiment ADD COLUMN IF NOT EXISTS backtest_id text, ALTER COLUMN backtest_name DROP NOT NULL`)
	require.NoError(t, err)
	return pool, auth.Account{ID: owner, Role: auth.RoleAdmin}, version
}

// failingSendStream fails its Send number failAt (1-based).
type failingSendStream struct {
	experimentStream
	sends  *int
	failAt int
}

func (s failingSendStream) Send(*servicePb.CreateExperimentResponse) error {
	*s.sends++
	if *s.sends == s.failAt {
		return errors.New("client went away")
	}
	return nil
}

// With auth on, a RUNNING step that can't be sent stops the experiment
// before the run, and the row is FAILED instead of left RUNNING.
func TestCreateExperimentAuthedRunningNotSent(t *testing.T) {
	pool, account, version := authedStrategy(t)
	ctx := auth.WithAccount(context.Background(), account)
	launcher := new(ss.MockExperimentLauncher)
	server := &StrategyServer{pgx: pool, launcher: launcher}
	sends := 0
	call := failingSendStream{experimentStream: experimentStream{ctx: ctx}, sends: &sends, failAt: 3}

	err := server.createExperimentAuthed(&servicePb.CreateExperimentRequest{
		StrategyName: "mean-rev", VersionId: version.GetId(), BacktestName: "spy",
	}, call)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Internal, st.Code())
	require.Equal(t, "Couldn't run experiment 'mean-rev': fbull couldn't be told the run had started. "+
		"Run `fbenv status` to check that the environment is running, "+
		"then run `fbull strategy experiment mean-rev --backtest spy` again.", st.Message())
	launcher.AssertNotCalled(t, "Run", mock.Anything, mock.Anything)

	var state int
	var stored string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT status, error FROM strategy_experiment WHERE version_id = $1`, version.GetId()).Scan(&state, &stored))
	require.Equal(t, int(pb.Experiment_FAILED), state)
	require.Equal(t, "send running: client went away", stored)
}

// failingRunner can't start a deployment loop.
type failingRunner struct{}

func (failingRunner) Add(context.Context, *pb.Deployment) error { return errors.New("loop table full") }
func (failingRunner) Remove(context.Context, string) error      { return nil }

// With auth on, a runner that can't start and an ERROR that can't be saved
// either: the row still reads RUNNING, so the sentence says so.
func TestDeployAuthedErrorNotStored(t *testing.T) {
	pool, account, version := authedStrategy(t)
	ctx := auth.WithAccount(context.Background(), account)
	experiments := repository.Experiment{Conn: pool}
	created, err := experiments.CreateForBacktest(ctx, version.GetId(), "bt-spy")
	require.NoError(t, err)
	require.NoError(t, experiments.UpdateStatus(ctx, created.GetId(), pb.Experiment_COMPLETED, nil))
	_, err = pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION refuse_deployment_error() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'disk full'; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER refuse_deployment_error BEFORE UPDATE ON strategy_deployment
		FOR EACH ROW WHEN (NEW.error IS NOT NULL) EXECUTE FUNCTION refuse_deployment_error();`)
	require.NoError(t, err)
	server := &StrategyServer{pgx: pool, runner: failingRunner{}}

	_, err = server.deployAuthed(ctx, &servicePb.DeployRequest{StrategyName: "mean-rev", VersionId: version.GetId()})
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Internal, st.Code())
	require.Equal(t, "Couldn't start deployment 'mean-rev', and the failure couldn't be saved, so the deployment still shows as running. "+
		"Run `fbenv status` to check that the environment is running, "+
		"then run `fbull strategy stop mean-rev`, then `fbull strategy deploy mean-rev`.", st.Message())

	var state int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM strategy_deployment WHERE version_id = $1`, version.GetId()).Scan(&state))
	require.Equal(t, int(pb.Deployment_RUNNING), state)
}
