package service_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lhjnilsson/foreverbull/internal/web"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/service"
	"github.com/lhjnilsson/foreverbull/pkg/service"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeServices struct {
	items     []*pb.Service
	err       error
	got       string
	listed    bool
	created   string
	createErr error
	stored    *pb.Service
}

func (f *fakeServices) List(context.Context) ([]*pb.Service, error) {
	f.listed = true
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
}

func (f *fakeServices) Create(_ context.Context, image string) (*pb.Service, error) {
	f.created = image
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.stored != nil {
		return f.stored, nil
	}
	return &pb.Service{Image: image}, nil
}

func (f *fakeServices) Get(_ context.Context, image string) (*pb.Service, error) {
	f.got = image
	if f.err != nil {
		return nil, f.err
	}
	for _, item := range f.items {
		if item.GetImage() == image {
			return item, nil
		}
	}
	return nil, service.ErrNotFound
}

func sampleService() *pb.Service {
	lookback := "20"
	return &pb.Service{
		Image: "lhjnilsson/algo:local",
		Algorithm: &pb.Algorithm{
			FilePath:   "algo.py",
			Namespaces: []string{"close"},
			Functions: []*pb.Algorithm_Function{{
				Name:     "handle_data",
				RunFirst: true,
				Parameters: []*pb.Algorithm_FunctionParameter{{
					Key:          "lookback",
					DefaultValue: &lookback,
					ValueType:    "int",
				}},
			}},
		},
		Statuses: []*pb.Service_Status{{
			Status:     pb.Service_Status_READY,
			OccurredAt: timestamppb.New(time.Date(2024, 3, 9, 0, 0, 0, 0, time.UTC)),
		}},
	}
}

func TestHTTP(t *testing.T) {
	sample := sampleService()
	want, err := web.Marshal(sample)
	require.NoError(t, err)

	t.Run("list", func(t *testing.T) {
		mux := http.NewServeMux()
		service.RegisterHTTP(mux, &fakeServices{items: []*pb.Service{sample}})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/services", nil))

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "["+string(want)+"]", rec.Body.String())
	})

	t.Run("image query", func(t *testing.T) {
		fake := &fakeServices{items: []*pb.Service{sample}}
		mux := http.NewServeMux()
		service.RegisterHTTP(mux, fake)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/services?image=lhjnilsson%2Falgo%3Alocal", nil))

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, string(want), rec.Body.String())
		require.Equal(t, "lhjnilsson/algo:local", fake.got)
		require.False(t, fake.listed)
	})

	t.Run("missing image", func(t *testing.T) {
		mux := http.NewServeMux()
		service.RegisterHTTP(mux, &fakeServices{})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/services?image=missing/image:local", nil))

		require.Equal(t, http.StatusNotFound, rec.Code)
		require.JSONEq(t, `{"error":"not found"}`, rec.Body.String())
	})

	t.Run("list failure hides the cause", func(t *testing.T) {
		mux := http.NewServeMux()
		service.RegisterHTTP(mux, &fakeServices{err: errors.New("postgres password=secret")})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/services", nil))

		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.NotContains(t, rec.Body.String(), "secret")
	})

	t.Run("create", func(t *testing.T) {
		created := sampleService()
		created.Statuses = []*pb.Service_Status{{
			Status: pb.Service_Status_CREATED,
		}}
		wantCreated, err := web.Marshal(created)
		require.NoError(t, err)
		fake := &fakeServices{stored: created}
		mux := http.NewServeMux()
		service.RegisterHTTP(mux, fake)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/services", `{"image":"lhjnilsson/algo:local"}`))

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		require.Equal(t, string(wantCreated), rec.Body.String())
		require.Equal(t, "lhjnilsson/algo:local", fake.created)
		require.False(t, fake.listed)
	})

	t.Run("create rejects an empty image", func(t *testing.T) {
		fake := &fakeServices{}
		mux := http.NewServeMux()
		service.RegisterHTTP(mux, fake)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/services", `{"image":"  "}`))

		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Empty(t, fake.created)
	})

	t.Run("create failure hides the cause", func(t *testing.T) {
		mux := http.NewServeMux()
		service.RegisterHTTP(mux, &fakeServices{createErr: errors.New("postgres password=secret")})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/services", `{"image":"algo:local"}`))

		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.NotContains(t, rec.Body.String(), "secret")
	})
}

func jsonRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}
