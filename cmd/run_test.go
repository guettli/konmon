package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthServer(t *testing.T) {
	now := time.Now()
	interval := 30 * time.Second
	tests := []struct {
		name            string
		started, last   time.Time
		healthz, readyz int
	}{
		{"just started, no evaluation", now, time.Time{}, http.StatusOK, http.StatusServiceUnavailable},
		{"no evaluation after the grace", now.Add(-startupGrace - time.Second), time.Time{}, http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{"recent evaluation", now.Add(-time.Hour), now.Add(-interval), http.StatusOK, http.StatusOK},
		{"evaluations stopped", now.Add(-time.Hour), now.Add(-3*interval - 2*time.Minute), http.StatusServiceUnavailable, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := healthServer("", func() time.Time { return tt.last }, tt.started, interval)
			for path, want := range map[string]int{"/healthz": tt.healthz, "/readyz": tt.readyz} {
				rec := httptest.NewRecorder()
				srv.Handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
				if rec.Code != want {
					t.Errorf("%s: got %d, want %d (%s)", path, rec.Code, want, rec.Body)
				}
			}
		})
	}
}
