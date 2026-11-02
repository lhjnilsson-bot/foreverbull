package command

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy"
	"github.com/lhjnilsson/foreverbull/pkg/strategy/internal/repository"
	ss "github.com/lhjnilsson/foreverbull/pkg/strategy/internal/stream"
	"github.com/lhjnilsson/foreverbull/pkg/strategy/internal/stream/dependency"
)

func ExperimentRun(ctx context.Context, msg stream.Message) error {
	cmd := ss.ExperimentRunCommand{}
	if err := msg.ParsePayload(&cmd); err != nil {
		return fmt.Errorf("error unmarshalling ExperimentRun payload: %w", err)
	}

	db, ok := msg.MustGet(stream.DBDep).(*pgxpool.Pool)
	if !ok {
		return fmt.Errorf("db dependency casting failed")
	}
	launcher, ok := msg.MustGet(dependency.ExperimentLauncherDep).(ss.ExperimentLauncher)
	if !ok {
		return fmt.Errorf("experiment launcher dependency casting failed")
	}

	experiments := repository.Experiment{Conn: db}
	experiment, err := experiments.Get(ctx, cmd.ExperimentID)
	if err != nil {
		return fmt.Errorf("get experiment: %w", err)
	}

	if err := experiments.UpdateStatus(ctx, experiment.Id, pb.Experiment_RUNNING, nil); err != nil {
		return fmt.Errorf("update experiment status: %w", err)
	}

	versions := repository.Version{Conn: db}
	version, err := versions.Get(ctx, experiment.VersionId)
	if err != nil {
		return experiments.Fail(ctx, experiment.Id, fmt.Errorf("get experiment version: %w", err))
	}

	runInput := ss.RunInput{
		ExperimentID: experiment.Id,
		BacktestName: experiment.BacktestName,
		ServiceImage: version.ServiceImage,
	}
	if environment.GetAuthEnabled() {
		backtestID, idErr := experiments.BacktestID(ctx, experiment.Id)
		if idErr != nil {
			return experiments.Fail(ctx, experiment.Id, fmt.Errorf("experiment backtest id: %w", idErr))
		}
		if backtestID == "" {
			return experiments.Fail(ctx, experiment.Id, fmt.Errorf("backtest id is empty"))
		}
		runInput.BacktestID = backtestID
	}
	out, runErr := launcher.Run(ctx, runInput)
	if out != nil && out.ExecutionID != "" {
		if err := experiments.SetExecutionID(ctx, experiment.Id, out.ExecutionID); err != nil {
			return experiments.Fail(ctx, experiment.Id, fmt.Errorf("set experiment execution id: %w", err))
		}
	}
	if runErr != nil {
		return experiments.Fail(ctx, experiment.Id, runErr)
	}

	return experiments.Complete(ctx, experiment.Id)
}
