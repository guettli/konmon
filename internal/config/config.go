// Package config reads konmon's config file: rules which ignore findings.
// It is meant to be reviewed and kept in git, e.g. as a ConfigMap.
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
}

// IgnoreRule drops the findings it matches. All fields which are set must
// match; Check, Resource, Namespace, Name, Condition, Container and Key are
// glob patterns (path.Match), Message is a regular expression.
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
	for i, r := range cfg.Ignore {
		if r == nil {
			return nil, fmt.Errorf("%s: ignore[%d] is empty", name, i)
		}
		r.Source = fmt.Sprintf("%s:ignore[%d]", name, i)
		if err := r.compile(); err != nil {
			return nil, fmt.Errorf("%s: %w", r.Source, err)
		}
	}
	return cfg, nil
}

func (r *IgnoreRule) compile() error {
	globs := map[string]string{
		"check": r.Check, "resource": r.Resource, "namespace": r.Namespace, "name": r.Name,
		"condition": r.Condition, "container": r.Container, "key": r.Key,
	}
	empty := r.Message == ""
	for field, g := range globs {
		if g == "" {
			continue
		}
		empty = false
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("%s: invalid pattern %q: %w", field, g, err)
		}
	}
	if empty {
		return errors.New("a rule needs at least one of check, resource, namespace, name, condition, container, key, message")
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
// only matched by a rule with check: check-error, so a broad rule never hides
// a failing check.
func (r *IgnoreRule) Match(f *checks.Finding, now time.Time) bool {
	if r.Expired(now) {
		return false
	}
	if f.Check == checks.ErrorCheck && r.Check != checks.ErrorCheck {
		return false
	}
	resource := ""
	if f.GVR.Resource != "" {
		resource = checks.FormatResource(f.GVR.GroupResource())
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

func glob(pattern, s string) bool {
	if pattern == "" {
		return true
	}
	ok, _ := path.Match(pattern, s)
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
