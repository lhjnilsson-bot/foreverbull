package command

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/socket"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	financebars "github.com/lhjnilsson/foreverbull/pkg/finance/bars"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/api"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/stream/dependency"
	fs "github.com/lhjnilsson/foreverbull/pkg/finance/stream"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
)

func SessionRun(ctx context.Context, msg stream.Message) error {
	cmd := fs.StartTradingSessionCommand{}

	err := msg.ParsePayload(&cmd)
	if err != nil {
		return fmt.Errorf("error parsing payload: %w", err)
	}

	db := msg.MustGet(stream.DBDep).(*pgxpool.Pool)
	repository := repository.Trading{Conn: db}

	trading, _ := msg.MustGet(dependency.TradingDep).(supplier.Trading)

	_, err = repository.Get(ctx, cmd.Id)
	if err != nil {
		return fmt.Errorf("error getting session: %w", err)
	}
	if trading == nil {
		return fmt.Errorf("trading is not enabled")
	}

	var (
		server   *grpc.Server
		activity <-chan bool
	)
	bars := financebars.New(db)
	if environment.GetAuthEnabled() {
		secret, ok := auth.LookupTradingSecret(cmd.Id)
		if !ok {
			return fmt.Errorf("%s is missing", auth.SessionMetadata())
		}
		server, activity, err = api.NewSecuredGRPCSessionServer(trading, secret, api.WithBars(bars, supplier.DefaultProvider), api.WithSessionID(cmd.Id))
	} else {
		server, activity, err = api.NewGRPCSessionServer(trading, api.WithBars(bars, supplier.DefaultProvider), api.WithSessionID(cmd.Id))
	}
	if err != nil {
		return fmt.Errorf("error creating grpc session server: %w", err)
	}

	listener, port, err := socket.ListenTCP("")
	if err != nil {
		return fmt.Errorf("error creating trading listener: %w", err)
	}

	go func() {
		// Serve has no caller once SessionRun returns; a listener that dies
		// is logged, and the client sees its connection refused.
		if err := server.Serve(listener); err != nil {
			log.Error().Err(err).Str("session", cmd.Id).Msg("error serving trading session server")
		}
	}()
	go func() {
		defer func() {
			server.Stop()
			if environment.GetAuthEnabled() {
				auth.ForgetTradingSecret(cmd.Id)
			}
		}()
		for {
			select {
			case _, active := <-activity:
				if !active {
					time.Sleep(time.Second / 4) // make sure reply is sent
					return
				}
			case <-time.After(10 * time.Minute):
				return
			}
		}
	}()

	if err := repository.UpdatePort(ctx, cmd.Id, port); err != nil {
		// No client can reach a server whose port isn't stored.
		server.Stop()
		return stream.Unreachable(fmt.Errorf("error storing session port: %w", err))
	}
	return nil
}
