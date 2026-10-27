package checks

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dop251/goja"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// maxFindingsPerCall protects against a check that returns one finding per
// array element of something large.
const maxFindingsPerCall = 100

// HistoryEntry is one change of a condition.
type HistoryEntry struct {
	ObservedAt         time.Time
	Change             string // "set" or "removed"
	Type               string
	Status             string
	Reason             string
	Message            string
	LastTransitionTime string
}

// History gives checks the condition history of objects.
type History interface {
	Conditions(uid string) []HistoryEntry
}

// jsRuntime is one goja runtime with all checks loaded. goja runtimes are not
// safe for concurrent use; each worker owns one.
type jsRuntime struct {
	vm      *goja.Runtime
	opts    Options
	checks  []*loadedCheck
	byName  map[string]*loadedCheck
	now     time.Time
	rand    uint64
	snap    *Snapshot
	hist    History
	current *loadedCheck
	// Saved before any check code runs: a check may replace the globals.
	freezeFn goja.Callable
	dateCtor *goja.Object
	// broken is set if preparing an evaluation failed; all calls fail.
	broken error
}

type loadedCheck struct {
	def *Definition
	fn  goja.Callable
	// base holds the functions of ctx; each call gets a fresh, frozen ctx
	// with base as prototype, so a check cannot leave state for the next
	// object (which may run in another runtime).
	base *goja.Object
}

var errTimeout = errors.New("timeout")

func newRuntime(defs []*Definition, opts Options) (*jsRuntime, error) {
	rt := &jsRuntime{vm: goja.New(), opts: opts, byName: map[string]*loadedCheck{}}
	// Checks must give the same result for the same input: Date and
	// Math.random are deterministic.
	rt.vm.SetTimeSource(func() time.Time { return rt.now })
	rt.vm.SetRandSource(rt.random)
	console := rt.vm.NewObject()
	for _, name := range []string{"log", "info", "warn", "error", "debug"} {
		if err := console.Set(name, rt.log); err != nil {
			return nil, err
		}
	}
	if err := rt.vm.Set("console", console); err != nil {
		return nil, err
	}
	rt.freezeFn, _ = goja.AssertFunction(rt.vm.Get("Object").ToObject(rt.vm).Get("freeze"))
	rt.dateCtor = rt.vm.Get("Date").ToObject(rt.vm)
	for _, d := range defs {
		obj, err := runProgram(rt.vm, d.program)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		fn, ok := goja.AssertFunction(obj.Get("check"))
		if !ok {
			return nil, fmt.Errorf("%s: no check function", d.Path)
		}
		lc := &loadedCheck{def: d, fn: fn}
		rt.checks = append(rt.checks, lc)
		rt.byName[d.Name] = lc
	}
	return rt, nil
}

// random is a xorshift generator, reset before every call.
func (rt *jsRuntime) random() float64 {
	rt.rand ^= rt.rand << 13
	rt.rand ^= rt.rand >> 7
	rt.rand ^= rt.rand << 17
	return float64(rt.rand>>11) / (1 << 53)
}

func (rt *jsRuntime) log(call goja.FunctionCall) goja.Value {
	args := make([]any, 0, len(call.Arguments))
	for _, a := range call.Arguments {
		args = append(args, a.String())
	}
	name := ""
	if rt.current != nil {
		name = rt.current.def.Name
	}
	rt.opts.Log.Debug(fmt.Sprint(args...), "check", name)
	return goja.Undefined()
}

// begin prepares the runtime for one evaluation: one ctx base per check.
func (rt *jsRuntime) begin(snap *Snapshot, hist History, now time.Time) {
	rt.snap, rt.hist, rt.now = snap, hist, now
	rt.broken = nil
	defer func() {
		if r := recover(); r != nil {
			rt.broken = fmt.Errorf("prepare evaluation: %v", r)
		}
	}()
	for _, lc := range rt.checks {
		lc.base = rt.newContextBase(lc)
	}
}

func (rt *jsRuntime) freeze(o *goja.Object) {
	if _, err := rt.freezeFn(goja.Undefined(), o); err != nil {
		panic(err)
	}
}

// set sets a property without calling setters a check may have put on
// Object.prototype.
func (rt *jsRuntime) set(o *goja.Object, name string, v any) {
	must(o.DefineDataProperty(name, rt.vm.ToValue(v), goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_TRUE))
}

func (rt *jsRuntime) newDate(t time.Time) goja.Value {
	d, err := rt.vm.New(rt.dateCtor, rt.vm.ToValue(t.UnixMilli()))
	must(err)
	return d
}

// callContext is the ctx argument of one call. It is fresh and frozen, so a
// check cannot change ctx.now or ctx itself for the next object. This is
// not isolation: module variables and globals are shared between calls.
func (rt *jsRuntime) callContext(lc *loadedCheck, resource *goja.Object) *goja.Object {
	ctx := rt.vm.NewObject()
	must(ctx.SetPrototype(lc.base))
	rt.set(ctx, "now", rt.newDate(rt.now))
	rt.set(ctx, "resource", resource)
	rt.freeze(ctx)
	return ctx
}

func (rt *jsRuntime) newContextBase(lc *loadedCheck) *goja.Object {
	vm := rt.vm
	ctx := vm.NewObject()
	rt.set(ctx, "params", wrap(vm, lc.def.Params))
	rt.set(ctx, "has", func(resource string) bool {
		gr := rt.resource(lc, resource)
		return rt.snap.Available(gr)
	})
	rt.set(ctx, "get", func(resource string, namespace goja.Value, name string) goja.Value {
		gr := rt.readable(lc, resource)
		ns := ""
		if namespace != nil && !goja.IsUndefined(namespace) && !goja.IsNull(namespace) {
			ns = namespace.String()
		}
		obj := rt.snap.get(gr, ns, name)
		if obj == nil {
			return goja.Null()
		}
		return wrap(vm, obj.Object)
	})
	rt.set(ctx, "list", func(resource string, opts goja.Value) goja.Value {
		gr := rt.readable(lc, resource)
		objs := rt.snap.list(gr, rt.listOptions(opts))
		vals := make([]any, 0, len(objs))
		for _, o := range objs {
			vals = append(vals, wrap(vm, o.Object))
		}
		return vm.NewArray(vals...)
	})
	history := vm.NewObject()
	rt.set(history, "conditions", func(obj goja.Value, since goja.Value) goja.Value {
		if !lc.def.uses("history") {
			panic(vm.NewTypeError(fmt.Sprintf("check %s uses ctx.history but does not declare uses: [\"history\"]", lc.def.Name)))
		}
		if rt.hist == nil {
			panic(vm.NewTypeError("condition history is not available (no database)"))
		}
		uid := uidOf(vm, obj)
		var sinceT time.Time
		if since != nil && !goja.IsUndefined(since) && !goja.IsNull(since) {
			t, err := toTime(since.Export())
			if err != nil {
				panic(vm.NewTypeError("history.conditions: since: " + err.Error()))
			}
			sinceT = t
		}
		var vals []any
		for _, h := range rt.hist.Conditions(uid) {
			if h.ObservedAt.Before(sinceT) {
				continue
			}
			o := vm.NewObject()
			at, _ := vm.New(vm.Get("Date"), vm.ToValue(h.ObservedAt.UnixMilli()))
			must(o.Set("observedAt", at))
			must(o.Set("change", h.Change))
			must(o.Set("type", h.Type))
			must(o.Set("status", h.Status))
			must(o.Set("reason", h.Reason))
			must(o.Set("message", h.Message))
			must(o.Set("lastTransitionTime", h.LastTransitionTime))
			vals = append(vals, o)
		}
		return vm.NewArray(vals...)
	})
	rt.freeze(history)
	rt.set(ctx, "history", history)
	rt.set(ctx, "log", rt.log)
	rt.freeze(ctx)
	return ctx
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func (rt *jsRuntime) resource(lc *loadedCheck, resource string) schema.GroupResource {
	gr, err := ParseResource(resource)
	if err != nil {
		panic(rt.vm.NewTypeError(err.Error()))
	}
	if !lc.def.canRead(gr) {
		panic(rt.vm.NewTypeError(fmt.Sprintf("check %s may not read %s: add it to reads", lc.def.Name, resource)))
	}
	return gr
}

// readable resolves a resource name and makes sure its objects are known.
// A type which is not watched must not look like a type without objects.
func (rt *jsRuntime) readable(lc *loadedCheck, resource string) schema.GroupResource {
	gr := rt.resource(lc, resource)
	if !rt.snap.Available(gr) {
		panic(rt.vm.NewTypeError(fmt.Sprintf(
			"resource %s is not available (not served by the cluster, forbidden, or not synced yet); use ctx.has() for optional types", resource)))
	}
	return gr
}

func (rt *jsRuntime) listOptions(v goja.Value) listOptions {
	var o listOptions
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return o
	}
	obj := v.ToObject(rt.vm)
	if ns := obj.Get("namespace"); ns != nil && !goja.IsUndefined(ns) && !goja.IsNull(ns) {
		o.namespace, o.hasNamespace = ns.String(), true
	}
	if l := obj.Get("labels"); l != nil && !goja.IsUndefined(l) && !goja.IsNull(l) {
		lo := l.ToObject(rt.vm)
		o.labels = map[string]string{}
		for _, k := range lo.Keys() {
			o.labels[k] = lo.Get(k).String()
		}
	}
	if owner := obj.Get("owner"); owner != nil && !goja.IsUndefined(owner) && !goja.IsNull(owner) {
		o.owner = uidOf(rt.vm, owner)
	}
	return o
}

// uidOf accepts an object (its metadata.uid) or a UID string.
func uidOf(vm *goja.Runtime, v goja.Value) string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		panic(vm.NewTypeError("expected an object or a uid"))
	}
	if s, ok := v.Export().(string); ok {
		return s
	}
	md := v.ToObject(vm).Get("metadata")
	if md == nil || goja.IsUndefined(md) {
		panic(vm.NewTypeError("expected an object with metadata.uid"))
	}
	uid := md.ToObject(vm).Get("uid")
	if uid == nil || goja.IsUndefined(uid) {
		panic(vm.NewTypeError("expected an object with metadata.uid"))
	}
	return uid.String()
}

func (rt *jsRuntime) run(t task) taskResult {
	var res taskResult
	vm := rt.vm
	obj := wrap(vm, t.item.Obj.Object)
	resource := vm.NewObject()
	rt.set(resource, "group", t.item.GVR.Group)
	rt.set(resource, "version", t.item.GVR.Version)
	rt.set(resource, "resource", t.item.GVR.Resource)
	rt.freeze(resource)
	for _, d := range t.defs {
		lc := rt.byName[d.Name]
		findings, err := rt.call(lc, obj, resource, t.item)
		res.calls++
		if err != nil {
			uid := string(t.item.Obj.GetUID())
			res.failures = append(res.failures, Failure{
				Check: d.Name, UID: uid, Object: t.item.Obj.GetNamespace() + "/" + t.item.Obj.GetName(), Err: err,
			})
			continue
		}
		res.findings = append(res.findings, findings...)
	}
	return res
}

func (rt *jsRuntime) call(lc *loadedCheck, obj goja.Value, resource *goja.Object, item Item) (findings []Finding, err error) {
	if rt.broken != nil {
		return nil, rt.broken
	}
	rt.current = lc
	rt.rand = 0x9E3779B97F4A7C15
	// A timer which fires just after the call ended must not interrupt the
	// next call: done is checked under the lock.
	var mu sync.Mutex
	done := false
	timer := time.AfterFunc(rt.opts.Timeout, func() {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			rt.vm.Interrupt(errTimeout)
		}
	})
	defer func() {
		mu.Lock()
		done = true
		mu.Unlock()
		timer.Stop()
		rt.vm.ClearInterrupt()
		rt.current = nil
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	v, err := lc.fn(goja.Undefined(), obj, rt.callContext(lc, resource))
	if err != nil {
		var ie *goja.InterruptedError
		if errors.As(err, &ie) {
			return nil, fmt.Errorf("check took longer than %s", rt.opts.Timeout)
		}
		return nil, err
	}
	return toFindings(lc.def, item, v)
}

func toFindings(d *Definition, item Item, v goja.Value) ([]Finding, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}
	exported, ok := v.Export().([]any)
	if !ok {
		return nil, fmt.Errorf("check returned %s, want an array of findings", v.ExportType())
	}
	if len(exported) > maxFindingsPerCall {
		return nil, fmt.Errorf("check returned %d findings, the limit is %d", len(exported), maxFindingsPerCall)
	}
	keys := map[string]struct{}{}
	findings := make([]Finding, 0, len(exported))
	for i, e := range exported {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("finding %d is %T, want an object", i, e)
		}
		f := newFinding(d, item)
		var err error
		if f.Message, err = stringField(m, "message"); err != nil {
			return nil, fmt.Errorf("finding %d: %w", i, err)
		}
		if f.Message == "" {
			return nil, fmt.Errorf("finding %d: message is empty", i)
		}
		if f.Key, err = stringField(m, "key"); err != nil {
			return nil, fmt.Errorf("finding %d: %w", i, err)
		}
		if f.Condition, err = stringField(m, "condition"); err != nil {
			return nil, fmt.Errorf("finding %d: %w", i, err)
		}
		if f.Container, err = stringField(m, "container"); err != nil {
			return nil, fmt.Errorf("finding %d: %w", i, err)
		}
		if s, ok := m["since"]; ok && s != nil {
			if f.Since, err = toTime(s); err != nil {
				return nil, fmt.Errorf("finding %d: since: %w", i, err)
			}
		}
		if dm, ok := m["details"]; ok && dm != nil {
			mm, ok := dm.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("finding %d: details must be an object", i)
			}
			f.Details = make(map[string]string, len(mm))
			for k, v := range mm {
				if v != nil {
					f.Details[k] = fmt.Sprint(v)
				}
			}
		}
		if _, dup := keys[f.Key]; dup {
			return nil, fmt.Errorf("two findings with key %q: set a distinct key per finding", f.Key)
		}
		keys[f.Key] = struct{}{}
		findings = append(findings, f)
	}
	return findings, nil
}

func newFinding(d *Definition, item Item) Finding {
	return Finding{
		Check: d.Name, Category: d.Category, Severity: d.Severity,
		GVR: item.GVR, Kind: item.Obj.GetKind(), Namespace: item.Obj.GetNamespace(), Name: item.Obj.GetName(),
		UID: string(item.Obj.GetUID()),
	}
}

// errorFindings returns one check-error finding per failing check, not one
// per object: a check which fails for many objects (e.g. a type in reads is
// forbidden) is one problem. The finding belongs to no object, so it stays
// the same finding while the failing objects change.
func errorFindings(defs []*Definition, failures []Failure) []Finding {
	byCheck := map[string][]Failure{}
	for _, f := range failures {
		byCheck[f.Check] = append(byCheck[f.Check], f)
	}
	var findings []Finding
	for _, d := range defs {
		fs := byCheck[d.Name]
		if len(fs) == 0 {
			continue
		}
		msg := fmt.Sprintf("check %s failed for %s: %v", d.Name, fs[0].Object, fs[0].Err)
		if len(fs) > 1 {
			msg = fmt.Sprintf("check %s failed for %d objects, e.g. %s: %v", d.Name, len(fs), fs[0].Object, fs[0].Err)
		}
		findings = append(findings, Finding{
			Check: ErrorCheck, Category: CategoryHealth, Severity: "warning",
			Key: d.Name, Message: msg,
		})
	}
	return findings
}

func stringField(m map[string]any, key string) (string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %T", key, v)
	}
	return s, nil
}

// toTime accepts a Date, an RFC3339 string or Unix milliseconds.
func toTime(v any) (time.Time, error) {
	switch t := v.(type) {
	case time.Time:
		return t, nil
	case string:
		if t == "" {
			return time.Time{}, nil
		}
		return time.Parse(time.RFC3339, t)
	case int64:
		return time.UnixMilli(t), nil
	case float64:
		return time.UnixMilli(int64(t)), nil
	}
	return time.Time{}, fmt.Errorf("want a Date, an RFC3339 string or Unix milliseconds, got %T", v)
}
