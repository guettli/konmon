package cmd

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReportFalsePositive(t *testing.T) {
	reportOpts.open = false
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

func TestReportFormsExist(t *testing.T) {
	for _, name := range []string{"false-positive.yml", "check-idea.yml", "bug.yml"} {
		data, err := readRepoFile(".github/ISSUE_TEMPLATE/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "false-positive.yml" {
			// The ids which falsePositiveURL fills must exist in the form.
			for _, id := range []string{"version", "check", "resource", "finding", "rule"} {
				if !strings.Contains(string(data), "id: "+id+"\n") {
					t.Errorf("%s has no field id %s", name, id)
				}
			}
		}
	}
}

func readRepoFile(p string) ([]byte, error) { return os.ReadFile(filepath.Join("..", p)) }
