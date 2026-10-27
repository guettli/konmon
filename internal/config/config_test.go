package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/guettli/konmon/internal/checks"
)

var now = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func widget(namespace, condition, message string) *checks.Finding {
	return &checks.Finding{
		Check: "conditions", Category: checks.CategoryHealth,
		GVR:       schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"},
		Namespace: namespace, Name: "w1", UID: "u-w1", Key: condition,
		Condition: condition, Message: message,
	}
}

func TestParseAndMatch(t *testing.T) {
	cfg, err := Parse([]byte(`
ignore:
  - resource: widgets.example.com
    namespace: team-*
    condition: Stalled
    message: "Waiting for .*"
    until: 2026-10-27
    comment: normal during sync
  - check: container-*
    comment: no requests anywhere
  - check: check-error
    key: x
    comment: known broken check
`), "konmon.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ignore[0].Source != "konmon.yaml:ignore[0]" {
		t.Errorf("source %q", cfg.Ignore[0].Source)
	}
	cases := []struct {
		f    *checks.Finding
		at   time.Time
		want int // index of the matching rule, -1 none
	}{
		{widget("team-a", "Stalled", `Condition Stalled=True Error "boom"`), now, -1}, // message must match
		{widget("team-a", "Stalled", "Waiting for upstream"), now, 0},
		{widget("team-a", "Failed/Stalled", "Waiting for upstream"), now, 0},               // one of the grouped types
		{widget("team-a", "Stalled", "Waiting for upstream"), now.Add(12 * time.Hour), -1}, // expired
		{widget("other", "Stalled", "Waiting for upstream"), now, -1},
		{widget("team-a", "Ready", "Waiting for upstream"), now, -1},
		{&checks.Finding{Check: "container-image-tag", Name: "d"}, now, 1},
		{widget("", "Stalled", "Waiting for x"), now, -1},                                        // cluster-scoped: namespace: team-* does not match
		{&checks.Finding{Check: checks.ErrorCheck, Message: "check conditions failed"}, now, -1}, // a broad rule never hides a failing check
		{&checks.Finding{Check: checks.ErrorCheck, Key: "x"}, now, 2},
	}
	for i, c := range cases {
		got := -1
		if r := cfg.Ignored(c.f, c.at); r != nil {
			for j := range cfg.Ignore {
				if cfg.Ignore[j] == r {
					got = j
				}
			}
		}
		if got != c.want {
			t.Errorf("case %d: matched rule %d, want %d", i, got, c.want)
		}
	}
	if exp := cfg.Expired(now.Add(12 * time.Hour)); len(exp) != 1 || exp[0] != cfg.Ignore[0] {
		t.Errorf("expired: %v", exp)
	}
	if exp := cfg.Expired(now); len(exp) != 0 {
		t.Errorf("not expired yet on its last day: %v", exp)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unknown field":   "ignore:\n  - chek: conditions\n    comment: x\n",
		"no matcher":      "ignore:\n  - comment: everything\n",
		"no comment":      "ignore:\n  - check: conditions\n",
		"bad glob":        "ignore:\n  - name: '['\n    comment: x\n",
		"bad regexp":      "ignore:\n  - message: '('\n    comment: x\n",
		"bad until":       "ignore:\n  - check: x\n    until: tomorrow\n    comment: x\n",
		"empty rule":      "ignore:\n  -\n",
		"unknown section": "ignores: []\n",
		"only stars":      "ignore:\n  - check: '*'\n    namespace: '**'\n    comment: x\n",
		"two documents":   "ignore:\n  - check: a\n    comment: x\n---\nignore:\n  - check: b\n    comment: y\n",
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data), "c.yaml"); err == nil {
			t.Errorf("%s: want an error", name)
		} else if !strings.Contains(err.Error(), "c.yaml") {
			t.Errorf("%s: error without file name: %v", name, err)
		}
	}
	if cfg, err := Parse(nil, "empty.yaml"); err != nil || len(cfg.Ignore) != 0 {
		t.Errorf("an empty file is an empty config: %v", err)
	}
}

func TestLoadMerges(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml")
	if err := os.WriteFile(a, []byte("ignore:\n  - check: a\n    comment: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("ignore:\n  - check: b\n    comment: y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Ignore) != 2 || cfg.Ignore[1].Source != b+":ignore[0]" {
		t.Errorf("merged: %+v", cfg.Ignore)
	}
	if _, err := Load([]string{filepath.Join(dir, "missing.yaml")}); err == nil {
		t.Error("want an error for a missing file")
	}
}

func TestExampleConfig(t *testing.T) {
	cfg, err := Load([]string{"../../examples/konmon.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Ignore) == 0 {
		t.Error("the example has no rules")
	}
}
