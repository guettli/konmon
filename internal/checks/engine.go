// Package checks loads checks written in TypeScript or JavaScript and runs
// them on the objects of a cluster. The core checks of konmon use the same
// mechanism as user checks: they are files embedded in the binary.
package checks

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Options configure Load.
type Options struct {
	// Disabled are glob patterns of check names to drop.
	Disabled []string
	// Only are glob patterns of the check names to keep; empty keeps all.
	// A pattern which matches no check is an error (a typo would otherwise
	// run no check at all).
	Only []string
	// Categories keeps only the checks of these categories; empty keeps
	// all.
	Categories []string
	// Params overrides parameters: check name → param → value (JSON or a
	// plain string).
	Params map[string]map[string]string
	// Timeout is the limit for one call of a check function.
	Timeout time.Duration
	// Workers is the number of JavaScript runtimes; 0 means GOMAXPROCS.
	Workers int
	// Exclude removes resource types from the match of all checks (e.g.
	// --exclude-resource). Checks can still read them if listed in reads.
	Exclude func(schema.GroupResource) bool
	Log     *slog.Logger
}

// Engine runs checks. Evaluate must not be called concurrently.
type Engine struct {
	defs     []*Definition
	defined  map[string]bool // all loaded check names, also disabled ones
	opts     Options
	mu       sync.Mutex
	runtimes []*jsRuntime
}

// Load compiles core and user checks. A user check with the name of a core
// check replaces it.
func Load(core, user []Source, opts Options) (*Engine, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = time.Second
	}
	if opts.Workers <= 0 {
		opts.Workers = runtime.GOMAXPROCS(0)
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	byName := map[string]*Definition{}
	var order []string
	add := func(srcs []Source, isCore bool) error {
		seen := map[string]string{}
		for _, src := range srcs {
			prg, err := compile(src)
			if err != nil {
				return err
			}
			d, err := define(src, prg, isCore)
			if err != nil {
				return err
			}
			if other, dup := seen[d.Name]; dup {
				return fmt.Errorf("check %q is defined in %s and %s", d.Name, other, src.Path)
			}
			seen[d.Name] = src.Path
			if old, ok := byName[d.Name]; ok {
				opts.Log.Info("check overrides core check", "check", d.Name, "path", src.Path, "core", old.Path)
			} else {
				order = append(order, d.Name)
			}
			byName[d.Name] = d
		}
		return nil
	}
	if err := add(core, true); err != nil {
		return nil, err
	}
	if err := add(user, false); err != nil {
		return nil, err
	}
	for name := range opts.Params {
		if _, ok := byName[name]; !ok {
			return nil, fmt.Errorf("params for unknown check %q", name)
		}
	}
	for _, p := range opts.Only {
		if !slices.ContainsFunc(order, func(name string) bool { return globMatch(name, []string{p}) }) {
			return nil, fmt.Errorf("no check matches %q", p)
		}
	}
	e := &Engine{opts: opts, defined: map[string]bool{}}
	for _, name := range order {
		d := byName[name]
		e.defined[name] = true
		if globMatch(name, opts.Disabled) || (len(opts.Only) > 0 && !globMatch(name, opts.Only)) ||
			(len(opts.Categories) > 0 && !slices.Contains(opts.Categories, d.Category)) {
			continue
		}
		if err := d.setParams(opts.Params[name]); err != nil {
			return nil, err
		}
		e.defs = append(e.defs, d)
	}
	if len(e.defs) == 0 && (len(opts.Only) > 0 || len(opts.Categories) > 0) {
		return nil, fmt.Errorf("no check left to run: selected %v, categories %v, disabled %v", opts.Only, opts.Categories, opts.Disabled)
	}
	sort.Slice(e.defs, func(i, j int) bool { return e.defs[i].Name < e.defs[j].Name })
	return e, nil
}

// Checks returns the enabled checks, sorted by name.
func (e *Engine) Checks() []*Definition { return e.defs }

// Watch reports whether objects of a type are needed: some check runs on
// them or reads them.
func (e *Engine) Watch(gr schema.GroupResource) bool {
	if Denied(gr) {
		return false
	}
	for _, d := range e.defs {
		if e.matches(d, gr) || d.reads.contains(gr) {
			return true
		}
	}
	return false
}

func (e *Engine) matches(d *Definition, gr schema.GroupResource) bool {
	return d.Matches(gr) && (e.opts.Exclude == nil || !e.opts.Exclude(gr))
}

// Store reports whether objects of a type are checked, and so worth keeping
// in the database. Types which are only read by checks stay in memory.
func (e *Engine) Store(gr schema.GroupResource) bool {
	for _, d := range e.defs {
		if e.matches(d, gr) {
			return true
		}
	}
	return false
}

// UsesHistory reports whether a check needs condition history.
func (e *Engine) UsesHistory() bool {
	for _, d := range e.defs {
		if d.uses("history") {
			return true
		}
	}
	return false
}

// Failure is a check which threw (or returned invalid findings) for one
// object. The previous findings of that check for that object should stay
// as they are: the check did not say they are resolved.
type Failure struct {
	Check string
	UID   string
	// Object is namespace/name, for messages.
	Object string
	Err    error
	// Uncovered: the check read a type in a namespace whose objects could
	// not be listed (RBAC, see Snapshot.SetPartial). That is no problem of
	// the cluster: no ErrorCheck finding.
	Uncovered bool
}

// Result of one evaluation.
type Result struct {
	// Findings of all checks, including one ErrorCheck finding per Failure.
	Findings []Finding
	Failures []Failure
	Objects  int
	Calls    int
	Duration time.Duration
}

type task struct {
	idx  int
	item Item
	defs []*Definition
}

type taskResult struct {
	findings []Finding
	failures []Failure
	calls    int
}

// Evaluate runs all checks on all objects of the snapshot. hist may be nil.
// It fails only if ctx ends; errors of single checks are reported as
// Failures.
func (e *Engine) Evaluate(ctx context.Context, snap *Snapshot, hist History, now time.Time) (*Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	start := time.Now()
	for len(e.runtimes) < e.opts.Workers {
		rt, err := newRuntime(e.defs, e.opts)
		if err != nil {
			return nil, err
		}
		e.runtimes = append(e.runtimes, rt)
	}

	byType := map[schema.GroupResource][]*Definition{}
	var tasks []task
	for i, it := range snap.Items() {
		gr := it.GVR.GroupResource()
		defs, ok := byType[gr]
		if !ok {
			for _, d := range e.defs {
				if e.matches(d, gr) {
					defs = append(defs, d)
				}
			}
			byType[gr] = defs
		}
		if len(defs) > 0 {
			tasks = append(tasks, task{idx: i, item: it, defs: defs})
		}
	}

	results := make([]taskResult, len(snap.Items()))
	ch := make(chan task)
	var wg sync.WaitGroup
	for _, rt := range e.runtimes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt.begin(snap, hist, now)
			for t := range ch {
				results[t.idx] = rt.run(t)
			}
		}()
	}
	var err error
feed:
	for _, t := range tasks {
		// select picks a random ready case: without this check a cancelled
		// evaluation could still run (and return) everything.
		if err = ctx.Err(); err != nil {
			break
		}
		select {
		case ch <- t:
		case <-ctx.Done():
			err = ctx.Err()
			break feed
		}
	}
	close(ch)
	wg.Wait()
	if err != nil {
		return nil, fmt.Errorf("evaluation aborted: %w", err)
	}

	res := &Result{Objects: len(tasks), Duration: time.Since(start)}
	for i := range results {
		res.Findings = append(res.Findings, results[i].findings...)
		res.Failures = append(res.Failures, results[i].failures...)
		res.Calls += results[i].calls
	}
	res.Findings = append(res.Findings, errorFindings(e.defs, res.Failures)...)
	return res, nil
}
