package auth

import (
	"context"
	"fmt"

	"github.com/lhjnilsson/foreverbull/internal/postgres"
)

const accountTable = `
CREATE TABLE IF NOT EXISTS account (
	id text PRIMARY KEY DEFAULT uuid_generate_v4()::text,
	role text NOT NULL CHECK (role IN ('none', 'pending', 'viewer', 'editor', 'admin')),
	created_at timestamptz NOT NULL DEFAULT now()
)`

const accountPasswordTable = `
CREATE TABLE IF NOT EXISTS account_password (
	account_id text PRIMARY KEY REFERENCES account (id) ON DELETE CASCADE,
	username text NOT NULL UNIQUE CHECK (username ~ '^[A-Za-z0-9._-]{1,64}$'),
	password_hash text NOT NULL
)`

const accountGitHubTable = `
CREATE TABLE IF NOT EXISTS account_github (
	account_id text PRIMARY KEY REFERENCES account (id) ON DELETE CASCADE,
	github_subject text NOT NULL UNIQUE,
	github_login text NOT NULL
)`

const browserSessionTable = `
CREATE TABLE IF NOT EXISTS browser_session (
	id text PRIMARY KEY,
	account_id text NOT NULL REFERENCES account (id) ON DELETE CASCADE,
	secret_hash bytea NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	expires_at timestamptz NOT NULL,
	last_seen_at timestamptz NOT NULL DEFAULT now(),
	revoked_at timestamptz
)`

const apiTokenTable = `
CREATE TABLE IF NOT EXISTS api_token (
	id text PRIMARY KEY DEFAULT uuid_generate_v4()::text,
	account_id text NOT NULL REFERENCES account (id) ON DELETE CASCADE,
	token_hash bytea NOT NULL UNIQUE,
	hint text NOT NULL,
	name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
	scope text NOT NULL DEFAULT 'write',
	created_at timestamptz NOT NULL DEFAULT now(),
	last_used_at timestamptz,
	expires_at timestamptz,
	revoked_at timestamptz,
	CONSTRAINT api_token_scope_check CHECK (scope IN ('read', 'write'))
)`

const oauthStateTable = `
CREATE TABLE IF NOT EXISTS oauth_state (
	state_hash bytea PRIMARY KEY,
	created_at timestamptz NOT NULL DEFAULT now(),
	expires_at timestamptz NOT NULL,
	verifier text NOT NULL DEFAULT ''
)`

const oauthStateVerifier = `
ALTER TABLE oauth_state ADD COLUMN IF NOT EXISTS verifier text NOT NULL DEFAULT ''`

const domainGrantTable = `
CREATE TABLE IF NOT EXISTS domain_grant (
	domain_kind text NOT NULL CHECK (domain_kind IN ('service', 'backtest', 'strategy')),
	domain_id text NOT NULL,
	grantee_id text NOT NULL REFERENCES account (id) ON DELETE CASCADE,
	level text NOT NULL CHECK (level IN ('viewer', 'editor')),
	created_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (domain_kind, domain_id, grantee_id)
)`

const apiTokenScopeColumn = `
ALTER TABLE api_token ADD COLUMN IF NOT EXISTS scope text NOT NULL DEFAULT 'write'`

const apiTokenScopeCheck = `
DO $$
BEGIN
	ALTER TABLE api_token
		ADD CONSTRAINT api_token_scope_check CHECK (scope IN ('read', 'write'));
EXCEPTION
	WHEN duplicate_object THEN
		NULL;
END
$$`

// apiTokenClient names the `fbull login` client (cli@<host>). A NULL client is
// a token minted from the web or `fbull token save`; it keeps a NULL
// expires_at and no idle timeout.
const apiTokenClient = `
ALTER TABLE api_token ADD COLUMN IF NOT EXISTS client text
	CHECK (client IS NULL OR char_length(client) BETWEEN 5 AND 64)`

// cliAuthCodeTable holds one-time codes from the CLI authorize page. Only
// SHA-256 of the code is stored. token_id is the token the code was exchanged
// for, so a second use can revoke it; reused_at closes the race between that
// second use and the first exchange writing token_id.
const cliAuthCodeTable = `
CREATE TABLE IF NOT EXISTS cli_auth_code (
	code_hash bytea PRIMARY KEY,
	account_id text NOT NULL REFERENCES account (id) ON DELETE CASCADE,
	client text NOT NULL CHECK (char_length(client) BETWEEN 5 AND 64),
	scope text NOT NULL CHECK (scope IN ('read', 'write')),
	code_challenge text NOT NULL,
	redirect_uri text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	expires_at timestamptz NOT NULL,
	used_at timestamptz,
	reused_at timestamptz,
	token_id text REFERENCES api_token (id) ON DELETE SET NULL
)`

const (
	apiTokenExpiresIndex       = `CREATE INDEX IF NOT EXISTS api_token_expires_at_idx ON api_token (expires_at) WHERE expires_at IS NOT NULL`
	cliAuthCodeExpiresIndex    = `CREATE INDEX IF NOT EXISTS cli_auth_code_expires_at_idx ON cli_auth_code (expires_at)`
	apiTokenAccountIndex       = `CREATE INDEX IF NOT EXISTS api_token_account_id_idx ON api_token (account_id)`
	browserSessionIndex        = `CREATE INDEX IF NOT EXISTS browser_session_account_id_idx ON browser_session (account_id)`
	browserSessionExpiresIndex = `CREATE INDEX IF NOT EXISTS browser_session_expires_at_idx ON browser_session (expires_at)`
	browserSessionLastSeen     = `ALTER TABLE browser_session ADD COLUMN IF NOT EXISTS last_seen_at timestamptz NOT NULL DEFAULT now()`
	oauthStateExpiresIndex     = `CREATE INDEX IF NOT EXISTS oauth_state_expires_at_idx ON oauth_state (expires_at)`
	domainGrantGranteeIndex    = `CREATE INDEX IF NOT EXISTS domain_grant_grantee_id_domain_kind_idx ON domain_grant (grantee_id, domain_kind)`
	uuidExtension              = `CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`
)

const domainGrantDeleteFunction = `
CREATE OR REPLACE FUNCTION domain_grant_before_delete() RETURNS trigger AS $$
BEGIN
	DELETE FROM domain_grant
	WHERE domain_kind = TG_ARGV[0]
	  AND domain_id = OLD.id;
	RETURN OLD;
END;
$$ LANGUAGE plpgsql`

// accountRoleDowngradeFunction revokes every token and session of an account
// whose role moves down. Roles change in SQL, so the trigger is the one place
// that sees every downgrade.
const accountRoleDowngradeFunction = `
CREATE OR REPLACE FUNCTION account_role_rank(role text) RETURNS int AS $$
	SELECT CASE role
		WHEN 'none' THEN 0
		WHEN 'pending' THEN 1
		WHEN 'viewer' THEN 2
		WHEN 'editor' THEN 3
		WHEN 'admin' THEN 4
	END
$$ LANGUAGE sql IMMUTABLE;

CREATE OR REPLACE FUNCTION account_role_downgrade() RETURNS trigger AS $$
BEGIN
	IF account_role_rank(NEW.role) < account_role_rank(OLD.role) THEN
		UPDATE api_token SET revoked_at = now()
		WHERE account_id = NEW.id AND revoked_at IS NULL;
		UPDATE browser_session SET revoked_at = now()
		WHERE account_id = NEW.id AND revoked_at IS NULL;
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql`

const accountRoleDowngradeTrigger = `
DO $$ BEGIN
	CREATE TRIGGER account_role_downgrade
	AFTER UPDATE OF role ON account
	FOR EACH ROW EXECUTE PROCEDURE account_role_downgrade();
EXCEPTION WHEN duplicate_object THEN NULL;
END $$`

// Migrate creates the auth tables. Once service, backtest, or strategy uses
// id as its primary key, it installs a delete trigger that removes that row's
// domain_grant entries. It does not assign owners and it does not change
// those primary keys. Calling it twice is safe.
func Migrate(ctx context.Context, conn postgres.Query) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if conn == nil {
		return fmt.Errorf("auth migrate: nil database")
	}

	statements := []struct {
		name string
		sql  string
	}{
		{"uuid-ossp", uuidExtension},
		{"account", accountTable},
		{"account_password", accountPasswordTable},
		{"account_github", accountGitHubTable},
		{"browser_session", browserSessionTable},
		{"api_token", apiTokenTable},
		{"api_token_scope", apiTokenScopeColumn},
		{"api_token_scope_check", apiTokenScopeCheck},
		{"api_token_client", apiTokenClient},
		{"cli_auth_code", cliAuthCodeTable},
		{"oauth_state", oauthStateTable},
		{"oauth_state_verifier", oauthStateVerifier},
		{"domain_grant", domainGrantTable},
		{"api_token_account_id_idx", apiTokenAccountIndex},
		{"api_token_expires_at_idx", apiTokenExpiresIndex},
		{"cli_auth_code_expires_at_idx", cliAuthCodeExpiresIndex},
		{"browser_session_account_id_idx", browserSessionIndex},
		{"browser_session_last_seen_at", browserSessionLastSeen},
		{"browser_session_expires_at_idx", browserSessionExpiresIndex},
		{"oauth_state_expires_at_idx", oauthStateExpiresIndex},
		{"domain_grant_grantee_id_domain_kind_idx", domainGrantGranteeIndex},
		{"domain_grant_before_delete", domainGrantDeleteFunction},
		{"account_role_downgrade", accountRoleDowngradeFunction},
		{"account_role_downgrade_trigger", accountRoleDowngradeTrigger},
	}
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement.sql); err != nil {
			return fmt.Errorf("auth migrate %s: %w", statement.name, err)
		}
	}
	return installDomainGrantTriggers(ctx, conn)
}

// installDomainGrantTriggers deletes grant rows in the same transaction as the
// domain row. One domain_id cannot reference three tables, so the trigger is
// the cascade. It waits until id is the primary key.
func installDomainGrantTriggers(ctx context.Context, conn postgres.Query) error {
	for _, table := range []string{DomainService, DomainBacktest, DomainStrategy} {
		if err := installDomainGrantTrigger(ctx, conn, table); err != nil {
			return err
		}
	}
	return nil
}

func installDomainGrantTrigger(ctx context.Context, conn postgres.Query, table string) error {
	exists, err := TableExists(ctx, conn, table)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	columns, err := PrimaryKeyColumns(ctx, conn, table)
	if err != nil {
		return err
	}
	if !SameColumns(columns, []string{"id"}) {
		return nil
	}

	var statement string
	switch table {
	case DomainService:
		statement = `DO $$ BEGIN
			CREATE TRIGGER domain_grant_delete_service
			BEFORE DELETE ON service
			FOR EACH ROW EXECUTE PROCEDURE domain_grant_before_delete('service');
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$;`
	case DomainBacktest:
		statement = `DO $$ BEGIN
			CREATE TRIGGER domain_grant_delete_backtest
			BEFORE DELETE ON backtest
			FOR EACH ROW EXECUTE PROCEDURE domain_grant_before_delete('backtest');
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$;`
	case DomainStrategy:
		statement = `DO $$ BEGIN
			CREATE TRIGGER domain_grant_delete_strategy
			BEFORE DELETE ON strategy
			FOR EACH ROW EXECUTE PROCEDURE domain_grant_before_delete('strategy');
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$;`
	default:
		return fmt.Errorf("domain_grant: unsupported table %s", table)
	}
	if _, err = conn.Exec(ctx, statement); err != nil {
		return fmt.Errorf("domain_grant: delete trigger on %s: %w", table, err)
	}
	return nil
}
