package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guettli/konmon/internal/usage"
)

var podGR = podRef.GVR.GroupResource()

func TestUsageBuckets(t *testing.T) {
	s := openTestStore(t)
	web := usage.Key{Namespace: "ns", Kind: "Deployment", Workload: "web", Container: "app"}
	db := usage.Key{Namespace: "other", Kind: "StatefulSet", Workload: "db", Container: "pg"}
	update(t, s, func(tx *Tx) error {
		// Two Pods of web in the first bucket, one in the second.
		if err := tx.AddUsage(t0, []usage.Sample{{Key: web, CPU: 0.1, Memory: 100}, {Key: db, CPU: 1, Memory: 5000}}); err != nil {
			return err
		}
		if err := tx.AddUsage(t0, []usage.Sample{{Key: web, CPU: 0.3, Memory: 300}}); err != nil {
			return err
		}
		return tx.AddUsage(t0.Add(time.Hour), []usage.Sample{{Key: web, CPU: 0.2, Memory: 50}})
	})

	all, err := s.Usage(context.Background(), t0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d buckets, want 3: %+v", len(all), all)
	}
	got, err := s.Usage(context.Background(), t0, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("namespace filter: got %d buckets, want 2", len(got))
	}
	first := got[0]
	if !first.Start.Equal(t0) {
		first = got[1]
	}
	if first.Samples != 2 || first.CPUSum < 0.399 || first.CPUSum > 0.401 || first.CPUMax != 0.3 || first.MemMax != 300 {
		t.Errorf("first bucket: %+v", first)
	}
	later, err := s.Usage(context.Background(), t0.Add(time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 1 {
		t.Errorf("since filter: got %d buckets, want 1", len(later))
	}

	update(t, s, func(tx *Tx) error {
		_, err := tx.Prune(t0.Add(time.Minute))
		return err
	})
	left, err := s.Usage(context.Background(), time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || !left[0].Start.Equal(t0.Add(time.Hour)) {
		t.Errorf("after prune: %+v", left)
	}
}

func TestUsageTableAddedWithoutVersionBump(t *testing.T) {
	path := filepath.Join(t.TempDir(), "konmon.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	// A database of a release before the usage table.
	if _, err := s.db.ExecContext(context.Background(), "DROP TABLE usage"); err != nil {
		t.Fatal(err)
	}
	update(t, s, func(tx *Tx) error {
		_, err := tx.UpsertObject(podRef, "1", "h1", []byte(`{"kind":"Pod"}`), false, t0)
		return err
	})
	_ = s.Close()

	// Read-only commands work on it and explain the missing usage data.
	ro, err := OpenExisting(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.Usage(context.Background(), t0, ""); err == nil || !strings.Contains(err.Error(), "no usage data") {
		t.Errorf("want a no-usage-data error, got %v", err)
	}
	_ = ro.Close()

	s, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	update(t, s, func(tx *Tx) error {
		return tx.AddUsage(t0, []usage.Sample{{Key: usage.Key{Namespace: "ns", Kind: "Pod", Workload: "p1", Container: "c"}, CPU: 1, Memory: 1}})
	})
	raw, err := s.ObjectJSON(context.Background(), podGR, "ns", "p1")
	if err != nil || string(raw) != `{"kind":"Pod"}` {
		t.Errorf("object kept: %q, %v", raw, err)
	}
	// The version stays: the previous release must still open the database.
	var version int
	if err := s.db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Errorf("schema version %d (%v), want 2", version, err)
	}
}

func TestObjectJSONSkipsDeleted(t *testing.T) {
	s := openTestStore(t)
	update(t, s, func(tx *Tx) error {
		if _, err := tx.UpsertObject(podRef, "1", "h1", []byte(`{}`), false, t0); err != nil {
			return err
		}
		return tx.MarkDeleted(podRef.UID, t0)
	})
	raw, err := s.ObjectJSON(context.Background(), podGR, "ns", "p1")
	if err != nil || raw != nil {
		t.Errorf("deleted object: %q, %v", raw, err)
	}
}
