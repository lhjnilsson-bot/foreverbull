package stream

import (
	"context"
	"fmt"

	"github.com/lhjnilsson/foreverbull/internal/stream"
	common "github.com/lhjnilsson/foreverbull/pkg/pb"
	service "github.com/lhjnilsson/foreverbull/pkg/pb/service"
)

// ExperimentRunCommand is published as strategy.experiment.run.
type ExperimentRunCommand struct {
	ExperimentID string `json:"experiment_id"`
}

func NewExperimentRunCommand(experimentID string) (stream.Message, error) {
	msg, err := stream.NewMessage("strategy", "experiment", "run", &ExperimentRunCommand{
		ExperimentID: experimentID,
	})
	if err != nil {
		return nil, fmt.Errorf("error creating experiment run command: %w", err)
	}
	return msg, nil
}

// RunInput is one experiment against an existing backtest universe.
type RunInput struct {
	ExperimentID string
	// StrategyName names the strategy in the user's step; it may be empty.
	StrategyName string
	BacktestName string
	BacktestID   string
	ServiceImage string
	Algorithm    *service.Algorithm
	StartDate    *common.Date
	EndDate      *common.Date
}

// RunOutput is the backtest session/execution created for the experiment.
type RunOutput struct {
	SessionID   string
	ExecutionID string
}

// ExperimentLauncher starts the existing backtest session/execution path.
// Tests inject a mock; the production impl talks to Backtest session gRPC.
type ExperimentLauncher interface {
	Run(ctx context.Context, in RunInput) (*RunOutput, error)
}
