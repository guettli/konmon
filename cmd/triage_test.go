package cmd

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/guettli/konmon/internal/checks"
	"github.com/guettli/konmon/internal/config"
)

func conditionFinding(namespace, name, condition, status, reason string) *checks.Finding {
	return &checks.Finding{
		Check: "conditions", Category: checks.CategoryHealth, Severity: "warning",
		GVR:       schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"},
		Namespace: namespace, Name: name, UID: namespace + "/" + name, Key: condition, Condition: condition,
		Message: "Condition " + condition + "=" + status + " " + reason + ` "x"`,
		Details: map[string]string{"status": status, "reason": reason},
	}
}

func runTriage(t *testing.T, path, input string, findings ...*checks.Finding) string {
	t.Helper()
	editor, err := config.OpenEditor(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s := &triageSession{
		ctx: context.Background(), in: bufio.NewReader(strings.NewReader(input)), out: &out,
		editor: editor, base: &config.Config{}, all: findings, universe: findings,
		now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
	}
	if err := s.run(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestTriage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.yaml")
	stalled := []*checks.Finding{
		conditionFinding("team-a", "w1", "Stalled", "True", "Waiting"),
		conditionFinding("team-b", "w2", "Stalled", "True", "Waiting"),
	}
	degraded := conditionFinding("team-a", "w3", "Degraded", "True", "Broken")
	image := &checks.Finding{
		Check: "container-image-tag", Category: checks.CategoryHygiene, Severity: "info",
		GVR:       schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		Namespace: "dev", Name: "d", UID: "d", Key: "c", Message: "container c uses image x:latest",
	}
	pvc := &checks.Finding{
		Check: "pvc-not-bound", Category: checks.CategoryHealth, Severity: "warning",
		GVR:       schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"},
		Namespace: "db", Name: "data", UID: "p", Message: "PVC is Pending",
	}
	// Groups: Stalled (2, health), Degraded, pvc-not-bound, then hygiene.
	input := strings.Join([]string{
		"t", "f", "", // Stalled: healthy when False, for widgets.example.com
		"x", "i", "3", "", "known broken", "30d", // Degraded: invalid choice is asked again; rung 3 = everywhere; empty comment asked again
		"k", "ticket 7", // pvc: acknowledged
		"s", "", "", // image: snooze 7 days, default comment
	}, "\n") + "\n"
	out := runTriage(t, path, input, append(stalled, degraded, pvc, image)...)
	for _, want := range []string{
		"5 findings in 4 groups",
		"[1/4] conditions  widgets.example.com  Stalled=True Waiting",
		"Note: the current status True is unhealthy by this rule",
		"If widgets.example.com comes from a public project, please tell the maintainers",
		"https://github.com/guettli/konmon/issues/new?",
		"A comment is required.",
		"Saved 4 changes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	cfg, err := config.Load([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if r := cfg.Conditions["widgets.example.com"]; r == nil || strings.Join(r.HealthyWhenFalse, ",") != "Stalled" {
		t.Errorf("conditions: %+v", cfg.Conditions)
	}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if r := cfg.Ignored(degraded, now); r == nil || r.Comment != "known broken" || r.Until != "2026-11-26" {
		t.Errorf("degraded rule: %+v", r)
	}
	if cfg.Ignored(stalled[0], now) != nil {
		t.Error("stalled must not be ignored, only taught")
	}
	if r := cfg.IsAcknowledged(pvc, now); r == nil || r.Comment != "ticket 7" {
		t.Errorf("pvc acknowledged: %+v", r)
	}
	if r := cfg.Ignored(image, now); r == nil || r.Until != "2026-11-03" || r.Comment != "snoozed" {
		t.Errorf("image snooze: %+v", r)
	}

	// A second run asks only about Stalled=True: taught healthy when False,
	// it is still a problem. Everything else is covered.
	out = runTriage(t, path, "", append(stalled, degraded, pvc, image)...)
	if !strings.Contains(out, "[1/4] conditions  widgets.example.com  Stalled=True") || strings.Contains(out, "[2/4]") ||
		strings.Contains(out, "[3/4]") || strings.Contains(out, "[4/4]") {
		t.Errorf("second run:\n%s", out)
	}
}

func TestTriageLadderCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.yaml")
	findings := []*checks.Finding{
		conditionFinding("team-a", "w1", "Stalled", "True", "Waiting"),
		conditionFinding("team-a", "w2", "Stalled", "True", "Waiting"),
		conditionFinding("team-a", "w3", "Stalled", "False", "Other"),
		conditionFinding("team-b", "w4", "Ready", "False", "X"),
	}
	out := runTriage(t, path, "i\nc\nq\n", findings...)
	for _, want := range []string{
		"1) only team-a/w1",
		"2) these findings in namespace team-a",
		"hides 2\n",
		"condition Stalled of widgets.example.com, any status and reason",
		"hides 3  (+1 more)",
		"every conditions finding of widgets.example.com",
		"hides 4  (+2 more)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if _, err := os.Stat(path); err == nil {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), "ignore:") {
			t.Errorf("cancel must not write a rule:\n%s", data)
		}
	}
}

func TestTriageTaughtButStillUnhealthyIsAsked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.yaml")
	if err := os.WriteFile(path, []byte("conditions:\n  widgets.example.com: {healthyWhenTrue: [Ready]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Ready=False is a real problem although Ready is taught: triage must ask.
	out := runTriage(t, path, "n\n", conditionFinding("a", "w1", "Ready", "False", "Down"))
	if !strings.Contains(out, "[1/1]") {
		t.Errorf("the group must be shown:\n%s", out)
	}
	cfg := &config.Config{Conditions: map[string]*config.ConditionRules{
		"*":                   {HealthyWhenFalse: []string{"Ready"}},
		"widgets.example.com": {HealthyWhenTrue: []string{"Ready"}},
	}}
	g := &triageGroup{check: "conditions", resource: "widgets.example.com", condition: "Ready", status: "False"}
	if healthyByPolarity(cfg, g) {
		t.Error("the rule of the resource wins over *")
	}
	g.status = "True"
	if !healthyByPolarity(cfg, g) {
		t.Error("Ready=True is healthy by the rule of the resource")
	}
}

func TestTriageCheckErrorLadder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.yaml")
	f := &checks.Finding{Check: checks.ErrorCheck, Category: checks.CategoryHealth, Severity: "warning", Key: "boom", Message: "check boom failed"}
	out := runTriage(t, path, "i\nc\nq\n", f)
	if !strings.Contains(out, "1) only failures of check boom") || strings.Contains(out, "every finding of check") {
		t.Errorf("ladder for a check-error:\n%s", out)
	}
}

func TestTriageSnoozeCheckErrorOnlyThatCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.yaml")
	boom := &checks.Finding{Check: checks.ErrorCheck, Category: checks.CategoryHealth, Severity: "warning", Key: "boom", Message: "check boom failed"}
	bang := &checks.Finding{Check: checks.ErrorCheck, Category: checks.CategoryHealth, Severity: "warning", Key: "bang", Message: "check bang failed"}
	out := runTriage(t, path, "s\n\n\nq\n", boom, bang)
	if !strings.Contains(out, "[1/2]") {
		t.Errorf("want one group per failing check:\n%s", out)
	}
	cfg, err := config.Load([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Ignore) != 1 || cfg.Ignore[0].Key == "" {
		t.Fatalf("rules: %+v", cfg.Ignore)
	}
	other := &checks.Finding{Check: checks.ErrorCheck, Key: "my-new-check", Message: "check my-new-check failed"}
	if cfg.Ignored(other, time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)) != nil {
		t.Errorf("the snooze hides another failing check: %+v", cfg.Ignore[0])
	}
}

func TestTriageRejectsPastUntil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.yaml")
	out := runTriage(t, path, "s\n2020-01-01\n3d\n\n", conditionFinding("a", "w1", "Ready", "False", "Down"))
	if !strings.Contains(out, "That day is in the past.") {
		t.Errorf("want a hint:\n%s", out)
	}
	cfg, err := config.Load([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Ignore) != 1 || cfg.Ignore[0].Until != "2026-10-30" {
		t.Errorf("rules: %+v", cfg.Ignore)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode of a new file: %v", info.Mode())
	}
}

func TestTriageBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.yaml")
	odd := conditionFinding("a", "w2", "Weird[*]", "False", "X")
	findings := []*checks.Finding{
		conditionFinding("a", "w1", "Ready", "False", "Down"),
		odd,
		{Check: checks.ErrorCheck, Category: checks.CategoryHealth, Key: "boom", Message: "check boom failed"},
	}
	editor, err := config.OpenEditor(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := &triageSession{ctx: context.Background(), out: &out, editor: editor, base: &config.Config{}, all: findings, universe: findings, now: now}
	if err := s.baseline("90d"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "added 2 ignore rules") || !strings.Contains(out.String(), "expiring after 2027-01-25") ||
		!strings.Contains(out.String(), "Not baselined: 1 finding") {
		t.Errorf("output:\n%s", out.String())
	}
	cfg, err := config.Load([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ignored(findings[0], now) == nil || cfg.Ignored(odd, now) == nil || cfg.Ignored(findings[2], now) != nil {
		t.Errorf("rules: %+v", cfg.Ignore)
	}
	other := conditionFinding("a", "w2", "Weird*", "False", "X") // the unquoted glob Weird[*] would match it
	if cfg.Ignored(other, now) != nil {
		t.Error("glob characters of a key must be quoted")
	}
	if cfg.Ignore[0].Comment != "baseline 2026-10-27" {
		t.Errorf("comment %q", cfg.Ignore[0].Comment)
	}
	if err := s.baseline("2020-01-01"); err == nil {
		t.Error("want an error for a date in the past")
	}

	// Nothing new (the real flow drops ignored findings first), and
	// acknowledged findings are never baselined.
	out.Reset()
	s.all = nil
	if err := s.baseline("90d"); err != nil || !strings.Contains(out.String(), "Baseline: no new findings.") {
		t.Errorf("no findings: %v\n%s", err, out.String())
	}
	out.Reset()
	f := conditionFinding("b", "w9", "Ready", "False", "Down")
	s.all = []*checks.Finding{f}
	s.base = &config.Config{Acknowledged: []*config.IgnoreRule{{Namespace: "b", Comment: "known"}}}
	if err := s.base.Acknowledged[0].Compile(); err != nil {
		t.Fatal(err)
	}
	if err := s.baseline("90d"); err != nil || !strings.Contains(out.String(), "Not baselined: 1 finding") {
		t.Errorf("acknowledged: %v\n%s", err, out.String())
	}
}

func TestTriageSaysWhenCountsArePartial(t *testing.T) {
	editor, err := config.OpenEditor(filepath.Join(t.TempDir(), "konmon.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	f := conditionFinding("team-a", "w1", "Stalled", "True", "Waiting")
	var out bytes.Buffer
	s := &triageSession{
		ctx: context.Background(), in: bufio.NewReader(strings.NewReader("i\nc\nq\n")), out: &out,
		editor: editor, base: &config.Config{}, all: []*checks.Finding{f}, universe: []*checks.Finding{f},
		now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), partial: true,
	}
	if err := s.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "(counts include only the objects konmon was allowed to list)") {
		t.Errorf("no note about partial counts:\n%s", out.String())
	}
}
