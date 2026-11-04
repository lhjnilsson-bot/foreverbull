package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lhjnilsson/foreverbull/internal/postgres"
)

const (
	// cliCodeTTL is how long an approved code can be exchanged.
	cliCodeTTL = 60 * time.Second
	// cliCodeLen is the random byte count of one code.
	cliCodeLen = 32
	// cliClientPrefix starts every CLI client name: cli@<host>.
	cliClientPrefix = "cli@"
	// cliClientMax matches the api_token name and client columns.
	cliClientMax = 64
	// cliStateMin and cliStateMax bound the CLI's state string. fbull sends
	// 32 random bytes, base64url, which is 43 characters.
	cliStateMin = 22
	cliStateMax = 128
	// cliChallengeLen is base64url of a SHA-256 digest without padding.
	cliChallengeLen = 43
	// cliVerifierMin and cliVerifierMax are RFC 7636 section 4.1.
	cliVerifierMin = 43
	cliVerifierMax = 128
	// cliRedirectHost is the only callback host. "localhost" is refused: it
	// can resolve to another address, and the CLI binds 127.0.0.1 only.
	cliRedirectHost = "127.0.0.1"
	cliRedirectPath = "/callback"
	// cliPortMin keeps the callback off privileged ports.
	cliPortMin = 1024
	cliPortMax = 65535
)

// ErrInvalidGrant is a code that is unknown, used, expired, or does not match
// its verifier or redirect. The response does not say which.
var ErrInvalidGrant = errors.New("invalid grant")

// CLIRequest is the validated query of /cli/authorize.
type CLIRequest struct {
	Client        string
	RedirectURI   string
	State         string
	CodeChallenge string
	Scope         string
}

// CLIToken is the result of a code exchange. Secret is the bearer, returned once.
type CLIToken struct {
	ID        string
	Client    string
	Scope     string
	Secret    string
	ExpiresAt time.Time
}

// ParseCLIRequest validates the authorize parameters. Each field fails on its own.
func ParseCLIRequest(values url.Values) (CLIRequest, error) {
	client, err := cliClient(values.Get("client"))
	if err != nil {
		return CLIRequest{}, err
	}
	redirect, err := cliRedirect(values.Get("redirect_uri"))
	if err != nil {
		return CLIRequest{}, err
	}
	state := values.Get("state")
	if len(state) < cliStateMin || len(state) > cliStateMax || !base64URLChars(state) {
		return CLIRequest{}, fmt.Errorf("state: %w", ErrBadRequest)
	}
	challenge := values.Get("code_challenge")
	if len(challenge) != cliChallengeLen || !base64URLChars(challenge) {
		return CLIRequest{}, fmt.Errorf("code_challenge: %w", ErrBadRequest)
	}
	if values.Get("code_challenge_method") != "S256" {
		return CLIRequest{}, fmt.Errorf("code_challenge_method: %w", ErrBadRequest)
	}
	scope := values.Get("scope")
	if scope != ScopeRead && scope != ScopeWrite {
		return CLIRequest{}, fmt.Errorf("scope: %w", ErrBadRequest)
	}
	return CLIRequest{
		Client: client, RedirectURI: redirect, State: state, CodeChallenge: challenge, Scope: scope,
	}, nil
}

func cliClient(raw string) (string, error) {
	if !strings.HasPrefix(raw, cliClientPrefix) || len(raw) > cliClientMax {
		return "", fmt.Errorf("client: %w", ErrBadRequest)
	}
	host := raw[len(cliClientPrefix):]
	if host == "" {
		return "", fmt.Errorf("client: %w", ErrBadRequest)
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_' {
			continue
		}
		return "", fmt.Errorf("client: %w", ErrBadRequest)
	}
	return raw, nil
}

// cliRedirect accepts exactly http://127.0.0.1:<port>/callback. The string
// must equal its own canonical form, so a second spelling of the same URL
// cannot be bound to a code.
func cliRedirect(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("redirect_uri: %w", ErrBadRequest)
	}
	if parsed.Scheme != "http" || parsed.Hostname() != cliRedirectHost || parsed.Path != cliRedirectPath {
		return "", fmt.Errorf("redirect_uri: %w", ErrBadRequest)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", fmt.Errorf("redirect_uri: %w", ErrBadRequest)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < cliPortMin || port > cliPortMax {
		return "", fmt.Errorf("redirect_uri port: %w", ErrBadRequest)
	}
	canonical := fmt.Sprintf("http://%s:%d%s", cliRedirectHost, port, cliRedirectPath)
	if raw != canonical {
		return "", fmt.Errorf("redirect_uri: %w", ErrBadRequest)
	}
	return canonical, nil
}

func base64URLChars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// CLIScope caps the requested scope by the role. A viewer gets read. An
// account that is none or pending cannot approve a CLI sign-in.
func CLIScope(role, requested string) (string, error) {
	switch role {
	case RoleEditor, RoleAdmin:
		return requested, nil
	case RoleViewer:
		return ScopeRead, nil
	case RoleNone, RolePending:
		return "", ErrForbidden
	default:
		return "", ErrForbidden
	}
}

// IssueCLICode stores SHA-256 of a new one-time code bound to the account,
// client, scope, challenge, and redirect, and returns the code.
func IssueCLICode(ctx context.Context, db postgres.Query, account Account, req CLIRequest, now time.Time) (string, error) {
	if account.ID == "" {
		return "", errors.New("cli code account is empty")
	}
	scope, err := CLIScope(account.Role, req.Scope)
	if err != nil {
		return "", err
	}
	raw := make([]byte, cliCodeLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("cli code: %w", err)
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	for i := range raw {
		raw[i] = 0
	}
	sum := sha256.Sum256([]byte(code))
	_, err = db.Exec(ctx, `
		INSERT INTO cli_auth_code (code_hash, account_id, client, scope, code_challenge, redirect_uri, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		sum[:], account.ID, req.Client, scope, req.CodeChallenge, req.RedirectURI, now, now.Add(cliCodeTTL))
	if err != nil {
		return "", fmt.Errorf("insert cli code: %w", err)
	}
	return code, nil
}

type cliCodeRow struct {
	accountID   string
	client      string
	scope       string
	challenge   string
	redirectURI string
	expiresAt   time.Time
}

// CLIClaim says whose code an exchange named. First is true only for the
// first claim of a code that had not expired: the attempt the caller counts
// against the account. A replay or an expired code sets AccountID alone.
type CLIClaim struct {
	AccountID string
	First     bool
}

// ExchangeCLICode turns a code into a CLI token. The first attempt marks the
// code used, whether or not it succeeds. A second attempt revokes the token
// the first one issued.
func ExchangeCLICode(
	ctx context.Context, db postgres.Query, code, verifier, redirectURI string, now time.Time,
) (CLIToken, CLIClaim, error) {
	if len(code) == 0 || len(code) > cliStateMax || !base64URLChars(code) {
		return CLIToken{}, CLIClaim{}, ErrInvalidGrant
	}
	sum := sha256.Sum256([]byte(code))
	row, err := claimCLICode(ctx, db, sum[:], now)
	if err != nil {
		return CLIToken{}, CLIClaim{AccountID: row.accountID}, err
	}
	if !row.expiresAt.After(now) {
		return CLIToken{}, CLIClaim{AccountID: row.accountID}, ErrInvalidGrant
	}
	claim := CLIClaim{AccountID: row.accountID, First: true}
	if redirectURI != row.redirectURI {
		return CLIToken{}, claim, ErrInvalidGrant
	}
	if !verifierMatches(verifier, row.challenge) {
		return CLIToken{}, claim, ErrInvalidGrant
	}
	token, err := mintCLIToken(ctx, db, row, now)
	if err != nil {
		return CLIToken{}, claim, err
	}
	var reused *time.Time
	err = db.QueryRow(ctx, `
		UPDATE cli_auth_code SET token_id = $2 WHERE code_hash = $1 RETURNING reused_at`, sum[:], token.ID).Scan(&reused)
	if err != nil {
		return CLIToken{}, claim, errors.Join(fmt.Errorf("bind cli code: %w", err), revokeTokenID(ctx, db, token.ID, now))
	}
	if reused != nil {
		if err := revokeTokenID(ctx, db, token.ID, now); err != nil {
			return CLIToken{}, claim, err
		}
		return CLIToken{}, claim, ErrInvalidGrant
	}
	return token, claim, nil
}

// claimCLICode marks an unused code used and returns it. A used code is
// marked reused and the token it issued is revoked.
func claimCLICode(ctx context.Context, db postgres.Query, hash []byte, now time.Time) (cliCodeRow, error) {
	var row cliCodeRow
	err := db.QueryRow(ctx, `
		UPDATE cli_auth_code SET used_at = $2
		WHERE code_hash = $1 AND used_at IS NULL
		RETURNING account_id, client, scope, code_challenge, redirect_uri, expires_at`, hash, now).Scan(
		&row.accountID, &row.client, &row.scope, &row.challenge, &row.redirectURI, &row.expiresAt)
	if err == nil {
		return row, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return cliCodeRow{}, fmt.Errorf("claim cli code: %w", err)
	}
	var (
		accountID string
		tokenID   *string
	)
	err = db.QueryRow(ctx, `
		UPDATE cli_auth_code SET reused_at = COALESCE(reused_at, $2)
		WHERE code_hash = $1
		RETURNING account_id, token_id`, hash, now).Scan(&accountID, &tokenID)
	if errors.Is(err, pgx.ErrNoRows) {
		return cliCodeRow{}, ErrInvalidGrant
	}
	if err != nil {
		return cliCodeRow{}, fmt.Errorf("mark cli code reused: %w", err)
	}
	if tokenID == nil {
		return cliCodeRow{accountID: accountID}, ErrInvalidGrant
	}
	if err := revokeTokenID(ctx, db, *tokenID, now); err != nil {
		return cliCodeRow{}, err
	}
	return cliCodeRow{accountID: accountID}, ErrInvalidGrant
}

// verifierMatches checks base64url(SHA-256(verifier)) against the stored
// challenge in constant time.
func verifierMatches(verifier, challenge string) bool {
	if len(verifier) < cliVerifierMin || len(verifier) > cliVerifierMax {
		return false
	}
	for i := 0; i < len(verifier); i++ {
		c := verifier[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			continue
		}
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// mintCLIToken reads the role again, because it can change between approve
// and exchange, and inserts the token with its 30-day expiry.
func mintCLIToken(ctx context.Context, db postgres.Query, row cliCodeRow, now time.Time) (CLIToken, error) {
	var role string
	err := db.QueryRow(ctx, `SELECT role FROM account WHERE id = $1`, row.accountID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return CLIToken{}, ErrInvalidGrant
	}
	if err != nil {
		return CLIToken{}, fmt.Errorf("lookup cli account: %w", err)
	}
	scope, err := CLIScope(role, row.scope)
	if err != nil {
		return CLIToken{}, err
	}
	secret, err := newTokenSecret()
	if err != nil {
		return CLIToken{}, err
	}
	sum := sha256.Sum256([]byte(secret))
	token := CLIToken{Client: row.client, Scope: scope, Secret: secret, ExpiresAt: now.Add(cliTokenLifetime)}
	err = db.QueryRow(ctx, `
		INSERT INTO api_token (account_id, token_hash, hint, name, scope, client, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`, row.accountID, sum[:], secret[:tokenHintLen], row.client, scope, row.client, now, token.ExpiresAt).
		Scan(&token.ID)
	if err != nil {
		return CLIToken{}, fmt.Errorf("insert cli token: %w", err)
	}
	return token, nil
}

func revokeTokenID(ctx context.Context, db postgres.Query, id string, now time.Time) error {
	if _, err := db.Exec(ctx, `
		UPDATE api_token SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, now); err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return nil
}
