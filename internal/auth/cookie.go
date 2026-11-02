package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
)

const (
	sessionCookie     = "fb_session"
	hostSessionCookie = "__Host-fb_session"
	sessionLifetime   = 7 * 24 * time.Hour
	sessionSecretLen  = 32
)

// OpenSession inserts a browser session and returns the cookie value.
// The table stores SHA-256 of the raw secret, not the secret.
func OpenSession(ctx context.Context, db postgres.Query, accountID string, now time.Time) (string, error) {
	if accountID == "" {
		return "", errors.New("session account is empty")
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	raw := make([]byte, sessionSecretLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("session secret: %w", err)
	}
	sum := sha256.Sum256(raw)
	secret := base64.RawURLEncoding.EncodeToString(raw)
	for i := range raw {
		raw[i] = 0
	}
	expires := now.Add(sessionLifetime)
	_, err = db.Exec(ctx, `
		INSERT INTO browser_session (id, account_id, secret_hash, expires_at)
		VALUES ($1, $2, $3, $4)`, id, accountID, sum[:], expires)
	if err != nil {
		return "", fmt.Errorf("insert browser session: %w", err)
	}
	return id + "." + secret, nil
}

// AccountFromRequest loads the account for fb_session. A bad cookie is ErrUnauthorized.
func AccountFromRequest(ctx context.Context, db postgres.Query, r *http.Request, now time.Time) (Account, error) {
	cookie, err := r.Cookie(sessionCookieName(r))
	if err != nil {
		return Account{}, ErrUnauthorized
	}
	return accountFromCookie(ctx, db, cookie.Value, now)
}

func accountFromCookie(ctx context.Context, db postgres.Query, value string, now time.Time) (Account, error) {
	id, raw, err := splitCookie(value)
	if err != nil {
		return Account{}, ErrUnauthorized
	}
	sum := sha256.Sum256(raw)
	var (
		account Account
		stored  []byte
		expires time.Time
		revoked *time.Time
	)
	err = db.QueryRow(ctx, `
		SELECT a.id, a.role, s.secret_hash, s.expires_at, s.revoked_at
		FROM browser_session AS s
		JOIN account AS a ON a.id = s.account_id
		WHERE s.id = $1`, id).Scan(&account.ID, &account.Role, &stored, &expires, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrUnauthorized
	}
	if err != nil {
		return Account{}, fmt.Errorf("lookup session: %w", err)
	}
	if !secretHashMatches(sum[:], stored) || revoked != nil || !expires.After(now) {
		return Account{}, ErrUnauthorized
	}
	if !knownRole(account.Role) {
		return Account{}, ErrUnauthorized
	}
	if _, err := db.Exec(ctx, `
		UPDATE browser_session
		SET last_seen_at = $2
		WHERE id = $1
		  AND last_seen_at < $2::timestamptz - interval '5 minutes'`, id, now); err != nil {
		return Account{}, &useNotRecordedError{what: "session", err: err}
	}
	return account, nil
}

func splitCookie(value string) (string, []byte, error) {
	dot := indexByte(value, '.')
	if dot <= 0 || dot == len(value)-1 {
		return "", nil, ErrUnauthorized
	}
	raw, err := base64.RawURLEncoding.DecodeString(value[dot+1:])
	if err != nil || len(raw) != sessionSecretLen {
		return "", nil, ErrUnauthorized
	}
	return value[:dot], raw, nil
}

func secretHashMatches(got, want []byte) bool {
	if len(got) != len(want) || len(want) == 0 {
		if len(want) > 0 {
			subtle.ConstantTimeCompare(want, want)
		}
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// SetSessionCookie writes fb_session. Secure is on when AUTH_COOKIE_SECURE is
// true or the request is HTTPS.
func SetSessionCookie(w http.ResponseWriter, r *http.Request, value string, now time.Time) {
	http.SetCookie(w, sessionCookieHeader(r, value, now.Add(sessionLifetime), int(sessionLifetime/time.Second)))
}

// ClearSessionCookie expires fb_session.
func ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, sessionCookieHeader(r, "", time.Unix(0, 0), -1))
}

func sessionCookieHeader(r *http.Request, value string, expires time.Time, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName(r),
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(r),
		Expires:  expires,
		MaxAge:   maxAge,
	}
}

func sessionCookieName(r *http.Request) string {
	if cookieSecure(r) {
		return hostSessionCookie
	}
	return sessionCookie
}

func cookieSecure(r *http.Request) bool {
	if environment.GetAuthCookieSecure() {
		return true
	}
	return r != nil && r.TLS != nil
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("session id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
