package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/stretchr/testify/require"
)

func TestMigrateTwiceKeepsRowsAndRejectsOwner(t *testing.T) {
	pool := newAuthDB(t)
	ctx := context.Background()

	require.NoError(t, Migrate(ctx, pool))
	require.NoError(t, Migrate(ctx, pool))

	for _, indexName := range []string{
		"api_token_account_id_idx",
		"api_token_expires_at_idx",
		"cli_auth_code_expires_at_idx",
		"browser_session_account_id_idx",
		"browser_session_expires_at_idx",
		"oauth_state_expires_at_idx",
		"domain_grant_grantee_id_domain_kind_idx",
	} {
		var exists bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_indexes
				WHERE schemaname = current_schema() AND indexname = $1
			)`, indexName).Scan(&exists)
		require.NoError(t, err)
		require.True(t, exists, indexName)
	}

	for _, role := range []string{RoleNone, RolePending, RoleViewer, RoleEditor, RoleAdmin} {
		_, err := pool.Exec(ctx, `INSERT INTO account (role) VALUES ($1)`, role)
		require.NoError(t, err, role)
	}
	for _, role := range []string{"member", "owner"} {
		_, err := pool.Exec(ctx, `INSERT INTO account (role) VALUES ($1)`, role)
		require.ErrorContains(t, err, "violates check constraint")
	}

	var id string
	err := pool.QueryRow(ctx, `INSERT INTO account (role) VALUES ('editor') RETURNING id`).Scan(&id)
	require.NoError(t, err)
	require.NotEmpty(t, id)

	_, err = pool.Exec(ctx, `
		INSERT INTO account_password (account_id, username, password_hash)
		VALUES ($1, $2, 'not-a-login')`, id, strings.Repeat("a", 65))
	require.ErrorContains(t, err, "violates check constraint")

	_, err = pool.Exec(ctx, `
		INSERT INTO domain_grant (domain_kind, domain_id, grantee_id, level)
		VALUES ('backtest', 'bt-1', $1, 'owner')`, id)
	require.ErrorContains(t, err, "violates check constraint")

	_, err = pool.Exec(ctx, `
		INSERT INTO domain_grant (domain_kind, domain_id, grantee_id, level)
		VALUES ('backtest', 'bt-1', $1, 'viewer')`, id)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		INSERT INTO domain_grant (domain_kind, domain_id, grantee_id, level)
		VALUES ('backtest', 'bt-1', $1, 'editor')`, id)
	require.ErrorContains(t, err, "duplicate key")

	_, err = pool.Exec(ctx, `
		INSERT INTO domain_grant (domain_kind, domain_id, grantee_id, level)
		VALUES ('strategy', 'st-1', $1, 'editor')`, id)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		INSERT INTO domain_grant (domain_kind, domain_id, grantee_id, level)
		VALUES ('account', 'st-1', $1, 'viewer')`, id)
	require.ErrorContains(t, err, "violates check constraint")

	require.NoError(t, Migrate(ctx, pool))

	var role string
	err = pool.QueryRow(ctx, `SELECT role FROM account WHERE id = $1`, id).Scan(&role)
	require.NoError(t, err)
	require.Equal(t, RoleEditor, role)

	var grants int
	err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM domain_grant WHERE grantee_id = $1`, id).Scan(&grants)
	require.NoError(t, err)
	require.Equal(t, 2, grants)
}

func TestMigrateDeletesGrantWithDomain(t *testing.T) {
	pool := newAuthDB(t)
	ctx := context.Background()
	require.NoError(t, Migrate(ctx, pool))

	_, err := pool.Exec(ctx, `CREATE TABLE service (id text PRIMARY KEY, image text NOT NULL)`)
	require.NoError(t, err)
	require.NoError(t, Migrate(ctx, pool))

	var account string
	err = pool.QueryRow(ctx, `INSERT INTO account (role) VALUES ('editor') RETURNING id`).Scan(&account)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO service (id, image) VALUES ('svc-1', 'algo')`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO domain_grant (domain_kind, domain_id, grantee_id, level)
		VALUES ('service', 'svc-1', $1, 'viewer')`, account)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `DELETE FROM service WHERE id = 'svc-1'`)
	require.NoError(t, err)

	var grants int
	err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM domain_grant`).Scan(&grants)
	require.NoError(t, err)
	require.Equal(t, 0, grants)

	var accounts int
	err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM account`).Scan(&accounts)
	require.NoError(t, err)
	require.Equal(t, 1, accounts)
}

func newAuthDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	test_helper.SetupEnvironment(t, &test_helper.Containers{Postgres: true})

	pool, err := pgxpool.New(context.Background(), environment.GetPostgresURL())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}
