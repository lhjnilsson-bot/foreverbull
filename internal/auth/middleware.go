package auth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
	"github.com/lhjnilsson/foreverbull/internal/web"
	"github.com/rs/zerolog/log"
)

// Protect refuses /api routes that are not anonymous unless the cookie is valid.
// It is installed only when AUTH_ENABLED is true. While the flag is false it
// serves the next handler unchanged.
func Protect(db postgres.Query) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !environment.GetAuthEnabled() {
				next.ServeHTTP(w, r)
				return
			}
			if anonymousRequest(r) {
				next.ServeHTTP(w, r)
				return
			}
			if r.Method == http.MethodPost && r.URL.Path == cliRevokePath {
				protectBearer(db, w, r, next)
				return
			}
			account, err := AccountFromRequest(r.Context(), db, r, time.Now())
			if errors.Is(err, ErrUnauthorized) {
				ClearSessionCookie(w, r)
				web.Unauthorized(w)
				return
			}
			if err != nil {
				// A failed lookup is not a bad cookie: keep the cookie, so a
				// database fault doesn't sign the user out, and answer 500.
				log.Error().Err(err).Str("path", r.URL.Path).Msg("session lookup failed")
				web.WriteError(w, err)
				return
			}
			r = r.WithContext(WithAccount(r.Context(), account))
			if !mutationAllowed(r) {
				web.Forbidden(w)
				return
			}
			if anyRoleRequest(r) {
				next.ServeHTTP(w, r)
				return
			}
			if account.Role == RoleNone || account.Role == RolePending {
				web.Forbidden(w)
				return
			}
			if account.Role == RoleViewer && r.Method != http.MethodGet && r.Method != http.MethodHead {
				web.Forbidden(w)
				return
			}
			if account.Role != RoleViewer && account.Role != RoleEditor && account.Role != RoleAdmin {
				web.Forbidden(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func anonymousRequest(r *http.Request) bool {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/api/health":
		return true
	case r.Method == http.MethodGet && path == "/api/auth/providers":
		return true
	case r.Method == http.MethodPost && path == "/api/auth/login":
		return true
	case r.Method == http.MethodGet && path == "/api/auth/github/start":
		return true
	case r.Method == http.MethodGet && path == "/api/auth/github/callback":
		return true
	case r.Method == http.MethodPost && path == "/api/auth/cli/token":
		// The code and its verifier are the credential.
		return true
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && !underAPI(path):
		return true
	default:
		return false
	}
}

func anyRoleRequest(r *http.Request) bool {
	path := r.URL.Path
	if r.Method == http.MethodGet && path == "/api/auth/me" {
		return true
	}
	if r.Method == http.MethodPost && path == "/cli/authorize" {
		// A viewer may approve a read token. The handler refuses none and pending.
		return true
	}
	if r.Method == http.MethodPost && path == "/api/auth/revoke-all" {
		return true
	}
	return r.Method == http.MethodPost && path == "/api/auth/logout"
}

func underAPI(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/")
}

// mutationAllowed rejects a cookie-authenticated write from another site.
// SameSite=Lax still sends the cookie on a top-level navigation from a sibling.
// A bearer token is not a browser cookie, so it skips this check. A write with
// neither Sec-Fetch-Site nor a matching Origin is refused.
func mutationAllowed(r *http.Request) bool {
	if r == nil {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if strings.HasPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer ") {
		return true
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	}
	return originMatchesPublic(r.Header.Get("Origin"))
}

func originMatchesPublic(origin string) bool {
	origin = strings.TrimSpace(origin)
	configured := strings.TrimSpace(environment.GetAuthPublicURL())
	if origin == "" || configured == "" {
		return false
	}
	got, err := url.Parse(origin)
	want, wantErr := url.Parse(configured)
	if err != nil || wantErr != nil || got.Host == "" || want.Host == "" || got.Scheme == "" || want.Scheme == "" {
		return false
	}
	return strings.EqualFold(got.Scheme, want.Scheme) && strings.EqualFold(got.Host, want.Host)
}
