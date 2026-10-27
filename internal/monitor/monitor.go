// Package monitor connects the watcher, the store and the notifier: it writes
// every change to the store, runs the checks periodically, and keeps the
// findings (and alerts) up to date.
package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/guettli/konmon/internal/checks"
	"github.com/guettli/konmon/internal/store"
	"github.com/guettli/konmon/internal/watch"
)

// Source provides the current objects of the cluster.
type Source interface {
	Synced() bool
	Snapshot() []watch.Item
	// Available returns the types whose objects are fully known.
	Available() []schema.GroupVersionResource
}

// Notifier receives the state of all findings after each evaluation.
type Notifier interface {
	Notify(ctx context.Context, now time.Time, firing, resolved []store.FindingRow) error
}

// Config configures a Monitor.
type Config struct {
	Checks *checks.Engine
	// IgnoreFindings drops findings whose Subject matches.
	IgnoreFindings []*regexp.Regexp
	// ExcludeNamespaces are glob patterns of namespaces to ignore completely.
	ExcludeNamespaces []string
	// StoreType decides which resource types are written to the database.
	// Types only read by checks stay in memory. nil stores everything.
	StoreType func(schema.GroupResource) bool
	// StoreRevisions decides for which resource types full revisions are kept.
	StoreRevisions func(schema.GroupResource) bool
	// HistoryWindow is how much condition history checks can see.
	HistoryWindow time.Duration
	// For is how long a finding must be open before it fires.
	For time.Duration
	// EvalInterval is the time between two evaluations.
	EvalInterval time.Duration
	// ResendResolved is how long resolved findings are passed to the notifier.
	ResendResolved time.Duration
	// Retention is how long history is kept. 0 keeps it forever.
	Retention time.Duration
	Log       *slog.Logger
}

// Monitor is the single writer of the store.
type Monitor struct {
	cfg      Config
	store    *store.Store
	source   Source
	notifier Notifier

	lastEval atomic.Int64 // unix ms of the last successful evaluation
}

// New creates a Monitor. notifier may be nil.
func New(cfg Config, s *store.Store, source Source, notifier Notifier) *Monitor {
	return &Monitor{cfg: cfg, store: s, source: source, notifier: notifier}
}

// LastEvaluation returns the time of the last successful evaluation.
func (m *Monitor) LastEvaluation() time.Time {
	v := m.lastEval.Load()
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

const (
	flushInterval = time.Second
	maxBatch      = 1000
	pruneInterval = time.Hour
)

// Run consumes events until ctx is done.
func (m *Monitor) Run(ctx context.Context, events <-chan any) error {
	flush := time.NewTicker(flushInterval)
	defer flush.Stop()
	eval := time.NewTicker(m.cfg.EvalInterval)
	defer eval.Stop()
	prune := time.NewTicker(pruneInterval)
	defer prune.Stop()

	var batch []any
	doFlush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := m.apply(ctx, batch, time.Now())
		batch = batch[:0]
		return err
	}
	for {
		select {
		case <-ctx.Done():
			// Use a fresh context: the events are already received and should
			// not get lost on shutdown.
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if len(batch) > 0 {
				return m.apply(flushCtx, batch, time.Now())
			}
			return nil
		case ev := <-events:
			batch = append(batch, ev)
			if len(batch) >= maxBatch {
				if err := doFlush(); err != nil {
					return err
				}
			}
		case <-flush.C:
			if err := doFlush(); err != nil {
				return err
			}
		case <-eval.C:
			if err := doFlush(); err != nil {
				return err
			}
			if err := m.Evaluate(ctx, time.Now()); err != nil {
				m.cfg.Log.Error("evaluation failed", "err", err)
			}
		case <-prune.C:
			m.prune(ctx, time.Now())
		}
	}
}

func (m *Monitor) excluded(namespace string) bool {
	if namespace == "" {
		return false
	}
	for _, p := range m.cfg.ExcludeNamespaces {
		if ok, err := path.Match(p, namespace); err == nil && ok {
			return true
		}
	}
	return false
}

func (m *Monitor) stored(gvr schema.GroupVersionResource) bool {
	return m.cfg.StoreType == nil || m.cfg.StoreType(gvr.GroupResource())
}

// apply writes a batch of watch events in one transaction.
func (m *Monitor) apply(ctx context.Context, batch []any, now time.Time) error {
	var changed, history, deleted int
	err := m.store.Update(ctx, func(tx *store.Tx) error {
		for _, ev := range batch {
			switch e := ev.(type) {
			case watch.Upsert:
				if m.excluded(e.Obj.GetNamespace()) || !m.stored(e.GVR) {
					continue
				}
				c, h, err := m.upsert(tx, e, now)
				if err != nil {
					return err
				}
				if c {
					changed++
				}
				history += h
			case watch.Delete:
				if err := tx.MarkDeleted(e.UID, now); err != nil {
					return err
				}
				deleted++
			case watch.Synced:
				if !m.stored(e.GVR) {
					continue
				}
				n, err := tx.MarkMissingDeleted(e.GVR.GroupResource(), e.UIDs, now)
				if err != nil {
					return err
				}
				if n > 0 {
					m.cfg.Log.Info("objects deleted while not watching", "resource", e.GVR.String(), "count", n)
				}
				deleted += n
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("apply %d events: %w", len(batch), err)
	}
	m.cfg.Log.Debug("applied events", "events", len(batch), "changed", changed, "conditionChanges", history, "deleted", deleted)
	return nil
}

func (m *Monitor) upsert(tx *store.Tx, e watch.Upsert, now time.Time) (changed bool, historyRows int, err error) {
	obj := e.Obj
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return false, 0, fmt.Errorf("encode %s %s/%s: %w", e.GVR.Resource, obj.GetNamespace(), obj.GetName(), err)
	}
	hash, err := contentHash(obj)
	if err != nil {
		return false, 0, err
	}
	ref := store.ObjectRef{
		GVR: e.GVR, Kind: obj.GetKind(), Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID()),
	}
	keep := m.cfg.StoreRevisions != nil && m.cfg.StoreRevisions(e.GVR.GroupResource())
	changed, err = tx.UpsertObject(ref, obj.GetResourceVersion(), hash, data, keep, now)
	if err != nil || !changed {
		return changed, 0, err
	}
	historyRows, err = tx.UpdateConditions(ref.UID, Conditions(obj, e.GVR), now)
	return changed, historyRows, err
}

// volatileConditionFields change without anything happening (heartbeats).
var volatileConditionFields = []string{"lastHeartbeatTime", "lastProbeTime", "lastUpdateTime"}

// contentHash hashes the object without fields which change without a real
// change: resourceVersion, managedFields and condition heartbeats.
func contentHash(obj *unstructured.Unstructured) (string, error) {
	c := obj.DeepCopy()
	unstructured.RemoveNestedField(c.Object, "metadata", "resourceVersion")
	unstructured.RemoveNestedField(c.Object, "metadata", "managedFields")
	for _, p := range conditionPaths {
		conds, found, err := unstructured.NestedSlice(c.Object, p...)
		if err != nil || !found {
			continue
		}
		for _, raw := range conds {
			if m, ok := raw.(map[string]any); ok {
				for _, f := range volatileConditionFields {
					delete(m, f)
				}
			}
		}
		if err := unstructured.SetNestedSlice(c.Object, conds, p...); err != nil {
			return "", err
		}
	}
	data, err := json.Marshal(c.Object)
	if err != nil {
		return "", fmt.Errorf("hash %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

var conditionPaths = [][]string{
	{"status", "conditions"},
	{"spec", "status", "conditions"}, // hetznerbaremetalhosts, see check-conditions
}

// Conditions extracts status.conditions of an object.
func Conditions(obj *unstructured.Unstructured, gvr schema.GroupVersionResource) []store.Condition {
	p := conditionPaths[0]
	if gvr.Resource == "hetznerbaremetalhosts" {
		p = conditionPaths[1]
	}
	raw, _, err := unstructured.NestedSlice(obj.Object, p...)
	if err != nil {
		return nil
	}
	result := make([]store.Condition, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		c := store.Condition{}
		c.Type, _ = m["type"].(string)
		if c.Type == "" {
			continue
		}
		c.Status, _ = m["status"].(string)
		c.Reason, _ = m["reason"].(string)
		c.Message, _ = m["message"].(string)
		c.LastTransitionTime, _ = m["lastTransitionTime"].(string)
		result = append(result, c)
	}
	return result
}

// Evaluate runs the checks, updates the findings and notifies. It does
// nothing until the source has synced: otherwise findings of objects which
// are not listed yet would be resolved.
func (m *Monitor) Evaluate(ctx context.Context, now time.Time) error {
	if !m.source.Synced() {
		m.cfg.Log.Debug("not synced yet, skipping evaluation")
		return nil
	}
	var items []checks.Item
	for _, it := range m.source.Snapshot() {
		if m.excluded(it.Obj.GetNamespace()) {
			continue
		}
		items = append(items, checks.Item{GVR: it.GVR, Obj: it.Obj})
	}
	var hist checks.History
	if m.cfg.Checks.UsesHistory() {
		rows, err := m.store.ConditionHistorySince(ctx, now.Add(-m.cfg.HistoryWindow))
		if err != nil {
			return err
		}
		hist = StoreHistory(rows)
	}
	// A slow check must not stall the event loop for long: Evaluate runs in
	// it. An aborted evaluation changes no findings, but the firing alerts
	// are sent again, or Alertmanager would resolve them.
	evalCtx, cancel := context.WithTimeout(ctx, 3*m.cfg.EvalInterval)
	res, err := m.cfg.Checks.Evaluate(evalCtx, checks.NewSnapshot(items, m.source.Available()), hist, now)
	cancel()
	if err != nil {
		nctx, ncancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		nerr := m.notifyOpen(nctx, now)
		ncancel()
		if nerr != nil {
			m.cfg.Log.Error("notify after aborted evaluation", "err", nerr)
		}
		return err
	}
	if res.Duration > m.cfg.EvalInterval {
		m.cfg.Log.Warn("evaluation is slower than --eval-interval", "duration", res.Duration.Round(time.Millisecond),
			"objects", res.Objects, "calls", res.Calls)
	}
	failed := make(map[[2]string]struct{}, len(res.Failures))
	for _, f := range res.Failures {
		failed[[2]string{f.Check, f.UID}] = struct{}{}
		m.cfg.Log.Warn("check failed", "check", f.Check, "uid", f.UID, "err", f.Err)
	}
	current := make([]checks.Finding, 0, len(res.Findings))
	for i := range res.Findings {
		if !m.ignored(&res.Findings[i]) {
			current = append(current, res.Findings[i])
		}
	}
	m.cfg.Log.Debug("evaluated", "objects", res.Objects, "calls", res.Calls, "findings", len(current),
		"failures", len(res.Failures), "duration", res.Duration.Round(time.Millisecond))

	// A check which failed for an object keeps its previous findings for
	// that object: otherwise they would resolve, and fire again later with
	// a new start time.
	keep := func(r *store.FindingRow) bool {
		_, ok := failed[[2]string{r.Finding.Check, r.Finding.UID}]
		return ok
	}
	var open, resolved []store.FindingRow
	err = m.store.Update(ctx, func(tx *store.Tx) error {
		var err error
		open, resolved, err = tx.SyncFindings(current, keep, now)
		return err
	})
	if err != nil {
		return err
	}
	for i := range resolved {
		f := &resolved[i].Finding
		m.cfg.Log.Info("finding resolved", "finding", f.Line(now), "openFor", now.Sub(resolved[i].FirstSeen).Round(time.Second))
	}
	var firing []store.FindingRow
	for _, r := range open {
		if forDur := m.forOf(&r.Finding); now.Sub(r.FirstSeen) >= forDur {
			// Log once: in the evaluation where the finding crossed For.
			if now.Sub(r.FirstSeen)-m.cfg.EvalInterval < forDur {
				m.cfg.Log.Info("finding firing", "finding", r.Finding.Line(now))
			}
			if r.Finding.Category == checks.CategoryHealth {
				firing = append(firing, r)
			}
		}
	}
	m.lastEval.Store(now.UnixMilli())
	if m.notifier == nil {
		return nil
	}
	recent, err := m.store.ResolvedSince(ctx, now.Add(-m.cfg.ResendResolved))
	if err != nil {
		return err
	}
	// Only report resolved findings which fired before.
	var resolvedFired []store.FindingRow
	for _, r := range recent {
		if r.ResolvedAt.Sub(r.FirstSeen) >= m.forOf(&r.Finding) && r.Finding.Category == checks.CategoryHealth {
			resolvedFired = append(resolvedFired, r)
		}
	}
	if err := m.notifier.Notify(ctx, now, firing, resolvedFired); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	return nil
}

// forOf is how long a finding must be open before it fires: the check's
// forSeconds, or --for.
func (m *Monitor) forOf(f *checks.Finding) time.Duration {
	if f.For > 0 {
		return f.For
	}
	return m.cfg.For
}

// notifyOpen sends the findings which are firing according to the store.
func (m *Monitor) notifyOpen(ctx context.Context, now time.Time) error {
	if m.notifier == nil {
		return nil
	}
	open, err := m.store.FindingsAt(ctx, now)
	if err != nil {
		return err
	}
	var firing []store.FindingRow
	for _, r := range open {
		if r.ResolvedAt.IsZero() && now.Sub(r.FirstSeen) >= m.forOf(&r.Finding) && r.Finding.Category == checks.CategoryHealth {
			firing = append(firing, r)
		}
	}
	return m.notifier.Notify(ctx, now, firing, nil)
}

func (m *Monitor) ignored(f *checks.Finding) bool {
	if len(m.cfg.IgnoreFindings) == 0 {
		return false
	}
	subject := f.Subject()
	for _, re := range m.cfg.IgnoreFindings {
		if re.MatchString(subject) {
			return true
		}
	}
	return false
}

// StoreHistory gives checks the condition history read from the store.
type StoreHistory map[string][]store.HistoryRow

func (h StoreHistory) Conditions(uid string) []checks.HistoryEntry {
	rows := h[uid]
	result := make([]checks.HistoryEntry, 0, len(rows))
	for _, r := range rows {
		result = append(result, checks.HistoryEntry{
			ObservedAt: r.ObservedAt, Change: r.Change, Type: r.Type, Status: r.Status,
			Reason: r.Reason, Message: r.Message, LastTransitionTime: r.LastTransitionTime,
		})
	}
	return result
}

func (m *Monitor) prune(ctx context.Context, now time.Time) {
	if m.cfg.Retention <= 0 {
		return
	}
	var n int64
	err := m.store.Update(ctx, func(tx *store.Tx) error {
		var err error
		n, err = tx.Prune(now.Add(-m.cfg.Retention))
		return err
	})
	if err != nil {
		m.cfg.Log.Error("prune failed", "err", err)
		return
	}
	m.cfg.Log.Info("pruned history", "rows", n, "olderThan", now.Add(-m.cfg.Retention).Format(time.RFC3339))
}
