package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	"github.com/stretchr/testify/require"
)

// hangingServer never answers until the request's own ctx ends, so a fetch
// only returns early when the caller's ctx reaches the HTTP request. started
// receives once per request.
func hangingServer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv, started
}

// cancelAfterStart cancels the fetch's ctx once the server has the request
// and checks the fetch returns context.Canceled well before the client
// timeout (30s), so the provider fetch stopped with the request.
func cancelAfterStart(t *testing.T, started <-chan struct{},
	fetch func(ctx context.Context) ([]*pb.OHLC, error),
) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-started:
			cancel()
		case <-time.After(10 * time.Second):
		}
	}()
	began := time.Now()
	_, err := fetch(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(began), 5*time.Second)
}

func TestYahooGetOHLCStopsWhenCtxIsCancelled(t *testing.T) {
	t.Parallel()
	srv, started := hangingServer(t)
	client := &YahooClient{chartURL: srv.URL + "/"}
	start := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	cancelAfterStart(t, started, func(ctx context.Context) ([]*pb.OHLC, error) {
		return client.GetOHLC(ctx, "XYZ", start, nil)
	})
}

func TestStooqGetOHLCStopsWhenCtxIsCancelled(t *testing.T) {
	t.Parallel()
	srv, started := hangingServer(t)
	client, err := NewStooqClient(WithStooqBaseURL(srv.URL))
	require.NoError(t, err)
	start := time.Date(2021, 1, 4, 0, 0, 0, 0, time.UTC)
	cancelAfterStart(t, started, func(ctx context.Context) ([]*pb.OHLC, error) {
		return client.GetOHLC(ctx, "AAPL", start, nil)
	})
}

func TestAlpacaGetOHLCStopsWhenCtxIsCancelled(t *testing.T) {
	t.Parallel()
	srv, started := hangingServer(t)
	// A 30s client timeout, so only the ctx can end the request in time.
	bars := &AlpacaBars{opts: marketdata.ClientOpts{
		APIKey: "key", APISecret: "secret", BaseURL: srv.URL,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}}
	start := time.Date(2020, 8, 27, 0, 0, 0, 0, time.UTC)
	cancelAfterStart(t, started, func(ctx context.Context) ([]*pb.OHLC, error) {
		return bars.GetOHLC(ctx, "AAPL", start, nil)
	})
}

// GetAsset and the ISIN lookup it makes use the caller's ctx too.
func TestYahooGetAssetStopsWhenCtxIsCancelled(t *testing.T) {
	t.Parallel()
	srv, started := hangingServer(t)
	client := &YahooClient{}
	client.httpClient = &http.Client{Timeout: 30 * time.Second, Transport: rewriteTo(srv.URL)}
	cancelAfterStart(t, started, func(ctx context.Context) ([]*pb.OHLC, error) {
		_, err := client.GetAsset(ctx, "XYZ")
		return nil, err
	})
}

// rewriteTo sends every request to target, so GetAsset's fixed Yahoo URL
// reaches the test server.
func rewriteTo(target string) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		out := req.Clone(req.Context())
		u := *req.URL
		u.Scheme = "http"
		u.Host = target[len("http://"):]
		out.URL = &u
		out.Host = u.Host
		return http.DefaultTransport.RoundTrip(out)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Alpaca's fetch budget bounds every send and every wait between them, so
// a fetch can't outlast Sync's idle timeout under the symbol lock. A wait
// that would run past the budget isn't started: the fetch fails at once
// with the status, not with a timeout.
// Not parallel: it changes alpacaFetchBudget.
func TestAlpacaGetOHLCStopsRetryingAtTheBudget(t *testing.T) {
	old := alpacaFetchBudget
	alpacaFetchBudget = 300 * time.Millisecond
	t.Cleanup(func() { alpacaFetchBudget = old })
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	bars := &AlpacaBars{
		opts:    marketdata.ClientOpts{APIKey: "key", APISecret: "secret", BaseURL: srv.URL},
		limiter: newRequestLimiterEvery(time.Millisecond),
	}
	began := time.Now()
	_, err := bars.GetOHLC(context.Background(), "AAPL", time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC), nil)
	require.NotErrorIs(t, err, context.DeadlineExceeded)
	var barsErr *AlpacaBarsError
	require.ErrorAs(t, err, &barsErr)
	require.Equal(t, http.StatusTooManyRequests, barsErr.HTTPCode)
	require.Equal(t, int32(1), hits.Load(), "the 2s backoff is past the 300ms budget")
	require.Less(t, time.Since(began), 300*time.Millisecond)
}
