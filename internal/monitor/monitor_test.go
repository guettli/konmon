package monitor

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/guettli/konmon/internal/checks"
	"github.com/guettli/konmon/internal/store"
	"github.com/guettli/konmon/internal/watch"
)

var (
	t0      = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	jobsGVR = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
)

type fakeSource struct {
	synced bool
	items  []watch.Item
}

func (f *fakeSource) Synced() bool           { return f.synced }
func (f *fakeSource) Snapshot() []watch.Item { return f.items }
func (f *fakeSource) Available() []schema.GroupVersionResource {
	return []schema.GroupVersionResource{jobsGVR}
}

type recordingNotifier struct {
	firing, resolved []store.FindingRow
}

func (r *recordingNotifier) Notify(_ context.Context, _ time.Time, firing, resolved []store.FindingRow) error {
	r.firing, r.resolved = firing, resolved
	return nil
}

func job(name, uid string, failed bool, heartbeat string) *unstructured.Unstructured {
	status := "False"
	if failed {
		status = "True"
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"status": map[string]any{
			"conditions": []any{
				map[string]any{
					"type": "Failed", "status": status, "reason": "BackoffLimitExceeded",
					"lastHeartbeatTime": heartbeat,
				},
			},
		},
	}}
	obj.SetNamespace("ns")
	obj.SetName(name)
	obj.SetUID(types.UID(uid))
	return obj
}

// flaky throws for jobs named "flaky" whose Failed condition is False, so
// tests can make a check fail for an object that had findings.
const flaky = `
import { defineCheck } from "konmon";
export default defineCheck({
  name: "flaky",
  match: { resources: ["jobs.batch"] },
  check(job) {
    if (job.metadata.name !== "flaky") return [];
    if (job.status.conditions[0].status === "False") throw new Error("boom");
    return [{ message: "job failed" }];
  },
});`

const tidy = `
export default {
  name: "tidy",
  category: "hygiene",
  match: { resources: ["jobs.batch"] },
  check(job) { return [{ message: "jobs should have labels" }]; },
};`

func newTestMonitor(t *testing.T, src Source, n Notifier) (*Monitor, *store.Store) {
	t.Helper()
	engine, err := checks.Load(checks.CoreFiles().Sources, []checks.Source{
		{Path: "flaky.ts", Code: flaky}, {Path: "tidy.js", Code: tidy},
	}, checks.Options{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "konmon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m := New(Config{
		Checks:            engine,
		ExcludeNamespaces: []string{"ignored-*"},
		StoreRevisions:    func(schema.GroupResource) bool { return true },
		For:               time.Minute,
		EvalInterval:      30 * time.Second,
		ResendResolved:    15 * time.Minute,
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, st, src, n)
	return m, st
}

func TestContentHashIgnoresHeartbeatAndResourceVersion(t *testing.T) {
	a := job("j", "u1", false, "t1")
	b := job("j", "u1", false, "t2")
	b.SetResourceVersion("42")
	c := job("j", "u1", true, "t1")
	ha, _ := contentHash(a)
	hb, _ := contentHash(b)
	hc, _ := contentHash(c)
	if ha != hb {
		t.Error("heartbeat or resourceVersion change should not change the hash")
	}
	if ha == hc {
		t.Error("status change should change the hash")
	}
	if _, found, _ := unstructured.NestedSlice(b.Object, "status", "conditions"); !found {
		t.Error("contentHash must not modify the object")
	}
}

func TestApplyRecordsConditionHistory(t *testing.T) {
	m, st := newTestMonitor(t, &fakeSource{}, nil)
	ctx := context.Background()
	steps := []any{
		watch.Upsert{GVR: jobsGVR, Obj: job("j", "u1", false, "t1")},
		watch.Upsert{GVR: jobsGVR, Obj: job("j", "u1", false, "t2")}, // heartbeat only
		watch.Upsert{GVR: jobsGVR, Obj: job("j", "u1", true, "t3")},
		watch.Upsert{GVR: jobsGVR, Obj: job("x", "u2", true, "t1").DeepCopy()},
	}
	for i, ev := range steps {
		if err := m.apply(ctx, []any{ev}, t0.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	history, err := st.ConditionHistory(ctx, "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[1].Status != "True" {
		t.Errorf("want 2 history rows ending with Failed=True, got %+v", history)
	}
	revs, err := st.Revisions(ctx, "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 {
		t.Errorf("want 2 revisions (heartbeat skipped), got %d", len(revs))
	}

	// Synced without u2: it was deleted while konmon was not watching.
	if err := m.apply(ctx, []any{watch.Synced{GVR: jobsGVR, UIDs: map[string]struct{}{"u1": {}}}}, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	objs, err := st.FindObjects(ctx, "jobs", "batch", "ns", "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].DeletedAt.IsZero() {
		t.Errorf("x should be marked deleted: %+v", objs)
	}
}

func TestApplySkipsExcludedNamespaces(t *testing.T) {
	m, st := newTestMonitor(t, &fakeSource{}, nil)
	obj := job("j", "u1", true, "")
	obj.SetNamespace("ignored-team")
	if err := m.apply(context.Background(), []any{watch.Upsert{GVR: jobsGVR, Obj: obj}}, t0); err != nil {
		t.Fatal(err)
	}
	objs, err := st.FindObjects(context.Background(), "jobs", "", "", "j")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 0 {
		t.Errorf("object in excluded namespace was stored: %+v", objs)
	}
}

func TestEvaluateFiresAfterForAndResolves(t *testing.T) {
	src := &fakeSource{}
	n := &recordingNotifier{}
	m, _ := newTestMonitor(t, src, n)
	ctx := context.Background()

	src.items = []watch.Item{{GVR: jobsGVR, Obj: job("j", "u1", true, "")}}
	if err := m.Evaluate(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if !m.LastEvaluation().IsZero() {
		t.Fatal("must not evaluate before the source is synced")
	}

	src.synced = true
	if err := m.Evaluate(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if len(n.firing) != 0 {
		t.Errorf("finding is pending, should not fire yet: %+v", n.firing)
	}
	if err := m.Evaluate(ctx, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(n.firing) != 1 {
		t.Fatalf("finding should fire after For, got %+v", n.firing)
	}

	src.items = []watch.Item{{GVR: jobsGVR, Obj: job("j", "u1", false, "")}}
	if err := m.Evaluate(ctx, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(n.firing) != 0 || len(n.resolved) != 1 {
		t.Errorf("want 0 firing, 1 resolved; got %d, %d", len(n.firing), len(n.resolved))
	}
	if err := m.Evaluate(ctx, t0.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(n.resolved) != 0 {
		t.Errorf("resolved alert should not be re-sent after ResendResolved: %+v", n.resolved)
	}
}

func TestEvaluateDoesNotReportPendingResolved(t *testing.T) {
	src := &fakeSource{synced: true}
	n := &recordingNotifier{}
	m, _ := newTestMonitor(t, src, n)
	ctx := context.Background()
	src.items = []watch.Item{{GVR: jobsGVR, Obj: job("j", "u1", true, "")}}
	if err := m.Evaluate(ctx, t0); err != nil {
		t.Fatal(err)
	}
	src.items = nil
	if err := m.Evaluate(ctx, t0.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(n.resolved) != 0 {
		t.Errorf("a finding which never fired must not be sent as resolved: %+v", n.resolved)
	}
}

func firingChecks(rows []store.FindingRow) []string {
	result := make([]string, 0, len(rows))
	for _, r := range rows {
		result = append(result, r.Finding.Check+" "+r.Finding.Name)
	}
	return result
}

func TestEvaluateKeepsFindingsOfFailedChecksAndSkipsHygiene(t *testing.T) {
	src := &fakeSource{synced: true}
	n := &recordingNotifier{}
	m, st := newTestMonitor(t, src, n)
	ctx := context.Background()
	src.items = []watch.Item{{GVR: jobsGVR, Obj: job("flaky", "u1", true, "")}}
	if err := m.Evaluate(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if err := m.Evaluate(ctx, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := firingChecks(n.firing); len(got) != 2 || got[0] != "conditions flaky" || got[1] != "flaky flaky" {
		t.Fatalf("want conditions and flaky firing (not the hygiene check), got %v", got)
	}

	// The check throws now: its finding must stay open with its start
	// time, and a check-error finding opens.
	src.items = []watch.Item{{GVR: jobsGVR, Obj: job("flaky", "u1", false, "")}}
	if err := m.Evaluate(ctx, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	open, err := st.FindingsAt(ctx, t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(open))
	for _, r := range open {
		names = append(names, r.Finding.Check)
		if r.Finding.Check == "flaky" && !r.FirstSeen.Equal(t0) {
			t.Errorf("flaky finding was reopened: %+v", r)
		}
	}
	want := map[string]bool{"flaky": true, checks.ErrorCheck: true, "tidy": true}
	if len(names) != len(want) {
		t.Fatalf("open findings: %v", names)
	}
	for _, name := range names {
		if !want[name] {
			t.Errorf("unexpected open finding %s", name)
		}
	}
	if len(n.resolved) != 1 || n.resolved[0].Finding.Check != "conditions" {
		t.Errorf("only the conditions finding is resolved: %+v", n.resolved)
	}
}

func TestAbortedEvaluationResendsFiringAlerts(t *testing.T) {
	src := &fakeSource{synced: true}
	n := &recordingNotifier{}
	m, _ := newTestMonitor(t, src, n)
	ctx := context.Background()
	src.items = []watch.Item{{GVR: jobsGVR, Obj: job("j", "u1", true, "")}}
	if err := m.Evaluate(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if err := m.Evaluate(ctx, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(n.firing) != 1 {
		t.Fatalf("want 1 firing, got %d", len(n.firing))
	}
	n.firing = nil
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := m.Evaluate(cancelled, t0.Add(2*time.Minute)); err == nil {
		t.Fatal("want an error for an aborted evaluation")
	}
	if len(n.firing) != 1 {
		t.Errorf("firing alerts must be re-sent after an aborted evaluation, got %d", len(n.firing))
	}
}

func TestCheckForSecondsOverridesFor(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "konmon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	engine, err := checks.Load(nil, []checks.Source{{Path: "slow.js", Code: `
export default { name: "slow", forSeconds: 300, match: { resources: ["jobs.batch"] }, check() { return [{ message: "x" }]; } };`}},
		checks.Options{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{synced: true, items: []watch.Item{{GVR: jobsGVR, Obj: job("j", "u1", false, "")}}}
	n := &recordingNotifier{}
	m := New(Config{
		Checks: engine, For: time.Minute, EvalInterval: 30 * time.Second, ResendResolved: 15 * time.Minute,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, st, src, n)
	ctx := context.Background()
	for _, at := range []time.Duration{0, 2 * time.Minute} {
		if err := m.Evaluate(ctx, t0.Add(at)); err != nil {
			t.Fatal(err)
		}
	}
	if len(n.firing) != 0 {
		t.Fatalf("must not fire before forSeconds (5m): %d", len(n.firing))
	}
	if err := m.Evaluate(ctx, t0.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(n.firing) != 1 {
		t.Fatalf("must fire after forSeconds: %d", len(n.firing))
	}
}
