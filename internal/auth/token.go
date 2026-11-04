package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lhjnilsson/foreverbull/internal/postgres"
)

const (
	tokenPrefix    = "fb_"
	tokenSecretLen = 32
	tokenHintLen   = 9
	tokenNameMax   = 64
	tokenTouch     = 5 * time.Minute

	// cliTokenLifetime is the absolute lifetime of a token from `fbull login`.
	cliTokenLifetime = 30 * 24 * time.Hour
	// cliTokenIdle ends a CLI token that has not been used for this long.
	cliTokenIdle = 14 * 24 * time.Hour
	// cliTokenTouch bounds last_used_at writes for a CLI token. The idle
	// timeout is days, so an hour of slack does not move it.
	cliTokenTouch = time.Hour
)

// ErrSignInExpired is a token past its absolute lifetime or its idle timeout.
// It is a 401 like ErrUnauthorized, with usercopy.SignInExpired as the message.
var ErrSignInExpired = fmt.Errorf("%w: sign-in expired", ErrUnauthorized)

// Token is a minted CLI token. Secret is set only on mint.
type Token struct {
	ID         string
	Name       string
	Scope      string
	Secret     string
	Hint       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// MintToken stores SHA-256 of the full token string. The secret is returned once.
// An empty scope is write. Viewers cannot mint, including a read token.
func MintToken(ctx context.Context, db postgres.Query, account Account, name, scope string) (Token, error) {
	if account.Role != RoleEditor && account.Role != RoleAdmin {
		return Token{}, ErrForbidden
	}
	if account.ID == "" {
		return Token{}, errors.New("token account is empty")
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > tokenNameMax {
		return Token{}, ErrBadRequest
	}
	scope, err := tokenScope(scope)
	if err != nil {
		return Token{}, err
	}
	secret, err := newTokenSecret()
	if err != nil {
		return Token{}, err
	}
	sum := sha256.Sum256([]byte(secret))
	var minted Token
	err = db.QueryRow(ctx, `
		INSERT INTO api_token (account_id, token_hash, hint, name, scope)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at`, account.ID, sum[:], secret[:tokenHintLen], name, scope).Scan(&minted.ID, &minted.CreatedAt)
	if err != nil {
		return Token{}, fmt.Errorf("insert token: %w", err)
	}
	minted.Name = name
	minted.Scope = scope
	minted.Secret = secret
	minted.Hint = secret[:tokenHintLen]
	return minted, nil
}

// newTokenSecret returns fb_ plus 32 random bytes, base64url. The random
// buffer is zeroed before return.
func newTokenSecret() (string, error) {
	raw := make([]byte, tokenSecretLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("token secret: %w", err)
	}
	secret := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	for i := range raw {
		raw[i] = 0
	}
	if len(secret) < tokenHintLen {
		return "", errors.New("token secret is short")
	}
	return secret, nil
}

func tokenScope(scope string) (string, error) {
	switch strings.TrimSpace(scope) {
	case "", ScopeWrite:
		return ScopeWrite, nil
	case ScopeRead:
		return ScopeRead, nil
	default:
		return "", ErrBadRequest
	}
}

// ListTokens returns the caller's tokens without the secret.
func ListTokens(ctx context.Context, db postgres.Query, account Account) ([]Token, error) {
	if account.Role != RoleEditor && account.Role != RoleAdmin {
		return nil, ErrForbidden
	}
	if account.ID == "" {
		return nil, errors.New("token account is empty")
	}
	rows, err := db.Query(ctx, `
		SELECT id, name, scope, hint, created_at, last_used_at, revoked_at
		FROM api_token
		WHERE account_id = $1
		ORDER BY created_at ASC`, account.ID)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		var token Token
		if err := rows.Scan(
			&token.ID, &token.Name, &token.Scope, &token.Hint,
			&token.CreatedAt, &token.LastUsedAt, &token.RevokedAt,
		); err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		out = append(out, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	return out, nil
}

// RevokeToken sets revoked_at on the caller's token. Another account's id is not found.
func RevokeToken(ctx context.Context, db postgres.Query, account Account, id string, now time.Time) error {
	if account.Role != RoleEditor && account.Role != RoleAdmin {
		return ErrForbidden
	}
	if account.ID == "" || id == "" {
		return ErrNotFound
	}
	tag, err := db.Exec(ctx, `
		UPDATE api_token
		SET revoked_at = $3
		WHERE id = $1 AND account_id = $2 AND revoked_at IS NULL`, id, account.ID, now)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AccountFromBearer loads the account for a fb_ token. A revoked or unknown
// token is ErrUnauthorized. A token past expires_at, or a CLI token idle for
// cliTokenIdle, is ErrSignInExpired. last_used_at is written at most once per
// tokenTouch, or once per cliTokenTouch for a CLI token. The token is not logged.
func AccountFromBearer(ctx context.Context, db postgres.Query, token string, now time.Time) (Account, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, tokenPrefix) || len(token) <= len(tokenPrefix) {
		return Account{}, ErrUnauthorized
	}
	sum := sha256.Sum256([]byte(token))
	var (
		account Account
		row     bearerRow
	)
	err := db.QueryRow(ctx, `
		SELECT a.id, a.role, t.scope, t.id, t.client, t.created_at, t.last_used_at, t.revoked_at, t.expires_at
		FROM api_token AS t
		JOIN account AS a ON a.id = t.account_id
		WHERE t.token_hash = $1`, sum[:]).Scan(
		&account.ID, &account.Role, &account.Scope, &row.id, &row.client,
		&row.createdAt, &row.lastUsedAt, &row.revokedAt, &row.expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrUnauthorized
	}
	if err != nil {
		return Account{}, fmt.Errorf("lookup token: %w", err)
	}
	if row.revokedAt != nil {
		return Account{}, ErrUnauthorized
	}
	if err := row.live(now); err != nil {
		return Account{}, err
	}
	if !knownRole(account.Role) || (account.Scope != ScopeRead && account.Scope != ScopeWrite) {
		return Account{}, ErrUnauthorized
	}
	touch := tokenTouch
	if row.client != nil {
		touch = cliTokenTouch
	}
	if _, err := db.Exec(ctx, `
		UPDATE api_token
		SET last_used_at = $2
		WHERE id = $1
		  AND (last_used_at IS NULL OR last_used_at < $3)`, row.id, now, now.Add(-touch)); err != nil {
		return Account{}, &useNotRecordedError{what: "token", err: err}
	}
	return account, nil
}

type bearerRow struct {
	id         string
	client     *string
	createdAt  time.Time
	lastUsedAt *time.Time
	revokedAt  *time.Time
	expiresAt  *time.Time
}

// live refuses a token past expires_at and a CLI token idle for cliTokenIdle.
// A token without a client keeps today's rule: only expires_at, which is NULL
// for a token minted from the web or saved with `fbull token save`.
func (row bearerRow) live(now time.Time) error {
	if row.expiresAt != nil && !row.expiresAt.After(now) {
		return ErrSignInExpired
	}
	if row.client == nil {
		return nil
	}
	if row.expiresAt == nil {
		// A CLI token is always issued with expires_at.
		return fmt.Errorf("cli token %s has no expires_at", row.id)
	}
	lastUse := row.createdAt
	if row.lastUsedAt != nil {
		lastUse = *row.lastUsedAt
	}
	if lastUse.Add(cliTokenIdle).After(now) {
		return nil
	}
	return ErrSignInExpired
}
