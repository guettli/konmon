package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/guettli/konmon/internal/checks"
)

// EscalateRule raises the severity of the findings it matches once they are
// open for After: a warning which nobody fixed for an hour is not a warning
// any more. The match fields are those of an ignore rule; a rule without
// any of them matches every finding.
type EscalateRule struct {
	Check     string `yaml:"check,omitempty"`
	Resource  string `yaml:"resource,omitempty"`
	Namespace string `yaml:"namespace,omitempty"`
	Name      string `yaml:"name,omitempty"`
	Condition string `yaml:"condition,omitempty"`
	Container string `yaml:"container,omitempty"`
	Key       string `yaml:"key,omitempty"`
	Message   string `yaml:"message,omitempty"`
	// After is how long the finding must be open, e.g. 30m. Required.
	After string `yaml:"after"`
	// Severity is the new severity: critical (default) or warning. A rule
	// never lowers a severity.
	Severity string `yaml:"severity,omitempty"`
	Comment  string `yaml:"comment,omitempty"`

	// Source is "file:index" for messages.
	Source string        `yaml:"-"`
	after  time.Duration `yaml:"-"`
	match  *IgnoreRule   `yaml:"-"` // nil: every finding
}

// Compile checks the rule and prepares it for Match. Parse calls it; call
// it for a rule which is built in code.
func (r *EscalateRule) Compile() error {
	if r.After == "" {
		return errors.New("after is required, e.g. after: 30m")
	}
	d, err := time.ParseDuration(r.After)
	if err != nil || d < 0 {
		return fmt.Errorf("after: want a duration like 30m or 2h, got %q", r.After)
	}
	r.after = d
	if r.Severity == "" {
		r.Severity = "critical"
	}
	if r.Severity != "critical" && r.Severity != "warning" {
		return fmt.Errorf("severity must be critical or warning, got %q", r.Severity)
	}
	m := &IgnoreRule{
		Check: r.Check, Resource: r.Resource, Namespace: r.Namespace, Name: r.Name,
		Condition: r.Condition, Container: r.Container, Key: r.Key, Message: r.Message,
		Comment: "escalate",
	}
	if strings.Trim(m.Check+m.Resource+m.Namespace+m.Name+m.Condition+m.Container+m.Key, "*") == "" && m.Message == "" {
		r.match = nil // every finding
		return nil
	}
	if err := m.Compile(); err != nil {
		return err
	}
	r.match = m
	return nil
}

// AfterDuration is the parsed After.
func (r *EscalateRule) AfterDuration() time.Duration { return r.after }

// Match reports whether the rule escalates the finding, open for openFor.
func (r *EscalateRule) Match(f *checks.Finding, openFor time.Duration, now time.Time) bool {
	if openFor < r.after {
		return false
	}
	if r.match == nil {
		return f.Check != checks.ErrorCheck || r.Check == checks.ErrorCheck
	}
	return r.match.Match(f, now)
}

var severityRank = map[string]int{"info": 1, "warning": 2, "critical": 3}

// Escalated returns the severity of a finding which is open for openFor and
// the rule which raised it, or the finding's severity and nil. Of several
// matching rules the highest severity wins.
func (c *Config) Escalated(f *checks.Finding, openFor time.Duration, now time.Time) (string, *EscalateRule) {
	sev := f.Severity
	var rule *EscalateRule
	for _, r := range c.Escalate {
		if severityRank[r.Severity] > severityRank[sev] && r.Match(f, openFor, now) {
			sev, rule = r.Severity, r
		}
	}
	return sev, rule
}
