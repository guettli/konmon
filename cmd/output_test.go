package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/guettli/konmon/internal/checks"
	"github.com/guettli/konmon/internal/store"
)

func testFinding(check, category, namespace, name string) checks.Finding {
	return checks.Finding{
		Check: check, Category: category, Severity: "warning",
		GVR:       schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		Kind:      "Deployment",
		Namespace: namespace, Name: name, UID: namespace + "/" + name,
		Message: "broken",
	}
}

func TestFindingFilter(t *testing.T) {
	health := testFinding("conditions", checks.CategoryHealth, "app-a", "x")
	hygiene := testFinding("container-image-tag", checks.CategoryHygiene, "app-b", "y")
	cluster := testFinding("conditions", checks.CategoryHealth, "", "node1")
	checkError := checks.Finding{Check: checks.ErrorCheck, Category: checks.CategoryHealth, Message: "check x failed for 2 objects, e.g. other/z"}

	cases := []struct {
		filter findingFilter
		want   []bool // health, hygiene, cluster, checkError
	}{
		{findingFilter{}, []bool{true, true, true, true}},
		{findingFilter{categories: []string{"health"}}, []bool{true, false, true, true}},
		{findingFilter{categories: []string{"hygiene"}}, []bool{false, true, false, true}},
		{findingFilter{namespaces: []string{"app-*"}}, []bool{true, true, false, true}},
		{findingFilter{namespaces: []string{"app-b"}, categories: []string{"health"}}, []bool{false, false, false, true}},
	}
	for i, c := range cases {
		for j, f := range []checks.Finding{health, hygiene, cluster, checkError} {
			if got := c.filter.match(&f); got != c.want[j] {
				t.Errorf("case %d, finding %d: match = %v, want %v", i, j, got, c.want[j])
			}
		}
	}

	for _, bad := range []findingFilter{
		{categories: []string{"healthy"}, output: "text"},
		{namespaces: []string{"["}, output: "text"},
		{output: "yaml"},
	} {
		if err := bad.validate(); err == nil {
			t.Errorf("want an error for %+v", bad)
		}
	}
}

func TestPrintFindingLines(t *testing.T) {
	a := testFinding("conditions", checks.CategoryHealth, "b", "x")
	b := testFinding("conditions", checks.CategoryHealth, "a", "x")
	c := testFinding("container-image-tag", checks.CategoryHygiene, "a", "y")
	var buf bytes.Buffer
	health, hygiene := printFindingLines(&buf, []shownFinding{{&c, "  c"}, {&a, "  b"}, {&b, "  a"}})
	if health != 2 || hygiene != 1 {
		t.Errorf("counts %d %d", health, hygiene)
	}
	if got, want := buf.String(), "  a\n  b\nHygiene (never alerts):\n  c\n"; got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	buf.Reset()
	printFindingLines(&buf, []shownFinding{{&a, "  b"}})
	if strings.Contains(buf.String(), "Hygiene") {
		t.Errorf("no hygiene header without hygiene findings: %q", buf.String())
	}

	all := findingFilter{}
	if got := all.countText(1, 2); got != "1 health finding, 2 hygiene findings" {
		t.Error(got)
	}
	onlyHealth := findingFilter{categories: []string{"health"}}
	if got := onlyHealth.countText(0, 0); got != "0 health findings" {
		t.Error(got)
	}
}

func TestFindingsCommand(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "konmon.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 24, 3, 0, 0, 0, time.UTC)
	err = st.Update(ctx, func(tx *store.Tx) error {
		_, _, err := tx.SyncFindings([]checks.Finding{
			testFinding("container-image-tag", checks.CategoryHygiene, "app-a", "y"),
			testFinding("conditions", checks.CategoryHealth, "app-b", "x"),
			testFinding("conditions", checks.CategoryHealth, "app-a", "x"),
		}, nil, t0)
		return err
	})
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}

	runFindings := func(args ...string) string {
		t.Helper()
		findingsFilter = findingFilter{output: "text"}
		findingsAt = ""
		var out bytes.Buffer
		rootCmd.SetOut(&out)
		rootCmd.SetArgs(append([]string{"findings", "--db", path}, args...))
		if err := rootCmd.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	text := runFindings("--at", t0.Add(time.Minute).Format(time.RFC3339))
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "  app-a deployments x broken") ||
		!strings.HasPrefix(lines[1], "  app-b ") || lines[2] != "Hygiene (never alerts):" ||
		!strings.HasPrefix(lines[4], "2 health findings, 1 hygiene finding open at ") {
		t.Errorf("unexpected output:\n%s", text)
	}

	var result findingsResult
	if err := json.Unmarshal([]byte(runFindings("-o", "json", "-n", "app-a")), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Findings) != 2 || result.Findings[0].Check != "conditions" || result.Findings[0].Resource != "deployments.apps" ||
		!result.Findings[0].FirstSeen.Equal(t0) || result.Findings[1].Category != checks.CategoryHygiene {
		t.Errorf("unexpected JSON: %+v", result)
	}

	cfgPath := filepath.Join(t.TempDir(), "konmon.yaml")
	if err := os.WriteFile(cfgPath, []byte("ignore:\n  - namespace: app-b\n    comment: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	findingsConfigs = nil
	text = runFindings("--at", t0.Add(time.Minute).Format(time.RFC3339), "--config", cfgPath)
	if strings.Contains(text, "app-b") || !strings.Contains(text, "1 health finding, 1 hygiene finding, 1 ignored open at") {
		t.Errorf("--config should hide app-b:\n%s", text)
	}
	findingsConfigs = nil

	findingsFilter = findingFilter{output: "text"}
	rootCmd.SetArgs([]string{"findings", "--db", filepath.Join(t.TempDir(), "typo.db")})
	if err := rootCmd.Execute(); err == nil || !strings.Contains(err.Error(), "no konmon database") {
		t.Errorf("want an error for a missing database, got %v", err)
	}
}

func TestIgnorer(t *testing.T) {
	c := checkFlags{ignoreFindings: []string{"broken"}}
	ig, err := c.ignorer()
	if err != nil {
		t.Fatal(err)
	}
	f := testFinding("conditions", checks.CategoryHealth, "a", "x")
	if ig.ignored(&f, time.Now()) == "" {
		t.Error("the regex should match")
	}
	checkError := checks.Finding{Check: checks.ErrorCheck, Message: "check broken failed"}
	if ig.ignored(&checkError, time.Now()) != "" {
		t.Error("a regex must never hide a failing check")
	}
	if _, err := (&checkFlags{ignoreFindings: []string{"("}}).ignorer(); err == nil {
		t.Error("want an error for an invalid regex")
	}
}

func TestConfigConditionsBecomeParam(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.yaml")
	if err := os.WriteFile(path, []byte("conditions:\n  widgets.example.com: {healthyWhenFalse: [Stalled]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := checkFlags{configs: []string{path}, timeout: time.Second}
	e, _, err := c.engine(slog.New(slog.DiscardHandler), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range e.Checks() {
		if d.Name == "conditions" {
			rules, _ := d.Params["rules"].(map[string]any)
			if _, ok := rules["widgets.example.com"]; !ok {
				t.Errorf("rules param: %v", d.Params["rules"])
			}
		}
	}
	c = checkFlags{configs: []string{path}, params: []string{`conditions.rules={}`}}
	if _, _, err := c.engine(slog.New(slog.DiscardHandler), nil, nil); err == nil {
		t.Error("want an error for --check-param conditions.rules together with a config")
	}
}
