package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/stretchr/testify/require"
)

// rowDB answers QueryRow with one row of values and Exec with execErr.
type rowDB struct {
	panicDB
	values  []any
	execErr error
}

type valuesRow []any

func (r valuesRow) Scan(dest ...any) error {
	for i, d := range dest {
		target := reflect.ValueOf(d).Elem()
		if r[i] == nil {
			target.Set(reflect.Zero(target.Type()))
			continue
		}
		target.Set(reflect.ValueOf(r[i]))
	}
	return nil
}

func (d rowDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return valuesRow(d.values)
}

func (d rowDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, d.execErr
}

// The write after a successful lookup is part of the request: when it
// fails, the request fails with that cause instead of carrying on.
func TestAccountUseWriteFails(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	broken := errors.New("connection lost")

	t.Run("bearer token use", func(t *testing.T) {
		// id, role, scope, token id, client, created_at, last_used_at, revoked_at, expires_at
		db := rowDB{values: []any{"acc-1", RoleEditor, ScopeWrite, "tok-1", nil, nil, nil, nil, nil}, execErr: broken}
		_, err := AccountFromBearer(ctx, db, tokenPrefix+"secret", now)
		require.ErrorIs(t, err, broken)
		require.ErrorIs(t, err, ErrUseNotRecorded)
		require.EqualError(t, err, "record token use: connection lost")
	})

	t.Run("browser session use", func(t *testing.T) {
		raw := make([]byte, sessionSecretLen)
		sum := sha256.Sum256(raw)
		value := "session-1." + base64.RawURLEncoding.EncodeToString(raw)
		db := rowDB{values: []any{"acc-1", RoleEditor, sum[:], now.Add(time.Hour), nil}, execErr: broken}
		_, err := accountFromCookie(ctx, db, value, now)
		require.ErrorIs(t, err, broken)
		require.ErrorIs(t, err, ErrUseNotRecorded)
		require.EqualError(t, err, "record session use: connection lost")
	})
}

// A login refresh that can't be written fails the sign-in with the cause.
func TestGitHubLoginRefreshFails(t *testing.T) {
	ctx := context.Background()
	broken := errors.New("connection lost")
	h := &handlers{db: rowDB{values: []any{"acc-1", RoleEditor}, execErr: broken}}
	_, err := h.upsertGitHub(ctx, githubIdentity{Subject: "42", Login: "renamed"})
	require.ErrorIs(t, err, broken)
	require.EqualError(t, err, "refresh github login: connection lost")
}

// Protect stops a request whose session use can't be recorded: 500, the
// handler doesn't run, and the cookie stays.
func TestProtectSessionUseWriteFails(t *testing.T) {
	t.Setenv(environment.AuthEnabled, "true")
	raw := make([]byte, sessionSecretLen)
	sum := sha256.Sum256(raw)
	db := rowDB{
		values:  []any{"acc-1", RoleEditor, sum[:], time.Now().Add(time.Hour), nil},
		execErr: errors.New("connection lost"),
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("the handler ran after the session use wasn't recorded")
	})
	req := httptest.NewRequest(http.MethodGet, "/api/backtests", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(req), Value: "session-1." + base64.RawURLEncoding.EncodeToString(raw)})
	rec := httptest.NewRecorder()

	Protect(db)(next).ServeHTTP(rec, req)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Empty(t, rec.Result().Cookies())
	require.NotContains(t, rec.Body.String(), "connection lost")
}
