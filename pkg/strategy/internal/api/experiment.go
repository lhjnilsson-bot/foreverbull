package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	common "github.com/lhjnilsson/foreverbull/pkg/pb"
	service "github.com/lhjnilsson/foreverbull/pkg/pb/service"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy/service"
	"github.com/lhjnilsson/foreverbull/pkg/strategy/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/strategy/internal/stream"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// grafanaDashboardUID is the provisioned Backtest execution dashboard.
// Deep links must keep this UID and the var-execution query key.
const grafanaDashboardUID = "ee9ep4jxtwkcgf"

// GrafanaExecutionURL builds a Grafana inspect-one-run deep link.
// grafanaURL is the Grafana origin (GRAFANA_URL). Setup already refused an
// empty or invalid origin, so an error here is returned, not replaced.
func GrafanaExecutionURL(grafanaURL, executionID string) (string, error) {
	u, err := environment.ParseGrafanaURL(grafanaURL)
	if err != nil {
		return "", err
	}
	built := u.JoinPath("d", grafanaDashboardUID)
	q := built.Query()
	q.Set("var-execution", executionID)
	built.RawQuery = q.Encode()
	return built.String(), nil
}

// experimentRun names the experiment a failed run belongs to, for the
// user's sentence.
type experimentRun struct {
	id       string
	strategy string
	backtest string
}

// failExperiment marks the experiment FAILED with cause and returns the
// plain causes for the user: first, then "the failure couldn't be saved"
// when the mark failed. The whole chain goes to the log.
func failExperiment(ctx context.Context, experiments *repository.Experiment, run experimentRun, first string, cause error) []string {
	causes := []string{first}
	if err := experiments.Fail(ctx, run.id, cause); repository.StatusNotStored(err) {
		causes = append(causes, usercopy.CauseFailureNotStored)
		cause = err
	}
	log.Error().Err(cause).Str("experiment", run.id).Msg("experiment run failed")
	return causes
}

// experimentRunError is the status a client sees for a failed run: one
// sentence naming the experiment, the plain causes, and the step.
func experimentRunError(run experimentRun, causes []string) error {
	return status.Error(codes.Internal, usercopy.ExperimentRunFailed(run.strategy, run.backtest, causes))
}

// failExperimentRun marks the experiment FAILED and sends the failed step.
// The status names the run's failure first, then any write, reload, or send
// that failed on the way, as plain causes; the errors go to the log.
func failExperimentRun(
	ctx context.Context,
	serverStream servicePb.StrategyServicer_CreateExperimentServer,
	rsp *servicePb.CreateExperimentResponse,
	experiments *repository.Experiment,
	run experimentRun,
	runErr error,
) error {
	causes := failExperiment(ctx, experiments, run, usercopy.CauseRunFailed, runErr)
	rsp.RUNNING.State = common.State_STATE_FAILED
	rsp.RUNNING.Message = runErr.Error()
	reloaded, err := experiments.Get(ctx, run.id)
	if err != nil {
		log.Error().Err(err).Str("experiment", run.id).Msg("error reloading failed experiment")
		causes = append(causes, usercopy.CauseNotReloaded)
	} else {
		rsp.Experiment = reloaded
	}
	if err := serverStream.Send(rsp); err != nil {
		log.Error().Err(err).Str("experiment", run.id).Msg("error sending failed experiment step")
		causes = append(causes, usercopy.CauseNotSent)
	}
	return experimentRunError(run, causes)
}

func (s *StrategyServer) CreateExperiment(
	req *servicePb.CreateExperimentRequest,
	serverStream servicePb.StrategyServicer_CreateExperimentServer,
) error {
	if s.pgx == nil {
		return status.Error(codes.Internal, "strategy storage is not configured")
	}
	if s.launcher == nil {
		return status.Error(codes.Internal, "experiment launcher is not configured")
	}
	if environment.GetAuthEnabled() {
		return s.createExperimentAuthed(req, serverStream)
	}

	ctx := serverStream.Context()
	rsp := &servicePb.CreateExperimentResponse{
		QUEUED:    &common.Step{State: common.State_STATE_UNSPECIFIED},
		RUNNING:   &common.Step{State: common.State_STATE_UNSPECIFIED},
		COMPLETED: &common.Step{State: common.State_STATE_UNSPECIFIED},
	}

	version, err := s.resolveVersion(ctx, req.GetStrategyName(), req.GetVersionId(), req.GetVersionRev())
	if err != nil {
		return err
	}

	if err := s.requireServiceReady(ctx, version.ServiceImage); err != nil {
		return err
	}
	if err := s.requireBacktest(ctx, req.GetBacktestName()); err != nil {
		return err
	}

	rsp.QUEUED.State = common.State_STATE_RUNNING
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("send queued: %w", err)
	}

	experiments := repository.Experiment{Conn: s.pgx}
	experiment, err := experiments.Create(ctx, version.Id, req.GetBacktestName())
	if err != nil {
		return status.Errorf(codes.Internal, "create experiment: %v", err)
	}

	rsp.QUEUED.State = common.State_STATE_COMPLETED
	rsp.Experiment = experiment
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("send queued complete: %w", err)
	}

	if err := experiments.UpdateStatus(ctx, experiment.Id, pb.Experiment_RUNNING, nil); err != nil {
		return status.Errorf(codes.Internal, "update experiment status: %v", err)
	}
	experiment.Status = pb.Experiment_RUNNING
	rsp.RUNNING.State = common.State_STATE_RUNNING
	rsp.Experiment = experiment

	run := experimentRun{id: experiment.Id, strategy: req.GetStrategyName(), backtest: req.GetBacktestName()}
	if err := serverStream.Send(rsp); err != nil {
		// The run isn't started when fbull can't be told it is running.
		return experimentRunError(run, failExperiment(ctx, &experiments, run,
			usercopy.CauseStartNotSent, fmt.Errorf("send running: %w", err)))
	}
	out, runErr := s.launcher.Run(ctx, stream.RunInput{
		ExperimentID: experiment.Id,
		StrategyName: req.GetStrategyName(),
		BacktestName: req.GetBacktestName(),
		ServiceImage: version.ServiceImage,
	})
	if out != nil && out.ExecutionID != "" {
		if err := experiments.SetExecutionID(ctx, experiment.Id, out.ExecutionID); err != nil {
			return experimentRunError(run, failExperiment(ctx, &experiments, run,
				usercopy.CauseExecutionNotStored, fmt.Errorf("set experiment execution id: %w", err)))
		}
	}
	if runErr != nil {
		return failExperimentRun(ctx, serverStream, rsp, &experiments, run, runErr)
	}

	if err := experiments.Complete(ctx, experiment.Id); err != nil {
		return status.Errorf(codes.Internal, "complete experiment: %v", err)
	}
	experiment, err = experiments.Get(ctx, experiment.Id)
	if err != nil {
		return status.Errorf(codes.Internal, "reload experiment: %v", err)
	}

	rsp.RUNNING.State = common.State_STATE_COMPLETED
	rsp.COMPLETED.State = common.State_STATE_COMPLETED
	rsp.Experiment = experiment
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("send completed: %w", err)
	}
	return nil
}

func (s *StrategyServer) GetExperiment(ctx context.Context, req *servicePb.GetExperimentRequest) (*servicePb.GetExperimentResponse, error) {
	if s.pgx == nil {
		return nil, status.Error(codes.Internal, "strategy storage is not configured")
	}
	if environment.GetAuthEnabled() {
		return s.getExperimentAuthed(ctx, req)
	}

	experiments := repository.Experiment{Conn: s.pgx}
	experiment, err := experiments.Get(ctx, req.GetExperimentId())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "experiment %q not found", req.GetExperimentId())
		}
		return nil, status.Errorf(codes.Internal, "get experiment: %v", err)
	}

	rsp := &servicePb.GetExperimentResponse{Experiment: experiment}
	if experiment.GetExecutionId() != "" {
		path, err := GrafanaExecutionURL(environment.GetGrafanaURL(), experiment.GetExecutionId())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "grafana link: %v", err)
		}
		rsp.GrafanaPath = &path
		lastPeriod, err := experiments.GetLastPeriod(ctx, experiment.GetExecutionId())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "get last period: %v", err)
		}
		rsp.LastPeriod = lastPeriod
	}
	return rsp, nil
}

func (s *StrategyServer) ListExperiments(ctx context.Context, req *servicePb.ListExperimentsRequest) (*servicePb.ListExperimentsResponse, error) {
	if s.pgx == nil {
		return nil, status.Error(codes.Internal, "strategy storage is not configured")
	}
	if environment.GetAuthEnabled() {
		return s.listExperimentsAuthed(ctx, req)
	}

	experiments := repository.Experiment{Conn: s.pgx}
	list, err := experiments.List(ctx, req.GetStrategyName(), req.GetVersionId(), req.GetBacktestName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list experiments: %v", err)
	}
	return &servicePb.ListExperimentsResponse{Experiments: list}, nil
}

func (s *StrategyServer) resolveVersion(ctx context.Context, strategyName, versionID string, versionRev int32) (*pb.Version, error) {
	strategies := repository.Strategy{Conn: s.pgx}
	if _, err := strategies.Get(ctx, strategyName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "strategy %q not found", strategyName)
		}
		return nil, status.Errorf(codes.Internal, "get strategy: %v", err)
	}

	versions := repository.Version{Conn: s.pgx}
	switch {
	case versionID != "":
		version, err := versions.Get(ctx, versionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, status.Errorf(codes.NotFound, "version %q not found", versionID)
			}
			return nil, status.Errorf(codes.Internal, "get version: %v", err)
		}
		if version.StrategyName != strategyName {
			return nil, status.Errorf(codes.InvalidArgument, "version %q does not belong to strategy %q",
				versionID, strategyName)
		}
		return version, nil
	case versionRev != 0:
		version, err := versions.GetByStrategyAndRev(ctx, strategyName, versionRev)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, status.Errorf(codes.NotFound, "strategy %q rev %d not found",
					strategyName, versionRev)
			}
			return nil, status.Errorf(codes.Internal, "get version: %v", err)
		}
		return version, nil
	default:
		return nil, status.Error(codes.InvalidArgument, "version_id or version_rev is required")
	}
}

func (s *StrategyServer) requireServiceReady(ctx context.Context, image string) error {
	var serviceStatus int
	err := s.pgx.QueryRow(ctx, `SELECT status FROM service WHERE image=$1`, image).Scan(&serviceStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Errorf(codes.FailedPrecondition, "service image %q not found", image)
	}
	if err != nil {
		return status.Errorf(codes.Internal, "get service: %v", err)
	}
	st := service.Service_Status_Status(serviceStatus)
	if st != service.Service_Status_READY {
		return status.Errorf(codes.FailedPrecondition,
			"service image %q is not READY (status=%s); interview the service first", image, st)
	}
	return nil
}

func (s *StrategyServer) requireBacktest(ctx context.Context, name string) error {
	var found string
	err := s.pgx.QueryRow(ctx, `SELECT name FROM backtest WHERE name=$1`, name).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Errorf(codes.NotFound,
			"backtest %q not found; create and ingest it with `fbull backtest create` — experiments do not auto-ingest",
			name)
	}
	if err != nil {
		return status.Errorf(codes.Internal, "get backtest: %v", err)
	}
	return nil
}
