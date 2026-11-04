package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lhjnilsson/foreverbull/internal/postgres"
)

const (
	RoleNone    = "none"
	RolePending = "pending"
	RoleViewer  = "viewer"
	RoleEditor  = "editor"
	RoleAdmin   = "admin"

	LevelViewer = "viewer"
	LevelEditor = "editor"

	// ScopeRead may call only the read RPCs. ScopeWrite follows the role.
	// An empty scope is a browser session, not a token.
	ScopeRead  = "read"
	ScopeWrite = "write"

	DomainService  = "service"
	DomainBacktest = "backtest"
	DomainStrategy = "strategy"
)

// DB is a pool or a connection that can run a transaction.
type DB interface {
	postgres.Query
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Account is a password login row. Role is whatever SQL stored.
// Scope is set for a bearer token. A browser session leaves it empty.
type Account struct {
	ID    string
	Role  string
	Scope string
}

// CreateEditor inserts role editor and a password hash in one transaction.
// Role changes are SQL, so this function has no role argument.
func CreateEditor(ctx context.Context, db DB, username, password string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if db == nil {
		return "", errors.New("account database is nil")
	}

	username, err := NormalizeUsername(username)
	if err != nil {
		return "", err
	}
	if err := CheckPasswordLength(password); err != nil {
		return "", err
	}

	var exists bool
	err = db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM account_password WHERE username = $1)`, username).Scan(&exists)
	if err != nil {
		return "", fmt.Errorf("lookup username: %w", err)
	}
	if exists {
		return "", fmt.Errorf("create account: %w", ErrUsernameExists)
	}

	hash, err := HashPassword(password)
	if err != nil {
		return "", err
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin account create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id string
	err = tx.QueryRow(ctx, `INSERT INTO account (role) VALUES ('editor') RETURNING id`).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert account: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO account_password (account_id, username, password_hash)
		VALUES ($1, $2, $3)`, id, username, hash)
	if err != nil {
		if isUniqueViolation(err) {
			return "", fmt.Errorf("create account: %w", ErrUsernameExists)
		}
		return "", fmt.Errorf("insert account password: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit account create: %w", err)
	}
	return id, nil
}

// RotatePassword replaces password_hash for username. It does not change
// role. A new password means the old one may be known, so every API token of
// the account is revoked and every browser_session row is deleted in the same
// transaction as the hash update.
func RotatePassword(ctx context.Context, db DB, username, password string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if db == nil {
		return "", errors.New("account database is nil")
	}

	username, err := NormalizeUsername(username)
	if err != nil {
		return "", err
	}
	if err := CheckPasswordLength(password); err != nil {
		return "", err
	}

	var id string
	err = db.QueryRow(ctx, `SELECT account_id FROM account_password WHERE username = $1`, username).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("rotate password: %w", ErrAccountNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("lookup account: %w", err)
	}

	hash, err := HashPassword(password)
	if err != nil {
		return "", err
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin password rotate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var updated string
	err = tx.QueryRow(ctx, `
		UPDATE account_password
		SET password_hash = $2
		WHERE username = $1
		RETURNING account_id`, username, hash).Scan(&updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("rotate password: %w", ErrAccountNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("update password: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		UPDATE api_token SET revoked_at = now()
		WHERE account_id = $1 AND revoked_at IS NULL`, updated); err != nil {
		return "", fmt.Errorf("revoke tokens: %w", err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM browser_session WHERE account_id = $1`, updated); err != nil {
		return "", fmt.Errorf("revoke sessions: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit password rotate: %w", err)
	}
	return updated, nil
}

// Authenticate checks username and password. A missing user and a wrong
// password are both ErrUnauthorized. The password is not part of the error.
func Authenticate(ctx context.Context, db postgres.Query, username, password string) (Account, error) {
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	if db == nil {
		return Account{}, errors.New("account database is nil")
	}
	if err := CheckPasswordLength(password); err != nil {
		return Account{}, err
	}

	username, err := NormalizeUsername(username)
	if err != nil {
		return Account{}, ErrUnauthorized
	}

	var (
		account Account
		hash    string
	)
	err = db.QueryRow(ctx, `
		SELECT a.id, a.role, p.password_hash
		FROM account_password AS p
		JOIN account AS a ON a.id = p.account_id
		WHERE p.username = $1`, username).Scan(&account.ID, &account.Role, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, dummyErr := verifyDummy(password); dummyErr != nil {
			return Account{}, dummyErr
		}
		return Account{}, ErrUnauthorized
	}
	if err != nil {
		return Account{}, fmt.Errorf("authenticate: %w", err)
	}
	if !knownRole(account.Role) {
		return Account{}, fmt.Errorf("account %s: unknown role", account.ID)
	}

	ok, err := VerifyPassword(hash, password)
	if err != nil {
		return Account{}, err
	}
	if !ok {
		return Account{}, ErrUnauthorized
	}
	return account, nil
}

func verifyDummy(password string) (bool, error) {
	encoded, err := dummyHash()
	if err != nil {
		return false, fmt.Errorf("password check: %w", err)
	}
	return VerifyPassword(encoded, password)
}

func knownRole(role string) bool {
	switch role {
	case RoleNone, RolePending, RoleViewer, RoleEditor, RoleAdmin:
		return true
	default:
		return false
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
