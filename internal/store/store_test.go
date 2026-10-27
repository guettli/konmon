package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/guettli/konmon/internal/checks"
)

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "konmon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func update(t *testing.T, s *Store, fn func(tx *Tx) error) {
	t.Helper()
	if err := s.Update(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

var podRef = ObjectRef{
	GVR:  schema.GroupVersionResource{Version: "v1", Resource: "pods"},
	Kind: "Pod", Namespace: "ns", Name: "p1", UID: "uid-1",
}

func TestReopenKeepsSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.db")
	for range 2 {
		s, err := Open(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	}
}

func TestUpsertObjectSkipsUnchangedHash(t *testing.T) {
	s := openTestStore(t)
	var changes []bool
	update(t, s, func(tx *Tx) error {
		for i, hash := range []string{"a", "a", "b"} {
			changed, err := tx.UpsertObject(podRef, "1", hash, []byte(`{"n":1}`), true, t0.Add(time.Duration(i)*time.Second))
			if err != nil {
				return err
			}
			changes = append(changes, changed)
		}
		return nil
	})
	if changes[0] != true || changes[1] != false || changes[2] != true {
		t.Errorf("changes = %v, want [true false true]", changes)
	}
	revs, err := s.Revisions(context.Background(), "uid-1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 || string(revs[0].Object) != `{"n":1}` {
		t.Errorf("unexpected revisions: %+v", revs)
	}
}

func TestUpdateConditionsRecordsOnlyChanges(t *testing.T) {
	s := openTestStore(t)
	ready := Condition{Type: "Ready", Status: "True", LastTransitionTime: "t1"}
	notReady := Condition{Type: "Ready", Status: "False", Reason: "Crash", LastTransitionTime: "t2"}
	scheduled := Condition{Type: "PodScheduled", Status: "True"}

	steps := []struct {
		conditions []Condition
		wantRows   int
	}{
		{[]Condition{ready, scheduled}, 2},
		{[]Condition{ready, scheduled}, 0},
		{[]Condition{{Type: "Ready", Status: "True", LastTransitionTime: "t9"}, scheduled}, 0}, // only time changed
		{[]Condition{notReady, scheduled}, 1},
		{[]Condition{notReady}, 1}, // PodScheduled removed
	}
	for i, step := range steps {
		update(t, s, func(tx *Tx) error {
			n, err := tx.UpdateConditions("uid-1", step.conditions, t0.Add(time.Duration(i)*time.Minute))
			if n != step.wantRows {
				t.Errorf("step %d: %d history rows, want %d", i, n, step.wantRows)
			}
			return err
		})
	}
	history, err := s.ConditionHistory(context.Background(), "uid-1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 4 {
		t.Fatalf("got %d history rows, want 4: %+v", len(history), history)
	}
	last := history[3]
	if last.Change != "removed" || last.Type != "PodScheduled" {
		t.Errorf("last row = %+v, want removed PodScheduled", last)
	}
}

func TestMarkMissingDeleted(t *testing.T) {
	s := openTestStore(t)
	other := podRef
	other.Name, other.UID = "p2", "uid-2"
	update(t, s, func(tx *Tx) error {
		for _, r := range []ObjectRef{podRef, other} {
			if _, err := tx.UpsertObject(r, "1", "h", []byte("{}"), false, t0); err != nil {
				return err
			}
		}
		n, err := tx.MarkMissingDeleted(podRef.GVR.GroupResource(), map[string]struct{}{"uid-1": {}}, t0.Add(time.Hour))
		if n != 1 {
			t.Errorf("marked %d deleted, want 1", n)
		}
		return err
	})
	objs, err := s.FindObjects(context.Background(), "pod", "", "ns", "p2")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || !objs[0].DeletedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("expected p2 deleted at t0+1h, got %+v", objs)
	}
}

func finding(name string) checks.Finding {
	return checks.Finding{
		Check: "conditions", Category: checks.CategoryHealth, GVR: podRef.GVR, Namespace: "ns", Name: name,
		UID: "uid-" + name, Key: "Ready", Condition: "Ready", Message: "Condition Ready=False",
	}
}

func TestSyncFindingsOpensAndResolves(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	sync := func(at time.Time, fs ...checks.Finding) (open, resolved []FindingRow) {
		update(t, s, func(tx *Tx) error {
			var err error
			open, resolved, err = tx.SyncFindings(fs, nil, at)
			return err
		})
		return open, resolved
	}

	open, _ := sync(t0, finding("a"), finding("b"))
	if len(open) != 2 {
		t.Fatalf("want 2 open, got %d", len(open))
	}
	open, resolved := sync(t0.Add(time.Minute), finding("a"))
	if len(open) != 1 || !open[0].FirstSeen.Equal(t0) {
		t.Errorf("finding a should stay open since t0: %+v", open)
	}
	if len(resolved) != 1 || resolved[0].Finding.Name != "b" {
		t.Errorf("finding b should be resolved: %+v", resolved)
	}
	// b comes back: a new period starts.
	open, _ = sync(t0.Add(2*time.Minute), finding("a"), finding("b"))
	if len(open) != 2 {
		t.Fatalf("want 2 open, got %d", len(open))
	}

	at, err := s.FindingsAt(ctx, t0.Add(90*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(at) != 1 || at[0].Finding.Name != "a" {
		t.Errorf("at t0+90s only a was open, got %+v", at)
	}
	hist, err := s.FindingsOfObject(ctx, "uid-b", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Errorf("b should have two periods, got %d", len(hist))
	}
}

func TestSyncFindingsKeepsFindingsOfFailedChecks(t *testing.T) {
	s := openTestStore(t)
	update(t, s, func(tx *Tx) error {
		_, _, err := tx.SyncFindings([]checks.Finding{finding("a"), finding("b")}, nil, t0)
		return err
	})
	var open, resolved []FindingRow
	update(t, s, func(tx *Tx) error {
		var err error
		open, resolved, err = tx.SyncFindings(nil, func(r *FindingRow) bool { return r.Finding.Name == "a" }, t0.Add(time.Minute))
		return err
	})
	if len(open) != 1 || open[0].Finding.Name != "a" || !open[0].FirstSeen.Equal(t0) {
		t.Errorf("a should stay open since t0: %+v", open)
	}
	if len(resolved) != 1 || resolved[0].Finding.Name != "b" {
		t.Errorf("b should be resolved: %+v", resolved)
	}
}

func TestConditionHistorySinceAndObjects(t *testing.T) {
	s := openTestStore(t)
	update(t, s, func(tx *Tx) error {
		if _, err := tx.UpsertObject(podRef, "1", "h", []byte(`{"kind":"Pod"}`), false, t0); err != nil {
			return err
		}
		if _, err := tx.UpdateConditions("uid-1", []Condition{{Type: "Ready", Status: "False"}}, t0); err != nil {
			return err
		}
		_, err := tx.UpdateConditions("uid-1", []Condition{{Type: "Ready", Status: "True"}}, t0.Add(time.Hour))
		return err
	})
	h, err := s.ConditionHistorySince(context.Background(), t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(h["uid-1"]) != 1 || h["uid-1"][0].Status != "True" {
		t.Errorf("history: %+v", h)
	}
	var seen []string
	err = s.Objects(context.Background(), func(ref ObjectRef, data []byte) error {
		seen = append(seen, ref.Name+" "+string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != `p1 {"kind":"Pod"}` {
		t.Errorf("objects: %v", seen)
	}
}

func TestPrune(t *testing.T) {
	s := openTestStore(t)
	update(t, s, func(tx *Tx) error {
		if _, err := tx.UpsertObject(podRef, "1", "h", []byte("{}"), true, t0); err != nil {
			return err
		}
		if _, err := tx.UpdateConditions("uid-1", []Condition{{Type: "Ready", Status: "True"}}, t0); err != nil {
			return err
		}
		if err := tx.MarkDeleted("uid-1", t0); err != nil {
			return err
		}
		n, err := tx.Prune(t0.Add(time.Hour))
		if n != 3 { // revision, history row, deleted object
			t.Errorf("pruned %d rows, want 3", n)
		}
		return err
	})
}

func TestOpenRefusesDevelopmentSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(context.Background(), "PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := Open(context.Background(), path); err == nil || !strings.Contains(err.Error(), "development version") {
		t.Errorf("want an error for schema version 1, got %v", err)
	}
}

func TestOpenExisting(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.db")
	if _, err := OpenExisting(context.Background(), missing); err == nil {
		t.Error("want an error for a missing file")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("OpenExisting created %s", missing)
	}
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExisting(context.Background(), empty); err == nil || !strings.Contains(err.Error(), "not a konmon database") {
		t.Errorf("want an error for an empty file, got %v", err)
	}
	if info, _ := os.Stat(empty); info.Size() != 0 {
		t.Errorf("OpenExisting changed %s", empty)
	}
	path := filepath.Join(dir, "konmon.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = OpenExisting(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// A copy in a read-only directory, with the WAL not checkpointed.
	s, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "konmon.db"), data, 0o400); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	s, err = OpenExisting(context.Background(), filepath.Join(ro, "konmon.db"))
	if err != nil {
		t.Fatalf("read-only copy: %v", err)
	}
	if _, err := s.FindingsAt(context.Background(), time.Now()); err != nil {
		t.Errorf("query read-only copy: %v", err)
	}
	_ = s.Close()
}
