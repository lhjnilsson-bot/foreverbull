package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lhjnilsson/foreverbull/internal/postgres"
	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	"github.com/lhjnilsson/foreverbull/internal/web"
)

// cliRevokePath is the one /api route that takes a bearer instead of the
// session cookie: `fbull logout` has only its token.
const cliRevokePath = "/api/auth/cli/revoke"

// RevokeAllCredentials revokes every API token and browser session of the
// account, including tokens saved with `fbull token save`.
func RevokeAllCredentials(ctx context.Context, db postgres.Query, accountID string, now time.Time) error {
	if accountID == "" {
		return errors.New("revoke account is empty")
	}
	if _, err := db.Exec(ctx, `
		UPDATE api_token SET revoked_at = $2
		WHERE account_id = $1 AND revoked_at IS NULL`, accountID, now); err != nil {
		return errors.Join(errors.New("revoke tokens"), err)
	}
	if _, err := db.Exec(ctx, `
		UPDATE browser_session SET revoked_at = $2
		WHERE account_id = $1 AND revoked_at IS NULL`, accountID, now); err != nil {
		return errors.Join(errors.New("revoke sessions"), err)
	}
	return nil
}

// revokeBearer revokes the presented token. It is already checked by
// AccountFromBearer, so a missing row is a race with another revoke.
func revokeBearer(ctx context.Context, db postgres.Query, accountID, token string, now time.Time) error {
	sum := sha256.Sum256([]byte(token))
	tag, err := db.Exec(ctx, `
		UPDATE api_token SET revoked_at = $3
		WHERE token_hash = $1 AND account_id = $2 AND revoked_at IS NULL`, sum[:], accountID, now)
	if err != nil {
		return errors.Join(errors.New("revoke token"), err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUnauthorized
	}
	return nil
}

func bearerToken(r *http.Request) (string, bool) {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(header, "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	return token, token != ""
}

// protectBearer authenticates cliRevokePath with its bearer. An expired
// sign-in is 401 with usercopy.SignInExpired so fbull can print it.
func protectBearer(db postgres.Query, w http.ResponseWriter, r *http.Request, next http.Handler) {
	token, ok := bearerToken(r)
	if !ok {
		web.Unauthorized(w)
		return
	}
	account, err := AccountFromBearer(r.Context(), db, token, time.Now())
	if errors.Is(err, ErrSignInExpired) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":   "sign_in_expired",
			"message": usercopy.SignInExpired,
		})
		return
	}
	if errors.Is(err, ErrUnauthorized) {
		web.Unauthorized(w)
		return
	}
	if err != nil {
		web.WriteError(w, err)
		return
	}
	next.ServeHTTP(w, r.WithContext(WithAccount(r.Context(), account)))
}

// cliRevoke revokes the bearer, or with {"all": true} every token and session
// of its account. Any role may revoke its own credentials.
func (h *handlers) cliRevoke(w http.ResponseWriter, r *http.Request) {
	account, ok := FromContext(r.Context())
	if !ok {
		web.Unauthorized(w)
		return
	}
	token, ok := bearerToken(r)
	if !ok {
		web.Unauthorized(w)
		return
	}
	var body struct {
		All bool `json:"all"`
	}
	if err := web.ReadJSON(r, &body); err != nil {
		web.BadRequest(w)
		return
	}
	if body.All {
		if err := RevokeAllCredentials(r.Context(), h.db, account.ID, h.now()); err != nil {
			h.logf("cli revoke account=%s all=true result=error err=%s", account.ID, revokeErrClass(err))
			web.WriteError(w, err)
			return
		}
		h.logf("cli revoke account=%s all=true result=ok", account.ID)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if err := revokeBearer(r.Context(), h.db, account.ID, token, h.now()); err != nil {
		h.logf("cli revoke account=%s all=false result=error err=%s", account.ID, revokeErrClass(err))
		WriteHTTP(w, err)
		return
	}
	h.logf("cli revoke account=%s all=false result=ok", account.ID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// revokeAll is the web UI's "sign out everywhere": every token and session.
func (h *handlers) revokeAll(w http.ResponseWriter, r *http.Request) {
	account, ok := FromContext(r.Context())
	if !ok {
		web.Unauthorized(w)
		return
	}
	ClearSessionCookie(w, r)
	if err := RevokeAllCredentials(r.Context(), h.db, account.ID, h.now()); err != nil {
		h.logf("revoke all account=%s result=error err=%s", account.ID, revokeErrClass(err))
		web.WriteError(w, err)
		return
	}
	h.logf("revoke all account=%s result=ok", account.ID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
