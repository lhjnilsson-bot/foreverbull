package api

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy/service"
	"github.com/lhjnilsson/foreverbull/pkg/strategy/internal/repository"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultCalendar = "XNYS"

func (s *StrategyServer) Deploy(ctx context.Context, req *servicePb.DeployRequest) (*servicePb.DeployResponse, error) {
	if s.pgx == nil {
		return nil, status.Error(codes.Internal, "strategy storage is not configured")
	}
	if environment.GetAuthEnabled() {
		return s.deployAuthed(ctx, req)
	}

	if req.GetMode() == pb.Deployment_LIVE {
		return nil, status.Error(codes.FailedPrecondition,
			"live mode is not enabled; deploy with paper")
	}

	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}
	backend := strings.TrimSpace(req.GetBackend())
	if !dryRun {
		if !req.GetConfirm() {
			return nil, status.Error(codes.FailedPrecondition,
				"confirm is required when dry_run=false")
		}
		if !environment.GetTradingEnabled() {
			return nil, status.Error(codes.FailedPrecondition,
				"paper PlaceOrder requires TRADING_ENABLED and a configured Trading backend")
		}
		if backend == "" || s.profiles == nil {
			return nil, status.Error(codes.FailedPrecondition,
				"paper PlaceOrder requires a trading profile name")
		}
		if _, ok := s.profiles.Get(backend); !ok {
			return nil, status.Errorf(codes.FailedPrecondition,
				"unknown trading profile %q", backend)
		}
	}

	version, err := s.resolveVersion(ctx, req.GetStrategyName(), req.GetVersionId(), req.GetVersionRev())
	if err != nil {
		return nil, err
	}
	if err := s.requireServiceReady(ctx, version.ServiceImage); err != nil {
		return nil, err
	}
	if err := s.requireCompletedExperiment(ctx, version); err != nil {
		return nil, err
	}

	calendar := req.GetCalendarCode()
	if calendar == "" {
		calendar = defaultCalendar
	}

	deployments := repository.Deployment{Conn: s.pgx}
	deployment, err := deployments.Create(
		ctx, version.Id, backend, calendar,
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
			failErr := deployments.Fail(ctx, deployment.Id, err)
			notStored := repository.StatusNotStored(failErr)
			log.Error().Err(failErr).Str("deployment", deployment.Id).Bool("error_not_stored", notStored).
				Msg("error starting deployment runner")
			return nil, status.Error(codes.Internal, usercopy.DeploymentStartFailed(req.GetStrategyName(), notStored))
		}
	}

	return &servicePb.DeployResponse{Deployment: deployment}, nil
}

func (s *StrategyServer) StopDeployment(ctx context.Context, req *servicePb.StopDeploymentRequest) (*servicePb.StopDeploymentResponse, error) {
	if s.pgx == nil {
		return nil, status.Error(codes.Internal, "strategy storage is not configured")
	}
	if environment.GetAuthEnabled() {
		return s.stopDeploymentAuthed(ctx, req)
	}

	deployments := repository.Deployment{Conn: s.pgx}
	var deployment *pb.Deployment
	var err error

	switch {
	case req.GetDeploymentId() != "":
		deployment, err = deployments.Get(ctx, req.GetDeploymentId())
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, status.Errorf(codes.NotFound, "deployment %q not found", req.GetDeploymentId())
			}
			return nil, status.Errorf(codes.Internal, "get deployment: %v", err)
		}
		if req.GetStrategyName() != "" && deployment.StrategyName != req.GetStrategyName() {
			return nil, status.Errorf(codes.InvalidArgument,
				"deployment %q does not belong to strategy %q", req.GetDeploymentId(), req.GetStrategyName())
		}
	case req.GetStrategyName() != "":
		strategies := repository.Strategy{Conn: s.pgx}
		if _, err := strategies.Get(ctx, req.GetStrategyName()); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, status.Errorf(codes.NotFound, "strategy %q not found", req.GetStrategyName())
			}
			return nil, status.Errorf(codes.Internal, "get strategy: %v", err)
		}
		deployment, err = deployments.GetRunningByStrategyName(ctx, req.GetStrategyName())
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				list, listErr := deployments.List(ctx, req.GetStrategyName())
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

	if deployment.Status == pb.Deployment_RUNNING {
		// Remove waits for the loop to end before the row reads STOPPED, so
		// nothing the loop writes lands after STOPPED. Both run detached from
		// the request and bounded, so a client that leaves can't cut them off.
		if s.runner != nil {
			removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), repository.StatusWriteTimeout)
			err := s.runner.Remove(removeCtx, deployment.Id)
			cancel()
			if err != nil {
				log.Error().Err(err).Str("deployment", deployment.Id).Msg("error stopping deployment runner")
				return nil, status.Error(codes.Internal, usercopy.DeploymentStopFailed(deployment.GetStrategyName()))
			}
		}
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), repository.StatusWriteTimeout)
		defer cancel()
		if err := deployments.Stop(stopCtx, deployment.Id); err != nil {
			return nil, status.Errorf(codes.Internal, "stop deployment: %v", err)
		}
		deployment, err = deployments.Get(stopCtx, deployment.Id)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "reload deployment: %v", err)
		}
	}

	return &servicePb.StopDeploymentResponse{Deployment: deployment}, nil
}

func (s *StrategyServer) ListDeploymentRuns(ctx context.Context, req *servicePb.ListDeploymentRunsRequest) (*servicePb.ListDeploymentRunsResponse, error) {
	if s.pgx == nil {
		return nil, status.Error(codes.Internal, "strategy storage is not configured")
	}
	if environment.GetAuthEnabled() {
		return s.listDeploymentRunsAuthed(ctx, req)
	}

	runs := repository.DeploymentRun{Conn: s.pgx}
	list, err := runs.List(ctx, req.GetStrategyName(), req.GetDeploymentId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list deployment runs: %v", err)
	}
	return &servicePb.ListDeploymentRunsResponse{Runs: list}, nil
}

func (s *StrategyServer) requireCompletedExperiment(ctx context.Context, version *pb.Version) error {
	experiments := repository.Experiment{Conn: s.pgx}
	list, err := experiments.List(ctx, version.StrategyName, version.Id, "")
	if err != nil {
		return status.Errorf(codes.Internal, "list experiments: %v", err)
	}
	for _, experiment := range list {
		if experiment.Status == pb.Experiment_COMPLETED {
			return nil
		}
	}
	return status.Errorf(codes.FailedPrecondition,
		"version %q has no completed experiment; run CreateExperiment before Deploy", version.Id)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
