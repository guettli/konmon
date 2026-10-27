package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/guettli/konmon/internal/checks"
)

// findingFilter selects the findings `check` and `findings` print.
type findingFilter struct {
	namespaces []string
	categories []string
	output     string
}

func (o *findingFilter) register(f *pflag.FlagSet) {
	f.StringSliceVarP(&o.namespaces, "namespace", "n", nil,
		"Only show findings of objects in these namespaces (cluster-scoped objects are left out; failing checks are always shown). Comma-separated, glob patterns allowed.")
	f.StringSliceVar(&o.categories, "category", nil, "Only show findings of these categories: health, hygiene (failing checks are always shown). Comma-separated. Default: both.")
	f.StringVarP(&o.output, "output", "o", "text", "Output format: text or json.")
}

func (o *findingFilter) validate() error {
	for _, c := range o.categories {
		if c != checks.CategoryHealth && c != checks.CategoryHygiene {
			return fmt.Errorf("--category %q: want %s or %s", c, checks.CategoryHealth, checks.CategoryHygiene)
		}
	}
	for _, p := range o.namespaces {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("--namespace: invalid pattern %q: %w", p, err)
		}
	}
	if o.output != "text" && o.output != "json" {
		return fmt.Errorf("--output %q: want text or json", o.output)
	}
	return nil
}

func (o *findingFilter) match(f *checks.Finding) bool {
	if f.Check == checks.ErrorCheck {
		// A failing check makes every result incomplete, whatever the
		// filter: never hide it.
		return true
	}
	if len(o.categories) > 0 && !slices.Contains(o.categories, f.Category) {
		return false
	}
	if len(o.namespaces) > 0 {
		if f.Namespace == "" || !matchGlob(f.Namespace, o.namespaces) {
			return false
		}
	}
	return true
}

// shownFinding is a finding with its text line, for printFindingLines.
type shownFinding struct {
	finding *checks.Finding
	line    string
}

// printFindingLines prints health findings first, then hygiene findings
// below a header, each group sorted.
func printFindingLines(w io.Writer, shown []shownFinding) (health, hygiene int) {
	var healthLines, hygieneLines []string
	for _, s := range shown {
		if s.finding.Category == checks.CategoryHygiene {
			hygieneLines = append(hygieneLines, s.line)
		} else {
			healthLines = append(healthLines, s.line)
		}
	}
	slices.Sort(healthLines)
	slices.Sort(hygieneLines)
	for _, l := range healthLines {
		_, _ = fmt.Fprintln(w, l)
	}
	if len(hygieneLines) > 0 {
		_, _ = fmt.Fprintln(w, "Hygiene (never alerts):")
		for _, l := range hygieneLines {
			_, _ = fmt.Fprintln(w, l)
		}
	}
	return len(healthLines), len(hygieneLines)
}

// countText is "1 health finding, 2 hygiene findings", without the
// categories which were not asked for (unless a failing check is counted).
func (o *findingFilter) countText(health, hygiene int) string {
	var parts []string
	if health > 0 || len(o.categories) == 0 || slices.Contains(o.categories, checks.CategoryHealth) {
		parts = append(parts, plural(health, "health finding"))
	}
	if len(o.categories) == 0 || slices.Contains(o.categories, checks.CategoryHygiene) {
		parts = append(parts, plural(hygiene, "hygiene finding"))
	}
	return strings.Join(parts, ", ")
}

// ignoredText is ", 3 ignored" or "".
func ignoredText(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(", %d ignored", n)
}

// plural is "1 check" or "2 checks".
func plural(n int, what string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, what)
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// jsonFinding is a finding in the output of -o json.
type jsonFinding struct {
	Check     string            `json:"check"`
	Category  string            `json:"category"`
	Severity  string            `json:"severity"`
	Resource  string            `json:"resource,omitempty"` // resource.group, as in --exclude-resource
	Kind      string            `json:"kind,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	Name      string            `json:"name,omitempty"`
	UID       string            `json:"uid,omitempty"`
	Key       string            `json:"key,omitempty"`
	Message   string            `json:"message"`
	Condition string            `json:"condition,omitempty"`
	Container string            `json:"container,omitempty"`
	Details   map[string]string `json:"details,omitempty"`
	Since     time.Time         `json:"since,omitzero"`
	FirstSeen time.Time         `json:"firstSeen,omitzero"`
}

func toJSONFinding(f *checks.Finding) jsonFinding {
	j := jsonFinding{
		Check: f.Check, Category: f.Category, Severity: f.Severity,
		Kind: f.Kind, Namespace: f.Namespace, Name: f.Name, UID: f.UID, Key: f.Key,
		Message: f.Message, Condition: f.Condition, Container: f.Container, Details: f.Details,
		Since: f.Since,
	}
	if f.GVR.Resource != "" {
		j.Resource = checks.FormatResource(f.GVR.GroupResource())
	}
	return j
}

// sortJSONFindings orders like the text output: health first, then by
// namespace, resource, name and check.
func sortJSONFindings(fs []jsonFinding) {
	slices.SortFunc(fs, func(a, b jsonFinding) int {
		ka := []string{categoryRank(a.Category), a.Namespace, a.Resource, a.Name, a.Check, a.Key}
		kb := []string{categoryRank(b.Category), b.Namespace, b.Resource, b.Name, b.Check, b.Key}
		return slices.Compare(ka, kb)
	})
}

func categoryRank(c string) string {
	if c == checks.CategoryHygiene {
		return "1"
	}
	return "0"
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// joinSorted sorts and joins names for messages.
func joinSorted(names []string) string {
	slices.Sort(names)
	return strings.Join(names, ", ")
}
