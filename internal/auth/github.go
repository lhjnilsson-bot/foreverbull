package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"

	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/web"
)

const (
	oauthCookie    = "fb_oauth_state"
	oauthLifetime  = 10 * time.Minute
	githubUserAPI  = "https://api.github.com/user"
	githubTimeout  = 10 * time.Second
	oauthStateSize = 32
)

type githubIdentity struct {
	Subject string
	Login   string
	token   *oauth2.Token
}

// githubExchange and githubMember are replaced in tests. Production calls GitHub.
var (
	githubExchange = exchangeGitHub
	githubMember   = githubOrgMember
	// githubMembershipAPI is the org membership endpoint prefix. Tests point
	// it at a fake server.
	githubMembershipAPI = "https://api.github.com/user/memberships/orgs/" //nolint: gochecknoglobals
)

func (h *handlers) githubStart(w http.ResponseWriter, r *http.Request) {
	if !githubConfigured() {
		web.NotFound(w)
		return
	}
	raw := make([]byte, oauthStateSize)
	if _, err := rand.Read(raw); err != nil {
		web.WriteError(w, err)
		return
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	verifier := oauth2.GenerateVerifier()
	sum := sha256.Sum256([]byte(state))
	_, err := h.db.Exec(r.Context(), `
		INSERT INTO oauth_state (state_hash, expires_at, verifier) VALUES ($1, $2, $3)`,
		sum[:], h.now().Add(oauthLifetime), verifier)
	if err != nil {
		web.WriteError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oauthCookie,
		Value:    state,
		Path:     "/api/auth/github",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(r),
		MaxAge:   int(oauthLifetime / time.Second),
	})
	http.Redirect(w, r, oauthConfig().AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (h *handlers) githubCallback(w http.ResponseWriter, r *http.Request) {
	if !githubConfigured() {
		web.NotFound(w)
		return
	}
	cookie, err := r.Cookie(oauthCookie)
	if cookie != nil {
		clearOAuthCookie(w, r)
	}
	if err != nil || cookie.Value == "" || cookie.Value != r.URL.Query().Get("state") {
		h.logf("github callback result=unauthorized")
		web.Unauthorized(w)
		return
	}
	sum := sha256.Sum256([]byte(cookie.Value))
	var verifier string
	err = h.db.QueryRow(r.Context(), `
		DELETE FROM oauth_state
		WHERE state_hash = $1 AND expires_at > $2
		RETURNING verifier`, sum[:], h.now()).Scan(&verifier)
	if err != nil || verifier == "" {
		h.logf("github callback result=unauthorized")
		web.Unauthorized(w)
		return
	}
	identity, err := githubExchange(r.Context(), r.URL.Query().Get("code"), verifier)
	if err != nil || identity.Subject == "" || identity.Login == "" {
		h.logf("github callback result=unauthorized")
		web.Unauthorized(w)
		return
	}
	allowed, err := githubOrgAllowed(r.Context(), identity)
	if err != nil {
		// Access stays denied. The status says GitHub could not be asked,
		// not that the account is outside the allowed orgs.
		h.logf("github callback result=org_check_failed login=%s: %v", identity.Login, err)
		web.StatusJSON(w, http.StatusBadGateway, `{"error":"github organization membership could not be checked"}`)
		return
	}
	if !allowed {
		h.logf("github callback result=unauthorized")
		web.Unauthorized(w)
		return
	}
	account, err := h.upsertGitHub(r.Context(), identity)
	if err != nil {
		web.WriteError(w, err)
		return
	}
	value, err := OpenSession(r.Context(), h.db, account.ID, h.now())
	if err != nil {
		web.WriteError(w, err)
		return
	}
	SetSessionCookie(w, r, value, h.now())
	h.logf("github callback result=ok account=%s", account.ID)
	http.Redirect(w, r, "/", http.StatusFound)
}

func clearOAuthCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     oauthCookie,
		Value:    "",
		Path:     "/api/auth/github",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(r),
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
}

func (h *handlers) upsertGitHub(ctx context.Context, identity githubIdentity) (Account, error) {
	var account Account
	err := h.db.QueryRow(ctx, `
		SELECT a.id, a.role
		FROM account_github AS g
		JOIN account AS a ON a.id = g.account_id
		WHERE g.github_subject = $1`, identity.Subject).Scan(&account.ID, &account.Role)
	if err == nil {
		// A GitHub login can change. Grants and owner lookups match on it,
		// so a stale login could point them at the wrong account.
		if _, err := h.db.Exec(ctx, `UPDATE account_github SET github_login = $2 WHERE account_id = $1`, account.ID, identity.Login); err != nil {
			return Account{}, fmt.Errorf("refresh github login: %w", err)
		}
		return account, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Account{}, fmt.Errorf("lookup github account: %w", err)
	}
	starter, ok := h.db.(txBeginner)
	if !ok || starter == nil {
		return Account{}, errors.New("github account database cannot begin")
	}
	tx, err := starter.Begin(ctx)
	if err != nil {
		return Account{}, fmt.Errorf("begin github account: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = tx.QueryRow(ctx, `INSERT INTO account (role) VALUES ('pending') RETURNING id`).Scan(&account.ID)
	if err != nil {
		return Account{}, fmt.Errorf("insert github account: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO account_github (account_id, github_subject, github_login)
		VALUES ($1, $2, $3)`, account.ID, identity.Subject, identity.Login)
	if err != nil {
		return Account{}, fmt.Errorf("insert github identity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, fmt.Errorf("commit github account: %w", err)
	}
	account.Role = RolePending
	return account, nil
}

type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

func oauthConfig() *oauth2.Config {
	scopes := []string{"read:user"}
	if strings.TrimSpace(environment.GetAuthGitHubAllowedOrgs()) != "" {
		scopes = append(scopes, "read:org")
	}
	return &oauth2.Config{
		ClientID:     environment.GetAuthGitHubClientID(),
		ClientSecret: environment.GetAuthGitHubClientSecret(),
		RedirectURL:  environment.GetAuthGitHubRedirectURL(),
		Endpoint:     github.Endpoint,
		Scopes:       scopes,
	}
}

// githubOrgAllowed reports whether the identity is an active member of one
// allowed org. An error means GitHub could not answer for some org and no
// other org admitted the identity; the caller still denies access.
func githubOrgAllowed(ctx context.Context, identity githubIdentity) (bool, error) {
	orgs := allowedOrgs()
	if len(orgs) == 0 {
		return true, nil
	}
	var errs []error
	for _, org := range orgs {
		member, err := githubMember(ctx, identity.token, org)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if member {
			return true, nil
		}
	}
	return false, errors.Join(errs...)
}

func allowedOrgs() []string {
	raw := strings.TrimSpace(environment.GetAuthGitHubAllowedOrgs())
	if raw == "" {
		return nil
	}
	var orgs []string
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field != "" {
			orgs = append(orgs, field)
		}
	}
	return orgs
}

// githubOrgMember asks GitHub for the caller's membership in org. 200 with
// state active is a member. 200 with another state and 404 are answers: not
// a member. Every other outcome (network error, 5xx, 401, 403, an unreadable
// body) is an error, because GitHub did not say.
func githubOrgMember(ctx context.Context, tok *oauth2.Token, org string) (bool, error) {
	if tok == nil {
		return false, errors.New("github token is missing")
	}
	if org == "" || strings.ContainsAny(org, "/?#") {
		return false, fmt.Errorf("github org %q is not a valid org name", org)
	}
	ctx, cancel := context.WithTimeout(ctx, githubTimeout)
	defer cancel()
	endpoint := githubMembershipAPI + url.PathEscape(org)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, fmt.Errorf("github org %s membership request: %w", org, err)
	}
	resp, err := oauthConfig().Client(ctx, tok).Do(req)
	if err != nil {
		return false, fmt.Errorf("github org %s membership: %w", org, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, fmt.Errorf("github org %s membership body: %w", org, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("github org %s membership status %d", org, resp.StatusCode)
	}
	var payload struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, fmt.Errorf("github org %s membership body is not JSON: %w", org, err)
	}
	return payload.State == "active", nil
}

func exchangeGitHub(ctx context.Context, code, verifier string) (githubIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, githubTimeout)
	defer cancel()
	if verifier == "" {
		return githubIdentity{}, errors.New("github verifier is empty")
	}
	tok, err := oauthConfig().Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return githubIdentity{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubUserAPI, nil)
	if err != nil {
		return githubIdentity{}, err
	}
	resp, err := oauthConfig().Client(ctx, tok).Do(req)
	if err != nil {
		return githubIdentity{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return githubIdentity{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return githubIdentity{}, fmt.Errorf("github user status %d", resp.StatusCode)
	}
	var payload struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return githubIdentity{}, err
	}
	if payload.ID == 0 || payload.Login == "" {
		return githubIdentity{}, errors.New("github user is incomplete")
	}
	return githubIdentity{
		Subject: strconv.FormatInt(payload.ID, 10),
		Login:   payload.Login,
		token:   tok,
	}, nil
}
