package marketdata

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/time/rate"
)

// Request rates per provider, shared by every download and ingest in the
// process. One 10,000-symbol request must not get the broker throttled and
// stop the download for everyone. Alpaca's free market-data plan allows 200
// requests a minute, so 3 a second stays under it. Yahoo and Stooq publish
// no limit; at these rates a 10,000-symbol download takes about an hour on
// Yahoo or Alpaca and about 85 minutes on Stooq, under downloadTimeLimit.
const (
	yahooRequestsPerSecond  = 5
	stooqRequestsPerSecond  = 2
	alpacaRequestsPerSecond = 3
)

// Backoff after a 429. Retry-After wins when it is a number of seconds up to
// maxRateLimitBackoff; otherwise the wait doubles from firstRateLimitBackoff.
const (
	firstRateLimitBackoff = 2 * time.Second
	maxRateLimitBackoff   = 30 * time.Second
)

// rateLimitRetries is how many times a Yahoo, Stooq, or Alpaca asset-status
// request is sent again after a 429. Alpaca bar requests go through the SDK's
// client, so their transport sends each request once and only slows the
// shared limiter down.
const rateLimitRetries = 3

// requestLimiter spaces requests to one provider at least interval apart,
// across every goroutine that holds it. A 429 pauses the provider for
// everyone: callers already waiting for a slot check the pause again when
// their slot comes, and wait it out before they send.
type requestLimiter struct {
	interval time.Duration
	slots    *rate.Limiter

	mu          sync.Mutex
	pausedUntil time.Time
}

func newRequestLimiter(requestsPerSecond int) *requestLimiter {
	return newRequestLimiterEvery(time.Second / time.Duration(requestsPerSecond))
}

// newRequestLimiterEvery allows one request per interval, with no burst.
func newRequestLimiterEvery(interval time.Duration) *requestLimiter {
	return &requestLimiter{interval: interval, slots: rate.NewLimiter(rate.Every(interval), 1)}
}

// providerLimiters holds one limiter per provider for the life of the
// process. The keys are the NormalizeProvider allowlist, so it can't grow.
var providerLimiters = map[string]*requestLimiter{
	"yahoo":  newRequestLimiter(yahooRequestsPerSecond),
	"stooq":  newRequestLimiter(stooqRequestsPerSecond),
	"alpaca": newRequestLimiter(alpacaRequestsPerSecond),
}

// limiterFor is the shared limiter of provider. An unknown provider is a
// programmer error: every client is built for one of the three.
func limiterFor(provider string) *requestLimiter {
	limiter, ok := providerLimiters[provider]
	if !ok {
		panic(fmt.Sprintf("no request limiter for provider %q", provider))
	}
	return limiter
}

// WaitRefusedError is a wait for a request slot that was refused because
// the slot, or the end of a 429 pause, comes after the ctx deadline.
// Nothing was sent and the slot was given back. It is a DeadlineExceeded,
// so callers read it as the provider taking too long, or as the task's own
// time limit when Deadline is that limit. Its text is for the server log
// only; user-facing copy never shows it.
type WaitRefusedError struct {
	Deadline time.Time
}

func (e *WaitRefusedError) Error() string {
	return "the next request slot comes after the deadline"
}

func (e *WaitRefusedError) Unwrap() error {
	return context.DeadlineExceeded
}

// paused is the end of the current 429 pause, or zero.
func (l *requestLimiter) paused() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pausedUntil
}

// wait blocks until the caller's slot, or until ctx ends. Callers reserve
// slots in arrival order. A wait that would end after ctx's deadline is
// refused at once with a WaitRefusedError, and a wait that ends early
// (cancel or deadline) gives its slot back, so the next caller isn't held
// behind slots nobody uses.
func (l *requestLimiter) wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := time.Now()
		if until := l.paused(); until.After(now) {
			if err := sleepUntil(ctx, now, until); err != nil {
				return err
			}
			continue
		}
		reservation := l.slots.ReserveN(now, 1)
		slot := now.Add(reservation.DelayFrom(now))
		if err := sleepUntil(ctx, now, slot); err != nil {
			reservation.Cancel()
			return err
		}
		// A 429 while this caller waited pauses it too: it waits out the
		// pause and takes a new slot after it.
		if l.paused().After(time.Now()) {
			continue
		}
		return nil
	}
}

// sleepUntil waits until at, or refuses at once when at is after ctx's
// deadline. It returns ctx's error when ctx ends first.
func sleepUntil(ctx context.Context, now, at time.Time) error {
	if !at.After(now) {
		return nil
	}
	if deadline, ok := ctx.Deadline(); ok && at.After(deadline) {
		return &WaitRefusedError{Deadline: deadline}
	}
	timer := time.NewTimer(at.Sub(now))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // the caller reads the ctx error as is
	case <-timer.C:
		return nil
	}
}

// pause keeps every caller, including those already waiting, from sending
// before now plus backoff.
func (l *requestLimiter) pause(backoff time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	until := time.Now().Add(backoff)
	if l.pausedUntil.Before(until) {
		l.pausedUntil = until
	}
}

// rateLimitBackoff is the wait after the attempt-th 429 in a row (0-based).
// Retry-After is checked as a number of seconds before it becomes a
// duration, so a huge value can't overflow into a short wait.
func rateLimitBackoff(header http.Header, attempt int) time.Duration {
	if seconds, err := strconv.ParseInt(header.Get("Retry-After"), 10, 64); err == nil &&
		seconds > 0 && seconds <= int64(maxRateLimitBackoff/time.Second) {
		return time.Duration(seconds) * time.Second
	}
	backoff := firstRateLimitBackoff << attempt
	if backoff > maxRateLimitBackoff || backoff <= 0 {
		return maxRateLimitBackoff
	}
	return backoff
}

// limitedTransport sends each request in a slot of the provider's shared
// limiter. The wait uses the request ctx, so the client timeout and the
// caller's cancel bound it like the request itself.
type limitedTransport struct {
	provider string
	limiter  *requestLimiter
	base     http.RoundTripper
	// retries is how many times a 429 is sent again; the last 429 is
	// returned to the caller as it came.
	retries int
}

func newLimitedTransport(provider string, limiter *requestLimiter, base http.RoundTripper, retries int) *limitedTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &limitedTransport{provider: provider, limiter: limiter, base: base, retries: retries}
}

func (t *limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Only a request without a body is sent again; the price requests are
	// all GETs.
	retries := t.retries
	if req.Body != nil && req.Body != http.NoBody {
		retries = 0
	}
	for attempt := 0; ; attempt++ {
		if err := t.limiter.wait(req.Context()); err != nil {
			return nil, err
		}
		resp, err := t.base.RoundTrip(req) //nolint:bodyclose // returned to the caller or closed below
		if err != nil {
			return nil, err //nolint:wrapcheck // passed through as is
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}
		backoff := rateLimitBackoff(resp.Header, attempt)
		t.limiter.pause(backoff)
		log.Warn().Str("provider", t.provider).Dur("backoff", backoff).Int("attempt", attempt+1).
			Msg("price provider answered 429, slowing every download to it")
		if attempt < retries {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
			continue
		}
		return resp, nil
	}
}
