package cmd

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestReportFalsePositive(t *testing.T) {
	t.Cleanup(func() {
		reportOpts = struct {
			open                                                    bool
			check, resource, condition, status, reason, rule, title string
		}{}
	})
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetArgs([]string{
		"report", "false-positive", "--check", "conditions", "--resource", "widgets.example.com",
		"--condition", "Stalled", "--status", "True", "--reason", "Waiting", "--rule", "conditions:\n  widgets.example.com: {healthyWhenTrue: [Stalled]}",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	_, link, ok := strings.Cut(out.String(), "https://")
	if !ok {
		t.Fatalf("no link in %q", out.String())
	}
	link = "https://" + strings.TrimSpace(link)
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	want := map[string]string{
		"template": "false-positive.yml",
		"title":    "False positive: conditions widgets.example.com Stalled=True Waiting",
		"check":    "conditions", "resource": "widgets.example.com", "finding": "Stalled=True Waiting",
		"version": "dev",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if !strings.Contains(q.Get("rule"), "healthyWhenTrue") {
		t.Errorf("rule: %q", q.Get("rule"))
	}
	if u.Host != "github.com" || u.Path != "/guettli/konmon/issues/new" {
		t.Errorf("link: %s", link)
	}
}

func TestReportForms(t *testing.T) {
	// The fields falsePositiveURL fills must exist in the form; every form
	// needs the keys GitHub requires and unique ids.
	wantIDs := map[string][]string{
		"false-positive.yml": {"version", "check", "resource", "finding", "rule"},
		"check-idea.yml":     {"version"},
		"bug.yml":            {"version"},
	}
	for name, ids := range wantIDs {
		data, err := readRepoFile(".github/ISSUE_TEMPLATE/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var form struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
			Body        []struct {
				Type string `yaml:"type"`
				ID   string `yaml:"id"`
			} `yaml:"body"`
		}
		if err := yaml.Unmarshal(data, &form); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if form.Name == "" || form.Description == "" || len(form.Body) == 0 {
			t.Errorf("%s: name, description and body are required", name)
		}
		seen := map[string]bool{}
		for _, b := range form.Body {
			if b.ID != "" && seen[b.ID] {
				t.Errorf("%s: duplicate id %s", name, b.ID)
			}
			seen[b.ID] = true
		}
		for _, id := range ids {
			if !seen[id] {
				t.Errorf("%s has no field id %s", name, id)
			}
		}
	}
}

func TestReportLongRule(t *testing.T) {
	link := falsePositiveURL("conditions", "widgets.example.com", "Stalled", "True", "Waiting", strings.Repeat("x", 10000))
	if len(link) > maxURL || !strings.Contains(link, "too+long") {
		t.Errorf("long rule: %d bytes", len(link))
	}
}

func readRepoFile(p string) ([]byte, error) { return os.ReadFile(filepath.Join("..", p)) }
