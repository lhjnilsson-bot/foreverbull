package marketdata

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	"github.com/stretchr/testify/require"
)

var errNoBarsInTest = errors.New("no bars")

// arrivalTolerance absorbs scheduling jitter between the client's slot and
// the server seeing the request; it is well under testRequestInterval.
const (
	testRequestInterval = 100 * time.Millisecond
	arrivalTolerance    = 25 * time.Millisecond
)

// arrivals records when a fake provider saw each request.
type arrivals struct {
	mu    sync.Mutex
	times []time.Time
}

func (a *arrivals) add() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.times = append(a.times, time.Now())
}

func (a *arrivals) sorted() []time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := slices.Clone(a.times)
	slices.SortFunc(out, func(x, y time.Time) int { return x.Compare(y) })
	return out
}

func fakeProvider(t *testing.T, body []byte, contentType string, seen *arrivals) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen.add()
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Two downloads that run at once share the provider's rate: their requests
// together arrive at least one interval apart, not one interval apart each.
func TestRateLimitIsSharedAcrossConcurrentDownloads(t *testing.T) {
	t.Parallel()
	start := time.Date(2021, 1, 4, 0, 0, 0, 0, time.UTC)
	end := time.Date(2021, 1, 8, 0, 0, 0, 0, time.UTC)
	stooqCSV, err := os.ReadFile("testdata/spy.us.csv")
	require.NoError(t, err)
	alpacaJSON, err := os.ReadFile("testdata/alpaca_aapl_split_adjusted.json")
	require.NoError(t, err)
	yahooJSON, err := os.ReadFile("testdata/yahoo_chart_null_bars.json")
	require.NoError(t, err)

	cases := []struct {
		provider string
		body     []byte
		mime     string
		client   func(t *testing.T, url string, limiter *requestLimiter) getOHLC
	}{
		{"stooq", stooqCSV, "text/csv", func(t *testing.T, url string, limiter *requestLimiter) getOHLC {
			t.Helper()
			client, err := NewStooqClient(WithStooqBaseURL(url))
			require.NoError(t, err)
			client.limiter = limiter
			return client.GetOHLC
		}},
		{"yahoo", yahooJSON, "application/json", func(_ *testing.T, url string, limiter *requestLimiter) getOHLC {
			return (&YahooClient{chartURL: url + "/", limiter: limiter}).GetOHLC
		}},
		{"alpaca", alpacaJSON, "application/json", func(_ *testing.T, url string, limiter *requestLimiter) getOHLC {
			return (&AlpacaBars{
				opts:    marketdata.ClientOpts{APIKey: "key", APISecret: "secret", BaseURL: url},
				limiter: limiter,
			}).GetOHLC
		}},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			t.Parallel()
			seen := &arrivals{}
			srv := fakeProvider(t, tc.body, tc.mime, seen)
			limiter := newRequestLimiterEvery(testRequestInterval)

			// Each download builds its own client, as DownloadHistoricalData
			// does per request; only the limiter is shared.
			const downloads, symbolsEach = 2, 3
			var wg sync.WaitGroup
			errs := make(chan error, downloads*symbolsEach)
			for range downloads {
				fetch := tc.client(t, srv.URL, limiter)
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range symbolsEach {
						_, err := fetch(context.Background(), "SPY", start, &end)
						errs <- err
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				require.NoError(t, err)
			}

			times := seen.sorted()
			require.Len(t, times, downloads*symbolsEach)
			for index := 1; index < len(times); index++ {
				gap := times[index].Sub(times[index-1])
				require.GreaterOrEqual(t, gap, testRequestInterval-arrivalTolerance,
					"request %d came %s after the one before", index, gap)
			}
		})
	}
}

type getOHLC = func(context.Context, string, time.Time, *time.Time) ([]*pb.OHLC, error)

// A 429 to one download slows the other too: no request reaches the provider
// before the Retry-After has passed, and the throttled one is sent again.
func TestRateLimit429BacksOffEveryDownload(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("testdata/spy.us.csv")
	require.NoError(t, err)
	var mu sync.Mutex
	var throttledAt time.Time
	after := make([]time.Time, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if throttledAt.IsZero() {
			throttledAt = time.Now()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		after = append(after, time.Now())
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	limiter := newRequestLimiterEvery(10 * time.Millisecond)
	start := time.Date(2021, 1, 4, 0, 0, 0, 0, time.UTC)
	end := time.Date(2021, 1, 8, 0, 0, 0, 0, time.UTC)
	first, err := NewStooqClient(WithStooqBaseURL(srv.URL))
	require.NoError(t, err)
	first.limiter = limiter
	second, err := NewStooqClient(WithStooqBaseURL(srv.URL))
	require.NoError(t, err)
	second.limiter = limiter

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, client := range []*StooqClient{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bars, err := client.GetOHLC(context.Background(), "SPY", start, &end)
			if err == nil && len(bars) == 0 {
				err = errNoBarsInTest
			}
			results <- err
		}()
		// The second download asks while the first is being throttled.
		time.Sleep(20 * time.Millisecond)
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, after, 2, "the throttled request is sent again and the other one is served")
	for _, at := range after {
		require.GreaterOrEqual(t, at.Sub(throttledAt), time.Second-arrivalTolerance)
	}
}

func TestRateLimitBackoff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		retryAfter string
		attempt    int
		want       time.Duration
	}{
		{"retry-after seconds", "5", 0, 5 * time.Second},
		{"retry-after at the cap", "30", 2, 30 * time.Second},
		{"retry-after over the cap doubles instead", "3600", 1, 4 * time.Second},
		{"retry-after zero doubles instead", "0", 0, 2 * time.Second},
		{"retry-after date doubles instead", "Wed, 21 Oct 2026 07:28:00 GMT", 0, 2 * time.Second},
		// 18446744074 s overflows to about 0.3 s once multiplied into a
		// time.Duration, so it must be refused as seconds first.
		{"retry-after overflow doubles instead", "18446744074", 2, 8 * time.Second},
		{"retry-after negative doubles instead", "-5", 0, 2 * time.Second},
		{"first 429", "", 0, 2 * time.Second},
		{"third 429", "", 2, 8 * time.Second},
		{"capped", "", 6, 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			header := http.Header{}
			if tc.retryAfter != "" {
				header.Set("Retry-After", tc.retryAfter)
			}
			require.Equal(t, tc.want, rateLimitBackoff(header, tc.attempt))
		})
	}
}

// A request waiting for its slot gives up when its ctx ends, so a cancelled
// or timed-out download doesn't sit in the queue.
func TestRateLimitWaitStopsWithTheContext(t *testing.T) {
	t.Parallel()
	limiter := newRequestLimiterEvery(time.Hour)
	require.NoError(t, limiter.wait(context.Background()))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	began := time.Now()
	require.ErrorIs(t, limiter.wait(ctx), context.DeadlineExceeded)
	require.Less(t, time.Since(began), time.Second)
}

// A wait whose slot comes after the ctx deadline is refused at once, not
// slept on, and the refusal is a DeadlineExceeded that keeps its deadline.
func TestRateLimitWaitRefusesASlotPastTheDeadline(t *testing.T) {
	t.Parallel()
	limiter := newRequestLimiterEvery(time.Hour)
	require.NoError(t, limiter.wait(context.Background()))

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	deadline, _ := ctx.Deadline()
	began := time.Now()
	err := limiter.wait(ctx)
	require.Less(t, time.Since(began), 100*time.Millisecond, "refused up front")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var refused *WaitRefusedError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, deadline, refused.Deadline)
}

// Waits that time out or are cancelled give their slots back: after 100 of
// them at the Stooq rate, the next real fetch still gets the next slot, not
// one 50 s away.
func TestRateLimitCancelledWaitsGiveTheirSlotsBack(t *testing.T) {
	t.Parallel()
	for name, endWait := range map[string]func() (context.Context, context.CancelFunc){
		"timed out": func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), time.Millisecond)
		},
		"cancelled": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(time.Millisecond, cancel)
			return ctx, cancel
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			limiter := newRequestLimiter(stooqRequestsPerSecond)
			require.NoError(t, limiter.wait(context.Background()))
			for range 100 {
				ctx, cancel := endWait()
				require.Error(t, limiter.wait(ctx))
				cancel()
			}
			began := time.Now()
			require.NoError(t, limiter.wait(context.Background()))
			require.Less(t, time.Since(began), 2*limiter.interval, "the next fetch waits one slot at most")
		})
	}
}

// A 429 pause also holds callers that were already waiting for a slot:
// none of them sends before the pause ends.
func TestRateLimitPauseHoldsCallersAlreadyWaiting(t *testing.T) {
	t.Parallel()
	limiter := newRequestLimiter(yahooRequestsPerSecond)
	pauseFor := time.Second
	var mu sync.Mutex
	sent := make([]time.Time, 0, 10)
	errs := make([]error, 0, 10)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := limiter.wait(context.Background())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			sent = append(sent, time.Now())
		}()
	}
	time.Sleep(50 * time.Millisecond)
	limiter.pause(pauseFor) // a 429 seen while nine callers still wait
	pausedAt := time.Now()
	wg.Wait()
	require.Empty(t, errs)

	beforePause := 0
	for _, at := range sent {
		if at.Before(pausedAt) {
			beforePause++
			continue
		}
		require.GreaterOrEqual(t, at.Sub(pausedAt), pauseFor-arrivalTolerance, "a waiting caller sent during the pause")
	}
	require.Equal(t, 1, beforePause, "only the slot taken before the 429 went out")
}

// Every client of a provider uses that provider's one limiter, whichever
// download built it.
func TestProviderClientsShareOneLimiter(t *testing.T) {
	t.Parallel()
	first, err := NewStooqClient()
	require.NoError(t, err)
	second, err := NewStooqClient()
	require.NoError(t, err)
	require.Same(t, limiterFor("stooq"), first.limiter)
	require.Same(t, first.limiter, second.limiter)

	alpaca, err := NewAlpacaBars("key", "secret")
	require.NoError(t, err)
	require.Same(t, limiterFor("alpaca"), alpaca.limiter)
	require.NotSame(t, limiterFor("yahoo"), limiterFor("stooq"))

	require.Equal(t, 200*time.Millisecond, newRequestLimiter(yahooRequestsPerSecond).interval)
	require.Equal(t, 500*time.Millisecond, newRequestLimiter(stooqRequestsPerSecond).interval)
	require.Equal(t, 333333333*time.Nanosecond, newRequestLimiter(alpacaRequestsPerSecond).interval)
	require.Panics(t, func() { limiterFor("unknown") })
}

// A real Yahoo request whose slot comes after its deadline fails at once
// with the refusal, read as a provider timeout; nothing is sent.
func TestYahooRequestRefusedPastItsDeadlineIsAProviderTimeout(t *testing.T) {
	t.Parallel()
	seen := &arrivals{}
	srv := fakeProvider(t, []byte(`{}`), "application/json", seen)
	limiter := newRequestLimiterEvery(time.Hour)
	require.NoError(t, limiter.wait(context.Background()))
	client := &YahooClient{chartURL: srv.URL + "/", limiter: limiter}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	end := time.Date(2021, 1, 8, 0, 0, 0, 0, time.UTC)
	_, err := client.GetOHLC(ctx, "AAPL", time.Date(2021, 1, 4, 0, 0, 0, 0, time.UTC), &end)
	var refused *WaitRefusedError
	require.ErrorAs(t, err, &refused)
	require.True(t, IsProviderTimeout(err))
	require.Empty(t, seen.sorted(), "nothing reaches the provider")
}
