package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeEvaluations struct {
	last time.Time
	n    uint64
}

func (f fakeEvaluations) LastEvaluation() time.Time { return f.last }
func (f fakeEvaluations) Evaluations() uint64       { return f.n }

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
			srv := healthServer("", fakeEvaluations{last: tt.last}, tt.started, interval)
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

func TestMetrics(t *testing.T) {
	last := time.UnixMilli(1_800_000_000_500)
	srv := healthServer("", fakeEvaluations{last: last, n: 42}, time.Unix(1_799_999_000, 0), 30*time.Second)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	for _, want := range []string{
		"# TYPE konmon_evaluations_total counter\nkonmon_evaluations_total 42\n",
		"\nkonmon_last_evaluation_timestamp_seconds 1800000000.5\n",
		"\nkonmon_evaluation_interval_seconds 30\n",
		"\nkonmon_start_time_seconds 1799999000\n",
		"\nkonmon_build_info{version=\"dev\"} 1\n",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("missing %q in\n%s", want, rec.Body)
		}
	}

	rec = httptest.NewRecorder()
	srv = healthServer("", fakeEvaluations{}, time.Now(), 30*time.Second)
	srv.Handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "\nkonmon_last_evaluation_timestamp_seconds 0\n") {
		t.Errorf("before the first evaluation the timestamp should be 0:\n%s", rec.Body)
	}
}

func TestLabelValue(t *testing.T) {
	if got, want := labelValue("a\\b\"c\nd"), `a\\b\"c\nd`; got != want {
		t.Errorf("labelValue = %s, want %s", got, want)
	}
}

func TestGlobMatcherIgnoresCase(t *testing.T) {
	match, err := globMatcher([]string{"Leases.*"})
	if err != nil {
		t.Fatal(err)
	}
	if !match(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}) {
		t.Error("Leases.* must match leases.coordination.k8s.io")
	}
}

func TestSplitResourceNameIgnoresCase(t *testing.T) {
	resource, group, name, err := splitResourceName("Deployments.Apps/My-App")
	if err != nil || resource != "deployments" || group != "apps" || name != "My-App" {
		t.Errorf("got %q %q %q %v", resource, group, name, err)
	}
}
