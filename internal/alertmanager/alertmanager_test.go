package alertmanager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/guettli/check-conditions/pkg/checkconditions"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/guettli/konmon/internal/store"
)

func TestNotifyPostsFiringResolvedAndWatchdog(t *testing.T) {
	var got []Alert
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/alerts" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()

	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	f := checkconditions.Finding{
		Kind:      checkconditions.FindingCondition,
		GVR:       schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		Namespace: "ns", Name: "web", ConditionTypes: []string{"Available"},
		Status: "False", Reason: "MinimumReplicasUnavailable",
	}
	firing := []store.FindingRow{{Finding: f, FirstSeen: now.Add(-5 * time.Minute)}}
	resolved := []store.FindingRow{{Finding: f, FirstSeen: now.Add(-time.Hour), ResolvedAt: now.Add(-time.Minute)}}

	c := &Client{
		URLs:           []string{srv.URL + "/"},
		Labels:         map[string]string{"cluster": "prod"},
		ResolveTimeout: 2 * time.Minute,
		Watchdog:       true,
	}
	if err := c.Notify(context.Background(), now, firing, resolved); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 alerts, got %d", len(got))
	}
	a := got[0]
	want := map[string]string{
		"alertname": "KonmonCondition", "severity": "warning", "cluster": "prod",
		"group": "apps", "resource": "deployments", "namespace": "ns", "name": "web", "condition": "Available",
	}
	for k, v := range want {
		if a.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, a.Labels[k], v)
		}
	}
	if !a.EndsAt.Equal(now.Add(2 * time.Minute)) {
		t.Errorf("firing endsAt = %s", a.EndsAt)
	}
	if a.Annotations["reason"] != "MinimumReplicasUnavailable" {
		t.Errorf("annotations = %v", a.Annotations)
	}
	if !got[1].EndsAt.Equal(now.Add(-time.Minute)) {
		t.Errorf("resolved endsAt = %s, want resolve time", got[1].EndsAt)
	}
	if got[2].Labels["alertname"] != "KonmonWatchdog" || got[2].Labels["cluster"] != "prod" {
		t.Errorf("watchdog labels = %v", got[2].Labels)
	}
}

func TestNotifyReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad alert", http.StatusBadRequest)
	}))
	defer srv.Close()
	c := &Client{URLs: []string{srv.URL}, Watchdog: true, ResolveTimeout: time.Minute}
	if err := c.Notify(context.Background(), time.Now(), nil, nil); err == nil {
		t.Fatal("expected error for HTTP 400")
	}
}

func TestNotifyNothingToSend(t *testing.T) {
	c := &Client{URLs: []string{"http://127.0.0.1:1"}}
	if err := c.Notify(context.Background(), time.Now(), nil, nil); err != nil {
		t.Fatalf("no alerts must not cause a request: %v", err)
	}
}
