package checks

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

// TestFile is a *.test.yaml file: test cases for one check, run without a
// cluster by `konmon checks test`.
type TestFile struct {
	Check string     `json:"check"`
	Cases []TestCase `json:"cases"`
}

// TestCase runs the check on one object.
type TestCase struct {
	Name string `json:"name"`
	// Resource of the object: "pods", "deployments.apps", ...
	Resource string `json:"resource"`
	// Version defaults to v1.
	Version string `json:"version"`
	// Now defaults to 2026-01-01T00:00:00Z.
	Now     *time.Time     `json:"now"`
	Params  map[string]any `json:"params"`
	Object  map[string]any `json:"object"`
	Related []struct {
		Resource string         `json:"resource"`
		Version  string         `json:"version"`
		Object   map[string]any `json:"object"`
	} `json:"related"`
	// Available lists further resource types which exist but have no
	// objects.
	Available []string `json:"available"`
	History   []struct {
		UID                string    `json:"uid"`
		ObservedAt         time.Time `json:"observedAt"`
		Change             string    `json:"change"`
		Type               string    `json:"type"`
		Status             string    `json:"status"`
		Reason             string    `json:"reason"`
		Message            string    `json:"message"`
		LastTransitionTime string    `json:"lastTransitionTime"`
	} `json:"history"`
	// Expect lists the findings; only the given fields are compared.
	Expect []map[string]any `json:"expect"`
	// ExpectError is a substring of the error, if the check should fail.
	ExpectError string `json:"expectError"`
}

// ParseTestFile parses a *.test.yaml file.
func ParseTestFile(data []byte) (*TestFile, error) {
	var tf TestFile
	if err := yaml.UnmarshalStrict(data, &tf); err != nil {
		return nil, err
	}
	if tf.Check == "" {
		return nil, fmt.Errorf("check is missing")
	}
	return &tf, nil
}

// TestResult is the outcome of one case.
type TestResult struct {
	Check string
	Case  string
	Err   error
}

var defaultTestNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Tested reports whether RunTests applies to a test file: its check is
// enabled and, for a test of a core check, not replaced by a user check.
func (e *Engine) Tested(tf *TestFile, core bool) bool {
	for _, d := range e.defs {
		if d.Name == tf.Check {
			return !core || d.Core
		}
	}
	return !core
}

// RunTests runs the cases of a test file against the check of that name.
func (e *Engine) RunTests(tf *TestFile) []TestResult {
	var def *Definition
	for _, d := range e.defs {
		if d.Name == tf.Check {
			def = d
		}
	}
	results := make([]TestResult, 0, len(tf.Cases))
	for i := range tf.Cases {
		tc := &tf.Cases[i]
		name := tc.Name
		if name == "" {
			name = fmt.Sprintf("case %d", i+1)
		}
		var err error
		if def == nil {
			err = fmt.Errorf("no check named %q", tf.Check)
		} else {
			err = e.runCase(def, tc)
		}
		results = append(results, TestResult{Check: tf.Check, Case: name, Err: err})
	}
	return results
}

type testHistory map[string][]HistoryEntry

func (h testHistory) Conditions(uid string) []HistoryEntry { return h[uid] }

func (e *Engine) runCase(def *Definition, tc *TestCase) error {
	d := *def
	d.Params = make(map[string]any, len(def.Params))
	for k, v := range def.Params {
		d.Params[k] = v
	}
	for k, v := range tc.Params {
		if _, ok := d.Params[k]; !ok {
			return fmt.Errorf("check %s has no param %q", d.Name, k)
		}
		d.Params[k] = v
	}
	item, err := testItem(tc.Resource, tc.Version, tc.Object)
	if err != nil {
		return fmt.Errorf("object: %w", err)
	}
	items := []Item{item}
	available := []schema.GroupVersionResource{item.GVR}
	for i, r := range tc.Related {
		it, err := testItem(r.Resource, r.Version, r.Object)
		if err != nil {
			return fmt.Errorf("related %d: %w", i, err)
		}
		items = append(items, it)
		available = append(available, it.GVR)
	}
	for _, r := range tc.Available {
		gr, err := ParseResource(r)
		if err != nil {
			return fmt.Errorf("available: %w", err)
		}
		available = append(available, gr.WithVersion("v1"))
	}
	hist := testHistory{}
	for _, h := range tc.History {
		uid := h.UID
		if uid == "" {
			uid = string(item.Obj.GetUID())
		}
		hist[uid] = append(hist[uid], HistoryEntry{
			ObservedAt: h.ObservedAt, Change: h.Change, Type: h.Type, Status: h.Status,
			Reason: h.Reason, Message: h.Message, LastTransitionTime: h.LastTransitionTime,
		})
	}
	now := defaultTestNow
	if tc.Now != nil {
		now = *tc.Now
	}

	rt, err := newRuntime([]*Definition{&d}, e.opts)
	if err != nil {
		return err
	}
	snap := NewSnapshot(items, available)
	rt.begin(snap, hist, now)
	if !d.Matches(item.GVR.GroupResource()) {
		return fmt.Errorf("check %s does not match resource %s", d.Name, tc.Resource)
	}
	res := rt.run(task{item: item, defs: []*Definition{&d}})
	if len(res.failures) > 0 {
		if tc.ExpectError == "" {
			return fmt.Errorf("check failed: %w", res.failures[0].Err)
		}
		if !strings.Contains(res.failures[0].Err.Error(), tc.ExpectError) {
			return fmt.Errorf("error %q does not contain %q", res.failures[0].Err.Error(), tc.ExpectError)
		}
		return nil
	}
	if tc.ExpectError != "" {
		return fmt.Errorf("expected an error containing %q, got none", tc.ExpectError)
	}
	return compareFindings(res.findings, tc.Expect, now)
}

func testItem(resource, version string, obj map[string]any) (Item, error) {
	if obj == nil {
		return Item{}, fmt.Errorf("missing")
	}
	gr, err := ParseResource(resource)
	if err != nil {
		return Item{}, err
	}
	if version == "" {
		version = "v1"
	}
	u := &unstructured.Unstructured{Object: normalize(obj).(map[string]any)}
	if u.GetUID() == "" {
		u.SetUID(types.UID("uid-" + u.GetNamespace() + "-" + u.GetName()))
	}
	return Item{GVR: gr.WithVersion(version), Obj: u}, nil
}

// normalize turns whole float64 numbers (YAML/JSON) into int64, as the API
// server's objects have them.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = normalize(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = normalize(x)
		}
		return t
	case float64:
		if t == float64(int64(t)) {
			return int64(t)
		}
	}
	return v
}

func compareFindings(got []Finding, want []map[string]any, now time.Time) error {
	if len(got) != len(want) {
		return fmt.Errorf("got %d findings, want %d:\n%s", len(got), len(want), formatFindings(got, now))
	}
	for i, w := range want {
		g := findingFields(&got[i])
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			gv, ok := g[k]
			if !ok {
				return fmt.Errorf("finding %d: unknown field %q", i, k)
			}
			if !equalField(k, gv, w[k]) {
				return fmt.Errorf("finding %d: %s = %v, want %v\n%s", i, k, gv, w[k], formatFindings(got, now))
			}
		}
	}
	return nil
}

func findingFields(f *Finding) map[string]any {
	details := map[string]any{}
	for k, v := range f.Details {
		details[k] = v
	}
	since := ""
	if !f.Since.IsZero() {
		since = f.Since.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"key": f.Key, "message": f.Message, "since": since, "condition": f.Condition,
		"container": f.Container, "details": details,
	}
}

func equalField(k string, got, want any) bool {
	if k == "details" {
		wm, ok := want.(map[string]any)
		if !ok {
			return false
		}
		gm, _ := got.(map[string]any)
		for dk, dv := range wm {
			if fmt.Sprint(gm[dk]) != fmt.Sprint(dv) {
				return false
			}
		}
		return len(wm) == len(gm)
	}
	if k == "since" {
		if t, ok := want.(time.Time); ok {
			want = t.UTC().Format(time.RFC3339)
		}
		if s, ok := want.(string); ok {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				want = t.UTC().Format(time.RFC3339)
			}
		}
	}
	return reflect.DeepEqual(got, want) || fmt.Sprint(got) == fmt.Sprint(want)
}

func formatFindings(fs []Finding, now time.Time) string {
	var b strings.Builder
	for i := range fs {
		data, _ := json.Marshal(findingFields(&fs[i]))
		fmt.Fprintf(&b, "    %s\n      %s\n", fs[i].Line(now), data)
	}
	return b.String()
}
