package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lhjnilsson/foreverbull/internal/web"
	backtestpb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	servicepb "github.com/lhjnilsson/foreverbull/pkg/pb/service"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy/service"
	"github.com/lhjnilsson/foreverbull/pkg/service"
	"github.com/lhjnilsson/foreverbull/pkg/strategy/internal/api"
	ss "github.com/lhjnilsson/foreverbull/pkg/strategy/internal/stream"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeStrategies struct {
	list    *servicePb.ListStrategiesResponse
	listErr error
	get     *servicePb.GetStrategyResponse
	getErr  error
	name    string
}

func (f *fakeStrategies) ListStrategies(context.Context, *servicePb.ListStrategiesRequest) (*servicePb.ListStrategiesResponse, error) {
	return f.list, f.listErr
}

func (f *fakeStrategies) GetStrategy(_ context.Context, req *servicePb.GetStrategyRequest) (*servicePb.GetStrategyResponse, error) {
	f.name = req.GetName()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.get, nil
}

type fakeStrategyWriter struct {
	created     *pb.Strategy
	err         error
	name        string
	description *string
	called      bool
}

func (f *fakeStrategyWriter) Create(_ context.Context, name string, description *string) (*pb.Strategy, error) {
	f.called = true
	f.name = name
	f.description = description
	if f.err != nil {
		return nil, f.err
	}
	return f.created, nil
}

func sampleStrategy() *pb.Strategy {
	description := "mean reversion"
	return &pb.Strategy{
		Id:          "st-1",
		Name:        "mean-rev",
		Description: &description,
		CreatedAt:   timestamppb.New(time.Date(2024, 3, 9, 0, 0, 0, 0, time.UTC)),
	}
}

func TestHTTP(t *testing.T) {
	sample := sampleStrategy()
	want, err := web.Marshal(sample)
	require.NoError(t, err)

	t.Run("list", func(t *testing.T) {
		mux := http.NewServeMux()
		api.RegisterHTTP(mux, &fakeStrategies{
			list: &servicePb.ListStrategiesResponse{Strategies: []*pb.Strategy{sample}},
		}, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/strategies", nil))

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "["+string(want)+"]", rec.Body.String())
	})

	t.Run("get", func(t *testing.T) {
		fake := &fakeStrategies{
			get: &servicePb.GetStrategyResponse{Strategy: sample},
		}
		mux := http.NewServeMux()
		api.RegisterHTTP(mux, fake, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/strategies/mean-rev", nil))

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, string(want), rec.Body.String())
		require.Equal(t, "mean-rev", fake.name)
	})

	t.Run("missing", func(t *testing.T) {
		mux := http.NewServeMux()
		api.RegisterHTTP(mux, &fakeStrategies{getErr: pgx.ErrNoRows}, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/strategies/missing", nil))

		require.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("grpc not found", func(t *testing.T) {
		mux := http.NewServeMux()
		api.RegisterHTTP(mux, &fakeStrategies{
			getErr: status.Error(codes.NotFound, "strategy \"missing\" not found"),
		}, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/strategies/missing", nil))

		require.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("create stays unmounted without a writer", func(t *testing.T) {
		mux := http.NewServeMux()
		api.RegisterHTTP(mux, &fakeStrategies{}, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/strategies", nil))

		require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	})

	t.Run("create", func(t *testing.T) {
		writer := &fakeStrategyWriter{created: sample}
		mux := http.NewServeMux()
		api.RegisterHTTP(mux, &fakeStrategies{}, writer)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/strategies", `{"name":" mean-rev "}`))

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, string(want), rec.Body.String())
		require.True(t, writer.called)
		require.Equal(t, "mean-rev", writer.name)
		require.Nil(t, writer.description)
	})

	t.Run("create rejects an empty name", func(t *testing.T) {
		writer := &fakeStrategyWriter{}
		mux := http.NewServeMux()
		api.RegisterHTTP(mux, &fakeStrategies{}, writer)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/strategies", `{"name":""}`))

		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.False(t, writer.called)
	})

	t.Run("create rejects a description field", func(t *testing.T) {
		writer := &fakeStrategyWriter{}
		mux := http.NewServeMux()
		api.RegisterHTTP(mux, &fakeStrategies{}, writer)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/strategies", `{"name":"mean-rev","description":"nope"}`))

		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.False(t, writer.called)
	})

	t.Run("create failure hides the cause", func(t *testing.T) {
		mux := http.NewServeMux()
		api.RegisterHTTP(mux, &fakeStrategies{}, &fakeStrategyWriter{err: errors.New("postgres password=secret")})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/strategies", `{"name":"mean-rev"}`))

		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.NotContains(t, rec.Body.String(), "secret")
	})

	t.Run("start execution", func(t *testing.T) {
		algorithm := &servicepb.Algorithm{
			FilePath: "algo.py",
			Functions: []*servicepb.Algorithm_Function{{
				Name: "handle_data",
			}},
		}
		lookup := &fakeServiceLookup{item: &servicepb.Service{
			Image:     "lhjnilsson/algo:local",
			Algorithm: algorithm,
		}}
		launcher := &fakeLauncher{out: &ss.RunOutput{SessionID: "sess-1", ExecutionID: "ex-9"}}
		mux := http.NewServeMux()
		api.RegisterExecutionHTTP(mux, lookup, launcher)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/backtests/spy/executions", `{"service":" lhjnilsson/algo:local "}`))

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		wantExecution, err := web.Marshal(&backtestpb.Execution{
			Id:       "ex-9",
			Backtest: "spy",
			Session:  "sess-1",
		})
		require.NoError(t, err)
		require.Equal(t, string(wantExecution), rec.Body.String())
		require.Contains(t, rec.Body.String(), `"id":"ex-9"`)
		require.NotContains(t, rec.Body.String(), `"status"`)
		require.Equal(t, "lhjnilsson/algo:local", lookup.image)
		require.Equal(t, "spy", launcher.in.BacktestName)
		require.Equal(t, "lhjnilsson/algo:local", launcher.in.ServiceImage)
		require.NotNil(t, launcher.in.Algorithm)
		require.True(t, proto.Equal(algorithm, launcher.in.Algorithm))
		require.Nil(t, launcher.in.StartDate)
		require.Nil(t, launcher.in.EndDate)
		require.Empty(t, launcher.in.ExperimentID)
	})

	t.Run("start forwards a stored algorithm with no functions", func(t *testing.T) {
		algorithm := &servicepb.Algorithm{}
		lookup := &fakeServiceLookup{item: &servicepb.Service{
			Image:     "algo:local",
			Algorithm: algorithm,
		}}
		launcher := &fakeLauncher{out: &ss.RunOutput{ExecutionID: "ex-empty"}}
		mux := http.NewServeMux()
		api.RegisterExecutionHTTP(mux, lookup, launcher)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/backtests/spy/executions", `{"service":"algo:local"}`))

		require.Equal(t, http.StatusOK, rec.Code)
		require.NotNil(t, launcher.in.Algorithm)
		require.True(t, proto.Equal(algorithm, launcher.in.Algorithm))
		require.Contains(t, rec.Body.String(), `"id":"ex-empty"`)
	})

	t.Run("start missing service", func(t *testing.T) {
		lookup := &fakeServiceLookup{err: service.ErrNotFound}
		launcher := &fakeLauncher{}
		mux := http.NewServeMux()
		api.RegisterExecutionHTTP(mux, lookup, launcher)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/backtests/spy/executions", `{"service":"missing/image:local"}`))

		require.Equal(t, http.StatusNotFound, rec.Code)
		require.JSONEq(t, `{"error":"not found"}`, rec.Body.String())
		require.False(t, launcher.called)
		require.Equal(t, "missing/image:local", lookup.image)
	})

	t.Run("start service has no algorithm", func(t *testing.T) {
		lookup := &fakeServiceLookup{item: &servicepb.Service{Image: "lhjnilsson/algo:local"}}
		launcher := &fakeLauncher{}
		mux := http.NewServeMux()
		api.RegisterExecutionHTTP(mux, lookup, launcher)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/backtests/spy/executions", `{"service":"lhjnilsson/algo:local"}`))

		require.Equal(t, http.StatusConflict, rec.Code)
		require.Contains(t, rec.Body.String(), "lhjnilsson/algo:local")
		require.Contains(t, rec.Body.String(), "no algorithm")
		require.False(t, launcher.called)
	})

	t.Run("start rejects an empty service", func(t *testing.T) {
		lookup := &fakeServiceLookup{}
		launcher := &fakeLauncher{}
		mux := http.NewServeMux()
		api.RegisterExecutionHTTP(mux, lookup, launcher)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/backtests/spy/executions", `{"service":" "}`))

		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.False(t, lookup.called)
		require.False(t, launcher.called)
	})

	t.Run("start rejects an image field", func(t *testing.T) {
		lookup := &fakeServiceLookup{}
		mux := http.NewServeMux()
		api.RegisterExecutionHTTP(mux, lookup, &fakeLauncher{})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/backtests/spy/executions", `{"image":"algo:local"}`))

		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.False(t, lookup.called)
	})

	t.Run("start failure hides the cause", func(t *testing.T) {
		lookup := &fakeServiceLookup{item: &servicepb.Service{
			Image:     "algo:local",
			Algorithm: &servicepb.Algorithm{FilePath: "algo.py"},
		}}
		launcher := &fakeLauncher{err: errors.New("postgres password=secret")}
		mux := http.NewServeMux()
		api.RegisterExecutionHTTP(mux, lookup, launcher)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/backtests/spy/executions", `{"service":"algo:local"}`))

		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.NotContains(t, rec.Body.String(), "secret")
		require.True(t, launcher.called)
		require.NotNil(t, launcher.in.Algorithm)
	})

	t.Run("start missing backtest", func(t *testing.T) {
		lookup := &fakeServiceLookup{item: &servicepb.Service{
			Image:     "algo:local",
			Algorithm: &servicepb.Algorithm{FilePath: "algo.py"},
		}}
		launcher := &fakeLauncher{err: ss.ErrBacktestMissing}
		mux := http.NewServeMux()
		api.RegisterExecutionHTTP(mux, lookup, launcher)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/backtests/missing/executions", `{"service":"algo:local"}`))

		require.Equal(t, http.StatusNotFound, rec.Code)
		require.NotContains(t, rec.Body.String(), "fbull")
		require.Equal(t, "missing", launcher.in.BacktestName)
		require.NotNil(t, launcher.in.Algorithm)
	})
}

func jsonRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

type fakeServiceLookup struct {
	item   *servicepb.Service
	err    error
	image  string
	called bool
}

func (f *fakeServiceLookup) Get(_ context.Context, image string) (*servicepb.Service, error) {
	f.called = true
	f.image = image
	if f.err != nil {
		return nil, f.err
	}
	return f.item, nil
}

type fakeLauncher struct {
	in     ss.RunInput
	out    *ss.RunOutput
	err    error
	called bool
}

func (f *fakeLauncher) Run(_ context.Context, in ss.RunInput) (*ss.RunOutput, error) {
	f.called = true
	f.in = in
	return f.out, f.err
}
