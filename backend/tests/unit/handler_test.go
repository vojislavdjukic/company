package unit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"company/backend/internal/api"
)

type fakeHealthService struct{ err error }

// CheckDatabase returns the configured test error; ctx is unused by this fake.
func (f fakeHealthService) CheckDatabase(context.Context) error { return f.err }

// TestHealthEndpoints checks liveness and readiness responses for healthy and unhealthy dependencies.
// t runs each endpoint case and reports status mismatches.
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
			api.NewHandler(tt.deps).ServeHTTP(recorder, request)
			if recorder.Code != tt.want {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.want)
			}
		})
	}
}
