package marketdata

import (
	"context"
	"errors"
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
	// retryServerErrors sends a 5xx again too, within the same retries.
	retryServerErrors bool
	// keepFailedStatus returns the retryable status at once, instead of
	// waiting, when the wait before the next send would end after the
	// request ctx's deadline. The caller then sees the provider's status,
	// not a timeout.
	keepFailedStatus bool
	// attemptTimeout bounds each send, the reading of its body included,
	// when it is above zero. The waits between sends are bounded by the
	// request ctx only.
	attemptTimeout time.Duration
	// observe, when set, is told each answer and its 0-based attempt.
	observe func(attempt int, resp *http.Response)
}

func newLimitedTransport(provider string, limiter *requestLimiter, base http.RoundTripper, retries int) *limitedTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &limitedTransport{provider: provider, limiter: limiter, base: base, retries: retries}
}

// retryable is an answer this transport sends again.
func (t *limitedTransport) retryable(code int) bool {
	if code == http.StatusTooManyRequests {
		return true
	}
	return t.retryServerErrors && code >= http.StatusInternalServerError && code <= 599
}

func (t *limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Only a request without a body is sent again; the price requests are
	// all GETs.
	retries := t.retries
	if req.Body != nil && req.Body != http.NoBody {
		retries = 0
	}
	ctx := req.Context()
	// last is the status of the last retryable answer, without its body,
	// once a retry is under way.
	var last *http.Response
	for attempt := 0; ; attempt++ {
		if err := t.limiter.wait(ctx); err != nil {
			return t.endedRetry(req, last, err)
		}
		resp, err := t.send(req) //nolint:bodyclose // returned to the caller or closed below
		if err != nil {
			return t.endedRetry(req, last, err)
		}
		if t.observe != nil {
			t.observe(attempt, resp)
		}
		if !t.retryable(resp.StatusCode) {
			return resp, nil
		}
		backoff := rateLimitBackoff(resp.Header, attempt)
		if resp.StatusCode == http.StatusTooManyRequests {
			t.limiter.pause(backoff)
			log.Warn().Str("provider", t.provider).Dur("backoff", backoff).Int("attempt", attempt+1).
				Msg("price provider answered 429, slowing every download to it")
		} else {
			log.Warn().Str("provider", t.provider).Int("status", resp.StatusCode).Dur("backoff", backoff).
				Int("attempt", attempt+1).Msg("price provider answered a server error")
		}
		if attempt >= retries {
			return resp, nil
		}
		waited := time.Now().Add(backoff)
		// The next send also waits out the provider's shared pause, which
		// another caller's 429 may have set past this backoff.
		next := waited
		if paused := t.limiter.paused(); paused.After(next) {
			next = paused
		}
		if deadline, ok := ctx.Deadline(); ok && t.keepFailedStatus && next.After(deadline) {
			if atTaskLimitDeadline(ctx, deadline) {
				// The task's own limit is reported as the limit (AtTaskLimit).
				drainClose(resp)
				return nil, &WaitRefusedError{Deadline: deadline}
			}
			log.Warn().Str("provider", t.provider).Int("status", resp.StatusCode).Time("next", next).
				Msg("price provider's wait runs past the deadline, failing with its status")
			return resp, nil
		}
		last = statusOnly(resp)
		drainClose(resp)
		// A 429 waits out the shared pause in limiter.wait. A 5xx waits its
		// own backoff first.
		if resp.StatusCode != http.StatusTooManyRequests {
			if err := sleepUntil(ctx, time.Now(), waited); err != nil {
				return t.endedRetry(req, last, err)
			}
		}
	}
}

// endedRetry is the result when a wait or a send fails. When keepFailedStatus
// is set and the request's deadline ended a retry, the caller gets the last
// retryable status rather than the deadline, so the provider's cause is
// named. A cancel without a deadline, a send that failed on its own, and the
// task's own time limit keep their error.
func (t *limitedTransport) endedRetry(req *http.Request, last *http.Response, err error) (*http.Response, error) {
	if last == nil || !t.keepFailedStatus {
		return nil, err
	}
	ctx := req.Context()
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, err
	}
	var refused *WaitRefusedError
	switch {
	case errors.As(err, &refused):
		deadline = refused.Deadline
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
	default:
		return nil, err
	}
	if atTaskLimitDeadline(ctx, deadline) {
		return nil, err
	}
	log.Warn().Err(err).Str("provider", t.provider).Int("status", last.StatusCode).
		Msg("price provider's retry ran into the deadline, failing with its last status")
	resp := *last
	resp.Body = http.NoBody
	resp.Request = req
	return &resp, nil
}

// statusOnly is resp's status and header without its body.
func statusOnly(resp *http.Response) *http.Response {
	return &http.Response{
		Status: resp.Status, StatusCode: resp.StatusCode,
		Proto: resp.Proto, ProtoMajor: resp.ProtoMajor, ProtoMinor: resp.ProtoMinor,
		Header: resp.Header.Clone(), ContentLength: 0,
	}
}

func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

// atTaskLimitDeadline reports that deadline is the task's own time limit
// (WithTaskLimit), not a shorter one such as the fetch budget.
func atTaskLimitDeadline(ctx context.Context, deadline time.Time) bool {
	at, ok := ctx.Value(taskLimitKey{}).(time.Time)
	return ok && !deadline.Before(at)
}

// send is one round trip, bounded by attemptTimeout when it is set. The
// timeout ends when the body is closed, so reading it counts.
func (t *limitedTransport) send(req *http.Request) (*http.Response, error) {
	if t.attemptTimeout <= 0 {
		return t.base.RoundTrip(req) //nolint:wrapcheck // passed through as is
	}
	ctx, cancel := context.WithTimeout(req.Context(), t.attemptTimeout)
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err //nolint:wrapcheck // passed through as is
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelOnClose ends a send's timeout when its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err //nolint:wrapcheck // passed through as is
}
