package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
	"github.com/lhjnilsson/foreverbull/internal/web"
)

const (
	loginBodyCap = 4 << 10
)

// loginHashCapacity bounds concurrent argon2id checks. Each one asks for 64 MiB.
func loginHashCapacity() int {
	n := runtime.NumCPU()
	if n < 2 {
		return 2
	}
	return n
}

type handlers struct {
	db      postgres.Query
	logf    LogFunc
	limiter *failureLimiter
	// cliLimiter bounds code exchanges per address, cliAccountLimiter per account.
	cliLimiter        *failureLimiter
	cliAccountLimiter *failureLimiter
	now               func() time.Time
	// hashes bounds concurrent argon2id checks. Each one uses 64 MiB.
	hashes chan struct{}
}

// RegisterHTTP mounts login, session, token, and grant routes.
// Call it only when authentication is on.
func RegisterHTTP(mux *http.ServeMux, db postgres.Query) {
	registerHTTP(mux, db, log.Printf)
}

func registerHTTP(mux *http.ServeMux, db postgres.Query, logf LogFunc) {
	if mux == nil || db == nil {
		return
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	h := &handlers{
		db: db, logf: logf, limiter: newFailureLimiter(), now: time.Now,
		cliLimiter: newFailureLimiter(), cliAccountLimiter: newFailureLimiter(),
		hashes: make(chan struct{}, loginHashCapacity()),
	}
	mux.HandleFunc("GET /api/auth/providers", h.providers)
	mux.HandleFunc("POST /api/auth/login", h.login)
	mux.HandleFunc("POST /api/auth/logout", h.logout)
	mux.HandleFunc("GET /api/auth/me", h.me)
	mux.HandleFunc("GET /api/auth/github/start", h.githubStart)
	mux.HandleFunc("GET /api/auth/github/callback", h.githubCallback)
	mux.HandleFunc("POST /api/auth/tokens", h.mintToken)
	mux.HandleFunc("GET /api/auth/tokens", h.listTokens)
	mux.HandleFunc("DELETE /api/auth/tokens/{id}", h.revokeToken)
	mux.HandleFunc("GET /cli/authorize", h.cliAuthorizePage)
	mux.HandleFunc("POST /cli/authorize", h.cliAuthorizeApprove)
	mux.HandleFunc("POST /api/auth/cli/token", h.cliToken)
	mux.HandleFunc("POST "+cliRevokePath, h.cliRevoke)
	mux.HandleFunc("POST /api/auth/revoke-all", h.revokeAll)
	mux.HandleFunc("PUT /api/services/grants", h.putServiceGrant)
	mux.HandleFunc("GET /api/services/grants", h.listServiceGrants)
	mux.HandleFunc("DELETE /api/services/grants", h.deleteServiceGrant)
	mux.HandleFunc("PUT /api/backtests/{name}/grants", h.putBacktestGrant)
	mux.HandleFunc("GET /api/backtests/{name}/grants", h.listBacktestGrants)
	mux.HandleFunc("DELETE /api/backtests/{name}/grants", h.deleteBacktestGrant)
	mux.HandleFunc("PUT /api/strategies/{name}/grants", h.putStrategyGrant)
	mux.HandleFunc("GET /api/strategies/{name}/grants", h.listStrategyGrants)
	mux.HandleFunc("DELETE /api/strategies/{name}/grants", h.deleteStrategyGrant)
}

func (h *handlers) providers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"password": true,
		"github":   githubConfigured(),
	})
}

func (h *handlers) login(w http.ResponseWriter, r *http.Request) {
	username, password, err := readLogin(r)
	if err != nil {
		web.BadRequest(w)
		return
	}
	// Longer than 128 is already under 1 KiB. Reject it before argon2 and before a database read.
	if err := CheckPasswordLength(password); err != nil {
		h.logf("login username=%s result=unauthorized", username)
		web.Unauthorized(w)
		return
	}
	remote := clientAddr(r)
	if !h.limiter.Begin(username, remote, h.now()) {
		h.logf("login username=%s result=limited", username)
		web.Unauthorized(w)
		return
	}
	failed := false
	defer func() { h.limiter.End(username, remote, failed, h.now()) }()
	if !h.acquireLoginHash() {
		h.logf("login username=%s result=busy", username)
		web.StatusJSON(w, http.StatusServiceUnavailable, `{"error":"unavailable"}`)
		return
	}
	defer h.releaseLoginHash()
	sweepExpired(r.Context(), h.db, h.now(), h.logf)
	account, err := Authenticate(r.Context(), h.db, username, password)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrPasswordLength) || errors.Is(err, ErrUsername) {
			failed = true
			h.logf("login username=%s result=unauthorized", username)
			web.Unauthorized(w)
			return
		}
		web.WriteError(w, err)
		return
	}
	h.logf("login username=%s result=ok", username)
	value, err := OpenSession(r.Context(), h.db, account.ID, h.now())
	if err != nil {
		web.WriteError(w, err)
		return
	}
	SetSessionCookie(w, r, value, h.now())
	h.writeMe(w, r, account)
}

func (h *handlers) acquireLoginHash() bool {
	if h.hashes == nil {
		return true
	}
	select {
	case h.hashes <- struct{}{}:
		return true
	default:
		return false
	}
}

func (h *handlers) releaseLoginHash() {
	if h.hashes == nil {
		return
	}
	<-h.hashes
}

func readLogin(r *http.Request) (string, string, error) {
	if r.Body == nil {
		return "", "", ErrBadRequest
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, loginBodyCap+1))
	if err != nil {
		return "", "", ErrBadRequest
	}
	if len(raw) > loginBodyCap {
		return "", "", ErrBadRequest
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return "", "", ErrBadRequest
	}
	var extra struct{}
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", "", ErrBadRequest
	}
	return strings.TrimSpace(body.Username), body.Password, nil
}

func (h *handlers) logout(w http.ResponseWriter, r *http.Request) {
	account, ok := FromContext(r.Context())
	if !ok {
		web.Unauthorized(w)
		return
	}
	// The cookie is cleared on both paths so the browser stops sending it even
	// when the server-side revoke failed.
	ClearSessionCookie(w, r)
	cookie, err := r.Cookie(sessionCookieName(r))
	if err == nil {
		id, _, splitErr := splitCookie(cookie.Value)
		if splitErr == nil {
			_, revokeErr := h.db.Exec(r.Context(), `
				UPDATE browser_session SET revoked_at = $2
				WHERE id = $1 AND account_id = $3 AND revoked_at IS NULL`, id, h.now(), account.ID)
			if revokeErr != nil {
				h.logf("logout account=%s result=revoke_failed err=%s", account.ID, revokeErrClass(revokeErr))
				web.StatusJSON(w, http.StatusInternalServerError, `{"error":"internal"}`)
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// revokeErrClass names a failed session revoke without the error text: a
// Postgres message can quote the session id from the cookie, and a connection
// error can carry the database URL and its password.
func revokeErrClass(err error) string {
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr):
		return "sqlstate=" + pgErr.Code
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return fmt.Sprintf("%T", err)
	}
}

func (h *handlers) me(w http.ResponseWriter, r *http.Request) {
	account, ok := FromContext(r.Context())
	if !ok {
		web.Unauthorized(w)
		return
	}
	h.writeMe(w, r, account)
}

func (h *handlers) writeMe(w http.ResponseWriter, r *http.Request, account Account) {
	login, kind, err := accountLogin(r.Context(), h.db, account.ID)
	if err != nil {
		web.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"id":         account.ID,
		"role":       account.Role,
		"login":      login,
		"login_kind": kind,
	})
}

func accountLogin(ctx context.Context, db postgres.Query, id string) (string, string, error) {
	var login, kind string
	err := db.QueryRow(ctx, `
		SELECT COALESCE(p.username, g.github_login, ''),
		       CASE WHEN p.account_id IS NOT NULL THEN 'password' ELSE 'github' END
		FROM account AS a
		LEFT JOIN account_password AS p ON p.account_id = a.id
		LEFT JOIN account_github AS g ON g.account_id = a.id
		WHERE a.id = $1`, id).Scan(&login, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrUnauthorized
	}
	if err != nil {
		return "", "", err
	}
	return login, kind, nil
}

func (h *handlers) mintToken(w http.ResponseWriter, r *http.Request) {
	account, ok := FromContext(r.Context())
	if !ok {
		web.Unauthorized(w)
		return
	}
	var body struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if err := web.ReadJSON(r, &body); err != nil {
		web.BadRequest(w)
		return
	}
	minted, err := MintToken(r.Context(), h.db, account, body.Name, body.Scope)
	if err != nil {
		WriteHTTP(w, err)
		return
	}
	h.logf("token mint account=%s token=%s", account.ID, minted.ID)
	writeJSON(w, http.StatusOK, map[string]string{
		"id":    minted.ID,
		"name":  minted.Name,
		"scope": minted.Scope,
		"token": minted.Secret,
		"hint":  minted.Hint,
	})
}

func (h *handlers) listTokens(w http.ResponseWriter, r *http.Request) {
	account, ok := FromContext(r.Context())
	if !ok {
		web.Unauthorized(w)
		return
	}
	tokens, err := ListTokens(r.Context(), h.db, account)
	if err != nil {
		WriteHTTP(w, err)
		return
	}
	type item struct {
		ID         string  `json:"id"`
		Name       string  `json:"name"`
		Scope      string  `json:"scope"`
		Hint       string  `json:"hint"`
		CreatedAt  string  `json:"created_at"`
		LastUsedAt *string `json:"last_used_at"`
		RevokedAt  *string `json:"revoked_at"`
	}
	out := make([]item, 0, len(tokens))
	for _, token := range tokens {
		row := item{
			ID:        token.ID,
			Name:      token.Name,
			Scope:     token.Scope,
			Hint:      token.Hint,
			CreatedAt: formatTime(token.CreatedAt),
		}
		if token.LastUsedAt != nil {
			value := formatTime(*token.LastUsedAt)
			row.LastUsedAt = &value
		}
		if token.RevokedAt != nil {
			value := formatTime(*token.RevokedAt)
			row.RevokedAt = &value
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) revokeToken(w http.ResponseWriter, r *http.Request) {
	account, ok := FromContext(r.Context())
	if !ok {
		web.Unauthorized(w)
		return
	}
	if err := RevokeToken(r.Context(), h.db, account, r.PathValue("id"), h.now()); err != nil {
		WriteHTTP(w, err)
		return
	}
	h.logf("token revoke token=%s", r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func githubConfigured() bool {
	return environment.GetAuthGitHubClientID() != "" &&
		environment.GetAuthGitHubClientSecret() != "" &&
		environment.GetAuthGitHubRedirectURL() != ""
}
