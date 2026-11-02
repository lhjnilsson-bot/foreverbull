package command

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lhjnilsson/foreverbull/internal/container"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	"github.com/lhjnilsson/foreverbull/pkg/backtest"
	backtestpb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	"github.com/lhjnilsson/foreverbull/pkg/service/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/service/internal/stream/dependency"
	ss "github.com/lhjnilsson/foreverbull/pkg/service/stream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// unimplemented is a command with no implementation. Its text is the plain
// sentence, which a command's ERROR state carries as is, and it still reads
// as gRPC Unimplemented.
type unimplemented string

func (e unimplemented) Error() string { return string(e) }

// GRPCStatus is the error's status for status.FromError and status.Code.
func (e unimplemented) GRPCStatus() *status.Status { return status.New(codes.Unimplemented, string(e)) }

// UpdateServiceStatus has no implementation. It answers Unimplemented so a
// caller never reads the status as updated.
func UpdateServiceStatus(ctx context.Context, message stream.Message) error {
	return unimplemented(usercopy.ServiceStatusUnimplemented)
}

func ServiceStart(ctx context.Context, message stream.Message) error {
	postgres, isDB := message.MustGet(stream.DBDep).(postgres.Query)
	if !isDB {
		return fmt.Errorf("db dependency casting failed")
	}

	runtime := message.MustGet(dependency.ContainerDep).(container.Engine)

	command := ss.ServiceStartCommand{}

	err := message.ParsePayload(&command)
	if err != nil {
		return fmt.Errorf("error unmarshalling ServiceStart payload: %w", err)
	}

	services := repository.Service{Conn: postgres}
	_, err = services.Get(ctx, command.Image)
	if errors.Is(err, repository.ErrServiceNotFound) {
		if _, err := services.Create(ctx, command.Image); err != nil {
			return fmt.Errorf("error creating service: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("error getting service: %w", err)
	}

	provider := strings.TrimSpace(command.OHLCProvider)
	if environment.GetAuthEnabled() && strings.TrimSpace(command.Backtest) != "" && strings.TrimSpace(command.BacktestID) == "" {
		return fmt.Errorf("backtest id is empty")
	}
	if provider == "" && environment.GetAuthEnabled() && strings.TrimSpace(command.BacktestID) != "" {
		reader, ok := backtest.NewStore(postgres).(interface {
			GetBacktestByID(context.Context, string) (*backtestpb.Backtest, error)
		})
		if !ok {
			return fmt.Errorf("backtest id is empty")
		}
		stored, getErr := reader.GetBacktestByID(ctx, strings.TrimSpace(command.BacktestID))
		if getErr != nil {
			return fmt.Errorf("error getting backtest %s: %w", command.BacktestID, getErr)
		}
		provider = strings.TrimSpace(stored.GetProvider())
	}
	if provider == "" && !environment.GetAuthEnabled() && strings.TrimSpace(command.Backtest) != "" {
		stored, getErr := backtest.NewStore(postgres).GetBacktest(ctx, strings.TrimSpace(command.Backtest))
		if getErr != nil {
			return fmt.Errorf("error getting backtest %s: %w", command.Backtest, getErr)
		}
		provider = strings.TrimSpace(stored.GetProvider())
	}
	ctx = container.WithOHLCProvider(ctx, provider)

	/*extraLabels := map[string]string{
	"orchestration_id": message.GetOrchestrationID(),
	}*/
	_, err = runtime.Start(ctx, command.Image, command.InstanceID)
	if err != nil {
		return fmt.Errorf("error starting container: %w", err)
	}

	instances := repository.Instance{Conn: postgres}

	_, err = instances.Create(ctx, command.InstanceID, &command.Image)
	if err != nil {
		return fmt.Errorf("error creating instance: %w", err)
	}

	return nil
}
