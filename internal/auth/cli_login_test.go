package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"github.com/lhjnilsson/foreverbull/internal/environment"
)

const (
	testRedirect = "http://127.0.0.1:43123/callback"
	testState    = "c3RhdGUtc3RhdGUtc3RhdGUtc3RhdGUtc3RhdGUtc3Q"
)

func testVerifier(seed string) (string, string) {
	verifier := strings.Repeat(seed, 43)[:43]
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func cliValues(challenge string) url.Values {
	return url.Values{
		"client":                {"cli@laptop"},
		"redirect_uri":          {testRedirect},
		"state":                 {testState},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {ScopeWrite},
	}
}

func TestParseCLIRequestAcceptsOnlyTheLoopbackCallback(t *testing.T) {
	_, challenge := testVerifier("a")
	_, err := ParseCLIRequest(cliValues(challenge))
	require.NoError(t, err)

	cases := []struct {
		name  string
		field string
		value string
	}{
		{"localhost host", "redirect_uri", "http://localhost:43123/callback"},
		{"other host", "redirect_uri", "http://203.0.113.5:43123/callback"},
		{"https scheme", "redirect_uri", "https://127.0.0.1:43123/callback"},
		{"wrong path", "redirect_uri", "http://127.0.0.1:43123/other"},
		{"trailing slash", "redirect_uri", "http://127.0.0.1:43123/callback/"},
		{"query", "redirect_uri", "http://127.0.0.1:43123/callback?x=1"},
		{"userinfo", "redirect_uri", "http://a@127.0.0.1:43123/callback"},
		{"no port", "redirect_uri", "http://127.0.0.1/callback"},
		{"privileged port", "redirect_uri", "http://127.0.0.1:80/callback"},
		{"leading zero port", "redirect_uri", "http://127.0.0.1:043123/callback"},
		{"plain method", "code_challenge_method", "plain"},
		{"short challenge", "code_challenge", "abc"},
		{"short state", "state", "abc"},
		{"admin scope", "scope", "admin"},
		{"empty scope", "scope", ""},
		{"no cli prefix", "client", "laptop"},
		{"empty host", "client", "cli@"},
		{"markup in client", "client", "cli@<b>x</b>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := cliValues(challenge)
			values.Set(tc.field, tc.value)
			_, err := ParseCLIRequest(values)
			require.ErrorIs(t, err, ErrBadRequest)
		})
	}
}

func TestCLIScopeIsCappedByRole(t *testing.T) {
	cases := []struct {
		role, requested, want string
		err                   error
	}{
		{RoleAdmin, ScopeWrite, ScopeWrite, nil},
		{RoleEditor, ScopeWrite, ScopeWrite, nil},
		{RoleEditor, ScopeRead, ScopeRead, nil},
		{RoleViewer, ScopeWrite, ScopeRead, nil},
		{RolePending, ScopeWrite, "", ErrForbidden},
		{RoleNone, ScopeRead, "", ErrForbidden},
		{"owner", ScopeRead, "", ErrForbidden},
	}
	for _, tc := range cases {
		got, err := CLIScope(tc.role, tc.requested)
		if tc.err != nil {
			require.ErrorIs(t, err, tc.err, tc.role)
			continue
		}
		require.NoError(t, err, tc.role)
		require.Equal(t, tc.want, got, tc.role)
	}
}

type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(s, format+"\n", args...)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// captureLogs sends the standard logger and zerolog to the sink for the test.
func captureLogs(t *testing.T) *logSink {
	t.Helper()
	sink := &logSink{}
	prevStd := stdlog.Writer()
	stdlog.SetOutput(sink)
	prevZ := zlog.Logger
	zlog.Logger = zerolog.New(sink)
	t.Cleanup(func() {
		stdlog.SetOutput(prevStd)
		zlog.Logger = prevZ
	})
	return sink
}

type cliServer struct {
	t       *testing.T
	handler http.Handler
	cookie  *http.Cookie
}

func newCLIServer(t *testing.T, sink *logSink) (*cliServer, *handlers) {
	t.Helper()
	t.Setenv(environment.AuthEnabled, "true")
	pool := newAuthDB(t)
	require.NoError(t, Migrate(context.Background(), pool))
	mux := http.NewServeMux()
	registerHTTP(mux, pool, sink.logf)
	h := &handlers{
		db: pool, logf: sink.logf, limiter: newFailureLimiter(), now: time.Now,
		cliLimiter: newFailureLimiter(), cliAccountLimiter: newFailureLimiter(),
	}
	return &cliServer{t: t, handler: Protect(pool)(mux)}, h
}

func (s *cliServer) signIn(ctx context.Context, h *handlers, username, role string) string {
	s.t.Helper()
	id, err := CreateEditor(ctx, h.db.(DB), username, "twelve-chars")
	require.NoError(s.t, err)
	_, err = h.db.Exec(ctx, `UPDATE account SET role = $2 WHERE id = $1`, id, role)
	require.NoError(s.t, err)
	value, err := OpenSession(ctx, h.db, id, time.Now())
	require.NoError(s.t, err)
	s.cookie = &http.Cookie{Name: sessionCookie, Value: value}
	return id
}

func (s *cliServer) do(req *http.Request) *httptest.ResponseRecorder {
	s.t.Helper()
	req.RemoteAddr = "127.0.0.1:5555"
	if s.cookie != nil {
		req.AddCookie(s.cookie)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func (s *cliServer) approve(values url.Values, site string) *httptest.ResponseRecorder {
	s.t.Helper()
	form := url.Values{}
	for k, v := range values {
		form[k] = v
	}
	form.Set("decision", "approve")
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/cli/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", site)
	return s.do(req)
}

func (s *cliServer) code(values url.Values) string {
	s.t.Helper()
	rec := s.approve(values, "same-origin")
	require.Equal(s.t, http.StatusSeeOther, rec.Code, rec.Body.String())
	location, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(s.t, err)
	require.Equal(s.t, testRedirect, location.Scheme+"://"+location.Host+location.Path)
	require.Equal(s.t, testState, location.Query().Get("state"))
	code := location.Query().Get("code")
	require.NotEmpty(s.t, code)
	return code
}

func (s *cliServer) exchange(code, verifier, redirect string) *httptest.ResponseRecorder {
	s.t.Helper()
	body, err := json.Marshal(map[string]string{"code": code, "code_verifier": verifier, "redirect_uri": redirect})
	require.NoError(s.t, err)
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/auth/cli/token", bytes.NewReader(body))
	cookie := s.cookie
	s.cookie = nil
	defer func() { s.cookie = cookie }()
	return s.do(req)
}

func (s *cliServer) revoke(token string, all bool) *httptest.ResponseRecorder {
	s.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080"+cliRevokePath,
		strings.NewReader(fmt.Sprintf(`{"all":%t}`, all)))
	req.Header.Set("Authorization", "Bearer "+token)
	cookie := s.cookie
	s.cookie = nil
	defer func() { s.cookie = cookie }()
	return s.do(req)
}

func codeCount(t *testing.T, h *handlers) int {
	t.Helper()
	var count int
	require.NoError(t, h.db.QueryRow(context.Background(), `SELECT COUNT(*) FROM cli_auth_code`).Scan(&count))
	return count
}

// The full flow: page, approve, exchange, use, reuse, logout. No log line
// may carry the code, the verifier, or the token.
func TestCLILoginFullFlowLogsNoSecret(t *testing.T) {
	sink := captureLogs(t)
	server, h := newCLIServer(t, sink)
	ctx := context.Background()
	accountID := server.signIn(ctx, h, "cli-owner", RoleEditor)
	verifier, challenge := testVerifier("v")
	values := cliValues(challenge)

	page := server.do(httptest.NewRequest(http.MethodGet, "http://localhost:8080/cli/authorize?"+values.Encode(), nil))
	require.Equal(t, http.StatusOK, page.Code)
	require.Contains(t, page.Body.String(), "cli@laptop")
	require.Contains(t, page.Body.String(), "<strong>write</strong>")
	require.Equal(t, "DENY", page.Header().Get("X-Frame-Options"))
	require.Zero(t, codeCount(t, h), "a GET never issues a code")

	require.Equal(t, http.StatusForbidden, server.approve(values, "cross-site").Code)
	require.Zero(t, codeCount(t, h))

	code := server.code(values)
	rec := server.exchange(code, verifier, testRedirect)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var issued struct {
		Login     string `json:"login"`
		Token     string `json:"token"`
		Scope     string `json:"scope"`
		Client    string `json:"client"`
		ExpiresAt string `json:"expires_at"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &issued))
	require.Equal(t, ScopeWrite, issued.Scope)
	require.Equal(t, "cli@laptop", issued.Client)
	require.Equal(t, "cli-owner", issued.Login)
	expires, err := time.Parse(time.RFC3339Nano, issued.ExpiresAt)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(cliTokenLifetime), expires, time.Minute)

	account, err := AccountFromBearer(ctx, h.db, issued.Token, time.Now())
	require.NoError(t, err)
	require.Equal(t, accountID, account.ID)

	// A second use of the code fails and revokes the token it issued.
	require.Equal(t, http.StatusBadRequest, server.exchange(code, verifier, testRedirect).Code)
	_, err = AccountFromBearer(ctx, h.db, issued.Token, time.Now())
	require.ErrorIs(t, err, ErrUnauthorized)

	// A fresh sign-in, then logout revokes it.
	verifier2, challenge2 := testVerifier("w")
	code2 := server.code(cliValues(challenge2))
	rec = server.exchange(code2, verifier2, testRedirect)
	require.Equal(t, http.StatusOK, rec.Code)
	var second struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &second))
	require.Equal(t, http.StatusOK, server.revoke(second.Token, false).Code)
	_, err = AccountFromBearer(ctx, h.db, second.Token, time.Now())
	require.ErrorIs(t, err, ErrUnauthorized)
	require.Equal(t, http.StatusUnauthorized, server.revoke(second.Token, false).Code)

	logs := sink.String()
	require.NotEmpty(t, logs)
	for name, secret := range map[string]string{
		"code": code, "code2": code2, "verifier": verifier, "verifier2": verifier2,
		"token": issued.Token, "token2": second.Token,
	} {
		require.NotContains(t, logs, secret, name)
	}
}

var pageTags = regexp.MustCompile(`<[^>]+>`)

// pageText is what a reader sees in the body: tags dropped, entities
// decoded, runs of whitespace as one space.
func pageText(t *testing.T, page string) string {
	t.Helper()
	require.Contains(t, page, "<title>Let fbull sign in?</title>")
	_, body, ok := strings.Cut(page, "<body>")
	require.True(t, ok)
	return strings.Join(strings.Fields(html.UnescapeString(pageTags.ReplaceAllString(body, ""))), " ")
}

func TestCLIAuthorizePageLetsTheFormRedirectToTheListener(t *testing.T) {
	ctx := context.Background()
	sink := captureLogs(t)
	server, h := newCLIServer(t, sink)
	server.signIn(ctx, h, "csp-editor", RoleEditor)
	_, challenge := testVerifier("v")
	page := server.do(httptest.NewRequest(http.MethodGet,
		"http://localhost:8080/cli/authorize?"+cliValues(challenge).Encode(), nil))
	require.Equal(t, http.StatusOK, page.Code)
	origin := strings.TrimSuffix(testRedirect, "/callback")
	require.Regexp(t, `^http://127\.0\.0\.1:[0-9]+$`, origin)
	require.Equal(t,
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self' "+origin+"; frame-ancestors 'none'",
		page.Header().Get("Content-Security-Policy"))

	invalid := server.do(httptest.NewRequest(http.MethodGet, "http://localhost:8080/cli/authorize?client=nope", nil))
	require.Equal(t,
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'",
		invalid.Header().Get("Content-Security-Policy"))
}

func TestCLIAuthorizePageStrings(t *testing.T) {
	sink := captureLogs(t)
	server, h := newCLIServer(t, sink)
	ctx := context.Background()
	_, challenge := testVerifier("v")
	values := cliValues(challenge)
	get := func(query string) string {
		return pageText(t, server.do(httptest.NewRequest(http.MethodGet, "http://localhost:8080/cli/authorize?"+query, nil)).Body.String())
	}

	require.Equal(t, "Sign in to foreverbull first "+
		"fbull is asking for access to your account. Sign in to foreverbull in this browser, then reload this page.",
		get(values.Encode()))
	require.Equal(t, "fbull can't sign in "+
		"This sign-in link isn't valid. Run `fbull login` again.", get("client=nope"))

	server.signIn(ctx, h, "page-editor", RoleEditor)
	require.Equal(t, "Let fbull sign in? "+
		"fbull on laptop is asking for write access as page-editor. "+
		"Approve only if you just ran fbull login on that computer. Approve Deny", get(values.Encode()))

	server.signIn(ctx, h, "page-viewer", RoleViewer)
	require.Contains(t, get(values.Encode()), "fbull on laptop is asking for read access as page-viewer.")

	server.signIn(ctx, h, "page-pending", RolePending)
	require.Equal(t, "fbull can't sign in "+
		"Your account isn't approved yet, so fbull can't sign in with it. "+
		"Ask whoever runs this foreverbull to approve your account, then run `fbull login` again.", get(values.Encode()))
}

func TestCLIExchangeRefusesAWrongVerifierRedirectOrExpiredCode(t *testing.T) {
	sink := captureLogs(t)
	server, h := newCLIServer(t, sink)
	ctx := context.Background()
	server.signIn(ctx, h, "cli-refusals", RoleEditor)
	verifier, challenge := testVerifier("v")
	values := cliValues(challenge)

	wrongVerifier, _ := testVerifier("x")
	code := server.code(values)
	require.Equal(t, http.StatusBadRequest, server.exchange(code, wrongVerifier, testRedirect).Code)
	require.Equal(t, http.StatusBadRequest, server.exchange(code, verifier, testRedirect).Code,
		"a failed attempt still uses the code")

	code = server.code(values)
	require.Equal(t, http.StatusBadRequest, server.exchange(code, verifier, "http://127.0.0.1:43124/callback").Code)

	code = server.code(values)
	_, err := h.db.Exec(ctx, `UPDATE cli_auth_code SET expires_at = now() - interval '1 second' WHERE used_at IS NULL`)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, server.exchange(code, verifier, testRedirect).Code)

	require.Equal(t, http.StatusBadRequest, server.exchange("bm90LWEtY29kZQ", verifier, testRedirect).Code)

	var tokens int
	require.NoError(t, h.db.QueryRow(ctx, `SELECT COUNT(*) FROM api_token`).Scan(&tokens))
	require.Zero(t, tokens)
}

func TestCLICodeExpiresAfterSixtySeconds(t *testing.T) {
	t.Setenv(environment.AuthEnabled, "true")
	pool := newAuthDB(t)
	ctx := context.Background()
	require.NoError(t, Migrate(ctx, pool))
	id, err := CreateEditor(ctx, pool, "cli-ttl", "twelve-chars")
	require.NoError(t, err)
	verifier, challenge := testVerifier("v")
	req, err := ParseCLIRequest(cliValues(challenge))
	require.NoError(t, err)
	now := time.Now()

	code, err := IssueCLICode(ctx, pool, Account{ID: id, Role: RoleEditor}, req, now)
	require.NoError(t, err)
	_, claim, err := ExchangeCLICode(ctx, pool, code, verifier, testRedirect, now.Add(cliCodeTTL))
	require.ErrorIs(t, err, ErrInvalidGrant)
	require.Equal(t, CLIClaim{AccountID: id}, claim, "an expired code is not a first claim")

	code, err = IssueCLICode(ctx, pool, Account{ID: id, Role: RoleEditor}, req, now)
	require.NoError(t, err)
	token, claim, err := ExchangeCLICode(ctx, pool, code, verifier, testRedirect, now.Add(cliCodeTTL-time.Second))
	require.NoError(t, err)
	require.Equal(t, CLIClaim{AccountID: id, First: true}, claim)
	require.NotEmpty(t, token.Secret)
}

func TestCLIViewerGetsReadAndPendingCannotApprove(t *testing.T) {
	sink := captureLogs(t)
	server, h := newCLIServer(t, sink)
	ctx := context.Background()
	verifier, challenge := testVerifier("v")
	values := cliValues(challenge)

	server.signIn(ctx, h, "cli-viewer", RoleViewer)
	page := server.do(httptest.NewRequest(http.MethodGet, "http://localhost:8080/cli/authorize?"+values.Encode(), nil))
	require.Contains(t, page.Body.String(), "<strong>read</strong>")
	rec := server.exchange(server.code(values), verifier, testRedirect)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"scope":"read"`)

	server.signIn(ctx, h, "cli-pending", RolePending)
	page = server.do(httptest.NewRequest(http.MethodGet, "http://localhost:8080/cli/authorize?"+values.Encode(), nil))
	require.Equal(t, http.StatusForbidden, page.Code)
	require.Equal(t, http.StatusForbidden, server.approve(values, "same-origin").Code)
}

func TestCLIAuthorizePageAsksForASignInWithoutASession(t *testing.T) {
	sink := captureLogs(t)
	server, _ := newCLIServer(t, sink)
	_, challenge := testVerifier("v")
	page := server.do(httptest.NewRequest(http.MethodGet,
		"http://localhost:8080/cli/authorize?"+cliValues(challenge).Encode(), nil))
	require.Equal(t, http.StatusOK, page.Code)
	require.Contains(t, page.Body.String(), "Sign in to foreverbull first")
	require.Equal(t, http.StatusUnauthorized, server.approve(cliValues(challenge), "same-origin").Code)
}

func TestCLIExchangeIsRateLimited(t *testing.T) {
	sink := captureLogs(t)
	server, _ := newCLIServer(t, sink)
	verifier, _ := testVerifier("v")
	limited := false
	for range loginIPLimit + 1 {
		rec := server.exchange("bm90LWEtY29kZQ", verifier, testRedirect)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
		require.Equal(t, http.StatusBadRequest, rec.Code)
	}
	require.True(t, limited)
}

func TestCLIReplayedCodesDoNotLockTheAccount(t *testing.T) {
	ctx := context.Background()
	sink := captureLogs(t)
	server, h := newCLIServer(t, sink)
	server.signIn(ctx, h, "cli-replayed", RoleEditor)
	verifier, challenge := testVerifier("v")
	values := cliValues(challenge)
	leaked := server.code(values)
	require.Equal(t, http.StatusOK, server.exchange(leaked, verifier, testRedirect).Code)

	// Each replay comes from its own address, so only the account cap applies.
	exchangeFrom := func(i int, code string) int {
		body, err := json.Marshal(map[string]string{"code": code, "code_verifier": verifier, "redirect_uri": testRedirect})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/auth/cli/token", bytes.NewReader(body))
		req.RemoteAddr = fmt.Sprintf("10.0.%d.%d:5555", i/250, i%250+1)
		rec := httptest.NewRecorder()
		server.handler.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := range loginIPLimit + 5 {
		require.Equal(t, http.StatusBadRequest, exchangeFrom(i, leaked), "replay %d", i)
	}
	require.Equal(t, http.StatusOK, exchangeFrom(loginIPLimit+5, server.code(values)))
}

func TestCLIExchangeRefusesPlainHTTPToARemoteHost(t *testing.T) {
	sink := captureLogs(t)
	server, _ := newCLIServer(t, sink)
	req := httptest.NewRequest(http.MethodPost, "http://fb.example.com/api/auth/cli/token", strings.NewReader(`{}`))
	require.Equal(t, http.StatusForbidden, server.do(req).Code)
	require.True(t, PlainHTTPHost("127.0.0.1:8080"))
	require.True(t, PlainHTTPHost("localhost:8080"))
	require.False(t, PlainHTTPHost("fb.example.com:8080"))
	require.False(t, PlainHTTPHost("127.0.0.1.example.com"))
}

func issueTestCLIToken(t *testing.T, pool DB, username, role string, now time.Time) (string, CLIToken) {
	t.Helper()
	ctx := context.Background()
	id, err := CreateEditor(ctx, pool, username, "twelve-chars")
	require.NoError(t, err)
	if role != RoleEditor {
		_, err = pool.Exec(ctx, `UPDATE account SET role = $2 WHERE id = $1`, id, role)
		require.NoError(t, err)
	}
	verifier, challenge := testVerifier("v")
	req, err := ParseCLIRequest(cliValues(challenge))
	require.NoError(t, err)
	code, err := IssueCLICode(ctx, pool, Account{ID: id, Role: role}, req, now)
	require.NoError(t, err)
	token, _, err := ExchangeCLICode(ctx, pool, code, verifier, testRedirect, now)
	require.NoError(t, err)
	return id, token
}

func TestCLITokenExpiresAfterThirtyDaysOrFourteenIdleDays(t *testing.T) {
	t.Setenv(environment.AuthEnabled, "true")
	pool := newAuthDB(t)
	ctx := context.Background()
	require.NoError(t, Migrate(ctx, pool))
	start := time.Now().UTC().Truncate(time.Second)
	_, token := issueTestCLIToken(t, pool, "cli-expiry", RoleEditor, start)

	// Unused for 14 days is idle.
	_, err := AccountFromBearer(ctx, pool, token.Secret, start.Add(cliTokenIdle))
	require.ErrorIs(t, err, ErrSignInExpired)
	require.ErrorIs(t, err, ErrUnauthorized)

	// Used every 13 days, it still ends at 30 days.
	for day := 13; day < 30; day += 13 {
		_, err = AccountFromBearer(ctx, pool, token.Secret, start.Add(time.Duration(day)*24*time.Hour))
		require.NoError(t, err, day)
	}
	_, err = AccountFromBearer(ctx, pool, token.Secret, start.Add(cliTokenLifetime))
	require.ErrorIs(t, err, ErrSignInExpired)
}

func TestCLITokenRecordsUseAtMostHourly(t *testing.T) {
	t.Setenv(environment.AuthEnabled, "true")
	pool := newAuthDB(t)
	ctx := context.Background()
	require.NoError(t, Migrate(ctx, pool))
	start := time.Now().UTC().Truncate(time.Second)
	_, token := issueTestCLIToken(t, pool, "cli-touch", RoleEditor, start)
	lastUsed := func() time.Time {
		var at time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT last_used_at FROM api_token WHERE id = $1`, token.ID).Scan(&at))
		return at.UTC()
	}

	_, err := AccountFromBearer(ctx, pool, token.Secret, start)
	require.NoError(t, err)
	require.True(t, start.Equal(lastUsed()))
	_, err = AccountFromBearer(ctx, pool, token.Secret, start.Add(59*time.Minute))
	require.NoError(t, err)
	require.True(t, start.Equal(lastUsed()))
	_, err = AccountFromBearer(ctx, pool, token.Secret, start.Add(61*time.Minute))
	require.NoError(t, err)
	require.True(t, start.Add(61*time.Minute).Equal(lastUsed()))
}

func TestSavedTokenStaysWithoutExpiryOrIdleTimeout(t *testing.T) {
	t.Setenv(environment.AuthEnabled, "true")
	pool := newAuthDB(t)
	ctx := context.Background()
	require.NoError(t, Migrate(ctx, pool))
	id, err := CreateEditor(ctx, pool, "saved-owner", "twelve-chars")
	require.NoError(t, err)
	minted, err := MintToken(ctx, pool, Account{ID: id, Role: RoleEditor}, "laptop", "")
	require.NoError(t, err)
	_, err = AccountFromBearer(ctx, pool, minted.Secret, time.Now().Add(365*24*time.Hour))
	require.NoError(t, err)
}

func TestRoleDowngradeRevokesTokensAndSessions(t *testing.T) {
	t.Setenv(environment.AuthEnabled, "true")
	pool := newAuthDB(t)
	ctx := context.Background()
	require.NoError(t, Migrate(ctx, pool))
	now := time.Now()
	id, token := issueTestCLIToken(t, pool, "cli-downgrade", RoleEditor, now)
	session, err := OpenSession(ctx, pool, id, now)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `UPDATE account SET role = 'admin' WHERE id = $1`, id)
	require.NoError(t, err)
	_, err = AccountFromBearer(ctx, pool, token.Secret, now)
	require.NoError(t, err, "an upgrade keeps the token")

	_, err = pool.Exec(ctx, `UPDATE account SET role = 'viewer' WHERE id = $1`, id)
	require.NoError(t, err)
	_, err = AccountFromBearer(ctx, pool, token.Secret, now)
	require.ErrorIs(t, err, ErrUnauthorized)
	_, err = accountFromCookie(ctx, pool, session, now)
	require.ErrorIs(t, err, ErrUnauthorized)
}

func TestLogoutAllRevokesEveryTokenAndSession(t *testing.T) {
	sink := captureLogs(t)
	server, h := newCLIServer(t, sink)
	ctx := context.Background()
	now := time.Now()
	id, token := issueTestCLIToken(t, h.db.(DB), "cli-all", RoleEditor, now)
	saved, err := MintToken(ctx, h.db, Account{ID: id, Role: RoleEditor}, "laptop", "")
	require.NoError(t, err)
	session, err := OpenSession(ctx, h.db, id, now)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, server.revoke(token.Secret, true).Code)
	for _, secret := range []string{token.Secret, saved.Secret} {
		_, err = AccountFromBearer(ctx, h.db, secret, now)
		require.ErrorIs(t, err, ErrUnauthorized)
	}
	_, err = accountFromCookie(ctx, h.db, session, now)
	require.ErrorIs(t, err, ErrUnauthorized)
}

func TestCLIRevokeOfAnExpiredTokenNamesFbullLogin(t *testing.T) {
	sink := captureLogs(t)
	server, h := newCLIServer(t, sink)
	_, token := issueTestCLIToken(t, h.db.(DB), "cli-expired", RoleEditor, time.Now().Add(-cliTokenLifetime))
	rec := server.revoke(token.Secret, false)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "Your sign-in has expired. Run `fbull login` to sign in again.")
}
