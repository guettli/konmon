package config

import (
	"strings"
	"testing"
	"time"

	"github.com/guettli/konmon/internal/checks"
)

func TestEscalate(t *testing.T) {
	cfg, err := Parse([]byte(`
escalate:
  - after: 1h
    comment: every finding which stays for an hour
  - check: pod-container-restarts
    message: CrashLoopBackOff
    after: 15m
  - namespace: dev-*
    after: 5m
    severity: warning
`), "konmon.yaml")
	if err != nil {
		t.Fatal(err)
	}
	crash := &checks.Finding{
		Check: "pod-container-restarts", Severity: "warning", Namespace: "nextcloud", Name: "db",
		Message: "container postgres restarted 406 times, currently CrashLoopBackOff",
	}
	info := &checks.Finding{Check: "suspended", Severity: "info", Namespace: "dev-a", Name: "cj"}
	failing := &checks.Finding{Check: checks.ErrorCheck, Severity: "warning", Key: "x"}
	cases := []struct {
		name    string
		f       *checks.Finding
		openFor time.Duration
		want    string
		rule    string
	}{
		{"too young", crash, 10 * time.Minute, "warning", ""},
		{"crash loop", crash, 15 * time.Minute, "critical", "konmon.yaml:escalate[1]"},
		{"catch-all", &checks.Finding{Check: "conditions", Severity: "warning", Namespace: "a"}, time.Hour, "critical", "konmon.yaml:escalate[0]"},
		{"info to warning", info, 6 * time.Minute, "warning", "konmon.yaml:escalate[2]"},
		{"info to critical wins", info, 2 * time.Hour, "critical", "konmon.yaml:escalate[0]"},
		{"a catch-all never escalates check-error", failing, 2 * time.Hour, "warning", ""},
		{"never lowers", &checks.Finding{Check: "x", Severity: "critical", Namespace: "dev-a"}, time.Hour, "critical", ""},
	}
	for _, c := range cases {
		sev, r := cfg.Escalated(c.f, c.openFor, now)
		src := ""
		if r != nil {
			src = r.Source
		}
		if sev != c.want || src != c.rule {
			t.Errorf("%s: got %s by %q, want %s by %q", c.name, sev, src, c.want, c.rule)
		}
	}
}

func TestEscalateErrors(t *testing.T) {
	for _, c := range []struct{ yaml, want string }{
		{"escalate:\n  - check: x\n", "after is required"},
		{"escalate:\n  - after: soon\n", "want a duration"},
		{"escalate:\n  - after: 1h\n    severity: info\n", "severity must be critical or warning"},
		{"escalate:\n  - after: 1h\n    nam: x\n", "field nam not found"},
		{"escalate:\n  - after: 1h\n    message: \"(\"\n", "message"},
	} {
		_, err := Parse([]byte(c.yaml), "k.yaml")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: want error with %q, got %v", c.yaml, c.want, err)
		}
	}
}
