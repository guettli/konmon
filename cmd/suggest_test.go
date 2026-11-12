package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/guettli/konmon/internal/store"
	"github.com/guettli/konmon/internal/usage"
)

const deployJSON = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"ns"},
"spec":{"template":{"spec":{"containers":[{"name":"app","resources":{"requests":{"cpu":"1","memory":"1Gi"}}},{"name":"side"}]}}}}`

func suggestFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "konmon.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	app := usage.Key{Namespace: "ns", Kind: "Deployment", Workload: "web", Container: "app"}
	side := usage.Key{Namespace: "ns", Kind: "Deployment", Workload: "web", Container: "side"}
	now := time.Now().UTC().Truncate(time.Hour)
	err = st.Update(context.Background(), func(tx *store.Tx) error {
		ref := store.ObjectRef{
			GVR:  schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			Kind: "Deployment", Namespace: "ns", Name: "web", UID: "u1",
		}
		if _, err := tx.UpsertObject(ref, "1", "h", []byte(deployJSON), false, now); err != nil {
			return err
		}
		// 30 hours of app, 2 hours of side.
		for h := 1; h <= 30; h++ {
			samples := []usage.Sample{{Key: app, CPU: 0.04, Memory: 100 << 20}}
			if h <= 2 {
				samples = append(samples, usage.Sample{Key: side, CPU: 0.001, Memory: 8 << 20})
			}
			if err := tx.AddUsage(now.Add(-time.Duration(h)*time.Hour), samples); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func runSuggest(t *testing.T, path, output string) string {
	t.Helper()
	oldDB, oldOpts := dbPath, suggestOpts
	t.Cleanup(func() { dbPath, suggestOpts = oldDB, oldOpts })
	dbPath = path
	suggestOpts.since = "168h"
	suggestOpts.minHours = 24
	suggestOpts.cpuPercentile = 95
	suggestOpts.memHeadroom = 0.15
	suggestOpts.output = output
	suggestOpts.live = 0
	var out, errOut bytes.Buffer
	if err := suggest(context.Background(), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestSuggestTable(t *testing.T) {
	out := runSuggest(t, suggestFixture(t), "table")
	for _, want := range []string{
		"Deployment/web  app        30h   1        40m",
		"1Gi      100Mi     115Mi",
		"side       2h    -",
		"(too little data)",
		"1 containers have less than 24h of data",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestSuggestYAMLAndJSON(t *testing.T) {
	path := suggestFixture(t)
	y := runSuggest(t, path, "yaml")
	want := "# ns Deployment/web\n- name: app\n  resources:\n    requests:\n      cpu: 40m\n      memory: 115Mi\n# container side: only 2h of data, need 24h\n"
	if y != want {
		t.Errorf("yaml:\n%s\nwant:\n%s", y, want)
	}
	var rows []jsonSuggestion
	if err := json.Unmarshal([]byte(runSuggest(t, path, "json")), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].SuggestCPU != "40m" || rows[0].CurrentCPU != "1" || rows[1].SuggestCPU != "" {
		t.Errorf("json: %+v", rows)
	}
}

func TestSuggestWithoutUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	dbPath, suggestOpts.output, suggestOpts.since, suggestOpts.cpuPercentile = path, "table", "168h", 95
	defer func() { dbPath = "konmon.db" }()
	var out bytes.Buffer
	if err := suggest(context.Background(), &out, &out); err == nil || !strings.Contains(err.Error(), "no usage recorded") {
		t.Errorf("want a no-usage error, got %v", err)
	}
}
