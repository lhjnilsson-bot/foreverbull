package auth

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/stretchr/testify/require"
)

// A session lookup that fails on the database is a 500 that keeps the
// cookie. Only a bad cookie is a 401 that clears it.
func TestProtectKeepsCookieOnLookupFailure(t *testing.T) {
	t.Setenv(environment.AuthEnabled, "true")
	cookie := "session-1." + strings.Repeat("A", 43)
	db := test_helper.FailingRowsConn{Err: errors.New("connection lost")}
	reached := false
	handler := Protect(db)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	rec := authed(handler, http.MethodGet, "/api/backtests", "", cookie)
	require.False(t, reached)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
	require.Empty(t, rec.Result().Cookies())

	rec = authed(handler, http.MethodGet, "/api/backtests", "", "not-a-session")
	require.False(t, reached)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.NotEmpty(t, rec.Result().Cookies())
}
