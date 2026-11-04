package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/numfmt"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
	"github.com/lhjnilsson/foreverbull/internal/redact"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/stream/dependency"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/suppliers/marketdata"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/ticker"
	fs "github.com/lhjnilsson/foreverbull/pkg/finance/stream"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	"github.com/rs/zerolog/log"
)

func Ingest(ctx context.Context, message stream.Message) error {
	postgres, isDB := message.MustGet(stream.DBDep).(postgres.Query)
	if !isDB {
		return errors.New("db dependency casting failed")
	}

	yahoo, isMd := message.MustGet(dependency.MarketDataDep).(supplier.Marketdata)
	if !isMd {
		return errors.New("marketdata dependency casting failed")
	}

	command := fs.IngestCommand{}

	err := message.ParsePayload(&command)
	if err != nil {
		return fmt.Errorf("error unmarshalling OHLCIngest payload: %w", err)
	}

	assets := repository.Asset{Conn: postgres}
	ohlc := repository.OHLC{Conn: postgres}

	start, err := time.Parse("2006-01-02", command.Start)
	if err != nil {
		return fmt.Errorf("error parsing start date: %w", err)
	}

	var end *time.Time

	if command.End != nil {
		e, err := time.Parse("2006-01-02", *command.End)
		if err != nil {
			return fmt.Errorf("error parsing end date: %w", err)
		}

		end = &e
	}

	provider, err := marketdata.NormalizeProvider(command.Provider)
	if err != nil {
		return err
	}
	profilesFile := environment.GetTradingProfilesFile()
	if err := marketdata.ValidatePriceSource(provider, command.Profile, profilesFile); err != nil {
		return err
	}
	bars, err := marketdata.OHLCClient(provider, command.Profile, profilesFile, yahoo)
	if err != nil {
		return err
	}

	// Every symbol becomes an asset row, a bars key, and task-state text, so
	// all are checked before anything is fetched or stored. The refusal names
	// the position, not the symbol that failed the check.
	for index, symbol := range command.Symbols {
		if !ticker.Valid(symbol) {
			return errors.New(ticker.InvalidMessage(index+1, len(command.Symbols)))
		}
	}

	// The time limit ends the whole ingest. Each symbol commits on its own,
	// so the ones before the cut stay stored.
	limit, err := environment.GetFinanceTaskTimeLimit()
	if err != nil {
		return err
	}
	ctx, stopAtLimit := marketdata.WithTaskLimit(ctx, limit, errIngestTimeLimit)
	defer stopAtLimit()

	for _, symbol := range command.Symbols {
		if err := ctx.Err(); err != nil {
			if stopped := ingestStopped(ctx, provider, symbol, limit, err); stopped != nil {
				return stopped
			}
			return err
		}
		_, err := assets.Get(ctx, symbol)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return logStoreError(&repository.StoreError{Symbol: symbol, Err: err}, "get asset")
			}

			a, err := yahoo.GetAsset(ctx, symbol)
			if err != nil {
				if stopped := ingestStopped(ctx, assetProvider, symbol, limit, err); stopped != nil {
					return stopped
				}
				return fmt.Errorf("error getting asset: %w", redact.URLError(err))
			}
			if !ticker.Valid(a.GetSymbol()) {
				log.Warn().Str("symbol", symbol).Str("provider_symbol", ticker.Logged(a.GetSymbol())).
					Msg("provider returned a ticker outside the symbol pattern")
				return invalidProviderTickerError(assetProvider, symbol)
			}

			// Outside the bars transaction: the asset row stays if the bars roll back.
			err = assets.Store(ctx, a.Symbol, a.Name)
			if err != nil {
				return logStoreError(&repository.StoreError{Symbol: symbol, Err: err}, "store asset")
			}
		}

		if err := ingestSymbol(ctx, &ohlc, bars, provider, symbol, start, end); err != nil {
			if stopped := ingestStopped(ctx, provider, symbol, limit, err); stopped != nil {
				return stopped
			}
			return err
		}
	}

	return nil
}

// One ingest command stops after FINANCE_TASK_TIME_LIMIT (2h by default).
// The ingest fetches one symbol at a time, and at the shared provider rates
// 10,000 symbols take about an hour and a half; past the limit a hung
// provider can't hold the task forever.

// errIngestTimeLimit is the ctx cause when the time limit ends an ingest,
// so an abort (a cancel) is still told apart from the limit.
var errIngestTimeLimit = errors.New("ingest time limit reached")

// ingestStopped is the task-state text when symbol didn't finish because the
// ingest's time limit ended or its provider call timed out, or nil for any
// other failure. The limit and a slow provider have different sentences. A
// provider wait refused because its slot comes after the limit is the
// limit. A database error is a timeout only once the limit ended, so a
// symbol stuck on its lock at the limit gets the limit's sentence.
func ingestStopped(ctx context.Context, provider, symbol string, limit time.Duration, err error) error {
	limitReached := marketdata.AtTaskLimit(ctx, err, errIngestTimeLimit)
	var stored *repository.StoreError
	providerTimeout := ctx.Err() == nil && !errors.As(err, &stored) && marketdata.IsProviderTimeout(err)
	if !limitReached && !providerTimeout {
		return nil
	}
	log.Warn().Err(redact.URLError(err)).Str("provider", provider).Str("symbol", symbol).
		Bool("time_limit", limitReached).Msg("ingest stopped: provider took too long")
	if limitReached {
		return ingestTimeLimitError(limit, symbol)
	}
	return fmt.Errorf("%w, so the price import stopped. Prices imported before %s were kept. Try again later.", //nolint:staticcheck // ST1005: agreed user-facing copy ends with a period
		&marketdata.TimeoutError{Provider: provider, Symbol: symbol}, symbol)
}

// ingestTimeLimitError is the agreed task-state text when the ingest's time
// limit stopped it at symbol, word for word.
func ingestTimeLimitError(limit time.Duration, symbol string) error {
	return fmt.Errorf("The price import hit the %s limit at %s. Prices imported before %s were kept. "+ //nolint:staticcheck // ST1005: agreed user-facing copy
		"Import fewer symbols at a time, or try again later.", numfmt.LimitSpan(limit), symbol, symbol)
}

// assetProvider is the provider GetAsset asks. The marketdata dependency is
// the Yahoo client whatever the command's provider: Search, GetAsset, and
// GetIndex stay on Yahoo (see the finance module).
const assetProvider = "yahoo"

// invalidProviderTickerError is the whole task-state text when the asset
// lookup returns a ticker outside the pattern. It names the provider through
// the allowlist and the requested symbol, which has passed ticker.Valid,
// never the provider's value. Nothing wraps it on the way to the task state.
func invalidProviderTickerError(provider, symbol string) error {
	return fmt.Errorf("%s returned an invalid ticker for %s, so its prices weren't imported. "+ //nolint:staticcheck // ST1005: agreed user-facing copy ends with a period
		"Check the symbol or leave it out, and report it at %s.",
		marketdata.ShownProvider(provider), symbol, repository.IssuesURL)
}

// ingestSymbol stores bars for one symbol through OHLC.Sync, the overlap and
// re-scale rule the DownloadHistoricalData RPC uses too. A database error
// becomes the fixed "Couldn't store prices for SYM. Try again …" sentence and
// its cause goes to the server log, so no SQL or driver text reaches the task
// state.
func ingestSymbol(ctx context.Context, ohlc *repository.OHLC, bars supplier.Marketdata,
	provider, symbol string, start time.Time, end *time.Time,
) error {
	err := ohlc.SyncSource(ctx, provider, symbol, start, end, func(from time.Time, to *time.Time) ([]*pb.OHLC, error) {
		return getOHLC(ctx, bars, symbol, from, to)
	}, bars)
	return logStoreError(err, "store bars")
}

// logStoreError logs the cause of a StoreError and returns it; its own text is
// the fixed sentence. Other errors come back unchanged.
func logStoreError(err error, step string) error {
	var stored *repository.StoreError
	if errors.As(err, &stored) {
		log.Error().Err(stored.Err).Str("symbol", stored.Symbol).Str("step", step).Msg("ingest failed to store prices")
	}
	return err
}

func getOHLC(ctx context.Context, bars supplier.Marketdata, symbol string, start time.Time, end *time.Time) ([]*pb.OHLC, error) {
	ohlcs, err := bars.GetOHLC(ctx, symbol, start, end)
	if err != nil {
		// The blocked sentence already names the provider, symbol, and retry.
		// It is checked before redact.URLError, which drops the chain.
		if errors.Is(err, marketdata.ErrProviderBlocked) {
			return nil, err
		}
		return nil, fmt.Errorf("error getting OHLC: %w", redact.URLError(err))
	}
	return ohlcs, nil
}
