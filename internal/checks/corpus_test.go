package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/guettli/check-conditions/pkg/checkconditions"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The core checks are a port of check-conditions. This test runs both on
// the same objects and compares the findings. The committed corpus holds
// hand-written edge cases. For real data, point KONMON_CORPUS_DB at a konmon
// database (it is not committed: it contains cluster data):
//
//	KONMON_CORPUS_DB=konmon.db go test ./internal/checks -run Corpus -v
//
// The database is read with a small query here instead of the store package,
// which imports this package.

type corpusItem struct {
	GVR    schema.GroupVersionResource `json:"gvr"`
	Object map[string]any              `json:"object"`
}

func TestCorpusMatchesCheckConditions(t *testing.T) {
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	items := readCorpusDir(t, filepath.Join("testdata", "corpus"))
	if db := os.Getenv("KONMON_CORPUS_DB"); db != "" {
		items = append(items, readCorpusDB(t, db)...)
		now = time.Now()
	}
	if len(items) == 0 {
		t.Fatal("empty corpus")
	}
	compareWithCheckConditions(t, items, now)
}

func compareWithCheckConditions(t *testing.T, items []Item, now time.Time) {
	t.Helper()
	e, err := Load(CoreFiles().Sources, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	args := &checkconditions.Arguments{
		WarnDeletionTimestampOlderThan: 10 * time.Minute,
		PodStartGracePeriod:            30 * time.Second,
		PodRestartWarnCount:            5,
	}
	want := map[string]bool{}
	for _, it := range items {
		if checkconditions.SkipResource(it.GVR.GroupResource()) {
			continue
		}
		for _, f := range args.Check(it.Obj, it.GVR, now) {
			if intendedDifference(&f) {
				continue
			}
			want[goKey(&f)] = true
		}
	}
	var available []schema.GroupVersionResource
	seen := map[schema.GroupVersionResource]bool{}
	for _, it := range items {
		if !seen[it.GVR] {
			seen[it.GVR] = true
			available = append(available, it.GVR)
		}
	}
	res, err := e.Evaluate(context.Background(), NewSnapshot(items, available), nil, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Failures {
		t.Errorf("check %s failed for %s: %v", f.Check, f.UID, f.Err)
	}
	got := map[string]bool{}
	for i := range res.Findings {
		got[tsKey(&res.Findings[i])] = true
	}
	var missing, extra []string
	for k := range want {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	for k := range got {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	for _, k := range missing {
		t.Errorf("only check-conditions reports: %s", k)
	}
	for _, k := range extra {
		t.Errorf("only konmon reports: %s", k)
	}
	t.Logf("%d objects, %d findings, %d differences", len(items), len(want), len(missing)+len(extra))
}

// intendedDifference reports findings of check-conditions which the
// konmon checks deliberately do not report (rules added after the port).
func intendedDifference(f *checkconditions.Finding) bool {
	return f.Kind == checkconditions.FindingCondition && f.Status == "False" &&
		len(f.ConditionTypes) == 1 && f.ConditionTypes[0] == "Suspended"
}

func goKey(f *checkconditions.Finding) string {
	uid := string(f.UID)
	switch f.Kind {
	case checkconditions.FindingCondition:
		return fmt.Sprintf("conditions %s %s=%s %s %q since=%s", uid, strings.Join(f.ConditionTypes, "/"),
			f.Status, f.Reason, f.Message, formatSince(f.LastTransitionTime))
	case checkconditions.FindingDeletionTimestamp:
		return fmt.Sprintf("deletion-timestamp %s since=%s", uid, formatSince(f.LastTransitionTime))
	case checkconditions.FindingContainerRestarts:
		return fmt.Sprintf("pod-container-restarts %s %s %d %s", uid, f.Container, f.RestartCount, f.State)
	case checkconditions.FindingError:
		return "conditions " + uid + " invalid"
	}
	return "unknown " + uid
}

func tsKey(f *Finding) string {
	switch f.Check {
	case "conditions":
		if f.Key == "invalid" {
			return "conditions " + f.UID + " invalid"
		}
		return fmt.Sprintf("conditions %s %s=%s %s %q since=%s", f.UID, f.Condition,
			f.Details["status"], f.Details["reason"], f.Details["message"], formatSince(f.Since))
	case "deletion-timestamp":
		return fmt.Sprintf("deletion-timestamp %s since=%s", f.UID, formatSince(f.Since))
	case "pod-container-restarts":
		return fmt.Sprintf("pod-container-restarts %s %s %s %s", f.UID, f.Container, f.Details["restartCount"], f.Details["state"])
	}
	return f.Check + " " + f.UID + " " + f.Message
}

func formatSince(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func readCorpusDir(t *testing.T, dir string) []Item {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	items := make([]Item, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var list []corpusItem
		if err := json.Unmarshal(data, &list); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		for _, c := range list {
			obj := normalize(c.Object).(map[string]any)
			items = append(items, Item{GVR: c.GVR, Obj: &unstructured.Unstructured{Object: obj}})
		}
	}
	return items
}

// BenchmarkEvaluate measures one evaluation of the core checks on a real
// database (KONMON_CORPUS_DB).
func BenchmarkEvaluate(b *testing.B) {
	db := os.Getenv("KONMON_CORPUS_DB")
	if db == "" {
		b.Skip("KONMON_CORPUS_DB is not set")
	}
	t := &testing.T{}
	items := readCorpusDB(t, db)
	var available []schema.GroupVersionResource
	seen := map[schema.GroupVersionResource]bool{}
	for _, it := range items {
		if !seen[it.GVR] {
			seen[it.GVR] = true
			available = append(available, it.GVR)
		}
	}
	e, err := Load(CoreFiles().Sources, nil, Options{})
	if err != nil {
		b.Fatal(err)
	}
	snap := NewSnapshot(items, available)
	now := time.Now()
	b.ResetTimer()
	for b.Loop() {
		if _, err := e.Evaluate(context.Background(), snap, nil, now); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(items)), "objects")
}
