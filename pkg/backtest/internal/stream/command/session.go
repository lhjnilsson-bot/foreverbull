package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/socket"
	"github.com/lhjnilsson/foreverbull/internal/storage"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/engine"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/api"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/stream/dependency"
	ss "github.com/lhjnilsson/foreverbull/pkg/backtest/stream"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
)

const (
	SessionTimeout = 30 * time.Minute
)

func SessionRun(ctx context.Context, msg stream.Message) error {
	command := ss.SessionRunCommand{}

	err := msg.ParsePayload(&command)
	if err != nil {
		return fmt.Errorf("error unmarshalling SessionRun payload: %w", err)
	}

	db := msg.MustGet(stream.DBDep).(*pgxpool.Pool)
	s := msg.MustGet(stream.StorageDep).(storage.Storage)
	sessions := repository.Session{Conn: db}

	session, err := sessions.Get(ctx, command.SessionID)
	if err != nil {
		log.Err(err).Msg("error getting session")
		return fmt.Errorf("error getting session: %w", err)
	}

	depEngine, err := msg.Call(ctx, dependency.GetEngineKey)
	if err != nil {
		log.Err(err).Msg("error getting zipline engine")
		return fmt.Errorf("error getting zipline engine: %w", err)
	}

	engine := depEngine.(engine.Engine)

	var server *grpc.Server
	var activity <-chan bool
	if environment.GetAuthEnabled() {
		secret, ok := auth.LookupBacktestSecret(command.SessionID)
		if !ok {
			return fmt.Errorf("%s is missing", auth.SessionMetadata())
		}
		backtestID, idErr := sessions.BacktestID(ctx, command.SessionID)
		if idErr != nil {
			return fmt.Errorf("error getting session backtest: %w", idErr)
		}
		ingestion, getErr := s.GetObject(ctx, storage.IngestionsBucket, backtestID)
		if getErr != nil {
			return fmt.Errorf("error getting ingestion: %w", getErr)
		}
		if err = engine.DownloadIngestion(ctx, ingestion); err != nil {
			log.Err(err).Msg("error downloading ingestion")
			return fmt.Errorf("error downloading ingestion: %w", err)
		}
		server, activity, err = api.NewSecuredGRPCSessionServer(session, db, engine, secret)
	} else {
		ingestion, pickErr := newestIngestion(ctx, s)
		if pickErr != nil {
			return pickErr
		}
		if err = engine.DownloadIngestion(ctx, ingestion); err != nil {
			log.Err(err).Msg("error downloading ingestion")
			return fmt.Errorf("error downloading ingestion: %w", err)
		}
		server, activity, err = api.NewGRPCSessionServer(session, db, engine)
	}
	if err != nil {
		log.Err(err).Msg("error creating grpc session server")
		return fmt.Errorf("error creating grpc session server: %w", err)
	}

	listener, port, err := socket.ListenTCP("")
	if err != nil {
		return fmt.Errorf("error creating session listener: %w", err)
	}

	go func() {
		err := server.Serve(listener)
		if err != nil {
			log.Error().Err(err).Msg("error serving session server")
		}
	}()
	go func() {
		defer func() {
			log.Info().Msg("closing session server")
			server.Stop()
			if environment.GetAuthEnabled() {
				auth.ForgetBacktestSecret(command.SessionID)
			}
		}()

		for {
			select {
			case _, active := <-activity:
				if !active {
					time.Sleep(time.Second / 4) // make sure reply is sent
					return
				}
			case <-time.After(SessionTimeout):
				return
			}
		}
	}()

	return publishSessionPort(ctx, &sessions, server, command.SessionID, port)
}

type sessionPortStore interface {
	UpdatePort(ctx context.Context, sessionID string, port int) error
}

// publishSessionPort stores the port the session server listens on. No
// client can reach a server whose port isn't stored, so a failed write
// stops the server and fails the command.
func publishSessionPort(ctx context.Context, sessions sessionPortStore, server *grpc.Server, sessionID string, port int) error {
	if err := sessions.UpdatePort(ctx, sessionID, port); err != nil {
		server.Stop()
		return stream.Unreachable(fmt.Errorf("error storing session port: %w", err))
	}
	return nil
}

func newestIngestion(ctx context.Context, store storage.Storage) (*storage.Object, error) {
	ingestions, err := store.ListObjects(ctx, storage.IngestionsBucket)
	if err != nil {
		log.Err(err).Msg("error listing ingestions")
		return nil, fmt.Errorf("error listing ingestions: %w", err)
	}
	if len(*ingestions) == 0 {
		return nil, errors.New("no ingestions found")
	}
	var ingestion *storage.Object
	for _, stored := range *ingestions {
		stored.Refresh() //nolint: errcheck
		if ingestion == nil || stored.LastModified.After(ingestion.LastModified) {
			ingestion = &stored
		}
	}
	if ingestion == nil {
		return nil, errors.New("no completed ingestions found, create one before running a session")
	}
	return ingestion, nil
}
