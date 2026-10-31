package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/guettli/konmon/internal/checks"
)

// Editor adds rules to a config file and keeps its comments and order.
type Editor struct {
	path string
	root *yaml.Node // the mapping of the document
	doc  *yaml.Node
}

// header is written into a config file which konmon creates.
const header = `# konmon config: ignore rules and condition polarity.
# See https://github.com/guettli/konmon#ignoring-findings-the-config-file
`

// OpenEditor reads the config file at path; a missing file is a new, empty
// config.
func OpenEditor(path string) (*Editor, error) {
	// Write the target of a symlink (e.g. into a dotfiles or gitops repo),
	// not over the link.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	e := &Editor{path: path}
	data, err := os.ReadFile(path)
	isNew := errors.Is(err, os.ErrNotExist)
	if err != nil && !isNew {
		return nil, err
	}
	if _, err := Parse(data, path); err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if doc.Kind == 0 { // empty or comments only: keep the comments
		doc = yaml.Node{Kind: yaml.DocumentNode, HeadComment: strings.TrimSpace(string(data))}
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: not a mapping", path)
	}
	if isNew {
		doc.HeadComment = strings.TrimSpace(header)
	}
	e.doc, e.root = &doc, doc.Content[0]
	return e, nil
}

// Path is the file the editor writes.
func (e *Editor) Path() string { return e.path }

// value returns the value of key in a mapping, creating it with kind if
// it is missing (or null).
func value(m *yaml.Node, key string, kind yaml.Kind) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			if v.Tag == "!!null" {
				*v = yaml.Node{Kind: kind}
			}
			return v
		}
	}
	v := &yaml.Node{Kind: kind}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
	return v
}

// AddIgnore appends a rule to the ignore list.
func (e *Editor) AddIgnore(r *IgnoreRule) error { return e.addRule("ignore", r) }

// AddAcknowledged appends a rule to the acknowledged list.
func (e *Editor) AddAcknowledged(r *IgnoreRule) error { return e.addRule("acknowledged", r) }

func (e *Editor) addRule(list string, r *IgnoreRule) error {
	if err := r.Compile(); err != nil {
		return err
	}
	var n yaml.Node
	if err := n.Encode(r); err != nil {
		return err
	}
	seq := value(e.root, list, yaml.SequenceNode)
	seq.Style = 0 // block style: "ignore: []" would put all rules on one line
	seq.Content = append(seq.Content, &n)
	return nil
}

// Polarity values for Teach.
const (
	HealthyWhenTrue  = "healthyWhenTrue"
	HealthyWhenFalse = "healthyWhenFalse"
	IgnoreTypes      = "ignoreTypes"
)

// Teach adds condition types to the polarity of a resource type
// (resource.group or "*"). list is HealthyWhenTrue, HealthyWhenFalse or
// IgnoreTypes. A type which is already in another list is moved.
func (e *Editor) Teach(resource, list string, types []string) error {
	if !slices.Contains([]string{HealthyWhenTrue, HealthyWhenFalse, IgnoreTypes}, list) {
		return fmt.Errorf("unknown polarity %q", list)
	}
	conditions := value(e.root, "conditions", yaml.MappingNode)
	if conditions.Kind != yaml.MappingNode {
		return errors.New("conditions is not a mapping")
	}
	resource, err := conditionsKey(conditions, resource)
	if err != nil {
		return err
	}
	rules := value(conditions, resource, yaml.MappingNode)
	if rules.Kind != yaml.MappingNode {
		return fmt.Errorf("conditions %s is not a mapping", resource)
	}
	for _, other := range []string{HealthyWhenTrue, HealthyWhenFalse, IgnoreTypes} {
		if other == list {
			continue
		}
		for i := 0; i+1 < len(rules.Content); i += 2 {
			if rules.Content[i].Value != other {
				continue
			}
			seq := rules.Content[i+1]
			seq.Content = slices.DeleteFunc(seq.Content, func(n *yaml.Node) bool { return slices.Contains(types, n.Value) })
		}
	}
	// Drop lists which are empty now.
	for i := 0; i+1 < len(rules.Content); {
		if rules.Content[i+1].Kind == yaml.SequenceNode && len(rules.Content[i+1].Content) == 0 {
			rules.Content = slices.Delete(rules.Content, i, i+2)
			continue
		}
		i += 2
	}
	seq := value(rules, list, yaml.SequenceNode)
	if len(seq.Content) == 0 {
		seq.Style = yaml.FlowStyle // new lists: [A, B]; existing ones keep their style
	}
	for _, t := range types {
		if !slices.ContainsFunc(seq.Content, func(n *yaml.Node) bool { return n.Value == t }) {
			seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: t})
		}
	}
	return nil
}

// conditionsKey returns the key in conditions which means the same resource
// type as resource, for example "Deployments.Apps" for "deployments.apps",
// or resource if there is none.
func conditionsKey(conditions *yaml.Node, resource string) (string, error) {
	want, err := normalizeResource(resource)
	if err != nil {
		return "", err
	}
	for i := 0; i+1 < len(conditions.Content); i += 2 {
		if key, err := normalizeResource(conditions.Content[i].Value); err == nil && key == want {
			return conditions.Content[i].Value, nil
		}
	}
	return want, nil
}

func normalizeResource(s string) (string, error) {
	if s == "*" {
		return s, nil
	}
	gr, err := checks.ParseResource(s)
	if err != nil {
		return "", err
	}
	return checks.FormatResource(gr), nil
}

// Bytes returns the file content.
func (e *Editor) Bytes() ([]byte, error) {
	if len(e.root.Content) == 0 {
		// Nothing but comments: an empty mapping would be written as {}.
		if e.doc.HeadComment == "" {
			return nil, nil
		}
		return []byte(e.doc.HeadComment + "\n"), nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(e.doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Config parses the current content.
func (e *Editor) Config() (*Config, error) {
	data, err := e.Bytes()
	if err != nil {
		return nil, err
	}
	return Parse(data, e.path)
}

// Save checks the content and writes the file atomically.
func (e *Editor) Save() error {
	data, err := e.Bytes()
	if err != nil {
		return err
	}
	if _, err := Parse(data, e.path); err != nil {
		return fmt.Errorf("not saved, the result is invalid: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(e.path), ".konmon-*.yaml")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	mode := os.FileMode(0o644) // a config file, no secrets
	if info, err := os.Stat(e.path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), e.path)
}
