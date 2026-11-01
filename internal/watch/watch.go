// Package watch discovers all resource types of a cluster and watches them
// with dynamic informers. Changes are sent as events on one channel.
package watch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	watchapi "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/pager"
)

// Upsert is sent when an object was added or changed.
type Upsert struct {
	GVR schema.GroupVersionResource
	Obj *unstructured.Unstructured
}

// Delete is sent when an object was deleted.
type Delete struct {
	GVR schema.GroupVersionResource
	UID string
}

// Synced is sent once per resource type, after the Upserts of the initial
// list. UIDs holds all objects which existed at that time.
type Synced struct {
	GVR  schema.GroupVersionResource
	UIDs map[string]struct{}
}

// Item is one object of a Snapshot.
type Item struct {
	GVR schema.GroupVersionResource
	Obj *unstructured.Unstructured
}

// Watcher watches all resource types accepted by Include.
type Watcher struct {
	Dynamic   dynamic.Interface
	Discovery discovery.DiscoveryInterface
	// Include decides which resource types get watched.
	Include func(schema.GroupResource) bool
	// Events receives Upsert, Delete and Synced values.
	Events chan<- any
	Log    *slog.Logger
	// SyncTimeout: Synced does not wait longer for an informer whose
	// initial list fails (e.g. a broken conversion webhook). Default 2m,
	// to stay within the startup probe of a Pod (5m in gitops).
	SyncTimeout time.Duration
	// Namespaces, for ListOnce: a namespaced type which may not be listed
	// in all namespaces (RBAC of a namespace user) is listed in each of
	// these instead.
	Namespaces []string

	mu        sync.Mutex
	informers map[schema.GroupResource]*running
	// skipped: types which may not be watched (probe failed), with the
	// error of the last probe.
	skipped map[schema.GroupResource]error
	// discovered: a refresh went through all resource types. Before that,
	// informers holds only the types started so far.
	discovered bool
	// ready: Synced returned true once. A type started later (a new CRD,
	// a new preferred version) does not block evaluations: see Pending.
	ready bool
}

type running struct {
	gvr      schema.GroupVersionResource
	informer cache.SharedIndexInformer
	cancel   context.CancelFunc
	synced   chan struct{}
	started  time.Time
	// calls: the outcome of the list and watch calls of the informer. Nil
	// for a placeholder.
	calls *calls
}

// calls records whether the list and watch calls of an informer fail. An
// informer retries forever, and its store keeps the objects of its last
// successful list: when access to a type is lost after the start (RBAC
// changed, a conversion webhook broke), its objects only go stale.
type calls struct {
	mu sync.Mutex
	// since: the first failed call after the last successful one; zero if
	// the last call succeeded.
	since  time.Time
	err    error
	warned bool
}

func (r *running) isSynced() bool {
	select {
	case <-r.synced:
		return true
	default:
		return false
	}
}

// Run discovers resource types and keeps the informers in line with them,
// re-running discovery every refresh. It blocks until ctx is done.
func (w *Watcher) Run(ctx context.Context, refresh time.Duration) error {
	w.mu.Lock()
	w.informers = map[schema.GroupResource]*running{}
	w.skipped = map[schema.GroupResource]error{}
	w.discovered = false
	w.ready = false
	w.mu.Unlock()

	ticker := time.NewTicker(refresh)
	defer ticker.Stop()
	for {
		if err := w.refresh(ctx); err != nil {
			if ctx.Err() == nil {
				w.Log.Error("discovery failed", "err", err)
			}
		} else {
			w.mu.Lock()
			w.discovered = true
			w.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			w.mu.Lock()
			for _, r := range w.informers {
				r.cancel()
			}
			w.mu.Unlock()
			return nil
		case <-ticker.C:
		}
	}
}

// Synced reports whether discovery went through all resource types and all
// informers have finished their initial list, or have tried for longer than
// SyncTimeout. Once true, it stays true: the objects of types which are not
// synced are missing from Snapshot, and Pending lists these types.
func (w *Watcher) Synced() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ready {
		return true
	}
	if !w.discovered || len(w.informers) == 0 {
		return false
	}
	timeout := w.syncTimeout()
	var late []string
	for _, r := range w.informers {
		if r.isSynced() {
			continue
		}
		if time.Since(r.started) < timeout {
			return false
		}
		late = append(late, r.gvr.String())
	}
	if len(late) > 0 {
		w.Log.Warn("resource types are not synced, evaluating without them", "resources", late, "timeout", timeout)
	}
	w.ready = true
	return true
}

func (w *Watcher) syncTimeout() time.Duration {
	if w.SyncTimeout <= 0 {
		return 2 * time.Minute
	}
	return w.SyncTimeout
}

// PendingType is a watched type whose informer has not finished its
// initial list. Stuck: it has tried for longer than SyncTimeout.
type PendingType struct {
	schema.GroupResource
	Stuck bool
}

// Pending returns the watched types whose informers have not finished their
// initial list.
func (w *Watcher) Pending() []PendingType {
	w.mu.Lock()
	defer w.mu.Unlock()
	var result []PendingType
	for gr, r := range w.informers {
		if !r.isSynced() {
			result = append(result, PendingType{GroupResource: gr, Stuck: time.Since(r.started) >= w.syncTimeout()})
		}
	}
	return result
}

// StaleType is a watched type whose list and watch calls have failed for
// longer than SyncTimeout, or whose first list has not finished after
// SyncTimeout. Snapshot still holds its objects as they were at the last
// successful call (none if it never synced).
type StaleType struct {
	schema.GroupResource
	Since time.Time
	Err   error
}

// Stale returns the watched types whose list and watch calls have failed
// for longer than SyncTimeout, or which have not synced within it.
func (w *Watcher) Stale() []StaleType {
	w.mu.Lock()
	rs := make([]*running, 0, len(w.informers))
	for _, r := range w.informers {
		rs = append(rs, r)
	}
	w.mu.Unlock()
	var result []StaleType
	now := time.Now()
	for _, r := range rs {
		if r.calls == nil {
			continue
		}
		r.calls.mu.Lock()
		since, err := r.calls.since, r.calls.err
		r.calls.mu.Unlock()
		if !r.isSynced() && (since.IsZero() || r.started.Before(since)) {
			// Its first list has not finished, even without failed calls
			// (a slow list, a WatchList stream without its end).
			since = r.started
			if err == nil {
				err = errors.New("the first list has not finished")
			}
		}
		if !since.IsZero() && now.Sub(since) >= w.syncTimeout() {
			result = append(result, StaleType{GroupResource: r.gvr.GroupResource(), Since: since, Err: err})
		}
	}
	return result
}

// SkippedType is a type which is not watched because the probe list call
// failed: Forbidden (RBAC), NotFound or MethodNotSupported. Reason is the
// Kubernetes status reason of the error (Forbidden, NotFound,
// MethodNotAllowed).
type SkippedType struct {
	schema.GroupResource
	Reason string
	Err    error
}

// Skipped returns the types which are not watched because konmon may not
// list them. The probe is tried again at every discovery.
func (w *Watcher) Skipped() []SkippedType {
	w.mu.Lock()
	defer w.mu.Unlock()
	result := make([]SkippedType, 0, len(w.skipped))
	for gr, err := range w.skipped {
		result = append(result, SkippedType{GroupResource: gr, Reason: string(apierrors.ReasonForError(err)), Err: err})
	}
	return result
}

// record notes the outcome of a list or watch call of the informer of gvr.
func (w *Watcher) record(ctx context.Context, gvr schema.GroupVersionResource, c *calls, err error) {
	if ctx.Err() != nil {
		return // the informer was stopped
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		if c.warned {
			w.Log.Info("watching resource again", "resource", gvr.String())
		}
		c.since, c.err, c.warned = time.Time{}, nil, false
		return
	}
	now := time.Now()
	if c.since.IsZero() {
		c.since = now
	}
	c.err = err
	if !c.warned && now.Sub(c.since) >= w.syncTimeout() {
		c.warned = true
		w.Log.Warn("cannot list or watch resource, its objects are stale", "resource", gvr.String(),
			"since", c.since.UTC().Format(time.RFC3339), "err", err)
	}
}

// recordInitialEvents passes on the events of a watch which streams all
// objects first (WatchList). It records success when the bookmark at the
// end of the initial objects arrives, and an error when an Error event or
// the end of the stream comes first.
func (w *Watcher) recordInitialEvents(ctx context.Context, gvr schema.GroupVersionResource, c *calls, wi watchapi.Interface) watchapi.Interface {
	p := &initialEventsWatch{Interface: wi, ch: make(chan watchapi.Event), stopped: make(chan struct{})}
	go func() {
		defer close(p.ch)
		done := false
		for ev := range wi.ResultChan() {
			if !done {
				switch ev.Type {
				case watchapi.Bookmark:
					if m, err := apimeta.Accessor(ev.Object); err == nil && m.GetAnnotations()[metav1.InitialEventsAnnotationKey] == "true" {
						done = true
						w.record(ctx, gvr, c, nil)
					}
				case watchapi.Error:
					done = true
					w.record(ctx, gvr, c, apierrors.FromObject(ev.Object))
				}
			}
			select {
			case p.ch <- ev:
			case <-p.stopped:
				return
			}
		}
		select {
		case <-p.stopped: // stopped by the reflector
		default:
			if !done {
				w.record(ctx, gvr, c, errors.New("watch ended before all objects were sent"))
			}
		}
	}()
	return p
}

type initialEventsWatch struct {
	watchapi.Interface
	ch      chan watchapi.Event
	stopped chan struct{}
	once    sync.Once
}

func (p *initialEventsWatch) ResultChan() <-chan watchapi.Event { return p.ch }

func (p *initialEventsWatch) Stop() {
	p.once.Do(func() { close(p.stopped) })
	p.Interface.Stop()
}

// Snapshot returns all objects of all synced informers.
func (w *Watcher) Snapshot() []Item {
	w.mu.Lock()
	rs := make([]*running, 0, len(w.informers))
	for _, r := range w.informers {
		rs = append(rs, r)
	}
	w.mu.Unlock()

	var items []Item
	for _, r := range rs {
		if !r.isSynced() {
			continue
		}
		for _, o := range r.informer.GetStore().List() {
			if u, ok := o.(*unstructured.Unstructured); ok {
				items = append(items, Item{GVR: r.gvr, Obj: u})
			}
		}
	}
	return items
}

// Available returns the types whose informers have synced.
func (w *Watcher) Available() []schema.GroupVersionResource {
	w.mu.Lock()
	defer w.mu.Unlock()
	result := make([]schema.GroupVersionResource, 0, len(w.informers))
	for _, r := range w.informers {
		if r.isSynced() {
			result = append(result, r.gvr)
		}
	}
	return result
}

// ListResult is the outcome of ListOnce.
type ListResult struct {
	Items     []Item
	Available []schema.GroupVersionResource
	// Forbidden types may not be listed (RBAC).
	Forbidden []schema.GroupVersionResource
	// Failed lists types (or API groups) which could not be listed or
	// discovered for other reasons.
	Failed []error
	// PerNamespace types are in Available, but listing them in all
	// namespaces is forbidden: their objects were only listed in these
	// namespaces of Watcher.Namespaces.
	PerNamespace map[schema.GroupVersionResource][]string
	// ForbiddenIn are the namespaces of Watcher.Namespaces in which these
	// PerNamespace types may not be listed.
	ForbiddenIn map[string][]schema.GroupVersionResource
}

// ListOnce lists all objects of all included types once, without
// informers, for an ad hoc check.
func (w *Watcher) ListOnce(ctx context.Context) (*ListResult, error) {
	gvrs, namespaced, partialErr, err := w.discover()
	if err != nil {
		return nil, err
	}
	res := &ListResult{}
	if partialErr != nil {
		res.Failed = append(res.Failed, partialErr)
	}
	type result struct {
		gvr       schema.GroupVersionResource
		items     []Item
		forbidden bool
		// listedIn and forbiddenIn: per namespace, after listing all
		// namespaces was forbidden.
		listedIn, forbiddenIn []string
		err                   error
	}
	results := make([]result, len(gvrs))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for i, gvr := range gvrs {
		g.Go(func() error {
			r := result{gvr: gvr}
			var err error
			r.items, err = w.list(gctx, gvr, metav1.NamespaceAll)
			if apierrors.IsForbidden(err) && namespaced[gvr] && len(w.Namespaces) > 0 {
				r.items = nil
				for _, ns := range w.Namespaces {
					items, nsErr := w.list(gctx, gvr, ns)
					if apierrors.IsForbidden(nsErr) {
						r.forbiddenIn = append(r.forbiddenIn, ns)
						continue
					}
					if err = nsErr; err != nil {
						break
					}
					r.items = append(r.items, items...)
					r.listedIn = append(r.listedIn, ns)
				}
				if err == nil && len(r.listedIn) == 0 {
					err = apierrors.NewForbidden(gvr.GroupResource(), "", errors.New("in all namespaces of --namespace"))
				}
			}
			switch {
			case err == nil:
			case apierrors.IsForbidden(err):
				r.forbidden = true
			case apierrors.IsNotFound(err), apierrors.IsMethodNotSupported(err):
			default:
				r.err = fmt.Errorf("list %s: %w", gvr.String(), err)
			}
			results[i] = r
			return nil
		})
	}
	_ = g.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, r := range results {
		switch {
		case r.forbidden:
			res.Forbidden = append(res.Forbidden, r.gvr)
		case r.err != nil:
			res.Failed = append(res.Failed, r.err)
		default:
			res.Items = append(res.Items, r.items...)
			res.Available = append(res.Available, r.gvr)
			if len(r.listedIn) > 0 {
				if res.PerNamespace == nil {
					res.PerNamespace = map[schema.GroupVersionResource][]string{}
					res.ForbiddenIn = map[string][]schema.GroupVersionResource{}
				}
				res.PerNamespace[r.gvr] = r.listedIn
				for _, ns := range r.forbiddenIn {
					res.ForbiddenIn[ns] = append(res.ForbiddenIn[ns], r.gvr)
				}
			}
		}
	}
	return res, nil
}

// list lists the objects of a type in one namespace, or in all.
func (w *Watcher) list(ctx context.Context, gvr schema.GroupVersionResource, namespace string) ([]Item, error) {
	var items []Item
	p := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return w.Dynamic.Resource(gvr).Namespace(namespace).List(ctx, opts)
	})
	err := p.EachListItem(ctx, metav1.ListOptions{}, func(obj runtime.Object) error {
		if u, ok := obj.(*unstructured.Unstructured); ok {
			u.SetManagedFields(nil)
			items = append(items, Item{GVR: gvr, Obj: u})
		}
		return nil
	})
	return items, err
}

func (w *Watcher) refresh(ctx context.Context) error {
	gvrs, _, partialErr, err := w.discover()
	if err != nil {
		return err
	}
	partial := partialErr != nil
	wanted := make(map[schema.GroupResource]schema.GroupVersionResource, len(gvrs))
	for _, gvr := range gvrs {
		wanted[gvr.GroupResource()] = gvr
	}

	// failedGroups: API groups whose discovery failed; their types may only
	// be missing because of that.
	failedGroups := map[string]bool{}
	var groupErr *discovery.ErrGroupDiscoveryFailed
	if errors.As(partialErr, &groupErr) {
		for gv := range groupErr.Groups {
			failedGroups[gv.Group] = true
		}
	}
	w.mu.Lock()
	for gr := range w.skipped {
		if _, ok := wanted[gr]; !ok && !failedGroups[gr.Group] {
			delete(w.skipped, gr) // the type is gone
		}
	}
	var gone []schema.GroupVersionResource
	for gr, r := range w.informers {
		gvr, ok := wanted[gr]
		if ok && gvr == r.gvr {
			continue
		}
		if !ok && partial {
			// The group may only be missing because its discovery failed.
			continue
		}
		w.Log.Info("stop watching", "resource", r.gvr.String())
		r.cancel()
		if ok {
			// A new preferred version: pending until start replaces it,
			// so its objects are not taken for gone in between.
			w.informers[gr] = &running{gvr: gvr, cancel: func() {}, synced: make(chan struct{}), started: time.Now()}
			continue
		}
		delete(w.informers, gr)
		gone = append(gone, r.gvr)
	}
	var toStart []schema.GroupVersionResource
	for gr, gvr := range wanted {
		if r, ok := w.informers[gr]; !ok || r.informer == nil {
			toStart = append(toStart, gvr)
		}
	}
	w.mu.Unlock()

	for _, gvr := range gone {
		// The resource type was removed (e.g. CRD deleted): all its objects are gone.
		select {
		case w.Events <- Synced{GVR: gvr, UIDs: map[string]struct{}{}}:
		case <-ctx.Done():
			return nil
		}
	}
	for _, gvr := range toStart {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.probe(ctx, gvr); err != nil {
			w.mu.Lock()
			if r, ok := w.informers[gvr.GroupResource()]; ok && r.informer == nil {
				delete(w.informers, gvr.GroupResource()) // the placeholder of a version change
			}
			_, known := w.skipped[gvr.GroupResource()]
			w.skipped[gvr.GroupResource()] = err
			w.mu.Unlock()
			if !known {
				w.Log.Warn("cannot watch resource, skipping", "resource", gvr.String(), "err", err)
			}
			continue
		}
		w.start(ctx, gvr)
	}
	return nil
}

// discover returns the preferred version of every listable and watchable
// resource type accepted by Include. partialErr is set if some API groups
// could not be discovered.
// discover returns the types to watch, and which of them are namespaced.
func (w *Watcher) discover() (gvrs []schema.GroupVersionResource, namespaced map[schema.GroupVersionResource]bool, partialErr, err error) {
	// The package-level helper is what DiscoveryClient.ServerPreferredResources
	// uses; calling it directly also works with the fake discovery client.
	lists, err := discovery.ServerPreferredResources(w.Discovery)
	if err != nil {
		if !discovery.IsGroupDiscoveryFailedError(err) || len(lists) == 0 {
			return nil, nil, nil, fmt.Errorf("server preferred resources: %w", err)
		}
		// Some API groups (often an orphaned APIService) failed. Go on with the rest.
		w.Log.Warn("partial discovery", "err", err)
		partialErr = fmt.Errorf("discovery: %w", err)
	}
	namespaced = map[schema.GroupVersionResource]bool{}
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for i := range list.APIResources {
			r := &list.APIResources[i]
			if strings.Contains(r.Name, "/") || !hasVerbs(r.Verbs, "list", "watch") {
				continue
			}
			gvr := gv.WithResource(r.Name)
			if w.Include != nil && !w.Include(gvr.GroupResource()) {
				continue
			}
			gvrs = append(gvrs, gvr)
			namespaced[gvr] = r.Namespaced
		}
	}
	return gvrs, namespaced, partialErr, nil
}

func hasVerbs(verbs metav1.Verbs, want ...string) bool {
	for _, v := range want {
		found := false
		for _, have := range verbs {
			if have == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// probe checks with a cheap list call that we may read this resource type.
// Starting an informer without permission would retry and log forever.
func (w *Watcher) probe(ctx context.Context, gvr schema.GroupVersionResource) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := w.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil && (apierrors.IsForbidden(err) || apierrors.IsNotFound(err) || apierrors.IsMethodNotSupported(err)) {
		return err
	}
	if err != nil && ctx.Err() == nil {
		// Other errors (e.g. timeouts) are transient: let the informer retry.
		w.Log.Warn("probe failed, watching anyway", "resource", gvr.String(), "err", err)
	}
	return nil
}

func (w *Watcher) start(ctx context.Context, gvr schema.GroupVersionResource) {
	ctx, cancel := context.WithCancel(ctx)
	c := &calls{}
	// The list watcher of dynamicinformer.NewFilteredDynamicInformer, plus
	// record.
	client := w.Dynamic.Resource(gvr)
	lw := &cache.ListWatch{
		ListWithContextFunc: func(lctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			obj, err := client.List(lctx, opts)
			w.record(ctx, gvr, c, err)
			return obj, err
		},
		WatchFuncWithContext: func(lctx context.Context, opts metav1.ListOptions) (watchapi.Interface, error) {
			wi, err := client.Watch(lctx, opts)
			if err == nil && opts.SendInitialEvents != nil && *opts.SendInitialEvents {
				// WatchList: the objects come in the stream, which can fail
				// after the call succeeded (e.g. a broken conversion webhook).
				return w.recordInitialEvents(ctx, gvr, c, wi), nil
			}
			w.record(ctx, gvr, c, err)
			return wi, err
		},
	}
	informer := cache.NewSharedIndexInformerWithOptions(cache.ToListWatcherWithWatchListSemantics(lw, w.Dynamic),
		&unstructured.Unstructured{}, cache.SharedIndexInformerOptions{ObjectDescription: gvr.String()})
	if err := informer.SetTransform(stripManagedFields); err != nil {
		w.Log.Error("set transform", "resource", gvr.String(), "err", err)
	}
	send := func(v any) {
		select {
		case w.Events <- v:
		case <-ctx.Done():
		}
	}
	reg, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				send(Upsert{GVR: gvr, Obj: u})
			}
		},
		UpdateFunc: func(_, obj any) {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				send(Upsert{GVR: gvr, Obj: u})
			}
		},
		DeleteFunc: func(obj any) {
			if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tomb.Obj
			}
			if u, ok := obj.(*unstructured.Unstructured); ok {
				send(Delete{GVR: gvr, UID: string(u.GetUID())})
			}
		},
	})
	if err != nil {
		w.Log.Error("add event handler", "resource", gvr.String(), "err", err)
		cancel()
		return
	}
	r := &running{gvr: gvr, informer: informer, cancel: cancel, synced: make(chan struct{}), started: time.Now(), calls: c}
	w.mu.Lock()
	w.informers[gvr.GroupResource()] = r
	delete(w.skipped, gvr.GroupResource())
	w.mu.Unlock()

	w.Log.Debug("start watching", "resource", gvr.String())
	go informer.RunWithContext(ctx)
	go func() {
		// reg.HasSynced is true once our handler got all objects of the
		// initial list, so Synced is sent after their Upserts.
		if !cache.WaitForCacheSync(ctx.Done(), reg.HasSynced) {
			return
		}
		uids := map[string]struct{}{}
		for _, o := range informer.GetStore().List() {
			if u, ok := o.(*unstructured.Unstructured); ok {
				uids[string(u.GetUID())] = struct{}{}
			}
		}
		close(r.synced)
		send(Synced{GVR: gvr, UIDs: uids})
	}()
}

// stripManagedFields drops metadata.managedFields: it is large and not
// interesting for monitoring.
func stripManagedFields(obj any) (any, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		u.SetManagedFields(nil)
		return u, nil
	}
	if acc, ok := obj.(metav1.Object); ok {
		acc.SetManagedFields(nil)
	}
	return obj, nil
}
