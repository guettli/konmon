package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditorNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.yaml")
	e, err := OpenEditor(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddIgnore(&IgnoreRule{Check: "conditions", Resource: "widgets.example.com", Condition: "Stalled", Comment: "normal"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Teach("widgets.example.com", HealthyWhenFalse, []string{"Stalled", "Degraded"}); err != nil {
		t.Fatal(err)
	}
	if err := e.AddAcknowledged(&IgnoreRule{Check: "pvc-not-bound", Namespace: "db", Comment: "known, ticket 7"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `# konmon config: ignore rules and condition polarity.
# See https://github.com/guettli/konmon#ignoring-findings-the-config-file

ignore:
  - check: conditions
    resource: widgets.example.com
    condition: Stalled
    comment: normal
conditions:
  widgets.example.com:
    healthyWhenFalse: [Stalled, Degraded]
acknowledged:
  - check: pvc-not-bound
    namespace: db
    comment: known, ticket 7
`
	if string(data) != want {
		t.Errorf("got:\n%s\nwant:\n%s", data, want)
	}
	cfg, err := Load([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Ignore) != 1 || len(cfg.Acknowledged) != 1 || cfg.Conditions["widgets.example.com"] == nil {
		t.Errorf("parsed: %+v", cfg)
	}
}

func TestEditorKeepsCommentsAndMovesTypes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.yaml")
	orig := `# my comment
ignore:
  # why this one
  - check: a
    comment: x # inline
conditions:
  widgets.example.com:
    healthyWhenTrue: [Stalled, Synced]
`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := OpenEditor(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Teach("widgets.example.com", HealthyWhenFalse, []string{"Stalled"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Teach("pods", IgnoreTypes, []string{"Flaky"}); err != nil {
		t.Fatal(err)
	}
	if err := e.AddIgnore(&IgnoreRule{Check: "b", Until: "2027-01-31", Comment: "y"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	for _, s := range []string{"# my comment", "# why this one", "# inline", "healthyWhenTrue: [Synced]", "healthyWhenFalse: [Stalled]", "ignoreTypes: [Flaky]", "until: \"2027-01-31\""} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}
	cfg, err := Load([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Ignore) != 2 {
		t.Errorf("rules: %d", len(cfg.Ignore))
	}

	if err := e.AddIgnore(&IgnoreRule{Comment: "no matcher"}); err == nil {
		t.Error("want an error for an invalid rule")
	}
}

func TestEditorRefusesInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.yaml")
	if err := os.WriteFile(path, []byte("ignor: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenEditor(path); err == nil {
		t.Error("want an error for an invalid file")
	}
}

func TestEditorSymlinkAndCommentsOnly(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(target, []byte("# note\n# note2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	e, err := OpenEditor(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddIgnore(&IgnoreRule{Check: "a", Comment: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Save(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link must stay a link: %v %v", info, err)
	}
	data, _ := os.ReadFile(target)
	if !strings.HasPrefix(string(data), "# note\n# note2\n") || !strings.Contains(string(data), "check: a") {
		t.Errorf("target:\n%s", data)
	}
}

func TestEditorFlowStyleListBecomesBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.yaml")
	if err := os.WriteFile(path, []byte("ignore: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := OpenEditor(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"a", "b"} {
		if err := e.AddIgnore(&IgnoreRule{Check: c, Comment: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := e.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if want := "ignore:\n  - check: a\n    comment: x\n  - check: b\n    comment: x\n"; string(data) != want {
		t.Errorf("got:\n%s", data)
	}
}

func TestEditorTeachFindsTheKeyInAnyCase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.yaml")
	orig := `conditions:
  Widgets.Example.COM:
    healthyWhenTrue: [Stalled]
`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := OpenEditor(path)
	if err != nil {
		t.Fatal(err)
	}
	// A second key for the same resource type would make the file invalid.
	key, err := e.Teach("widgets.example.com", HealthyWhenFalse, []string{"Stalled"})
	if err != nil {
		t.Fatal(err)
	}
	if key != "Widgets.Example.COM" {
		t.Errorf("key %q", key)
	}
	if err := e.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	want := `conditions:
  Widgets.Example.COM:
    healthyWhenFalse: [Stalled]
`
	if string(data) != want {
		t.Errorf("got:\n%s\nwant:\n%s", data, want)
	}
	if _, err := e.Teach("no/such", HealthyWhenFalse, []string{"Stalled"}); err == nil {
		t.Error("want an error for an invalid resource")
	}
}
