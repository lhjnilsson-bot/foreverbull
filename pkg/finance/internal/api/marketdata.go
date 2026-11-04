package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/numfmt"
	"github.com/lhjnilsson/foreverbull/internal/redact"
	financeapi "github.com/lhjnilsson/foreverbull/pkg/finance/api"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/suppliers/marketdata"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/ticker"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	internal_pb "github.com/lhjnilsson/foreverbull/pkg/pb"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type MarketdataServer struct {
	servicePb.UnimplementedMarketdataServer

	pgx        *pgxpool.Pool
	marketdata supplier.Marketdata
}

func NewMarketdataServer(pgx *pgxpool.Pool, md supplier.Marketdata) *MarketdataServer {
	return &MarketdataServer{
		pgx:        pgx,
		marketdata: md,
	}
}

func (fs *MarketdataServer) GetAsset(ctx context.Context, req *servicePb.GetAssetRequest) (*servicePb.GetAssetResponse, error) {
	identifier := req.GetIdentifier()
	if identifier == nil {
		return nil, fmt.Errorf("identifier is nil")
	}
	var asset *pb.Asset
	var err error
	switch identifier := identifier.(type) {
	case *servicePb.GetAssetRequest_Symbol:
		asset, err = fs.marketdata.GetAsset(ctx, identifier.Symbol)
		if err != nil {
			if errors.Is(err, marketdata.ErrNoISIN) {
				// The provider served the lookup and has no ISIN, so the
				// equity can't be followed across ticker changes. Callers
				// branch on NotFound plus the shared prefix; the message
				// names only the requested symbol and the cause stays in the
				// log.
				log.Error().Err(redact.URLError(err)).Str("symbol", identifier.Symbol).
					Msg("getting asset failed: no ISIN")
				return nil, status.Error(codes.NotFound, financeapi.NoISINMessage(identifier.Symbol))
			}
			return nil, fmt.Errorf("error getting asset: %w", redact.URLError(err))
		}
	case *servicePb.GetAssetRequest_Isin:
		assets, err := fs.marketdata.SearchAsset(ctx, identifier.Isin)
		if err != nil {
			return nil, fmt.Errorf("error getting asset: %w", redact.URLError(err))
		}
		if len(assets) == 0 {
			return nil, fmt.Errorf("asset not found by isin: %s", identifier.Isin)
		}
		for _, a := range assets {
			if a.ISIN == identifier.Isin {
				asset = a
				break
			}
		}
	default:
		return nil, fmt.Errorf("unknown identifier type: %T", identifier)
	}
	if asset == nil {
		return nil, fmt.Errorf("asset not found: %s", req.GetSymbol())
	}

	return &servicePb.GetAssetResponse{
		Asset: asset,
	}, nil
}

// GetIndex looks up every member of the index at the provider, so it is
// limited to an editor or an admin like DownloadHistoricalData.
func (fs *MarketdataServer) GetIndex(ctx context.Context, req *servicePb.GetIndexRequest) (*servicePb.GetIndexResponse, error) {
	if err := requireTradingEditor(ctx); err != nil {
		return nil, err
	}
	assets, err := fs.marketdata.GetIndex(ctx, req.GetSymbol())
	if err != nil {
		// The cause, which names each member that failed, is logged
		// redacted; the client-facing interceptor keeps it off the status.
		redacted := redact.URLError(err)
		log.Error().Err(redacted).Str("symbol", req.GetSymbol()).Msg("getting index failed")
		return nil, fmt.Errorf("error getting index: %w", redacted)
	}

	return &servicePb.GetIndexResponse{
		Assets: assets,
	}, nil
}

const downloadWorkers = 10

// One DownloadHistoricalData stops after FINANCE_TASK_TIME_LIMIT (2h by
// default). At the shared provider rates a 10,000-symbol download takes
// about an hour on Yahoo or Alpaca and about 85 minutes on Stooq, so 2h
// leaves room for 429 backoffs while a hung provider still can't hold the
// request and its workers forever. Symbols stored before the limit stay
// stored.

// errDownloadTimeLimit is the ctx cause when downloadTimeLimit ends a
// download, so it is told apart from the client's own deadline or cancel.
var errDownloadTimeLimit = errors.New("download time limit reached")

// metadataProvider is the provider GetAsset asks, whatever the request's
// price provider: asset metadata stays on Yahoo.
const metadataProvider = "yahoo"

// downloadSymbols returns the request's symbols under their stored names
// (marketdata.StoredSymbol: BRK.B, BRK-B and BRKB are BRKB) with repeats left
// out, in request order, so each symbol is fetched, stored and reported once.
//
// More than marketdata.MaxDownloadSymbols distinct symbols is
// InvalidArgument; counting stops at the first symbol over the cap. Nothing
// is fetched or stored then. The caller has already refused a symbol outside
// the ticker pattern (blank included) by its position.
func downloadSymbols(symbols []string) ([]string, error) {
	// Distinct stored names, the count marketdata.DownloadSymbolCount gives
	// backtest create.
	seen := make(map[string]struct{}, min(len(symbols), marketdata.MaxDownloadSymbols+1))
	out := make([]string, 0, min(len(symbols), marketdata.MaxDownloadSymbols))
	for _, symbol := range symbols {
		name := marketdata.StoredSymbol(symbol)
		if _, ok := seen[name]; ok {
			continue
		}
		if len(out) == marketdata.MaxDownloadSymbols {
			return nil, status.Error(codes.InvalidArgument, tooManySymbolsMessage)
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

// tooManySymbolsMessage refuses a request over the per-request cap. Backtest
// create counts the same way before it stores a backtest and shows its own
// sentence with the count; this one is for other callers of the RPC.
var tooManySymbolsMessage = fmt.Sprintf(
	"one download can carry at most %s distinct symbols, and this one has more. Split it into smaller downloads.",
	numfmt.Thousands(marketdata.MaxDownloadSymbols))

// downloadSender serialises Send on the download stream. gRPC doesn't allow
// concurrent SendMsg on one stream, and every download worker reports here.
// The first failed Send calls stop: the client can't get any later result,
// so the workers must not keep fetching and storing for it.
type downloadSender struct {
	mu     sync.Mutex
	stream servicePb.Marketdata_DownloadHistoricalDataServer
	stop   context.CancelFunc
	err    error
	// reported holds every symbol sent, at most MaxDownloadSymbols, so a
	// download stopped by its time limit can name one that didn't finish.
	reported map[string]struct{}
}

// newDownloadSender wires stop, which cancels the download's workers.
func newDownloadSender(stream servicePb.Marketdata_DownloadHistoricalDataServer, stop context.CancelFunc) *downloadSender {
	return &downloadSender{stream: stream, stop: stop}
}

func (s *downloadSender) send(rsp *servicePb.DownloadHistoricalDataResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	if err := s.stream.Send(rsp); err == nil {
		if s.reported == nil {
			s.reported = make(map[string]struct{})
		}
		s.reported[rsp.GetSymbol()] = struct{}{}
	} else {
		log.Err(err).Msg("error sending response, stopping the download")
		s.err = err
		// A sender built without newDownloadSender has no stop; it still
		// records the failure and sends nothing more instead of panicking.
		if s.stop != nil {
			s.stop()
		}
	}
}

// firstUnreported is the first of symbols, in request order, that no
// response was sent for, or "" when every one was.
func (s *downloadSender) firstUnreported(symbols []string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, symbol := range symbols {
		if _, ok := s.reported[symbol]; !ok {
			return symbol
		}
	}
	return ""
}

// sendErr is the first failed Send, or nil.
func (s *downloadSender) sendErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// downloadFailedError is the generic STATUS_ERROR text for a failure whose
// details (DB, driver or provider text) must stay in the log. The RPC is
// reachable by editors, so raw internal errors don't go out.
func downloadFailedError(symbol string) error {
	return fmt.Errorf("couldn't download prices for %s. Try again in a few minutes, "+ //nolint:staticcheck // ST1005: agreed user-facing copy ends with a period
		"or use another provider with --provider.", symbol)
}

// internalDownloadError logs err with context and returns the generic text.
func internalDownloadError(symbol, step string, err error) error {
	log.Error().Err(redact.URLError(err)).Str("symbol", symbol).Str("step", step).Msg("historical data download failed")
	return downloadFailedError(symbol)
}

// noPriceDataError is the STATUS_ERROR text when the provider answered for
// every ticker of the instrument and none has bars. DownloadFinancialData
// uses this prefix for the no-bars sentence; any STATUS_ERROR fails create.
// Only a successful empty answer may produce it. A failed request, search
// or store is reported with downloadFailedError instead.
func noPriceDataError(symbol string) error {
	return fmt.Errorf("%w for %s", errNoPriceData, symbol)
}

// sameTicker reports whether a and b name one ticker: the same stored name
// (marketdata.StoredSymbol: BRK.B, BRK-B and BRKB are BRKB), in any case.
func sameTicker(a, b string) bool {
	return strings.EqualFold(marketdata.StoredSymbol(a), marketdata.StoredSymbol(b))
}

// noBarsAnswer reports whether a GetOHLC error is the provider saying it has
// no bars for the ticker, as opposed to a failed request.
func noBarsAnswer(err error) bool {
	return errors.Is(err, marketdata.ErrNoBars)
}

var errNoPriceData = errors.New("no price data")

// ambiguousSymbolError is the STATUS_ERROR text when the asked ticker maps to
// more than one instrument. provider is the normalised provider and symbol
// passed the ticker check; the backtest service matches this exact shape.
func ambiguousSymbolError(provider, symbol string) error {
	return fmt.Errorf("%s has more than one instrument for %s", provider, symbol)
}

// noISINError is the STATUS_ERROR text when the provider answered that the
// asked equity has no ISIN, so it can't be followed across ticker changes.
// DownloadFinancialData matches financeapi.NoISINPrefix for its own
// sentence; a retry can't fix it. symbol is the requested symbol, never
// provider text.
func noISINError(symbol string) error {
	return errors.New(financeapi.NoISINMessage(symbol))
}

// providerTimedOut reports a provider call that ran out of time while the
// download itself still had time. A database error is never one, even when
// its driver says timeout: that stays the store failure it is.
//
// A provider wait refused because its slot comes after the download's own
// time limit is the limit, not the provider: this waits for the limit to
// end ctx and reports false, so the symbol isn't failed and the download
// ends with the time-limit text naming it.
func providerTimedOut(ctx context.Context, err error) bool {
	if ctx.Err() == nil && marketdata.AtTaskLimit(ctx, err, errDownloadTimeLimit) {
		<-ctx.Done()
	}
	if ctx.Err() != nil {
		return false
	}
	var stored *repository.StoreError
	if errors.As(err, &stored) {
		return false
	}
	return marketdata.IsProviderTimeout(err)
}

func failDownload(rsp *servicePb.DownloadHistoricalDataResponse, err error) {
	rsp.Status = servicePb.DownloadHistoricalDataResponse_STATUS_ERROR
	rsp.Error = err.Error()
	var barsStatus *financeapi.BarsStatusError
	if errors.As(err, &barsStatus) {
		rsp.ProviderStatus = barsStatus.ProviderStatus()
	}
	var dividendFetch *financeapi.DividendFetchError
	if errors.As(err, &dividendFetch) {
		rsp.DividendFailure = dividendFetch.DividendFailureProto()
	}
	var dividend *repository.DividendError
	if errors.As(err, &dividend) {
		invalid := &financeapi.DividendFetchError{Provider: dividend.Provider, Symbol: dividend.Symbol, Kind: financeapi.DividendInvalid}
		rsp.DividendFailure = invalid.DividendFailureProto()
	}
}

// barsStatusError is the named STATUS_ERROR when the provider answered a
// bars request for symbol with an HTTP error status after its bounded
// retries, or nil. The status and the number of tries are logged; the
// provider's own text never is, because it can carry the response body.
func barsStatusError(symbol, provider, step string, err error) error {
	var status *marketdata.AlpacaBarsError
	if !errors.As(err, &status) {
		return nil
	}
	log.Error().Str("symbol", symbol).Str("provider", provider).Str("step", step).
		Int("status", status.HTTPCode).Int("tries", status.Tries).
		Msg("historical data download failed: the provider answered with an error status")
	return &financeapi.BarsStatusError{Provider: provider, Symbol: symbol, Status: status.HTTPCode}
}

// failSymbol reports a symbol as STATUS_ERROR. Create fails on that status.
// After a block cancels the download, an in-flight symbol fails only because
// of that cancel, so it is not reported and stays enabled for the retry.
func failSymbol(ctx context.Context, sender *downloadSender,
	rsp *servicePb.DownloadHistoricalDataResponse, err error,
) {
	if ctx.Err() != nil {
		return
	}
	failDownload(rsp, err)
	sender.send(rsp)
}

func (fs *MarketdataServer) downloadSymbol(
	ctx context.Context,
	sender *downloadSender,
	symbol string,
	start time.Time,
	end *time.Time,
	provider string,
	assetRepo repository.Asset,
	ohlcRepo repository.OHLC,
	bars supplier.Marketdata,
) *marketdata.BlockedError {
	rsp := &servicePb.DownloadHistoricalDataResponse{
		Symbol: symbol,
		Status: servicePb.DownloadHistoricalDataResponse_STATUS_OK,
	}

	asset, err := fs.marketdata.GetAsset(ctx, symbol)
	if err != nil {
		if errors.Is(err, marketdata.ErrNoISIN) {
			// A retry can't help: the provider can't identify the
			// symbol. The cause stays in the log.
			log.Error().Err(redact.URLError(err)).Str("symbol", symbol).Str("step", "get asset").
				Msg("historical data download failed: no ISIN")
			failSymbol(ctx, sender, rsp, noISINError(symbol))
			return nil
		}
		if providerTimedOut(ctx, err) {
			failSymbol(ctx, sender, rsp, &marketdata.TimeoutError{Provider: metadataProvider, Symbol: symbol})
			return nil
		}
		failSymbol(ctx, sender, rsp, internalDownloadError(symbol, "get asset", err))
		return nil
	}
	// The provider's ticker becomes the stored asset, the bars' key, and the
	// symbol in a refused-bar sentence, so it is checked like a request one.
	if !ticker.Valid(asset.GetSymbol()) {
		log.Warn().Str("symbol", symbol).Str("provider_symbol", ticker.Logged(asset.GetSymbol())).
			Msg("provider returned a ticker outside the symbol pattern")
		failSymbol(ctx, sender, rsp, downloadFailedError(symbol))
		return nil
	}

	if err := assetRepo.Store(ctx, asset.Symbol, asset.Name); err != nil {
		failSymbol(ctx, sender, rsp, internalDownloadError(symbol, "store asset", err))
		return nil
	}

	storeSymbol := asset.Symbol
	// The provider may answer for another ticker than the one asked, such as
	// a renamed company's current one (FB answered as META), and the bars are
	// then fetched and stored under that ticker. It is named as a later
	// ticker, so the bars are never read without a word. Another spelling of
	// the asked ticker (aapl as AAPL, BRK-B for BRKB) is the asked ticker.
	answeredAs := ""
	if !sameTicker(asset.Symbol, symbol) {
		answeredAs = asset.Symbol
	}
	// A provider that knows it would ask for nothing (Alpaca, when no
	// completed session is in the range or its 15-minute cap on recent data
	// empties it) answers with no bars that say nothing about the ticker.
	// The symbol has nothing to store yet, so it is neither fetched nor
	// looked up under a later ticker.
	if nothingToFetch(bars, start, end) {
		log.Info().Str("symbol", symbol).Str("provider", provider).
			Msg("historical data download skipped: nothing completed to fetch in the range yet")
		sender.send(rsp)
		return nil
	}
	ohlcs, err := askedTickerBars(ctx, bars, symbol, provider, storeSymbol, start, end)
	if err != nil {
		var blocked *marketdata.BlockedError
		if errors.As(err, &blocked) {
			return blocked
		}
		failSymbol(ctx, sender, rsp, err)
		return nil
	}
	if len(ohlcs) > 0 && answeredAs != "" {
		rsp.LaterTicker = &servicePb.DownloadHistoricalDataResponse_LaterTicker{Symbol: answeredAs}
	}
	if len(ohlcs) == 0 {
		// A download that already ended looks up no later ticker. Like the
		// lookup's own ctx check below, the symbol is not reported; the
		// download returns ctx.Err().
		if ctx.Err() != nil {
			return nil
		}
		// The provider named another ticker for the symbol and has no bars
		// under it. Another later ticker would contradict the one it named,
		// and a response names one, so the symbol fails instead of guessing.
		if answeredAs != "" {
			log.Warn().Str("symbol", symbol).Str("provider_symbol", answeredAs).
				Msg("provider answered for another ticker that has no bars")
			failSymbol(ctx, sender, rsp, noPriceDataError(symbol))
			return nil
		}
		fetched, fetchErr := fs.fetchBarsViaLaterTicker(ctx, symbol, start, end, bars, assetRepo)
		if fetchErr != nil {
			var blocked *marketdata.BlockedError
			if errors.As(fetchErr, &blocked) {
				return blocked
			}
			// Only the no-bars sentinel selects the "no price data for …
			// between …" sentence. A failed lookup, search, GetOHLC or store
			// under another ticker, or a DB error in Resolve or ListAliases,
			// says nothing about the prices, so it is logged and the user gets
			// the generic download failure.
			if errors.Is(fetchErr, errNoPriceData) {
				failSymbol(ctx, sender, rsp, noPriceDataError(symbol))
				return nil
			}
			if errors.Is(fetchErr, repository.ErrAmbiguousSymbol) {
				failSymbol(ctx, sender, rsp, ambiguousSymbolError(provider, symbol))
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}
			if named := barsStatusError(symbol, provider, "find later ticker", fetchErr); named != nil {
				failSymbol(ctx, sender, rsp, named)
				return nil
			}
			failSymbol(ctx, sender, rsp, internalDownloadError(symbol, "find later ticker", fetchErr))
			return nil
		}
		storeSymbol = fetched.symbol
		ohlcs = fetched.bars
		// The client records this on the backtest and shows it, so the
		// later ticker is never used without a word. No day is sent: the
		// provider doesn't say when the ticker changed.
		rsp.LaterTicker = &servicePb.DownloadHistoricalDataResponse_LaterTicker{Symbol: fetched.symbol}
	}

	// The same overlap and re-scale rule as ingest, in one locked transaction
	// per symbol: a refused bar or a DB error stores none of them, and stored
	// days after a split are re-scaled instead of left next to new ones. Only
	// the refused-bar sentence goes to the client as is. The asset row above
	// and a later ticker's asset and asset_symbol rows are written outside
	// this transaction and stay when the bars roll back; a retry finds them
	// and stores the bars again.
	fetch := prefetchedBars(ctx, bars, storeSymbol, start, end, ohlcs)
	if err := ohlcRepo.SyncSource(ctx, provider, storeSymbol, start, end, fetch, bars); err != nil {
		var blocked *marketdata.BlockedError
		if errors.As(err, &blocked) {
			return blocked
		}
		var invalid *repository.InvalidPriceError
		var stored *repository.StoreError
		var status *marketdata.AlpacaBarsError
		var dividendFetch *financeapi.DividendFetchError
		var dividend *repository.DividendError
		switch {
		case errors.As(err, &dividendFetch), errors.As(err, &dividend):
			// Both are whole sentences naming the provider, the symbol and
			// the cause, and nothing of the symbol was stored. failDownload
			// sends the cause as dividend_failure too.
		case providerTimedOut(ctx, err):
			// A refetch under the symbol lock ran out of time. It ends before
			// the transaction's idle timeout, so it is told as the provider's
			// delay, not as a failure to store.
			err = &marketdata.TimeoutError{Provider: provider, Symbol: symbol}
		case errors.As(err, &invalid):
		case errors.Is(err, repository.ErrNoBars):
			err = noPriceDataError(symbol)
		case errors.As(err, &stored):
			err = internalDownloadError(symbol, "store bars", stored.Err)
		case errors.As(err, &status):
			err = barsStatusError(symbol, provider, "refetch bars", err)
		default:
			err = internalDownloadError(symbol, "refetch bars", err)
		}
		failSymbol(ctx, sender, rsp, err)
		return nil
	}

	sender.send(rsp)
	return nil
}

// missingPriceDownloadError is the STATUS_ERROR when the provider sent a
// bar with no price anywhere in the range, even after the retry. The
// bars are not stored and no later ticker is tried: the ticker has bars,
// with a gap. The sentence names the asked symbol, not the provider's, and
// DownloadFinancialData reads its days for the create's missing-session
// error (pkg/backtest/universe.md §7.5). Nil when err is something else.
func missingPriceDownloadError(symbol, provider string, err error) error {
	var missing *marketdata.MissingPriceError
	if !errors.As(err, &missing) {
		return nil
	}
	return &marketdata.MissingPriceError{Provider: provider, Symbol: symbol, Days: missing.Days}
}

// emptyRangeSupplier is a provider that can tell, before a request, that a
// fetch from start to end would ask it for nothing.
type emptyRangeSupplier interface {
	NothingToFetch(start time.Time, end *time.Time) bool
}

var _ emptyRangeSupplier = (*marketdata.AlpacaBars)(nil)

// nothingToFetch reports that bars would ask its provider for nothing from
// start to end, so an empty answer is not the provider's "no bars".
func nothingToFetch(bars supplier.Marketdata, start time.Time, end *time.Time) bool {
	ranged, ok := bars.(emptyRangeSupplier)
	return ok && ranged.NothingToFetch(start, end)
}

// askedTickerBars fetches the bars of the asked ticker. Empty bars with a
// nil error are the provider's answer that it has none (an empty answer,
// or ErrNoBars such as Yahoo's Not Found or Stooq's No data). That is the
// only result that opens the later-ticker lookup: a rename shows up as no
// bars, and nothing else does. Every other failure is returned for the
// symbol to fail with; a *marketdata.BlockedError is returned as is.
func askedTickerBars(ctx context.Context, bars supplier.Marketdata, symbol, provider, storeSymbol string,
	start time.Time, end *time.Time,
) ([]*pb.OHLC, error) {
	ohlcs, err := bars.GetOHLC(ctx, storeSymbol, start, end)
	if err == nil && len(ohlcs) > 0 {
		return ohlcs, nil
	}
	// A failed request (HTTP 401, a 429 after the provider's own retries,
	// 5xx, a refused connection, a timeout or a cancelled download) says
	// nothing about whether the ticker has prices. It fails the symbol at
	// once: no retry and no later ticker.
	firstMissing := missingPriceDownloadError(symbol, provider, err)
	if err != nil && !noBarsAnswer(err) && firstMissing == nil {
		return nil, askedTickerFailure(ctx, symbol, provider, err)
	}
	// Yahoo sometimes answers a listed symbol (BK) with no data, and a bar
	// with no price gets one more ask too (universe.md §7.5). Ask once more
	// before treating the ticker as renamed or missing.
	ohlcs, err = retryOHLC(ctx, bars, storeSymbol, start, end)
	if err == nil && len(ohlcs) > 0 {
		return ohlcs, nil
	}
	// The first ask had bars with a gap and the retry has none. The two
	// answers disagree, so this is not a rename: the symbol fails with the
	// first ask's missing days and no later ticker is looked up.
	if firstMissing != nil && (err == nil || noBarsAnswer(err)) {
		return nil, firstMissing
	}
	if err == nil {
		return nil, nil
	}
	var blocked *marketdata.BlockedError
	if errors.As(err, &blocked) {
		return nil, blocked
	}
	// A session with no price fails the symbol after the one retry; a
	// later ticker can't fill it.
	if refused := missingPriceDownloadError(symbol, provider, err); refused != nil {
		return nil, refused
	}
	if noBarsAnswer(err) {
		return nil, nil
	}
	// The retry failed; we don't know whether the symbol has prices, so
	// report the failure, not "no price data".
	if named := barsStatusError(symbol, provider, "get ohlc", err); named != nil {
		return nil, named
	}
	return nil, internalDownloadError(symbol, "get ohlc", err)
}

// askedTickerFailure is the error a failed first ask for the asked ticker
// fails the symbol with: the provider's block, its timeout, its HTTP error
// status, or the generic download failure with the cause in the log.
func askedTickerFailure(ctx context.Context, symbol, provider string, err error) error {
	var blocked *marketdata.BlockedError
	if errors.As(err, &blocked) {
		return blocked
	}
	if providerTimedOut(ctx, err) {
		return &marketdata.TimeoutError{Provider: provider, Symbol: symbol}
	}
	if named := barsStatusError(symbol, provider, "get ohlc", err); named != nil {
		return named
	}
	return internalDownloadError(symbol, "get ohlc", err)
}

// ohlcRetryDelay is the pause before asking a provider again after it
// returned no bars or a bar with no price.
const ohlcRetryDelay = 250 * time.Millisecond

// retryOHLC asks for the bars a second time after a short pause.
func retryOHLC(ctx context.Context, bars supplier.Marketdata, symbol string,
	start time.Time, end *time.Time,
) ([]*pb.OHLC, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(ohlcRetryDelay):
	}
	return bars.GetOHLC(ctx, symbol, start, end)
}

// prefetchedBars is the fetch Sync uses for one download. The bars already
// fetched for start through end serve any request inside that span (Sync
// asks from the last stored day for an incremental fetch); anything else,
// such as a refetch from an earlier stored day, goes to the provider.
func prefetchedBars(ctx context.Context, bars supplier.Marketdata, symbol string, start time.Time, end *time.Time,
	fetched []*pb.OHLC,
) repository.FetchBars {
	return func(from time.Time, to *time.Time) ([]*pb.OHLC, error) {
		if !withinSpan(start, end, from, to) {
			return bars.GetOHLC(ctx, symbol, from, to)
		}
		out := make([]*pb.OHLC, 0, len(fetched))
		for _, b := range fetched {
			day := b.GetTimestamp().AsTime().UTC().Format(time.DateOnly)
			if day < from.UTC().Format(time.DateOnly) {
				continue
			}
			if to != nil && day > to.UTC().Format(time.DateOnly) {
				continue
			}
			out = append(out, b)
		}
		return out, nil
	}
}

// withinSpan reports whether the trading days from through to lie inside
// start through end. A nil end is open (through today).
func withinSpan(start time.Time, end *time.Time, from time.Time, to *time.Time) bool {
	day := func(t time.Time) string { return t.UTC().Format(time.DateOnly) }
	if day(from) < day(start) {
		return false
	}
	if end == nil {
		return true
	}
	return to != nil && day(*to) <= day(*end)
}

type fetchedBars struct {
	symbol string
	bars   []*pb.OHLC
}

// fetchBarsViaLaterTicker loads bars under another ticker of the same instrument
// when the asked ticker has none. Yahoo keeps a renamed company's history only
// under the current ticker. Candidates come from asset_symbol, then from a
// search by the instrument's identity. Bars are stored under the ticker that
// returned them; asset_symbol gains that ticker when it was not stored yet.
//
// Only a no-bars answer (ErrNoBars or an empty slice) moves on to the next
// candidate. A failed search, GetOHLC, GetAsset, asset store or alias write
// returns its error and no bars, so the symbol fails instead of being
// reported as having no prices.
func (fs *MarketdataServer) fetchBarsViaLaterTicker(
	ctx context.Context,
	symbol string,
	start time.Time,
	end *time.Time,
	bars supplier.Marketdata,
	assetRepo repository.Asset,
) (*fetchedBars, error) {
	instruments := repository.Instruments{Conn: fs.pgx}
	inst, err := instruments.Resolve(ctx, symbol, start)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, noPriceDataError(symbol)
		}
		return nil, err
	}

	candidates, searchErr, err := laterTickerCandidates(ctx, instruments, fs.marketdata, inst.ISIN, symbol)
	if err != nil {
		return nil, err
	}
	// A failed search may have missed the current ticker, so we don't know
	// which ticker holds the prices. Fail rather than go on with the aliases.
	if searchErr != nil {
		return nil, fmt.Errorf("search for the current ticker of %s (%s) failed: %w", symbol, inst.ISIN, searchErr)
	}
	if len(candidates) == 0 {
		return nil, noPriceDataError(symbol)
	}

	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		ohlcs, err := bars.GetOHLC(ctx, candidate, start, end)
		// A cancelled download is not "no price data", whatever GetOHLC said.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err != nil {
			var blocked *marketdata.BlockedError
			if errors.As(err, &blocked) {
				return nil, blocked
			}
			if !noBarsAnswer(err) {
				return nil, err
			}
			continue
		}
		if len(ohlcs) == 0 {
			continue
		}

		// GetAsset has no typed "no such asset" error, so any error fails
		// the symbol, as it does for the asked ticker in downloadSymbol.
		a, err := fs.marketdata.GetAsset(ctx, candidate)
		if err != nil {
			return nil, fmt.Errorf("get asset for later ticker %s: %w", candidate, err)
		}
		if err := assetRepo.Store(ctx, a.Symbol, a.Name); err != nil {
			return nil, fmt.Errorf("store asset for later ticker %s: %w", candidate, err)
		}

		laterFrom := start
		if end != nil {
			laterFrom = end.AddDate(0, 0, 1)
		} else {
			laterFrom = time.Now().UTC().AddDate(0, 0, 1)
		}
		if err := instruments.EnsureLaterAlias(ctx, inst.ISIN, symbol, candidate, laterFrom); err != nil {
			return nil, fmt.Errorf("record later ticker %s for %s: %w", candidate, inst.ISIN, err)
		}
		return &fetchedBars{symbol: candidate, bars: ohlcs}, nil
	}
	return nil, noPriceDataError(symbol)
}

// laterTickerCandidates lists the instrument's other tickers: asset_symbol
// aliases, then the provider's search by ISIN. searchErr is the search
// failure and err is a failed alias read; the caller fails on either.
func laterTickerCandidates(
	ctx context.Context,
	instruments repository.Instruments,
	md supplier.Marketdata,
	isin, asked string,
) (candidates []string, searchErr, err error) {
	asked = strings.TrimSpace(asked)
	seen := map[string]struct{}{asked: {}}
	candidates = make([]string, 0)

	aliases, err := instruments.ListAliases(ctx, isin)
	if err != nil {
		return nil, nil, err
	}
	for _, alias := range aliases {
		symbol := strings.TrimSpace(alias.Symbol)
		if !candidateSymbol(isin, symbol) {
			continue
		}
		if _, ok := seen[symbol]; ok {
			continue
		}
		seen[symbol] = struct{}{}
		candidates = append(candidates, symbol)
	}

	found, searchErr := md.SearchAsset(ctx, isin)
	if searchErr != nil {
		log.Info().Err(redact.URLError(searchErr)).Str("isin", isin).Msg("search for current ticker failed")
	} else {
		for _, a := range found {
			symbol := strings.TrimSpace(a.Symbol)
			if !candidateSymbol(isin, symbol) {
				continue
			}
			// Only a hit with this exact ISIN is the same instrument. A hit
			// without an ISIN (an ETF or fund quote) could be another
			// company, and its prices must not be stored for this one.
			if a.ISIN != isin {
				continue
			}
			if _, ok := seen[symbol]; ok {
				continue
			}
			seen[symbol] = struct{}{}
			candidates = append(candidates, symbol)
		}
	}
	return candidates, searchErr, nil
}

// candidateSymbol reports whether a later-ticker candidate may be fetched.
// Bars and the alias are stored under it and a refused bar names it, so a
// stored alias or a search result outside the ticker pattern is skipped.
func candidateSymbol(isin, symbol string) bool {
	if symbol == "" {
		return false
	}
	if ticker.Valid(symbol) {
		return true
	}
	log.Warn().Str("isin", isin).Str("candidate", ticker.Logged(symbol)).Msg("skipping a later ticker outside the symbol pattern")
	return false
}

func (fs *MarketdataServer) DownloadHistoricalData(req *servicePb.DownloadHistoricalDataRequest, serverStream servicePb.Marketdata_DownloadHistoricalDataServer) error {
	ctx := serverStream.Context()
	if err := requireTradingEditor(ctx); err != nil {
		return err
	}
	// Every symbol is echoed in a response, so all are checked before any
	// download starts. The first invalid one is named by position only.
	symbols := req.GetSymbols()
	for index, symbol := range symbols {
		if !ticker.Valid(symbol) {
			return status.Error(codes.InvalidArgument, ticker.InvalidMessage(index+1, len(symbols)))
		}
	}

	symbols, err := downloadSymbols(symbols)
	if err != nil {
		return err
	}

	provider, err := marketdata.NormalizeProvider(req.GetProvider())
	if err != nil {
		return err
	}
	profilesFile := environment.GetTradingProfilesFile()
	if err := marketdata.ValidatePriceSource(provider, req.GetProfile(), profilesFile); err != nil {
		return err
	}
	bars, err := marketdata.OHLCClient(provider, req.GetProfile(), profilesFile, fs.marketdata)
	if err != nil {
		return err
	}

	start := internal_pb.DateToTime(req.GetStartDate())
	var end *time.Time
	if req.GetEndDate() != nil {
		e := internal_pb.DateToTime(req.GetEndDate())
		end = &e
	}

	assetRepo := repository.Asset{Conn: fs.pgx}
	ohlcRepo := repository.OHLC{Conn: fs.pgx}

	// The time limit ends the whole download. A symbol stored before it stays
	// stored; one still in flight rolls back and is named in the timeout.
	limit, err := environment.GetFinanceTaskTimeLimit()
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	ctx, stopAtLimit := marketdata.WithTaskLimit(ctx, limit, errDownloadTimeLimit)
	defer stopAtLimit()
	// The first blocked symbol or failed Send cancels the rest. Workers that
	// already started finish their symbol, so the error is still returned once.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var blockedOnce sync.Once
	var blocked *marketdata.BlockedError

	sender := newDownloadSender(serverStream, cancel)
	var wg sync.WaitGroup
	assetCh := make(chan string, downloadWorkers)
	for range downloadWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for symbol := range assetCh {
				if ctx.Err() != nil {
					continue
				}
				if err := fs.downloadSymbol(ctx, sender, symbol, start, end, provider, assetRepo, ohlcRepo, bars); err != nil {
					blockedOnce.Do(func() {
						blocked = err
						cancel()
					})
				}
			}
		}()
	}

feed:
	for _, symbol := range symbols {
		select {
		case assetCh <- symbol:
		case <-ctx.Done():
			break feed
		}
	}
	close(assetCh)
	wg.Wait()

	if blocked != nil {
		return status.Error(codes.FailedPrecondition, blocked.Error())
	}
	if err := sender.sendErr(); err != nil {
		return err
	}
	if errors.Is(context.Cause(ctx), errDownloadTimeLimit) {
		if symbol := sender.firstUnreported(symbols); symbol != "" {
			log.Warn().Str("provider", provider).Str("symbol", symbol).Dur("limit", limit).
				Msg("download stopped at its time limit")
			return status.Error(codes.DeadlineExceeded, (&marketdata.TimeLimitError{Symbol: symbol, Limit: limit}).Error())
		}
		return nil
	}
	return ctx.Err()
}

func (fs *MarketdataServer) GetOHLC(ctx context.Context, req *servicePb.GetOHLCRequest) (*servicePb.GetOHLCResponse, error) {
	symbols := req.GetSymbols()
	if len(symbols) == 0 {
		return nil, status.Error(codes.InvalidArgument, "symbols is required")
	}
	if req.GetAsOf() == nil {
		return nil, status.Error(codes.InvalidArgument, "as_of is required")
	}
	provider, err := marketdata.NormalizeProvider(req.GetProvider())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	limit := int(req.GetLimit())
	ohlcRepo := repository.OHLC{Conn: fs.pgx}
	bars, err := ohlcRepo.List(ctx, provider, symbols, req.GetAsOf().AsTime(), limit)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "symbols:") || strings.Contains(msg, "limit:") {
			return nil, status.Error(codes.InvalidArgument, msg)
		}
		return nil, fmt.Errorf("list ohlc: %w", err)
	}
	return &servicePb.GetOHLCResponse{Bars: bars}, nil
}
