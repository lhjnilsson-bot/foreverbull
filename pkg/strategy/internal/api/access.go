package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
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

func accountFrom(ctx context.Context) (auth.Account, error) {
	account, ok := auth.FromContext(ctx)
	if !ok || account.ID == "" {
		return auth.Account{}, auth.GRPCStatus(auth.ErrUnauthorized)
	}
	return account, nil
}

func (s *StrategyServer) listStrategiesAuthed(ctx context.Context) (*servicePb.ListStrategiesResponse, error) {
	account, err := accountFrom(ctx)
	if err != nil {
		return nil, err
	}
	strategies := repository.Strategy{Conn: s.pgx}
	list, err := strategies.ListVisible(ctx, account)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list strategies: %v", err)
	}
	return &servicePb.ListStrategiesResponse{Strategies: list}, nil
}

func (s *StrategyServer) getStrategyAuthed(ctx context.Context, req *servicePb.GetStrategyRequest) (*servicePb.GetStrategyResponse, error) {
	account, err := accountFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := auth.Resolve(ctx, s.pgx, account, auth.DomainStrategy, req.GetName(), req.GetOwner(), auth.ActionRead)
	if err != nil {
		return nil, auth.GRPCStatus(err)
	}
	strategies := repository.Strategy{Conn: s.pgx}
	found, err := strategies.GetOwned(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "not found")
		}
		return nil, status.Errorf(codes.Internal, "get strategy: %v", err)
	}
	return &servicePb.GetStrategyResponse{Strategy: found}, nil
}

func (s *StrategyServer) deleteStrategyAuthed(ctx context.Context, req *servicePb.DeleteStrategyRequest) (*servicePb.DeleteStrategyResponse, error) {
	if s.pgx == nil {
		return nil, status.Error(codes.Internal, "strategy storage is not configured")
	}
	account, err := accountFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := auth.Resolve(ctx, s.pgx, account, auth.DomainStrategy, req.GetName(), req.GetOwner(), auth.ActionDelete)
	if err != nil {
		return nil, auth.GRPCStatus(err)
	}
	strategies := repository.Strategy{Conn: s.pgx}
	if err := strategies.DeleteByID(ctx, id); err != nil {
		if errors.Is(err, auth.ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "not found")
		}
		return nil, status.Errorf(codes.Internal, "delete strategy: %v", err)
	}
	return &servicePb.DeleteStrategyResponse{}, nil
}

func (s *StrategyServer) createExperimentAuthed(
	req *servicePb.CreateExperimentRequest,
	serverStream servicePb.StrategyServicer_CreateExperimentServer,
) error {
	ctx := serverStream.Context()
	account, err := accountFrom(ctx)
	if err != nil {
		return err
	}
	strategyID, err := auth.Resolve(ctx, s.pgx, account, auth.DomainStrategy, req.GetStrategyName(), req.GetStrategyOwner(), auth.ActionTrigger)
	if err != nil {
		return auth.GRPCStatus(err)
	}
	backtestID, err := auth.Resolve(ctx, s.pgx, account, auth.DomainBacktest, req.GetBacktestName(), req.GetBacktestOwner(), auth.ActionTrigger)
	if err != nil {
		return auth.GRPCStatus(err)
	}
	version, err := s.resolveVersionOwned(ctx, strategyID, req.GetVersionId(), req.GetVersionRev())
	if err != nil {
		return err
	}
	if err := s.serviceReadyByVersion(ctx, version); err != nil {
		return err
	}

	rsp := &servicePb.CreateExperimentResponse{
		QUEUED:    &common.Step{State: common.State_STATE_UNSPECIFIED},
		RUNNING:   &common.Step{State: common.State_STATE_UNSPECIFIED},
		COMPLETED: &common.Step{State: common.State_STATE_UNSPECIFIED},
	}
	rsp.QUEUED.State = common.State_STATE_RUNNING
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("send queued: %w", err)
	}
	experiments := repository.Experiment{Conn: s.pgx}
	experiment, err := experiments.CreateForBacktest(ctx, version.GetId(), backtestID)
	if err != nil {
		return status.Errorf(codes.Internal, "create experiment: %v", err)
	}
	rsp.QUEUED.State = common.State_STATE_COMPLETED
	rsp.Experiment = experiment
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("send queued complete: %w", err)
	}
	if err := experiments.UpdateStatus(ctx, experiment.GetId(), pb.Experiment_RUNNING, nil); err != nil {
		return status.Errorf(codes.Internal, "update experiment status: %v", err)
	}
	experiment.Status = pb.Experiment_RUNNING
	rsp.RUNNING.State = common.State_STATE_RUNNING
	rsp.Experiment = experiment
	run := experimentRun{id: experiment.GetId(), strategy: req.GetStrategyName(), backtest: req.GetBacktestName()}
	if err := serverStream.Send(rsp); err != nil {
		// The run isn't started when fbull can't be told it is running.
		return experimentRunError(run, failExperiment(ctx, &experiments, run,
			usercopy.CauseStartNotSent, fmt.Errorf("send running: %w", err)))
	}
	out, runErr := s.launcher.Run(ctx, stream.RunInput{
		ExperimentID: experiment.GetId(),
		BacktestName: req.GetBacktestName(),
		BacktestID:   backtestID,
		ServiceImage: version.GetServiceImage(),
	})
	if out != nil && out.ExecutionID != "" {
		if err := experiments.SetExecutionID(ctx, experiment.GetId(), out.ExecutionID); err != nil {
			return experimentRunError(run, failExperiment(ctx, &experiments, run,
				usercopy.CauseExecutionNotStored, fmt.Errorf("set experiment execution id: %w", err)))
		}
	}
	if runErr != nil {
		return failExperimentRun(ctx, serverStream, rsp, &experiments, run, runErr)
	}
	if err := experiments.Complete(ctx, experiment.GetId()); err != nil {
		return status.Errorf(codes.Internal, "complete experiment: %v", err)
	}
	experiment, err = experiments.Get(ctx, experiment.GetId())
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

func (s *StrategyServer) getExperimentAuthed(ctx context.Context, req *servicePb.GetExperimentRequest) (*servicePb.GetExperimentResponse, error) {
	account, err := accountFrom(ctx)
	if err != nil {
		return nil, err
	}
	experiments := repository.Experiment{Conn: s.pgx}
	experiment, err := experiments.Get(ctx, req.GetExperimentId())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "not found")
		}
		return nil, status.Errorf(codes.Internal, "get experiment: %v", err)
	}
	if err := auth.Can(ctx, s.pgx, account, auth.DomainStrategy, experiment.GetStrategyId(), auth.ActionRead); err != nil {
		return nil, auth.GRPCStatus(err)
	}
	rsp := &servicePb.GetExperimentResponse{Experiment: experiment}
	if experiment.GetExecutionId() == "" {
		return rsp, nil
	}
	path, err := GrafanaExecutionURL(environment.GetGrafanaURL(), experiment.GetExecutionId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "grafana link: %v", err)
	}
	rsp.GrafanaPath = &path
	visible, err := executionBacktestVisible(ctx, s.pgx, account, experiment.GetExecutionId())
	if err != nil {
		log.Error().Err(err).Str("execution", experiment.GetExecutionId()).Msg("error checking access to the execution's backtest")
		return nil, status.Error(codes.Internal, usercopy.ExecutionBacktestCheckFailed(experiment.GetExecutionId(), experiment.GetBacktestName()))
	}
	if !visible {
		return rsp, nil
	}
	lastPeriod, err := experiments.GetLastPeriod(ctx, experiment.GetExecutionId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get last period: %v", err)
	}
	rsp.LastPeriod = lastPeriod
	return rsp, nil
}

// executionBacktestVisible reports whether the caller may read the backtest
// behind an execution. An execution with no session row, or a backtest the
// caller can't see, is an answer: not visible. A failed read is an error, not
// "not visible", so the last period isn't hidden by a database fault.
func executionBacktestVisible(ctx context.Context, db postgres.Query, account auth.Account, executionID string) (bool, error) {
	var backtestID string
	err := db.QueryRow(ctx, `
		SELECT session.backtest_id
		FROM backtest_execution
		JOIN session ON session.id = backtest_execution.session
		WHERE backtest_execution.id = $1`, executionID).Scan(&backtestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read backtest of execution %s: %w", executionID, err)
	}
	if backtestID == "" {
		return false, nil
	}
	err = auth.Can(ctx, db, account, auth.DomainBacktest, backtestID, auth.ActionRead)
	if errors.Is(err, auth.ErrNotFound) || errors.Is(err, auth.ErrForbidden) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check access to backtest %s: %w", backtestID, err)
	}
	return true, nil
}

func (s *StrategyServer) listExperimentsAuthed(ctx context.Context, req *servicePb.ListExperimentsRequest) (*servicePb.ListExperimentsResponse, error) {
	account, err := accountFrom(ctx)
	if err != nil {
		return nil, err
	}
	backtestID := ""
	if req.GetBacktestName() != "" {
		backtestID, err = auth.Resolve(ctx, s.pgx, account, auth.DomainBacktest, req.GetBacktestName(), req.GetOwner(), auth.ActionRead)
		if err != nil {
			return nil, auth.GRPCStatus(err)
		}
	}
	experiments := repository.Experiment{Conn: s.pgx}
	if req.GetStrategyName() != "" {
		strategyID, err := auth.Resolve(ctx, s.pgx, account, auth.DomainStrategy, req.GetStrategyName(), req.GetOwner(), auth.ActionRead)
		if err != nil {
			return nil, auth.GRPCStatus(err)
		}
		list, err := experiments.ListByStrategyID(ctx, strategyID, req.GetVersionId(), backtestID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list experiments: %v", err)
		}
		return &servicePb.ListExperimentsResponse{Experiments: list}, nil
	}
	strategies := repository.Strategy{Conn: s.pgx}
	visible, err := strategies.ListVisible(ctx, account)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list experiments: %v", err)
	}
	all := make([]*pb.Experiment, 0)
	for _, item := range visible {
		part, err := experiments.ListByStrategyID(ctx, item.GetId(), req.GetVersionId(), backtestID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list experiments: %v", err)
		}
		all = append(all, part...)
	}
	return &servicePb.ListExperimentsResponse{Experiments: all}, nil
}

func (s *StrategyServer) resolveVersionOwned(ctx context.Context, strategyID, versionID string, versionRev int32) (*pb.Version, error) {
	versions := repository.Version{Conn: s.pgx}
	switch {
	case versionID != "":
		version, err := versions.Get(ctx, versionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, status.Error(codes.NotFound, "not found")
			}
			return nil, status.Errorf(codes.Internal, "get version: %v", err)
		}
		if version.GetStrategyId() != strategyID {
			return nil, status.Errorf(codes.InvalidArgument, "version %q does not belong to strategy %q", versionID, strategyID)
		}
		return version, nil
	case versionRev != 0:
		version, err := versions.GetByStrategyIDAndRev(ctx, strategyID, versionRev)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, status.Error(codes.NotFound, "not found")
			}
			return nil, status.Errorf(codes.Internal, "get version: %v", err)
		}
		return version, nil
	default:
		return nil, status.Error(codes.InvalidArgument, "version_id or version_rev is required")
	}
}

func (s *StrategyServer) serviceReadyByVersion(ctx context.Context, version *pb.Version) error {
	var serviceStatus int
	err := s.pgx.QueryRow(ctx, `
		SELECT service.status
		FROM strategy_version
		JOIN service ON service.id = strategy_version.service_id
		WHERE strategy_version.id = $1`, version.GetId()).Scan(&serviceStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Errorf(codes.FailedPrecondition, "service image %q not found", version.GetServiceImage())
	}
	if err != nil {
		return status.Errorf(codes.Internal, "get service: %v", err)
	}
	st := service.Service_Status_Status(serviceStatus)
	if st != service.Service_Status_READY {
		return status.Errorf(codes.FailedPrecondition,
			"service image %q is not READY (status=%s); interview the service first", version.GetServiceImage(), st)
	}
	return nil
}

func (s *StrategyServer) deployAuthed(ctx context.Context, req *servicePb.DeployRequest) (*servicePb.DeployResponse, error) {
	if req.GetMode() == pb.Deployment_LIVE {
		return nil, status.Error(codes.FailedPrecondition, "live mode is not enabled; deploy with paper")
	}
	account, err := accountFrom(ctx)
	if err != nil {
		return nil, err
	}
	strategyID, err := auth.Resolve(ctx, s.pgx, account, auth.DomainStrategy, req.GetStrategyName(), req.GetOwner(), auth.ActionTrigger)
	if err != nil {
		return nil, auth.GRPCStatus(err)
	}
	version, err := s.resolveVersionOwned(ctx, strategyID, req.GetVersionId(), req.GetVersionRev())
	if err != nil {
		return nil, err
	}
	if err := s.serviceReadyByVersion(ctx, version); err != nil {
		return nil, err
	}
	if err := s.requireCompletedExperimentOwned(ctx, strategyID, version.GetId()); err != nil {
		return nil, err
	}
	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}
	backend := strings.TrimSpace(req.GetBackend())
	if err := s.paperReady(backend, dryRun, req.GetConfirm()); err != nil {
		return nil, err
	}
	calendar := req.GetCalendarCode()
	if calendar == "" {
		calendar = defaultCalendar
	}
	deployments := repository.Deployment{Conn: s.pgx}
	deployment, err := deployments.Create(
		ctx, version.GetId(), backend, calendar,
		pb.Deployment_PAPER, req.GetSymbols(), pb.Deployment_SESSION_CLOSE, dryRun,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, status.Errorf(codes.FailedPrecondition,
				"strategy %q already has a running deployment; stop it first", req.GetStrategyName())
		}
		return nil, status.Errorf(codes.Internal, "create deployment: %v", err)
	}
	if s.runner != nil {
		if err := s.runner.Add(ctx, deployment); err != nil {
			failErr := deployments.Fail(ctx, deployment.GetId(), err)
			notStored := repository.StatusNotStored(failErr)
			log.Error().Err(failErr).Str("deployment", deployment.GetId()).Bool("error_not_stored", notStored).
				Msg("error starting deployment runner")
			return nil, status.Error(codes.Internal, usercopy.DeploymentStartFailed(req.GetStrategyName(), notStored))
		}
	}
	return &servicePb.DeployResponse{Deployment: deployment}, nil
}

func (s *StrategyServer) paperReady(backend string, dryRun, confirm bool) error {
	if dryRun {
		return nil
	}
	if !confirm {
		return status.Error(codes.FailedPrecondition, "confirm is required when dry_run=false")
	}
	if !environment.GetTradingEnabled() {
		return status.Error(codes.FailedPrecondition, "paper PlaceOrder requires TRADING_ENABLED and a configured Trading backend")
	}
	if backend == "" || s.profiles == nil {
		return status.Error(codes.FailedPrecondition, "paper PlaceOrder requires a trading profile name")
	}
	if _, ok := s.profiles.Get(backend); !ok {
		return status.Errorf(codes.FailedPrecondition, "unknown trading profile %q", backend)
	}
	return nil
}

func (s *StrategyServer) requireCompletedExperimentOwned(ctx context.Context, strategyID, versionID string) error {
	experiments := repository.Experiment{Conn: s.pgx}
	list, err := experiments.ListByStrategyID(ctx, strategyID, versionID, "")
	if err != nil {
		return status.Errorf(codes.Internal, "list experiments: %v", err)
	}
	for _, experiment := range list {
		if experiment.GetStatus() == pb.Experiment_COMPLETED {
			return nil
		}
	}
	return status.Errorf(codes.FailedPrecondition,
		"version %q has no completed experiment; run CreateExperiment before Deploy", versionID)
}

func (s *StrategyServer) stopDeploymentAuthed(ctx context.Context, req *servicePb.StopDeploymentRequest) (*servicePb.StopDeploymentResponse, error) {
	account, err := accountFrom(ctx)
	if err != nil {
		return nil, err
	}
	deployments := repository.Deployment{Conn: s.pgx}
	var deployment *pb.Deployment
	switch {
	case req.GetDeploymentId() != "":
		deployment, err = deployments.Get(ctx, req.GetDeploymentId())
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, status.Error(codes.NotFound, "not found")
			}
			return nil, status.Errorf(codes.Internal, "get deployment: %v", err)
		}
		if err := auth.Can(ctx, s.pgx, account, auth.DomainStrategy, deployment.GetStrategyId(), auth.ActionTrigger); err != nil {
			return nil, auth.GRPCStatus(err)
		}
		if req.GetStrategyName() != "" {
			id, resolveErr := auth.Resolve(ctx, s.pgx, account, auth.DomainStrategy, req.GetStrategyName(), req.GetOwner(), auth.ActionTrigger)
			if resolveErr != nil {
				return nil, auth.GRPCStatus(resolveErr)
			}
			if id != deployment.GetStrategyId() {
				return nil, status.Errorf(codes.InvalidArgument,
					"deployment %q does not belong to strategy %q", req.GetDeploymentId(), req.GetStrategyName())
			}
		}
	case req.GetStrategyName() != "":
		strategyID, resolveErr := auth.Resolve(ctx, s.pgx, account, auth.DomainStrategy, req.GetStrategyName(), req.GetOwner(), auth.ActionTrigger)
		if resolveErr != nil {
			return nil, auth.GRPCStatus(resolveErr)
		}
		deployment, err = deployments.GetRunningByStrategyID(ctx, strategyID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				list, listErr := deployments.ListByStrategyID(ctx, strategyID)
				if listErr != nil {
					return nil, status.Errorf(codes.Internal, "list deployments: %v", listErr)
				}
				if len(list) == 0 {
					return nil, status.Errorf(codes.NotFound, "strategy %q has no deployments", req.GetStrategyName())
				}
				return &servicePb.StopDeploymentResponse{Deployment: list[len(list)-1]}, nil
			}
			return nil, status.Errorf(codes.Internal, "get running deployment: %v", err)
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "strategy_name or deployment_id is required")
	}
	if deployment.GetStatus() != pb.Deployment_RUNNING {
		return &servicePb.StopDeploymentResponse{Deployment: deployment}, nil
	}
	// Remove waits for the loop to end before the row reads STOPPED, so
	// nothing the loop writes lands after STOPPED. Both run detached from
	// the request and bounded, so a client that leaves can't cut them off.
	if s.runner != nil {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), repository.StatusWriteTimeout)
		err := s.runner.Remove(removeCtx, deployment.GetId())
		cancel()
		if err != nil {
			log.Error().Err(err).Str("deployment", deployment.GetId()).Msg("error stopping deployment runner")
			return nil, status.Error(codes.Internal, usercopy.DeploymentStopFailed(deployment.GetStrategyName()))
		}
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), repository.StatusWriteTimeout)
	defer cancel()
	if err := deployments.Stop(stopCtx, deployment.GetId()); err != nil {
		return nil, status.Errorf(codes.Internal, "stop deployment: %v", err)
	}
	deployment, err = deployments.Get(stopCtx, deployment.GetId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reload deployment: %v", err)
	}
	return &servicePb.StopDeploymentResponse{Deployment: deployment}, nil
}

func (s *StrategyServer) listDeploymentRunsAuthed(ctx context.Context, req *servicePb.ListDeploymentRunsRequest) (*servicePb.ListDeploymentRunsResponse, error) {
	account, err := accountFrom(ctx)
	if err != nil {
		return nil, err
	}
	runs := repository.DeploymentRun{Conn: s.pgx}
	deployments := repository.Deployment{Conn: s.pgx}
	if req.GetDeploymentId() != "" {
		deployment, err := deployments.Get(ctx, req.GetDeploymentId())
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, status.Error(codes.NotFound, "not found")
			}
			return nil, status.Errorf(codes.Internal, "list deployment runs: %v", err)
		}
		if err := auth.Can(ctx, s.pgx, account, auth.DomainStrategy, deployment.GetStrategyId(), auth.ActionRead); err != nil {
			return nil, auth.GRPCStatus(err)
		}
		if req.GetStrategyName() != "" {
			id, resolveErr := auth.Resolve(ctx, s.pgx, account, auth.DomainStrategy, req.GetStrategyName(), req.GetOwner(), auth.ActionRead)
			if resolveErr != nil {
				return nil, auth.GRPCStatus(resolveErr)
			}
			if id != deployment.GetStrategyId() {
				return nil, status.Errorf(codes.InvalidArgument,
					"deployment %q does not belong to strategy %q", req.GetDeploymentId(), req.GetStrategyName())
			}
		}
		list, err := runs.ListByStrategyID(ctx, deployment.GetStrategyId(), req.GetDeploymentId())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list deployment runs: %v", err)
		}
		return &servicePb.ListDeploymentRunsResponse{Runs: list}, nil
	}
	if req.GetStrategyName() != "" {
		strategyID, err := auth.Resolve(ctx, s.pgx, account, auth.DomainStrategy, req.GetStrategyName(), req.GetOwner(), auth.ActionRead)
		if err != nil {
			return nil, auth.GRPCStatus(err)
		}
		list, err := runs.ListByStrategyID(ctx, strategyID, "")
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list deployment runs: %v", err)
		}
		return &servicePb.ListDeploymentRunsResponse{Runs: list}, nil
	}
	strategies := repository.Strategy{Conn: s.pgx}
	visible, err := strategies.ListVisible(ctx, account)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list deployment runs: %v", err)
	}
	all := make([]*pb.DeploymentRun, 0)
	for _, item := range visible {
		part, err := runs.ListByStrategyID(ctx, item.GetId(), "")
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list deployment runs: %v", err)
		}
		all = append(all, part...)
	}
	return &servicePb.ListDeploymentRunsResponse{Runs: all}, nil
}
