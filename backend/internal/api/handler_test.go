package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeHealthService struct{ err error }

func (f fakeHealthService) CheckDatabase(context.Context) error { return f.err }

func TestHealthEndpoints(t *testing.T) {
	tests := []struct {
		name string
		path string
		deps fakeHealthService
		want int
	}{
		{name: "live", path: "/healthz", want: http.StatusOK},
		{name: "ready", path: "/readyz", want: http.StatusOK},
		{name: "database unavailable", path: "/readyz", deps: fakeHealthService{err: errors.New("offline")}, want: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, tt.path, nil)
			NewHandler(tt.deps).ServeHTTP(recorder, request)
			if recorder.Code != tt.want {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.want)
			}
		})
	}
}
