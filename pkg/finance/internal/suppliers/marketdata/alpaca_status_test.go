package marketdata

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	"github.com/stretchr/testify/require"
)

const (
	statusTestKey    = "AKSTATUSTESTKEY"
	statusTestSecret = "statustestsecretvalue"
	statusTestBody   = "detail-only-in-the-body"
)

// alpacaStatusServer answers every bars request with status and body, and
// counts the requests. A private limiter keeps a 429 from pausing the
// other tests' Alpaca fetches.
func alpacaStatusServer(t *testing.T, status int, header http.Header, body string) (*AlpacaBars, *atomic.Int32, string) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		for name, values := range header {
			w.Header()[name] = values
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	now := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)
	return &AlpacaBars{
		opts: marketdata.ClientOpts{
			APIKey: statusTestKey, APISecret: statusTestSecret, BaseURL: srv.URL,
		},
		now:     func() time.Time { return now },
		limiter: newRequestLimiterEvery(time.Millisecond),
	}, &hits, srv.URL
}

func statusTestFetch(t *testing.T, bars *AlpacaBars) error {
	t.Helper()
	start := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)
	ohlcs, err := bars.GetOHLC(context.Background(), "XYZ", start, &end)
	require.Error(t, err)
	require.Nil(t, ohlcs)
	return err
}

// requireNoLeak checks the message carries none of the response body, the
// request URL, or the key and secret.
func requireNoLeak(t *testing.T, err error, srvURL string) {
	t.Helper()
	for _, secret := range []string{statusTestBody, srvURL, "/v2/stocks", statusTestKey, statusTestSecret} {
		require.NotContains(t, err.Error(), secret)
	}
}

// A 422 is Alpaca finding the request invalid. It fails at once with the
// named error: no SDK retry and no second request with an earlier end.
func TestAlpacaGetOHLC422FailsAtOnceWithTheNamedError(t *testing.T) {
	t.Parallel()
	bodies := map[string]string{
		"alpaca json": `{"code":42210000,"message":"` + statusTestBody + `"}`,
		"plain text":  statusTestBody,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bars, hits, srvURL := alpacaStatusServer(t, http.StatusUnprocessableEntity, nil, body)

			err := statusTestFetch(t, bars)

			require.Equal(t, int32(1), hits.Load(), "a 422 is sent once")
			require.ErrorIs(t, err, ErrAlpacaBarsRejected)
			var barsErr *AlpacaBarsError
			require.ErrorAs(t, err, &barsErr)
			require.Equal(t, http.StatusUnprocessableEntity, barsErr.HTTPCode)
			require.Equal(t, 1, barsErr.Tries)
			require.EqualError(t, err, "alpaca rejected the bars request for XYZ as invalid (HTTP 422 Unprocessable Entity)")
			requireNoLeak(t, err, srvURL)
		})
	}
}

// A 429 or any 5xx is sent again alpacaBarsRetries times and no more:
// foreverbull's transport is the one retry layer, the SDK's is off. The
// last status is the error, named with the number of tries.
func TestAlpacaGetOHLCRetriesA429OrA5xxABoundedNumberOfTimes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusTooManyRequests, "alpaca bars for XYZ failed: HTTP 429 Too Many Requests after 4 tries"},
		{http.StatusInternalServerError, "alpaca bars for XYZ failed: HTTP 500 Internal Server Error after 4 tries"},
		{http.StatusBadGateway, "alpaca bars for XYZ failed: HTTP 502 Bad Gateway after 4 tries"},
		{http.StatusServiceUnavailable, "alpaca bars for XYZ failed: HTTP 503 Service Unavailable after 4 tries"},
		{http.StatusGatewayTimeout, "alpaca bars for XYZ failed: HTTP 504 Gateway Timeout after 4 tries"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			// Retry-After 1 keeps the three waits at a second each.
			header := http.Header{"Retry-After": {"1"}}
			bars, hits, srvURL := alpacaStatusServer(t, tc.status, header, `{"message":"`+statusTestBody+`"}`)

			err := statusTestFetch(t, bars)

			require.Equal(t, int32(1+3), hits.Load(), "sent once and retried 3 times")
			require.NotErrorIs(t, err, ErrAlpacaBarsRejected)
			var barsErr *AlpacaBarsError
			require.ErrorAs(t, err, &barsErr)
			require.Equal(t, tc.status, barsErr.HTTPCode)
			require.Equal(t, 1+alpacaBarsRetries, barsErr.Tries)
			require.EqualError(t, err, tc.want)
			requireNoLeak(t, err, srvURL)
		})
	}
}

// scriptedAlpaca answers the n-th request (from 1) with answer. A private
// limiter keeps a 429 from pausing the other tests' Alpaca fetches.
func scriptedAlpaca(t *testing.T, answer func(n int32, w http.ResponseWriter, r *http.Request)) (*AlpacaBars, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer(hits.Add(1), w, r)
	}))
	t.Cleanup(srv.Close)
	now := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)
	return &AlpacaBars{
		opts:    marketdata.ClientOpts{APIKey: statusTestKey, APISecret: statusTestSecret, BaseURL: srv.URL},
		now:     func() time.Time { return now },
		limiter: newRequestLimiterEvery(time.Millisecond),
	}, &hits
}

func writeStatus(w http.ResponseWriter, status int, retryAfter string) {
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"message":"` + statusTestBody + `"}`))
}

// alpacaBarPage is one page of XYZ bars with one bar on day, and token as
// the next page token ("" for the last page).
func alpacaBarPage(day time.Time, token string) string {
	next := "null"
	if token != "" {
		next = `"` + token + `"`
	}
	return `{"bars":{"XYZ":[{"t":"` + day.Format(time.RFC3339) +
		`","o":10,"h":11,"l":9,"c":10.5,"v":100,"n":1,"vw":10}]},"next_page_token":` + next + `}`
}

// The wait before a retry is the Retry-After Alpaca sent.
func TestAlpacaGetOHLCWaitsTheRetryAfterBeforeARetry(t *testing.T) {
	t.Parallel()
	day := time.Date(2024, 1, 2, 5, 0, 0, 0, time.UTC)
	bars, hits := scriptedAlpaca(t, func(n int32, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			writeStatus(w, http.StatusTooManyRequests, "2")
			return
		}
		_, _ = w.Write([]byte(alpacaBarPage(day, "")))
	})
	start := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)

	began := time.Now()
	ohlcs, err := bars.GetOHLC(context.Background(), "XYZ", start, &end)

	require.NoError(t, err)
	require.Len(t, ohlcs, 1)
	require.Equal(t, int32(2), hits.Load())
	require.GreaterOrEqual(t, time.Since(began), 2*time.Second)
}

// Cancelling the ctx ends the wait before a retry at once.
func TestAlpacaGetOHLCWaitBeforeARetryEndsWithTheCtx(t *testing.T) {
	t.Parallel()
	answered := make(chan struct{})
	bars, hits := scriptedAlpaca(t, func(n int32, w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, http.StatusServiceUnavailable, "20")
		if n == 1 {
			close(answered)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-answered
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)

	began := time.Now()
	_, err := bars.GetOHLC(ctx, "XYZ", start, &end)

	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(began), 5*time.Second)
	require.Equal(t, int32(1), hits.Load())
}

// A Retry-After that would end after the caller's deadline is not waited
// for: the fetch fails at once with the 429, not with a timeout.
func TestAlpacaGetOHLCFailsWithThe429WhenRetryAfterOutlastsTheDeadline(t *testing.T) {
	t.Parallel()
	bars, hits := scriptedAlpaca(t, func(_ int32, w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, http.StatusTooManyRequests, "20")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)

	began := time.Now()
	_, err := bars.GetOHLC(ctx, "XYZ", start, &end)

	require.Less(t, time.Since(began), time.Second, "no wait was started")
	require.Equal(t, int32(1), hits.Load())
	require.NotErrorIs(t, err, context.DeadlineExceeded)
	var barsErr *AlpacaBarsError
	require.ErrorAs(t, err, &barsErr)
	require.Equal(t, http.StatusTooManyRequests, barsErr.HTTPCode)
	require.Equal(t, 1, barsErr.Tries)
	require.EqualError(t, err, "alpaca bars for XYZ failed: HTTP 429 Too Many Requests")
}

// Tries counts the sends of the failing page's request: an earlier page
// that came back is not a try.
func TestAlpacaGetOHLCTriesCountsRetriesNotPages(t *testing.T) {
	t.Parallel()
	day := time.Date(2024, 1, 2, 5, 0, 0, 0, time.UTC)
	var tokens []string
	var mu sync.Mutex
	bars, hits := scriptedAlpaca(t, func(n int32, w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, r.URL.Query().Get("page_token"))
		mu.Unlock()
		if n == 1 {
			_, _ = w.Write([]byte(alpacaBarPage(day, "page2")))
			return
		}
		writeStatus(w, http.StatusServiceUnavailable, "1")
	})
	start := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)

	_, err := bars.GetOHLC(context.Background(), "XYZ", start, &end)

	require.Equal(t, int32(1+1+3), hits.Load(), "one page, then the second page sent 1+3 times")
	require.Equal(t, []string{"", "page2", "page2", "page2", "page2"}, tokens)
	var barsErr *AlpacaBarsError
	require.ErrorAs(t, err, &barsErr)
	require.Equal(t, 1+alpacaBarsRetries, barsErr.Tries)
	require.EqualError(t, err, "alpaca bars for XYZ failed: HTTP 503 Service Unavailable after 4 tries")
}

// Any other error status is sent once and named by its status.
func TestAlpacaGetOHLCOtherStatusFailsOnceNamingTheStatus(t *testing.T) {
	t.Parallel()
	bars, hits, srvURL := alpacaStatusServer(t, http.StatusForbidden, nil, `{"message":"`+statusTestBody+`"}`)

	err := statusTestFetch(t, bars)

	require.Equal(t, int32(1), hits.Load())
	require.NotErrorIs(t, err, ErrAlpacaBarsRejected)
	require.EqualError(t, err, "alpaca bars for XYZ failed: HTTP 403 Forbidden")
	requireNoLeak(t, err, srvURL)
}

// A shared pause set by another caller's 429 that outlasts the deadline is
// not waited for either: the fetch fails at once with the last status.
func TestAlpacaGetOHLCFailsWithTheStatusWhenTheSharedPauseOutlastsTheDeadline(t *testing.T) {
	t.Parallel()
	var bars *AlpacaBars
	bars, hits := scriptedAlpaca(t, func(_ int32, w http.ResponseWriter, _ *http.Request) {
		// Another download's 429 pauses Alpaca for everyone for 20s.
		bars.limiter.pause(20 * time.Second)
		writeStatus(w, http.StatusServiceUnavailable, "1")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)

	began := time.Now()
	_, err := bars.GetOHLC(ctx, "XYZ", start, &end)

	require.Less(t, time.Since(began), time.Second, "no wait was started")
	require.Equal(t, int32(1), hits.Load())
	require.NotErrorIs(t, err, context.DeadlineExceeded)
	require.EqualError(t, err, "alpaca bars for XYZ failed: HTTP 503 Service Unavailable")
}

// A wait that ends just before the deadline, so the retry's send runs into
// it, fails with the last status seen, not with the deadline.
func TestAlpacaGetOHLCFailsWithTheLastStatusWhenTheDeadlineEndsARetry(t *testing.T) {
	t.Parallel()
	bars, hits := scriptedAlpaca(t, func(n int32, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			writeStatus(w, http.StatusServiceUnavailable, "1")
			return
		}
		<-r.Context().Done() // the retry hangs until the deadline
	})
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)

	_, err := bars.GetOHLC(ctx, "XYZ", start, &end)

	require.Equal(t, int32(2), hits.Load())
	require.NotErrorIs(t, err, context.DeadlineExceeded)
	var barsErr *AlpacaBarsError
	require.ErrorAs(t, err, &barsErr)
	require.Equal(t, http.StatusServiceUnavailable, barsErr.HTTPCode)
	require.Equal(t, 1, barsErr.Tries, "the one send that answered")
	require.EqualError(t, err, "alpaca bars for XYZ failed: HTTP 503 Service Unavailable")
}

// A caller's cancel with no deadline during a retry's send stays a cancel.
func TestAlpacaGetOHLCCancelDuringARetryStaysACancel(t *testing.T) {
	t.Parallel()
	retried := make(chan struct{})
	bars, hits := scriptedAlpaca(t, func(n int32, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			writeStatus(w, http.StatusServiceUnavailable, "1")
			return
		}
		close(retried)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-retried
		cancel()
	}()
	start := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)

	_, err := bars.GetOHLC(ctx, "XYZ", start, &end)

	require.ErrorIs(t, err, context.Canceled)
	var barsErr *AlpacaBarsError
	require.NotErrorAs(t, err, &barsErr)
	require.Equal(t, int32(2), hits.Load())
}

// A wait past the task's own time limit is the limit, not the provider:
// AtTaskLimit still reads it, so the download names the limit.
func TestAlpacaGetOHLCWaitPastTheTaskLimitIsTheLimit(t *testing.T) {
	t.Parallel()
	limitHit := errors.New("task limit")
	bars, hits := scriptedAlpaca(t, func(_ int32, w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, http.StatusTooManyRequests, "20")
	})
	ctx, cancel := WithTaskLimit(context.Background(), 2*time.Second, limitHit)
	defer cancel()
	start := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)

	_, err := bars.GetOHLC(ctx, "XYZ", start, &end)

	require.Equal(t, int32(1), hits.Load())
	require.True(t, AtTaskLimit(ctx, err, limitHit), "%v", err)
	var barsErr *AlpacaBarsError
	require.NotErrorAs(t, err, &barsErr)
}
