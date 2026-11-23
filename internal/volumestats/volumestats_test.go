package volumestats

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"github.com/guettli/konmon/internal/checks"
)

const summaryN1 = `{"node": {"nodeName": "n1"}, "pods": [
  {"podRef": {"name": "db-0", "namespace": "default"}, "volume": [
    {"time": "2026-01-01T00:00:00Z", "name": "data", "usedBytes": 90, "capacityBytes": 100, "availableBytes": 10,
     "inodes": 1000, "inodesUsed": 10, "inodesFree": 990, "pvcRef": {"name": "data-db-0", "namespace": "default"}},
    {"time": "2026-01-01T00:00:00Z", "name": "tmp", "usedBytes": 1, "capacityBytes": 100}
  ]},
  {"podRef": {"name": "reader", "namespace": "default"}, "volume": [
    {"time": "2026-01-01T00:01:00Z", "name": "d", "usedBytes": 91, "pvcRef": {"name": "data-db-0", "namespace": "default"}}
  ]},
  {"podRef": {"name": "old", "namespace": "default"}, "volume": [
    {"time": "2025-12-31T00:00:00Z", "name": "d", "usedBytes": 1, "pvcRef": {"name": "data-db-0", "namespace": "default"}}
  ]}
]}`

func TestParse(t *testing.T) {
	got, err := parse([]byte(summaryN1), "n1")
	if err != nil {
		t.Fatal(err)
	}
	// Only volumes of PVCs; of several Pods the newest measurement.
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	v := got[checks.VolumeKey{Namespace: "default", PVC: "data-db-0"}]
	if v.Pod != "reader" || v.Volume != "d" || v.Node != "n1" || *v.UsedBytes != 91 || v.CapacityBytes != nil {
		t.Errorf("got %+v", v)
	}
	if _, err := parse([]byte("<html>"), "n1"); err == nil || !strings.Contains(err.Error(), "node n1") {
		t.Errorf("err %v", err)
	}
}

func TestCollector(t *testing.T) {
	failN2 := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes/n1/proxy/stats/summary":
			_, _ = w.Write([]byte(summaryN1))
		case "/api/v1/nodes/n2/proxy/stats/summary":
			if failN2 {
				http.Error(w, "nope", http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"pods": [{"podRef": {"name": "p", "namespace": "x"}, "volume": [
			  {"time": "2026-01-01T00:00:00Z", "name": "v", "usedBytes": 5, "pvcRef": {"name": "c", "namespace": "x"}}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client, err := rest.RESTClientFor(&rest.Config{
		Host:    srv.URL,
		APIPath: "/api",
		ContentConfig: rest.ContentConfig{
			GroupVersion:         &schema.GroupVersion{Version: "v1"},
			NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &Collector{Client: client, MaxAge: 3 * time.Minute}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if s, _ := c.Stats(t0); s != nil {
		t.Fatal("stats before the first update")
	}
	n2 := checks.VolumeKey{Namespace: "x", PVC: "c"}
	if failed := c.Update(t.Context(), []string{"n1", "n2"}, t0); len(failed) != 0 {
		t.Fatalf("failed: %v", failed)
	}
	if s, complete := c.Stats(t0); len(s) != 2 || !complete {
		t.Fatalf("stats: %v %v", s, complete)
	}

	// A failing Node: incomplete, its stats are kept and become stale
	// after MaxAge, but they are not dropped.
	failN2 = true
	if failed := c.Update(t.Context(), []string{"n1", "n2"}, t0.Add(time.Minute)); failed["n2"] == nil || len(failed) != 1 {
		t.Fatalf("failed: %v", failed)
	}
	if s, complete := c.Stats(t0.Add(time.Minute)); len(s) != 2 || complete || s[n2].Stale || len(c.Failed()) != 1 {
		t.Fatalf("stats: %v %v", s, complete)
	}
	c.Update(t.Context(), []string{"n1", "n2"}, t0.Add(4*time.Minute))
	if s, _ := c.Stats(t0.Add(4 * time.Minute)); len(s) != 2 || !s[n2].Stale || s[checks.VolumeKey{Namespace: "default", PVC: "data-db-0"}].Stale {
		t.Fatalf("stats after MaxAge: %v", s)
	}

	// The Nodes cannot be listed: incomplete, everything becomes stale.
	failN2 = false
	c.Update(t.Context(), []string{"n1", "n2"}, t0.Add(5*time.Minute))
	c.ListFailed(errors.New("forbidden"))
	if f := c.Failed(); len(f) != 1 || f[""] == nil {
		t.Fatalf("failed: %v", f)
	}
	s, complete := c.Stats(t0.Add(9 * time.Minute))
	if complete || len(s) != 2 {
		t.Fatalf("stats: %v %v", s, complete)
	}
	for k, v := range s {
		if !v.Stale {
			t.Errorf("%v not stale", k)
		}
	}

	// A Node which is gone is dropped at once.
	c.Update(t.Context(), []string{"n1"}, t0.Add(10*time.Minute))
	if s, complete := c.Stats(t0.Add(10 * time.Minute)); len(s) != 1 || !complete || len(c.Failed()) != 0 {
		t.Fatalf("stats: %v %v", s, complete)
	}
}
