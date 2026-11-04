package auth

import (
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/web"
)

const (
	// cliFormCap bounds the Approve form. Its fields are under 1 KiB.
	cliFormCap = 4 << 10
	// cliExchangeKey is the limiter's username slot for the code exchange.
	cliExchangeKey = "\x00cli-token"
)

var cliPage = template.Must(template.New("cli").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>Let fbull sign in?</title>
<style>
body{font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;line-height:1.5}
code{background:#f2f2f2;padding:0 .25rem}
button{font-size:1rem;padding:.4rem 1rem;margin-right:.5rem}
</style>
</head>
<body>
{{if .SignIn}}
<h1>Sign in to foreverbull first</h1>
<p>fbull is asking for access to your account. <a href="/">Sign in to foreverbull</a> in this browser, then reload this page.</p>
{{else if .Refused}}
<h1>fbull can't sign in</h1>
<p>{{.Refused}}</p>
{{else}}
<h1>Let fbull sign in?</h1>
<p>fbull on <code>{{.Host}}</code> is asking for <strong>{{.Scope}}</strong> access as <strong>{{.Login}}</strong>.</p>
<p>Approve only if you just ran <code>fbull login</code> on that computer.</p>
<form method="post" action="/cli/authorize">
<input type="hidden" name="client" value="{{.Request.Client}}">
<input type="hidden" name="redirect_uri" value="{{.Request.RedirectURI}}">
<input type="hidden" name="state" value="{{.Request.State}}">
<input type="hidden" name="code_challenge" value="{{.Request.CodeChallenge}}">
<input type="hidden" name="code_challenge_method" value="S256">
<input type="hidden" name="scope" value="{{.Request.Scope}}">
<button type="submit" name="decision" value="approve">Approve</button>
<button type="submit" name="decision" value="deny">Deny</button>
</form>
{{end}}
</body>
</html>
`))

// The page's refusal lines. Each ends with what to do next.
const (
	cliPageInvalidLink = "This sign-in link isn't valid. Run `fbull login` again."
	cliPageNotApproved = "Your account isn't approved yet, so fbull can't sign in with it. " +
		"Ask whoever runs this foreverbull to approve your account, then run `fbull login` again."
)

type cliPageData struct {
	SignIn  bool
	Refused string
	Request CLIRequest
	Host    string
	Scope   string
	Login   string
}

func writeCLIPage(w http.ResponseWriter, code int, data cliPageData) {
	// The page asks for an Approve click, so it must not be framed.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", cliPageCSP(data.Request.RedirectURI))
	w.WriteHeader(code)
	_ = cliPage.Execute(w, data)
}

// cliPageCSP allows the form to post to the broker and the redirect that
// follows Approve or Deny to reach the CLI listener. Chrome and Safari apply
// form-action to that redirect, so without the listener's origin the browser
// stops there. redirectURI is empty or the canonical form cliRedirect checked.
func cliPageCSP(redirectURI string) string {
	formAction := "'self'"
	if origin := strings.TrimSuffix(redirectURI, cliRedirectPath); origin != "" {
		formAction += " " + origin
	}
	return "default-src 'none'; style-src 'unsafe-inline'; form-action " + formAction + "; frame-ancestors 'none'"
}

// cliAuthorizePage shows the client and the scope. A GET never issues a code.
func (h *handlers) cliAuthorizePage(w http.ResponseWriter, r *http.Request) {
	req, err := ParseCLIRequest(r.URL.Query())
	if err != nil {
		writeCLIPage(w, http.StatusBadRequest, cliPageData{
			Refused: cliPageInvalidLink,
		})
		return
	}
	account, err := AccountFromRequest(r.Context(), h.db, r, h.now())
	if errors.Is(err, ErrUnauthorized) {
		writeCLIPage(w, http.StatusOK, cliPageData{SignIn: true})
		return
	}
	if err != nil {
		web.WriteError(w, err)
		return
	}
	scope, err := CLIScope(account.Role, req.Scope)
	if err != nil {
		writeCLIPage(w, http.StatusForbidden, cliPageData{
			Refused: cliPageNotApproved,
		})
		return
	}
	login, _, err := accountLogin(r.Context(), h.db, account.ID)
	if err != nil {
		web.WriteError(w, err)
		return
	}
	writeCLIPage(w, http.StatusOK, cliPageData{
		Request: req, Host: strings.TrimPrefix(req.Client, cliClientPrefix), Scope: scope, Login: login,
	})
}

// cliAuthorizeApprove runs behind Protect, which has checked the session
// cookie and refused a cross-site POST. It redirects to the CLI listener with
// a code on Approve and with error=access_denied on Deny.
func (h *handlers) cliAuthorizeApprove(w http.ResponseWriter, r *http.Request) {
	account, ok := FromContext(r.Context())
	if !ok {
		web.Unauthorized(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, cliFormCap)
	if err := r.ParseForm(); err != nil {
		web.BadRequest(w)
		return
	}
	req, err := ParseCLIRequest(r.PostForm)
	if err != nil {
		web.BadRequest(w)
		return
	}
	query := url.Values{"state": {req.State}}
	switch r.PostForm.Get("decision") {
	case "approve":
		code, err := IssueCLICode(r.Context(), h.db, account, req, h.now())
		if err != nil {
			WriteHTTP(w, err)
			return
		}
		query.Set("code", code)
		h.logf("cli authorize account=%s client=%s result=approved", account.ID, req.Client)
	case "deny":
		query.Set("error", "access_denied")
		h.logf("cli authorize account=%s client=%s result=denied", account.ID, req.Client)
	default:
		web.BadRequest(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, req.RedirectURI+"?"+query.Encode(), http.StatusSeeOther)
}

// cliToken exchanges a code and its verifier for a token. The token is in the
// response body only. Plain HTTP is refused unless TLS terminates in front of
// the broker or the request is to a loopback or Kind host.
func (h *handlers) cliToken(w http.ResponseWriter, r *http.Request) {
	if !cliTransportAllowed(r) {
		web.StatusJSON(w, http.StatusForbidden, `{"error":"https required"}`)
		return
	}
	var body struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
		RedirectURI  string `json:"redirect_uri"`
	}
	if err := web.ReadJSON(r, &body); err != nil {
		web.BadRequest(w)
		return
	}
	remote := clientAddr(r)
	if !h.cliLimiter.Begin(cliExchangeKey, remote, h.now()) {
		web.StatusJSON(w, http.StatusTooManyRequests, `{"error":"too many requests"}`)
		return
	}
	// Every attempt counts toward the address cap. A failed one adds no
	// backoff: codes are 256 bits, and a reused code already revokes.
	defer h.cliLimiter.End(cliExchangeKey, remote, false, h.now())
	token, claim, err := ExchangeCLICode(r.Context(), h.db, body.Code, body.CodeVerifier, body.RedirectURI, h.now())
	accountID := claim.AccountID
	// Only the first claim of a live code counts against the account, so a
	// leaked code replayed over and over can't lock the account out.
	if claim.First {
		if !h.cliAccountLimiter.Begin(cliExchangeKey, accountID, h.now()) {
			h.refuseLimitedAccount(w, r, accountID, token, err)
			return
		}
		h.cliAccountLimiter.End(cliExchangeKey, accountID, false, h.now())
	}
	if err != nil {
		h.writeExchangeError(w, accountID, err)
		return
	}
	login, _, err := accountLogin(r.Context(), h.db, accountID)
	if err != nil {
		h.writeExchangeError(w, accountID, errors.Join(err, revokeTokenID(r.Context(), h.db, token.ID, h.now())))
		return
	}
	h.logf("cli token account=%s token=%s client=%s result=issued", accountID, token.ID, token.Client)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{
		"login":      login,
		"token":      token.Secret,
		"scope":      token.Scope,
		"client":     token.Client,
		"expires_at": formatTime(token.ExpiresAt),
	})
}

// refuseLimitedAccount answers 429 for an account over the exchange limit. A
// token the exchange already issued is revoked first; a failed revoke is a 500.
func (h *handlers) refuseLimitedAccount(w http.ResponseWriter, r *http.Request, accountID string, token CLIToken, err error) {
	if err == nil {
		if revokeErr := revokeTokenID(r.Context(), h.db, token.ID, h.now()); revokeErr != nil {
			h.logf("cli token account=%s result=limited_revoke_failed err=%s", accountID, revokeErrClass(revokeErr))
			web.WriteError(w, revokeErr)
			return
		}
	}
	h.logf("cli token account=%s result=limited", accountID)
	web.StatusJSON(w, http.StatusTooManyRequests, `{"error":"too many requests"}`)
}

func (h *handlers) writeExchangeError(w http.ResponseWriter, accountID string, err error) {
	switch {
	case errors.Is(err, ErrInvalidGrant):
		h.logf("cli token account=%s result=invalid_grant", accountID)
		web.StatusJSON(w, http.StatusBadRequest, `{"error":"invalid_grant"}`)
	case errors.Is(err, ErrForbidden):
		h.logf("cli token account=%s result=forbidden", accountID)
		web.Forbidden(w)
	default:
		h.logf("cli token account=%s result=error err=%s", accountID, revokeErrClass(err))
		web.WriteError(w, err)
	}
}

// cliTransportAllowed is true over TLS, behind a TLS proxy (AUTH_COOKIE_SECURE),
// or when the Host is 127.0.0.1 or localhost, which is the Kind host port.
// Host is the caller's header, so this only catches a misconfigured client;
// fbull refuses plain HTTP to any other host before it sends the code.
func cliTransportAllowed(r *http.Request) bool {
	if r.TLS != nil || environment.GetAuthCookieSecure() {
		return true
	}
	return PlainHTTPHost(r.Host)
}

// PlainHTTPHost is true for 127.0.0.1, ::1, and localhost (the Kind host
// port mapping). The port is ignored.
func PlainHTTPHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	switch strings.ToLower(host) {
	case "127.0.0.1", "::1", "localhost":
		return true
	default:
		return false
	}
}
