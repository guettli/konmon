package checks

import (
	"sort"

	"github.com/dop251/goja"
)

// Objects come straight from the informer cache and are shared between
// runtimes. goja's ToValue would hand the Go maps to JavaScript by
// reference, so a check calling conditions.sort() would change the cache.
// These wrappers expose the objects read-only, without copying them. Writes
// fail (a TypeError in strict mode, which esbuild output is).

func wrap(vm *goja.Runtime, v any) goja.Value {
	switch t := v.(type) {
	case map[string]any:
		return vm.NewDynamicObject(&roObject{vm: vm, m: t})
	case []any:
		return vm.NewDynamicArray(&roArray{vm: vm, a: t})
	default:
		return vm.ToValue(v)
	}
}

type roObject struct {
	vm *goja.Runtime
	m  map[string]any
}

func (o *roObject) Get(key string) goja.Value {
	v, ok := o.m[key]
	if !ok {
		return nil
	}
	return wrap(o.vm, v)
}

func (o *roObject) Set(string, goja.Value) bool { return false }

func (o *roObject) Has(key string) bool {
	_, ok := o.m[key]
	return ok
}

func (o *roObject) Delete(string) bool { return false }

func (o *roObject) Keys() []string {
	keys := make([]string, 0, len(o.m))
	for k := range o.m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic Object.keys and JSON.stringify
	return keys
}

type roArray struct {
	vm *goja.Runtime
	a  []any
}

func (a *roArray) Len() int { return len(a.a) }

func (a *roArray) Get(idx int) goja.Value {
	if idx < 0 || idx >= len(a.a) {
		return nil
	}
	return wrap(a.vm, a.a[idx])
}

func (a *roArray) Set(int, goja.Value) bool { return false }

func (a *roArray) SetLen(int) bool { return false }

// unwrap replaces the read-only wrappers in an exported value with the
// objects they wrap: a check may return parts of an object, e.g.
// details: {labels: pod.metadata.labels}.
func unwrap(v any) any {
	switch t := v.(type) {
	case *roObject:
		return t.m
	case *roArray:
		return t.a
	case map[string]any:
		for k, e := range t {
			t[k] = unwrap(e)
		}
	case []any:
		for i, e := range t {
			t[i] = unwrap(e)
		}
	}
	return v
}
