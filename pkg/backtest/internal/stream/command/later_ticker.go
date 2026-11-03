package command

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	financeServicePB "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"github.com/rs/zerolog/log"
)

// laterTickerPattern is the ticker shape the finance service stores bars
// under. A later ticker outside it is not shown or stored.
var laterTickerPattern = regexp.MustCompile(`^[A-Za-z0-9.^=_-]{1,32}$`)

// errBadLaterTicker is a download response whose later ticker can't be a
// resolution: no valid ticker, or the asked ticker itself.
var errBadLaterTicker = errors.New("download named an unusable later ticker")

// laterTickerResolution returns the resolution an OK download response
// names, and false when its bars came from the asked ticker itself. It
// carries no day: the provider doesn't say when the ticker changed.
func laterTickerResolution(msg *financeServicePB.DownloadHistoricalDataResponse) (repository.TickerResolution, bool, error) {
	later := msg.GetLaterTicker()
	if later == nil {
		return repository.TickerResolution{}, false, nil
	}
	symbol := msg.GetSymbol()
	if !laterTickerPattern.MatchString(symbol) {
		return repository.TickerResolution{}, false, fmt.Errorf("asked ticker %q: %w", symbol, errBadLaterTicker)
	}
	readAs := later.GetSymbol()
	if !laterTickerPattern.MatchString(readAs) {
		return repository.TickerResolution{}, false, fmt.Errorf("%s read as %q: %w", symbol, readAs, errBadLaterTicker)
	}
	if readAs == symbol {
		return repository.TickerResolution{}, false, fmt.Errorf("%s read as itself: %w", symbol, errBadLaterTicker)
	}
	return repository.TickerResolution{Symbol: symbol, ReadAs: readAs}, true, nil
}

// saveTickerResolutions stores the resolutions on the backtest this create
// stored, by id when AUTH_ENABLED is on.
func saveTickerResolutions(ctx context.Context, backtests repository.Backtest, name, id string,
	resolutions []repository.TickerResolution,
) error {
	if environment.GetAuthEnabled() {
		return backtests.SaveTickerResolutionsByID(ctx, id, resolutions)
	}
	return backtests.SaveTickerResolutions(ctx, name, resolutions)
}

// discardUnrecordedResolutions deletes the backtest when a later-ticker
// resolution couldn't be read or stored, so no backtest reads one company
// under another ticker without saying so. The cause goes to the log.
func discardUnrecordedResolutions(ctx context.Context, backtests repository.Backtest, name, id string, cause error) error {
	log.Error().Err(cause).Str("backtest", name).Msg("error recording later-ticker resolutions")
	err := deleteAfterFailure(ctx, backtests, name, id)
	removed := err == nil || errors.Is(err, pgx.ErrNoRows)
	if !removed {
		log.Warn().Err(err).Str("backtest", name).Msg("error deleting backtest after unrecorded later tickers")
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return plainSentence(laterTickerNotRecordedMessage(name, removed))
}

// laterTickerNotRecordedMessage is the step's whole message when the
// resolutions couldn't be recorded. The cause is the environment's or a
// fault, so the step is to check the environment and run create again.
func laterTickerNotRecordedMessage(name string, removed bool) string {
	saved, next := savedAndNext(name, usercopy.RetryStep("fbull backtest create"), removed)
	return fmt.Sprintf("Couldn't load data for backtest '%s': %s %s %s", name, usercopy.LaterTickerNotRecorded, saved, next)
}
