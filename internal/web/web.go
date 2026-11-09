package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"go.uber.org/fx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	// readHeaderTimeout bounds how long the process waits for request headers.
	readHeaderTimeout = 5 * time.Second

	// maxJSONBytes caps one JSON response. A list stops at this size.
	maxJSONBytes = 4 << 20

	// maxRequestBytes caps one JSON request body.
	maxRequestBytes = 1 << 20
)

// NewMux is the process HTTP mux. Domain modules add routes to the same mux.
// gRPC stays on its own listener.
func NewMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", health)
	mux.HandleFunc("GET /api/config", uiConfig)
	mux.HandleFunc("GET /", serveApp(pageFiles()))
	return mux
}

// uiConfig tells the page where Grafana is. The value is the public origin
// only: no userinfo, query, or fragment. An unusable GRAFANA_URL is an empty
// string so the page hides the links instead of rendering a broken one.
func uiConfig(w http.ResponseWriter, _ *http.Request) {
	origin := ""
	if public, ok := environment.PublicHTTPOrigin(environment.GetGrafanaURL()); ok {
		origin = public
	}
	body, err := json.Marshal(struct {
		GrafanaURL string `json:"grafanaUrl"`
	}{GrafanaURL: origin})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, []byte(`{"error":"internal"}`))
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []byte(`{"ok":true}`))
}

func listenAddress() string {
	return ":" + environment.GetHTTPPort()
}

// Marshal encodes one protobuf message the same way the list and detail routes do.
// Default scalars stay (a disabled symbol, an empty provider string, status CREATED).
// Unset optional fields stay absent so the JSON does not invent nulls.
func Marshal(msg proto.Message) ([]byte, error) {
	encoded, err := (protojson.MarshalOptions{EmitDefaultValues: true}).Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("encode json: %w", err)
	}
	return encoded, nil
}

// WriteMessage writes one protobuf message as protojson.
func WriteMessage(w http.ResponseWriter, msg proto.Message) {
	encoded, err := Marshal(msg)
	if err != nil {
		WriteError(w, err)
		return
	}
	if len(encoded) > maxJSONBytes {
		WriteError(w, fmt.Errorf("response exceeds %d bytes", maxJSONBytes))
		return
	}
	writeJSON(w, http.StatusOK, encoded)
}

// WriteMessages writes a JSON array of protojson objects, one per list item.
func WriteMessages[T proto.Message](w http.ResponseWriter, items []T) {
	body, err := encodeList(items)
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func encodeList[T proto.Message](items []T) ([]byte, error) {
	body := make([]byte, 0, 2)
	body = append(body, '[')
	for i := range items {
		encoded, err := Marshal(items[i])
		if err != nil {
			return nil, err
		}
		next := len(body) + len(encoded) + 1
		if i > 0 {
			next++
		}
		if next > maxJSONBytes {
			return nil, fmt.Errorf("response exceeds %d bytes", maxJSONBytes)
		}
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, encoded...)
	}
	body = append(body, ']')
	return body, nil
}

// ReadJSON decodes one JSON object from the request body.
// The body is capped at maxRequestBytes. A second value or an unknown field is refused.
func ReadJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return fmt.Errorf("missing body")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(raw) > maxRequestBytes {
		return fmt.Errorf("body exceeds %d bytes", maxRequestBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode json: %w", err)
	}
	var extra struct{}
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing json")
	}
	return nil
}

// StatusJSON writes one JSON body. Callers pass a fixed object, not err.Error().
func StatusJSON(w http.ResponseWriter, code int, body string) {
	writeJSON(w, code, []byte(body))
}

// Unauthorized writes 401. The body does not say which credential failed.
func Unauthorized(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, []byte(`{"error":"unauthorized"}`))
}

// Forbidden writes 403.
func Forbidden(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, []byte(`{"error":"forbidden"}`))
}

// Conflict writes 409 with a fixed error string.
func Conflict(w http.ResponseWriter, message string) {
	encoded, err := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: message})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, []byte(`{"error":"internal"}`))
		return
	}
	writeJSON(w, http.StatusConflict, encoded)
}

// NotFound writes the missing-resource response.
func NotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, []byte(`{"error":"not found"}`))
}

// BadRequest writes the rejected-input response. The cause stays off the wire.
func BadRequest(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, []byte(`{"error":"bad request"}`))
}

// WriteError maps a missing row to 404. Permission and auth failures use
// their status. Other failures stay 500 and do not echo the cause.
func WriteError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	if status.Code(err) == codes.NotFound || errors.Is(err, pgx.ErrNoRows) {
		NotFound(w)
		return
	}
	switch status.Code(err) {
	case codes.Unauthenticated:
		Unauthorized(w)
	case codes.PermissionDenied:
		Forbidden(w)
	case codes.AlreadyExists:
		Conflict(w, "already exists")
	case codes.FailedPrecondition:
		if status.Convert(err).Message() == "name is ambiguous" {
			Conflict(w, "name is ambiguous")
			return
		}
		writeJSON(w, http.StatusInternalServerError, []byte(`{"error":"internal"}`))
	case codes.OK, codes.Canceled, codes.Unknown, codes.InvalidArgument, codes.DeadlineExceeded,
		codes.NotFound, codes.ResourceExhausted, codes.Aborted, codes.OutOfRange,
		codes.Unimplemented, codes.Internal, codes.Unavailable, codes.DataLoss:
		writeJSON(w, http.StatusInternalServerError, []byte(`{"error":"internal"}`))
	}
}

func writeJSON(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if _, err := w.Write(body); err != nil {
		return
	}
}

var (
	guardMu      sync.Mutex
	requestGuard func(http.Handler) http.Handler
)

// UseGuard wraps the mux when the process starts, and only when authentication
// is on. A nil guard leaves the mux as it is. The guard is read on OnStart,
// after startup has decided whether authentication is on.
func UseGuard(guard func(http.Handler) http.Handler) {
	guardMu.Lock()
	requestGuard = guard
	guardMu.Unlock()
}

func handlerFor(mux *http.ServeMux) http.Handler {
	guardMu.Lock()
	guard := requestGuard
	guardMu.Unlock()
	if guard == nil || !environment.GetAuthEnabled() {
		return mux
	}
	return guard(mux)
}

// start listens on HTTP_PORT. Serve runs on its own listener, not the gRPC port.
// Shutdown makes Serve return ErrServerClosed. That is a stop, not a crash.
// While authentication is off, the mux is served as it is today.
func start(lc fx.Lifecycle, mux *http.ServeMux) error {
	server := &http.Server{
		ReadHeaderTimeout: readHeaderTimeout,
	}
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			server.Handler = handlerFor(mux)
			listener, err := net.Listen("tcp", listenAddress())
			if err != nil {
				return fmt.Errorf("failed to listen: %w", err)
			}
			go func() {
				if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					panic(err)
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			if err := server.Shutdown(ctx); err != nil {
				return fmt.Errorf("failed to stop http: %w", err)
			}
			return nil
		},
	})
	return nil
}

var Module = fx.Options( //nolint: gochecknoglobals
	fx.Provide(NewMux),
	fx.Invoke(start),
)
