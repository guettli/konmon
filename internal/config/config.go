// Package config reads konmon's config file: rules which ignore findings,
// and the polarity of condition types. It is meant to be reviewed and kept
// in git, e.g. as a ConfigMap.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/guettli/konmon/internal/checks"
)

// Config is the content of one or more config files.
type Config struct {
	Ignore []*IgnoreRule `yaml:"ignore"`
	// Acknowledged are real problems somebody looked at: they are still
	// reported, but 'konmon triage' does not ask about them again.
	Acknowledged []*IgnoreRule `yaml:"acknowledged"`
	// Conditions teaches the conditions check which status of a condition
	// type is healthy, by resource type ("resource.group", a bare name is
	// the core group, "*" any resource).
	Conditions map[string]*ConditionRules `yaml:"conditions"`
}

// ConditionRules is the polarity of condition types of one resource type.
// Unlike an ignore rule, a taught condition is still reported when it has
// the other status.
type ConditionRules struct {
	HealthyWhenTrue  []string `yaml:"healthyWhenTrue,omitempty" json:"healthyWhenTrue,omitempty"`
	HealthyWhenFalse []string `yaml:"healthyWhenFalse,omitempty" json:"healthyWhenFalse,omitempty"`
	// IgnoreTypes are informational: both values are fine.
	IgnoreTypes []string `yaml:"ignoreTypes,omitempty" json:"ignoreTypes,omitempty"`
}

// IgnoreRule drops the findings it matches. All fields which are set must
// match; Check, Resource, Namespace, Name, Condition, Container and Key are
// glob patterns (path.Match, but "*" also matches "/"), Message is a regular expression.
type IgnoreRule struct {
	Check     string `yaml:"check,omitempty"`
	Resource  string `yaml:"resource,omitempty"` // resource.group, as in --exclude-resource
	Namespace string `yaml:"namespace,omitempty"`
	Name      string `yaml:"name,omitempty"`
	Condition string `yaml:"condition,omitempty"`
	Container string `yaml:"container,omitempty"`
	Key       string `yaml:"key,omitempty"`
	Message   string `yaml:"message,omitempty"`
	// Until is the last day (YYYY-MM-DD, UTC) on which the rule matches.
	Until string `yaml:"until,omitempty"`
	// Comment says why the findings are fine. Required.
	Comment string `yaml:"comment"`

	// Source is "file:index" for messages.
	Source  string         `yaml:"-"`
	message *regexp.Regexp `yaml:"-"`
	until   time.Time      `yaml:"-"` // exclusive end
}

// Load reads and merges config files. No paths: an empty config.
func Load(paths []string) (*Config, error) {
	cfg := &Config{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		c, err := Parse(data, p)
		if err != nil {
			return nil, err
		}
		cfg.Ignore = append(cfg.Ignore, c.Ignore...)
		cfg.Acknowledged = append(cfg.Acknowledged, c.Acknowledged...)
		for resource, r := range c.Conditions {
			if cfg.Conditions == nil {
				cfg.Conditions = map[string]*ConditionRules{}
			}
			merged := cfg.Conditions[resource]
			if merged == nil {
				merged = &ConditionRules{}
				cfg.Conditions[resource] = merged
			}
			merged.HealthyWhenTrue = append(merged.HealthyWhenTrue, r.HealthyWhenTrue...)
			merged.HealthyWhenFalse = append(merged.HealthyWhenFalse, r.HealthyWhenFalse...)
			merged.IgnoreTypes = append(merged.IgnoreTypes, r.IgnoreTypes...)
			if err := merged.validate(); err != nil {
				return nil, fmt.Errorf("%s: conditions %s: %w", p, resource, err)
			}
		}
	}
	return cfg, nil
}

// Parse parses one config file. name is used in error messages.
func Parse(data []byte, name string) (*Config, error) {
	cfg := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // a typo must not ignore nothing, or everything
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var more any
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		// Rules in a second document would be silently dropped.
		return nil, fmt.Errorf("%s: only one YAML document is allowed (no ---)", name)
	}
	conditions := make(map[string]*ConditionRules, len(cfg.Conditions))
	for key, r := range cfg.Conditions {
		resource := "*"
		if key != "*" {
			gr, err := checks.ParseResource(key)
			if err != nil {
				return nil, fmt.Errorf("%s: conditions: %w", name, err)
			}
			resource = checks.FormatResource(gr)
		}
		if r == nil {
			return nil, fmt.Errorf("%s: conditions %s is empty", name, key)
		}
		if _, dup := conditions[resource]; dup {
			return nil, fmt.Errorf("%s: conditions %s is given twice", name, resource)
		}
		if err := r.validate(); err != nil {
			return nil, fmt.Errorf("%s: conditions %s: %w", name, key, err)
		}
		conditions[resource] = r
	}
	cfg.Conditions = conditions
	for _, list := range []struct {
		name  string
		rules []*IgnoreRule
	}{{"ignore", cfg.Ignore}, {"acknowledged", cfg.Acknowledged}} {
		for i, r := range list.rules {
			if r == nil {
				return nil, fmt.Errorf("%s: %s[%d] is empty", name, list.name, i)
			}
			r.Source = fmt.Sprintf("%s:%s[%d]", name, list.name, i)
			if err := r.Compile(); err != nil {
				return nil, fmt.Errorf("%s: %w", r.Source, err)
			}
		}
	}
	return cfg, nil
}

func (r *ConditionRules) validate() error {
	seen := map[string]string{}
	for _, l := range []struct {
		name  string
		types []string
	}{{"healthyWhenTrue", r.HealthyWhenTrue}, {"healthyWhenFalse", r.HealthyWhenFalse}, {"ignoreTypes", r.IgnoreTypes}} {
		for _, t := range l.types {
			if strings.TrimSpace(t) == "" {
				return fmt.Errorf("%s: empty condition type", l.name)
			}
			if other, ok := seen[t]; ok && other != l.name {
				return fmt.Errorf("condition type %s is in %s and %s", t, other, l.name)
			}
			seen[t] = l.name
		}
	}
	if len(seen) == 0 {
		return errors.New("no condition types: use healthyWhenTrue, healthyWhenFalse or ignoreTypes")
	}
	return nil
}

// Compile checks the rule and prepares it for Match. Parse calls it; call
// it for a rule which is built in code.
func (r *IgnoreRule) Compile() error {
	r.Resource = strings.ToLower(r.Resource) // resource names are lowercase
	globs := map[string]string{
		"check": r.Check, "resource": r.Resource, "namespace": r.Namespace, "name": r.Name,
		"condition": r.Condition, "container": r.Container, "key": r.Key,
	}
	matchesAll := r.Message == ""
	for field, g := range globs {
		if g == "" {
			continue
		}
		if strings.Trim(g, "*") != "" {
			matchesAll = false
		}
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("%s: invalid pattern %q: %w", field, g, err)
		}
	}
	if matchesAll {
		return errors.New("a rule needs at least one of check, resource, namespace, name, condition, container, key, message, other than \"*\"")
	}
	if strings.TrimSpace(r.Comment) == "" {
		return errors.New("comment is required: say why these findings are fine")
	}
	if r.Message != "" {
		re, err := regexp.Compile(r.Message)
		if err != nil {
			return fmt.Errorf("message: %w", err)
		}
		r.message = re
	}
	if r.Until != "" {
		t, err := time.Parse(time.DateOnly, r.Until)
		if err != nil {
			return fmt.Errorf("until: want YYYY-MM-DD, got %q", r.Until)
		}
		r.until = t.AddDate(0, 0, 1)
	}
	return nil
}

// Expired reports whether the rule's until has passed.
func (r *IgnoreRule) Expired(now time.Time) bool {
	return !r.until.IsZero() && !now.Before(r.until)
}

// Match reports whether the rule drops the finding. A check-error finding is
// only matched by a rule with check: check-error (key is the name of the
// failing check), so a broad rule never hides a failing check.
func (r *IgnoreRule) Match(f *checks.Finding, now time.Time) bool {
	if r.Expired(now) {
		return false
	}
	if f.Check == checks.ErrorCheck && (r.Check != checks.ErrorCheck || strings.Trim(r.Key, "*") == "") {
		// Only a rule with the name of the failing check hides it. Triage
		// up to v0.0.14 wrote rules without the key.
		return false
	}
	resource := ""
	if f.GVR.Resource != "" {
		resource = checks.FormatResource(f.GVR.GroupResource())
	}
	if r.Namespace != "" && f.Namespace == "" {
		return false // a namespace pattern never matches cluster-scoped objects
	}
	if !glob(r.Check, f.Check) || !glob(r.Resource, resource) || !glob(r.Namespace, f.Namespace) ||
		!glob(r.Name, f.Name) || !glob(r.Container, f.Container) || !glob(r.Key, f.Key) {
		return false
	}
	if r.Condition != "" && !matchCondition(r.Condition, f.Condition) {
		return false
	}
	if r.message != nil && !r.message.MatchString(f.Message) {
		return false
	}
	return true
}

// matchCondition matches the whole condition or one of the types of a
// grouped finding ("Failed/FailureTarget").
func matchCondition(pattern, condition string) bool {
	if glob(pattern, condition) {
		return true
	}
	for _, t := range strings.Split(condition, "/") {
		if glob(pattern, t) {
			return true
		}
	}
	return false
}

// glob is path.Match, except that "*" also matches "/": keys like
// "backend/ns/svc:80" have slashes.
func glob(pattern, s string) bool {
	if pattern == "" {
		return true
	}
	ok, _ := path.Match(strings.ReplaceAll(pattern, "/", "\x00"), strings.ReplaceAll(s, "/", "\x00"))
	return ok
}

// Ignored returns the first rule which drops the finding, or nil.
func (c *Config) Ignored(f *checks.Finding, now time.Time) *IgnoreRule {
	for _, r := range c.Ignore {
		if r.Match(f, now) {
			return r
		}
	}
	return nil
}

// IsAcknowledged returns the acknowledged rule which matches the finding,
// or nil.
func (c *Config) IsAcknowledged(f *checks.Finding, now time.Time) *IgnoreRule {
	for _, r := range c.Acknowledged {
		if r.Match(f, now) {
			return r
		}
	}
	return nil
}

// Expired returns the rules whose until has passed.
func (c *Config) Expired(now time.Time) []*IgnoreRule {
	var result []*IgnoreRule
	for _, r := range c.Ignore {
		if r.Expired(now) {
			result = append(result, r)
		}
	}
	return result
}
