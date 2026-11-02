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
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
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

// alpacaFetchBudget bounds one GetOHLC, retries and pages included. It stays
// below Sync's 90s idle-in-transaction timeout. Tests shorten it.
var alpacaFetchBudget = 60 * time.Second

// client returns an SDK client whose requests carry ctx. The SDK takes no
// context, so each call gets its own client with a transport that attaches
// ctx to every request: a cancelled download stops the Alpaca fetch. Each
// request, the SDK's retries included, waits for a slot of the shared
// Alpaca limiter under that ctx, so the wait counts against the fetch budget.
func (a *AlpacaBars) client(ctx context.Context) *marketdata.Client {
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
	// The SDK retries a 429 itself, so this transport doesn't (0 retries).
	limited := newLimitedTransport("alpaca", limiter, base, 0)
	opts.HTTPClient = &http.Client{Timeout: timeout, Transport: &ctxTransport{ctx: ctx, base: limited}}
	return marketdata.NewClient(opts)
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
		return []*pb.OHLC{}, nil
	}
	endAt := until
	if end != nil || limit.Before(until) {
		endAt = limit.Add(-time.Nanosecond)
	}
	// Bars are returned for the asked ticker; only the request uses Alpaca's form.
	symbol = alpacaSymbol(symbol)
	// The SDK retries 429 and 500 up to ten times, a second apart, each try
	// with its own timeout, so one fetch could outlast Sync's idle timeout
	// when it runs under the symbol lock. A failed request isn't retried, so
	// ending ctx bounds the whole fetch.
	ctx, cancel := context.WithTimeout(ctx, alpacaFetchBudget)
	defer cancel()
	md := a.client(ctx)
	bars, err := md.GetBars(symbol, alpacaDailyBars(start, endAt))
	if err != nil {
		var apiErr *alpaca.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnprocessableEntity {
			bars, err = md.GetBars(symbol, alpacaDailyBars(start, endAt.Add(-15*time.Minute)))
			if err != nil {
				return nil, fmt.Errorf("error getting bars: %w", err)
			}
		} else {
			return nil, fmt.Errorf("error getting bars: %w", err)
		}
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
