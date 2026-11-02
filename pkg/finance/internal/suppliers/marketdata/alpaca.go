package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AlpacaBars downloads daily bars with a profile's key. Asset metadata stays on Yahoo.
type AlpacaBars struct {
	opts marketdata.ClientOpts
	// now replaces the clock, so tests can fix today's session.
	now func() time.Time
	// limiter spaces the Alpaca requests. NewAlpacaBars sets the
	// process-wide Alpaca limiter, so every download shares one rate.
	limiter *requestLimiter
	// tradingURL is the trading API origin for asset status. Empty is the
	// paper host. Tests point it at an httptest server.
	tradingURL string
}

// AlpacaPaperHost is the paper trading host. Asset status is read from it.
// The paper and live hosts serve the same catalog; a live-only key can be
// refused here, and a missing status is not inactive.
const AlpacaPaperHost = "https://paper-api.alpaca.markets"

// alpacaTradingURL is AlpacaPaperHost. Tests replace AlpacaBars.tradingURL.
const alpacaTradingURL = AlpacaPaperHost

// NewAlpacaBars builds a bars client. An empty key or secret fails before any request.
func NewAlpacaBars(apiKey, apiSecret string) (*AlpacaBars, error) {
	if strings.TrimSpace(apiKey) == "" || strings.TrimSpace(apiSecret) == "" {
		return nil, errors.New("alpaca price source requires an api key and secret")
	}
	return &AlpacaBars{opts: marketdata.ClientOpts{
		APIKey:    apiKey,
		APISecret: apiSecret,
	}, limiter: limiterFor("alpaca")}, nil
}

// alpacaTimeout is the SDK's own default request timeout, kept for the
// per-call client below.
const alpacaTimeout = 10 * time.Second

// alpacaRecentDataLimit is Alpaca's free-plan limit on recent SIP data: a
// bars request whose end is within the last 15 minutes is refused with a
// 422. Every bars request ends at least this long before now.
const alpacaRecentDataLimit = 15 * time.Minute

// alpacaFetchBudget bounds one GetOHLC, retries and pages included. It stays
// below Sync's 90s idle-in-transaction timeout. Tests shorten it.
var alpacaFetchBudget = 60 * time.Second

// clientWith returns an SDK client whose requests carry ctx, telling
// recorder each answer when it is set. The SDK takes no context, so each
// call gets its own client with a transport that attaches ctx to every
// request: a cancelled download stops the Alpaca fetch. Each send, retries
// included, waits for a slot of the shared Alpaca limiter under that ctx,
// so the wait counts against the fetch budget.
//
// The SDK's own retry is off: it retries only a 429 and a 500, sleeps a
// fixed second without the ctx, and each try gets a new client timeout.
// limitedTransport is the one retry layer instead: a 429 or any 5xx is sent
// again alpacaBarsRetries times, waiting Retry-After or the backoff under
// the ctx, and a wait that would end after the fetch budget isn't started,
// so the fetch fails with the provider's status, not a timeout. The client
// timeout bounds each send, not the sends and the waits together.
func (a *AlpacaBars) clientWith(ctx context.Context, recorder *statusRecorder) *marketdata.Client {
	opts := a.opts
	base := http.DefaultTransport
	timeout := alpacaTimeout
	if opts.HTTPClient != nil {
		if opts.HTTPClient.Transport != nil {
			base = opts.HTTPClient.Transport
		}
		timeout = opts.HTTPClient.Timeout
	}
	limiter := a.limiter
	if limiter == nil {
		limiter = limiterFor("alpaca")
	}
	limited := newLimitedTransport("alpaca", limiter, base, alpacaBarsRetries)
	limited.retryServerErrors = true
	limited.keepFailedStatus = true
	limited.attemptTimeout = timeout
	if recorder != nil {
		limited.observe = recorder.observe
	}
	opts.HTTPClient = &http.Client{Transport: &ctxTransport{ctx: ctx, base: limited}}
	// A negative limit turns the SDK's retry off; zero would be its ten.
	opts.RetryLimit = -1
	return marketdata.NewClient(opts)
}

// alpacaBarsRetries is how many times a bar request is sent again after a
// 429 or any 5xx, so each is sent at most 1+3 times. It is
// rateLimitRetries, like the other providers. Every other status, a 422
// included, is sent once.
const alpacaBarsRetries = rateLimitRetries

// barsClient is client with a recorder of the answers Alpaca sent, so a
// failed GetBars is reported from the status alone. The SDK's error carries
// the response body, which never reaches the caller.
func (a *AlpacaBars) barsClient(ctx context.Context) (*marketdata.Client, *statusRecorder) {
	recorder := &statusRecorder{}
	return a.clientWith(ctx, recorder), recorder
}

// statusRecorder keeps the last answer of one GetBars call and how many
// times that page's request was sent. A new page starts the count again,
// so pagination is not counted as tries.
type statusRecorder struct {
	mu     sync.Mutex
	tries  int
	code   int
	status string
}

func (r *statusRecorder) observe(attempt int, resp *http.Response) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tries = attempt + 1
	r.code = resp.StatusCode
	r.status = resp.Status
}

func (r *statusRecorder) last() (code int, status string, tries int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.code, r.status, r.tries
}

// ErrAlpacaBarsRejected matches an AlpacaBarsError for HTTP 422: Alpaca
// found the bars request invalid. Sending it again can't change the answer,
// so it is never retried, with a shorter end or otherwise.
var ErrAlpacaBarsRejected = errors.New("alpaca rejected the bars request")

// AlpacaBarsError is an HTTP error status from Alpaca's bars endpoint, after
// any retries. Error names the symbol and the status, and the number of
// tries when there was more than one. It does not include the response
// body, the request URL, or the key.
//
// It deliberately has no Unwrap: the SDK error it replaces carries the
// response body, and that text must not be reachable through the chain.
type AlpacaBarsError struct {
	Symbol string
	// HTTPCode is the status code, such as 503.
	HTTPCode int
	// HTTPStatus is the status line, such as "503 Service Unavailable".
	HTTPStatus string
	// Tries is how many times the failing request was sent, retries
	// included and other pages not.
	Tries int
}

func (e *AlpacaBarsError) Error() string {
	if e.HTTPCode == http.StatusUnprocessableEntity {
		return fmt.Sprintf("alpaca rejected the bars request for %s as invalid (HTTP %s)", e.Symbol, e.HTTPStatus)
	}
	if e.Tries > 1 {
		return fmt.Sprintf("alpaca bars for %s failed: HTTP %s after %d tries", e.Symbol, e.HTTPStatus, e.Tries)
	}
	return fmt.Sprintf("alpaca bars for %s failed: HTTP %s", e.Symbol, e.HTTPStatus)
}

func (e *AlpacaBarsError) Is(target error) bool {
	return target == ErrAlpacaBarsRejected && e.HTTPCode == http.StatusUnprocessableEntity
}

// barsFailure is the error of a failed GetBars. An error status becomes an
// AlpacaBarsError and the SDK's text, which carries the body, is only
// logged. A request that got no answer, even after an earlier try did, is
// returned as before, so a timeout stays a timeout.
func barsFailure(symbol string, recorder *statusRecorder, err error) error {
	code, status, tries := recorder.last()
	var urlErr *url.Error
	if code < http.StatusMultipleChoices || errors.As(err, &urlErr) {
		return fmt.Errorf("error getting bars: %w", err)
	}
	log.Warn().Str("symbol", symbol).Int("status", code).Int("tries", tries).
		Msg("alpaca bars request returned a status")
	if status == "" {
		status = fmt.Sprintf("%d %s", code, http.StatusText(code))
	}
	return &AlpacaBarsError{Symbol: symbol, HTTPCode: code, HTTPStatus: status, Tries: tries}
}

// ctxTransport sends each request with ctx, so cancelling ctx aborts it.
type ctxTransport struct {
	ctx  context.Context //nolint:containedctx // the SDK has no per-request ctx
	base http.RoundTripper
}

func (t *ctxTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req.WithContext(t.ctx)) //nolint:wrapcheck // passed through as is
}

func (a *AlpacaBars) GetAsset(context.Context, string) (*pb.Asset, error) {
	return nil, errors.New("alpaca price source does not serve asset metadata")
}

func (a *AlpacaBars) SearchAsset(context.Context, string) ([]*pb.Asset, error) {
	return nil, errors.New("alpaca price source does not serve asset search")
}

func (a *AlpacaBars) GetIndex(context.Context, string) ([]*pb.Asset, error) {
	return nil, errors.New("alpaca price source does not serve index membership")
}

// alpacaAdjustment is the corporate-action basis of stored Alpaca bars.
// The SDK sends adjustment=raw when the field is empty, so a 4:1 split
// shows as a -75% day. Split-adjusted matches Yahoo's close (split, no
// dividends), the default provider; dividends are not adjusted, so a run
// stays a price return as on Yahoo (pkg/finance/profiles.md, adjustment).
const alpacaAdjustment = marketdata.AdjustmentSplit

// alpacaDailyBars names every field that changes a stored price: daily
// bars and the split adjustment. Neither is left to an SDK default.
func alpacaDailyBars(start, end time.Time) marketdata.GetBarsRequest {
	return marketdata.GetBarsRequest{
		TimeFrame:  marketdata.OneDay,
		Adjustment: alpacaAdjustment,
		Start:      start,
		End:        end,
	}
}

func (a *AlpacaBars) GetOHLC(ctx context.Context, symbol string, start time.Time, end *time.Time) ([]*pb.OHLC, error) {
	now := clockNow(a.now)
	endAt, ok := alpacaFetchEnd(start, end, now)
	if !ok {
		return []*pb.OHLC{}, nil
	}
	// Bars are returned for the asked ticker; only the request uses Alpaca's form.
	symbol = alpacaSymbol(symbol)
	// A 429 or a 5xx is sent again alpacaBarsRetries times (clientWith),
	// each send with its own timeout, so one fetch could outlast Sync's idle
	// timeout when it runs under the symbol lock. Every send and every wait
	// runs under this ctx, so the budget bounds the whole fetch.
	ctx, cancel := context.WithTimeout(ctx, alpacaFetchBudget)
	defer cancel()
	md, recorder := a.barsClient(ctx)
	// A 422 fails here with ErrAlpacaBarsRejected. It is not asked again
	// with an earlier end: the end is already the last completed session
	// and at least alpacaRecentDataLimit before now.
	bars, err := md.GetBars(symbol, alpacaDailyBars(start, endAt))
	if err != nil {
		return nil, barsFailure(symbol, recorder, err)
	}

	last := lastFetchedSession(end, now)
	ohlcs := make([]*pb.OHLC, 0, len(bars))
	for _, bar := range bars {
		// Alpaca could send today's live bar, or a later day's, past the end.
		if afterLastSession("alpaca", symbol, bar.Timestamp.In(xnysZone), last) {
			continue
		}
		ohlcs = append(ohlcs, &pb.OHLC{
			Open:      bar.Open,
			High:      bar.High,
			Low:       bar.Low,
			Close:     bar.Close,
			Volume:    bar.Volume,
			Timestamp: timestamppb.New(bar.Timestamp),
		})
	}
	return ohlcs, nil
}

// NothingToFetch reports that GetOHLC from start to end would ask Alpaca
// nothing at the current clock: no completed session is in the range, or
// alpacaRecentDataLimit leaves it empty. GetOHLC then returns no bars
// without a request. That empty answer is not Alpaca saying the ticker has
// no prices, so the download checks this first and neither fetches nor
// looks for a later ticker.
func (a *AlpacaBars) NothingToFetch(start time.Time, end *time.Time) bool {
	_, ok := alpacaFetchEnd(start, end, clockNow(a.now))
	return !ok
}

// alpacaFetchEnd is the end of the bars request from start to end at now,
// or false when nothing is left to ask for.
func alpacaFetchEnd(start time.Time, end *time.Time, now time.Time) (time.Time, bool) {
	until := now.UTC()
	if end != nil {
		// A daily bar is stamped at midnight New York, after an end at
		// 00:00 UTC on the same date, so the end date needs the next
		// midnight New York.
		until = inclusiveFetchEnd(*end)
	}
	// Today's unfinished session is never asked for: while the market is
	// open, Alpaca's bar for today has prices and would be stored as final.
	// Alpaca's end is inclusive and a daily bar is stamped at midnight New
	// York, so a fetch that ends at a midnight New York stops just before it.
	limit, ok := completedFetchEnd(start, until, now)
	if !ok {
		return time.Time{}, false
	}
	endAt := until
	if end != nil || limit.Before(until) {
		endAt = limit.Add(-time.Nanosecond)
	}
	// Alpaca's free plan refuses recent SIP data with a 422, so the end is
	// never later than alpacaRecentDataLimit before now. It is set here,
	// before the request: a 422 is not retried with an earlier end.
	if latest := now.Add(-alpacaRecentDataLimit); endAt.After(latest) {
		endAt = latest
	}
	return endAt, endAt.After(start)
}

// AssetStatus is the trading API's asset status, "active" or "inactive".
// Any other answer, including a missing status or a failed request, is an
// error. It is never read as inactive.
func (a *AlpacaBars) AssetStatus(ctx context.Context, symbol string) (string, error) {
	asked := strings.ToUpper(strings.TrimSpace(symbol))
	base := a.tradingURL
	if base == "" {
		base = alpacaTradingURL
	}
	endpoint := strings.TrimRight(base, "/") + "/v2/assets/" + url.PathEscape(alpacaSymbol(symbol))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", lookupTransport("asset status", asked, ctx, err)
	}
	req.Header.Set("APCA-API-KEY-ID", a.opts.APIKey)
	req.Header.Set("APCA-API-SECRET-KEY", a.opts.APISecret)
	resp, err := a.statusClient(ctx).Do(req)
	if err != nil {
		return "", lookupTransport("asset status", asked, ctx, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		log.Warn().Err(err).Str("symbol", asked).Msg("alpaca asset status response couldn't be read")
		return "", fmt.Errorf("alpaca asset status for %s couldn't be read", asked)
	}
	if resp.StatusCode != http.StatusOK {
		return "", lookupStatus("asset status", asked, resp.StatusCode, resp.Status)
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Warn().Err(err).Str("symbol", asked).Msg("alpaca asset status response couldn't be read")
		return "", fmt.Errorf("alpaca asset status for %s couldn't be read", asked)
	}
	status := strings.ToLower(strings.TrimSpace(payload.Status))
	switch status {
	case "active", "inactive":
		return status, nil
	default:
		log.Warn().Str("symbol", asked).Str("status", payload.Status).
			Msg("alpaca asset status response couldn't be read")
		return "", fmt.Errorf("alpaca asset status for %s is %q", asked, payload.Status)
	}
}

// statusClient sends the asset-status request through the Alpaca limiter.
// A 429 is sent again, unlike GetOHLC, whose SDK retries on its own.
func (a *AlpacaBars) statusClient(ctx context.Context) *http.Client {
	base := http.DefaultTransport
	timeout := alpacaTimeout
	if a.opts.HTTPClient != nil {
		if a.opts.HTTPClient.Transport != nil {
			base = a.opts.HTTPClient.Transport
		}
		if a.opts.HTTPClient.Timeout > 0 {
			timeout = a.opts.HTTPClient.Timeout
		}
	}
	limiter := a.limiter
	if limiter == nil {
		limiter = limiterFor("alpaca")
	}
	limited := newLimitedTransport("alpaca", limiter, base, rateLimitRetries)
	return &http.Client{Timeout: timeout, Transport: &ctxTransport{ctx: ctx, base: limited}}
}

var _ supplier.Marketdata = (*AlpacaBars)(nil)
