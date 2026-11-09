package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestHTTP(t *testing.T) {
	t.Run("health", func(t *testing.T) {
		mux := NewMux()
		req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		require.Equal(t, `{"ok":true}`, rec.Body.String())
	})

	t.Run("config is the grafana origin", func(t *testing.T) {
		t.Setenv(environment.GrafanaURL, "https://example.com/grafana/")
		mux := NewMux()
		req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.JSONEq(t, `{"grafanaUrl":"https://example.com/grafana"}`, rec.Body.String())
	})

	t.Run("config drops credentials and hides an unusable origin", func(t *testing.T) {
		t.Setenv(environment.GrafanaURL, "http://user:hunter2@localhost:3000/grafana?token=secret")
		mux := NewMux()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		require.JSONEq(t, `{"grafanaUrl":"http://localhost:3000/grafana"}`, rec.Body.String())
		require.NotContains(t, rec.Body.String(), "hunter2")
		require.NotContains(t, rec.Body.String(), "secret")

		t.Setenv(environment.GrafanaURL, "not a url")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		require.JSONEq(t, `{"grafanaUrl":""}`, rec.Body.String())
		require.NotContains(t, rec.Body.String(), "not a url")
	})

	t.Run("health rejects other methods", func(t *testing.T) {
		mux := NewMux()
		req := httptest.NewRequest(http.MethodPost, "/api/health", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	})

	t.Run("listen address uses HTTP_PORT and does not listen", func(t *testing.T) {
		t.Setenv(environment.HTTPPort, "18080")
		require.Equal(t, ":18080", listenAddress())
	})

	t.Run("not found", func(t *testing.T) {
		rec := httptest.NewRecorder()
		WriteError(rec, pgx.ErrNoRows)
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.JSONEq(t, `{"error":"not found"}`, rec.Body.String())

		rec = httptest.NewRecorder()
		WriteError(rec, status.Error(codes.NotFound, "missing"))
		require.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("internal error hides the cause", func(t *testing.T) {
		rec := httptest.NewRecorder()
		WriteError(rec, errors.New("postgres password=secret"))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
		require.NotContains(t, rec.Body.String(), "secret")
	})

	t.Run("bad request hides the cause", func(t *testing.T) {
		rec := httptest.NewRecorder()
		BadRequest(rec)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.JSONEq(t, `{"error":"bad request"}`, rec.Body.String())
		require.NotContains(t, rec.Body.String(), "secret")
	})

	t.Run("read json", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"image":"algo:local"}`))
		var body struct {
			Image string `json:"image"`
		}
		require.NoError(t, ReadJSON(req, &body))
		require.Equal(t, "algo:local", body.Image)
	})

	t.Run("read json refuses an unknown field", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"image":"algo:local","token":"secret"}`))
		var body struct {
			Image string `json:"image"`
		}
		err := ReadJSON(req, &body)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
	})

	t.Run("page", func(t *testing.T) {
		mux := NewMux()
		for _, path := range []string{"/", "/index.html", "/overview", "/docs", "/services", "/backtests/nasdaq100", "/strategies/mean-rev"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code, path)
			require.Contains(t, rec.Header().Get("Content-Type"), "text/html")
			require.Contains(t, rec.Body.String(), `data-shell="stub"`)
			require.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
		}
	})

	t.Run("asset", func(t *testing.T) {
		mux := NewMux()
		req := httptest.NewRequest(http.MethodGet, "/assets/mark.css", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Header().Get("Content-Type"), "text/css")
		require.Equal(t, "body{color:#1c1915}\n", rec.Body.String())
		require.Contains(t, rec.Header().Get("Cache-Control"), "immutable")
	})

	t.Run("missing asset is not the page", func(t *testing.T) {
		mux := NewMux()
		req := httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		require.Equal(t, http.StatusNotFound, rec.Code)
		require.NotContains(t, rec.Body.String(), "Foreverbull")
	})

	t.Run("unknown api stays json", func(t *testing.T) {
		mux := NewMux()
		req := httptest.NewRequest(http.MethodGet, "/api/missing", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
		require.JSONEq(t, `{"error":"not found"}`, rec.Body.String())
	})

	t.Run("post page is refused", func(t *testing.T) {
		mux := NewMux()
		req := httptest.NewRequest(http.MethodPost, "/services", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	})
}
