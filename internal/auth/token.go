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
)

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
	raw := make([]byte, tokenSecretLen)
	if _, err = rand.Read(raw); err != nil {
		return Token{}, fmt.Errorf("token secret: %w", err)
	}
	secret := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	for i := range raw {
		raw[i] = 0
	}
	if len(secret) < tokenHintLen {
		return Token{}, errors.New("token secret is short")
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

// AccountFromBearer loads the account for a fb_ token. It updates last_used_at
// at most once per five minutes. The token is not logged.
func AccountFromBearer(ctx context.Context, db postgres.Query, token string, now time.Time) (Account, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, tokenPrefix) || len(token) <= len(tokenPrefix) {
		return Account{}, ErrUnauthorized
	}
	sum := sha256.Sum256([]byte(token))
	var (
		account Account
		tokenID string
		revoked *time.Time
		expires *time.Time
	)
	err := db.QueryRow(ctx, `
		SELECT a.id, a.role, t.scope, t.id, t.revoked_at, t.expires_at
		FROM api_token AS t
		JOIN account AS a ON a.id = t.account_id
		WHERE t.token_hash = $1
		  AND (t.expires_at IS NULL OR t.expires_at > $2)`, sum[:], now).Scan(
		&account.ID, &account.Role, &account.Scope, &tokenID, &revoked, &expires)
	if errors.Is(err, pgx.ErrNoRows) || revoked != nil || (expires != nil && !expires.After(now)) {
		return Account{}, ErrUnauthorized
	}
	if err != nil {
		return Account{}, fmt.Errorf("lookup token: %w", err)
	}
	if !knownRole(account.Role) || (account.Scope != ScopeRead && account.Scope != ScopeWrite) {
		return Account{}, ErrUnauthorized
	}
	if _, err := db.Exec(ctx, `
		UPDATE api_token
		SET last_used_at = $2
		WHERE id = $1
		  AND (last_used_at IS NULL OR last_used_at < $2::timestamptz - interval '5 minutes')`, tokenID, now); err != nil {
		return Account{}, &useNotRecordedError{what: "token", err: err}
	}
	return account, nil
}
