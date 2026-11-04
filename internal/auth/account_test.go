package auth

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/lhjnilsson/foreverbull/internal/environment"
)

func TestCreateEditorHashesAndRotateKeepsRole(t *testing.T) {
	t.Setenv(environment.AuthEnabled, "true")
	pool := newAuthDB(t)
	ctx := context.Background()
	require.NoError(t, Migrate(ctx, pool))

	const (
		username = "reader"
		password = " twelve-chars"
		wrong    = "wrong-password"
		rotated  = "rotated-pass"
	)

	_, err := CreateEditor(ctx, pool, username, "short")
	require.ErrorIs(t, err, ErrPasswordLength)
	require.Equal(t, 0, countAccounts(t, ctx, pool))

	_, err = CreateEditor(ctx, pool, "bad name", password)
	require.ErrorIs(t, err, ErrUsername)
	require.Equal(t, 0, countAccounts(t, ctx, pool))

	id, err := CreateEditor(ctx, pool, "  "+username+"  ", password)
	require.NoError(t, err)
	require.NotEmpty(t, id)

	otherID, err := CreateEditor(ctx, pool, "Reader", password)
	require.NoError(t, err)
	require.NotEqual(t, id, otherID)

	var (
		role string
		hash string
	)
	err = pool.QueryRow(ctx, `
		SELECT a.role, p.password_hash
		FROM account AS a
		JOIN account_password AS p ON p.account_id = a.id
		WHERE p.username = $1`, username).Scan(&role, &hash)
	require.NoError(t, err)
	require.Equal(t, "editor", role)
	require.NotContains(t, hash, password)
	require.Greater(t, len(hash), len("$argon2id$v=19$m=65536,t=3,p=2$"))

	ok, err := VerifyPassword(hash, wrong)
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = VerifyPassword(hash, password)
	require.NoError(t, err)
	require.True(t, ok)

	account, err := Authenticate(ctx, pool, username, wrong)
	require.ErrorIs(t, err, ErrUnauthorized)
	require.Empty(t, account.ID)
	require.NotContains(t, err.Error(), password)
	require.NotContains(t, err.Error(), wrong)

	account, err = Authenticate(ctx, pool, username, password)
	require.NoError(t, err)
	require.Equal(t, id, account.ID)
	require.Equal(t, "editor", account.Role)

	_, err = Authenticate(ctx, pool, "missing-user", password)
	require.ErrorIs(t, err, ErrUnauthorized)

	_, err = CreateEditor(ctx, pool, username, "other-password")
	require.ErrorIs(t, err, ErrUsernameExists)
	var hashAfterDuplicate string
	err = pool.QueryRow(ctx, `SELECT password_hash FROM account_password WHERE username = $1`, username).Scan(&hashAfterDuplicate)
	require.NoError(t, err)
	require.Equal(t, hash, hashAfterDuplicate)
	require.Equal(t, 2, countAccounts(t, ctx, pool))

	_, err = pool.Exec(ctx, `UPDATE account SET role = 'viewer' WHERE id = $1`, id)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		INSERT INTO browser_session (id, account_id, secret_hash, expires_at)
		VALUES ('session-1', $1, $2, now() + interval '1 hour')`, id, []byte{1, 2, 3})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO browser_session (id, account_id, secret_hash, expires_at)
		VALUES ('other-user', $1, $2, now() + interval '1 hour')`, otherID, []byte{9})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO api_token (account_id, token_hash, hint, name)
		VALUES ($1, $2, 'fb_abc123', 'laptop')`, id, []byte{4, 5, 6})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO api_token (account_id, token_hash, hint, name)
		VALUES ($1, $2, 'fb_def456', 'other-laptop')`, otherID, []byte{7, 8, 9})
	require.NoError(t, err)

	rotatedID, err := RotatePassword(ctx, pool, username, rotated)
	require.NoError(t, err)
	require.Equal(t, id, rotatedID)

	err = pool.QueryRow(ctx, `SELECT role FROM account WHERE id = $1`, id).Scan(&role)
	require.NoError(t, err)
	require.Equal(t, "viewer", role)

	var sessions int
	err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM browser_session WHERE account_id = $1`, id).Scan(&sessions)
	require.NoError(t, err)
	require.Zero(t, sessions)
	err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM browser_session WHERE id = 'other-user'`).Scan(&sessions)
	require.NoError(t, err)
	require.Equal(t, 1, sessions)
	var tokenOpen bool
	err = pool.QueryRow(ctx, `SELECT revoked_at IS NULL FROM api_token WHERE account_id = $1`, id).Scan(&tokenOpen)
	require.NoError(t, err)
	require.False(t, tokenOpen, "a password rotation revokes the account's tokens")
	err = pool.QueryRow(ctx, `SELECT revoked_at IS NULL FROM api_token WHERE account_id = $1`, otherID).Scan(&tokenOpen)
	require.NoError(t, err)
	require.True(t, tokenOpen, "another account's token stays")

	_, err = Authenticate(ctx, pool, username, password)
	require.ErrorIs(t, err, ErrUnauthorized)
	account, err = Authenticate(ctx, pool, username, rotated)
	require.NoError(t, err)
	require.Equal(t, id, account.ID)
	require.Equal(t, "viewer", account.Role)

	_, err = RotatePassword(ctx, pool, "nobody", rotated)
	require.ErrorIs(t, err, ErrAccountNotFound)
}

func countAccounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()

	var n int
	err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM account`).Scan(&n)
	require.NoError(t, err)
	return n
}
