package checks

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"time"

	"github.com/dop251/goja"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Definition is a loaded check.
type Definition struct {
	Name     string
	Category string
	Severity string
	// Path is the file the check was loaded from; Core is true for the
	// checks embedded in konmon.
	Path string
	Core bool

	MatchResources []string
	ExcludeTypes   []string
	ReadsResources []string
	Uses           []string
	// For overrides konmon's --for for findings of this check (0: default).
	For time.Duration
	// Params are the effective parameters: defaults merged with overrides.
	Params map[string]any

	match, exclude, reads resourceSet
	program               *goja.Program
}

// Matches reports whether the check runs on objects of this type.
func (d *Definition) Matches(gr schema.GroupResource) bool {
	return d.match.contains(gr) && !d.exclude.contains(gr) && !Denied(gr)
}

// canRead reports whether the check may read objects of this type with
// ctx.get and ctx.list.
func (d *Definition) canRead(gr schema.GroupResource) bool {
	return d.Matches(gr) || (d.reads.contains(gr) && !Denied(gr))
}

func (d *Definition) uses(feature string) bool {
	for _, u := range d.Uses {
		if u == feature {
			return true
		}
	}
	return false
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// rawDefinition is the object passed to defineCheck, without the function.
type rawDefinition struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Severity string `json:"severity"`
	Match    struct {
		Resources []string `json:"resources"`
		Exclude   []string `json:"exclude"`
	} `json:"match"`
	Reads  []string       `json:"reads"`
	Uses   []string       `json:"uses"`
	For    float64        `json:"forSeconds"`
	Params map[string]any `json:"params"`
}

// define runs a compiled check file once and reads its definition.
func define(src Source, prg *goja.Program, core bool) (*Definition, error) {
	vm := goja.New()
	obj, err := runProgram(vm, prg)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src.Path, err)
	}
	if _, ok := goja.AssertFunction(obj.Get("check")); !ok {
		return nil, fmt.Errorf("%s: the check has no check(obj, ctx) function", src.Path)
	}
	// JSON.stringify drops the function and gives us plain data.
	stringify, _ := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("stringify"))
	data, err := stringify(goja.Undefined(), obj)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src.Path, err)
	}
	var raw rawDefinition
	if err := json.Unmarshal([]byte(data.String()), &raw); err != nil {
		return nil, fmt.Errorf("%s: invalid check definition: %w", src.Path, err)
	}
	d := &Definition{
		Name: raw.Name, Category: raw.Category, Severity: raw.Severity, Path: src.Path, Core: core,
		MatchResources: raw.Match.Resources, ExcludeTypes: raw.Match.Exclude, ReadsResources: raw.Reads,
		Uses: raw.Uses, Params: raw.Params, program: prg, For: time.Duration(raw.For * float64(time.Second)),
	}
	if d.Params == nil {
		d.Params = map[string]any{}
	}
	if !nameRE.MatchString(d.Name) || d.Name == ErrorCheck {
		return nil, fmt.Errorf("%s: invalid check name %q: use lower case letters, digits and dashes", src.Path, d.Name)
	}
	if d.Category == "" {
		d.Category = CategoryHealth
	}
	if d.Category != CategoryHealth && d.Category != CategoryHygiene {
		return nil, fmt.Errorf("%s: category must be %q or %q", src.Path, CategoryHealth, CategoryHygiene)
	}
	if d.Severity == "" {
		d.Severity = "warning"
	}
	if !validSeverity(d.Severity) {
		return nil, fmt.Errorf("%s: severity must be critical, warning or info", src.Path)
	}
	if len(d.MatchResources) == 0 {
		return nil, fmt.Errorf("%s: match.resources is empty", src.Path)
	}
	if d.match, err = parseResourceSet(d.MatchResources, true); err != nil {
		return nil, fmt.Errorf("%s: match.resources: %w", src.Path, err)
	}
	if d.exclude, err = parseResourceSet(d.ExcludeTypes, false); err != nil {
		return nil, fmt.Errorf("%s: match.exclude: %w", src.Path, err)
	}
	if d.reads, err = parseResourceSet(d.ReadsResources, false); err != nil {
		return nil, fmt.Errorf("%s: reads: %w", src.Path, err)
	}
	if d.For < 0 {
		return nil, fmt.Errorf("%s: forSeconds must not be negative", src.Path)
	}
	for _, u := range d.Uses {
		if u != "history" {
			return nil, fmt.Errorf("%s: unknown entry %q in uses (known: history)", src.Path, u)
		}
	}
	return d, nil
}

// runProgram runs a bundled check file and returns its default export.
func runProgram(vm *goja.Runtime, prg *goja.Program) (*goja.Object, error) {
	if _, err := vm.RunProgram(prg); err != nil {
		return nil, err
	}
	mod := vm.Get(globalName)
	if mod == nil || goja.IsUndefined(mod) {
		return nil, fmt.Errorf("no module")
	}
	def := mod.ToObject(vm).Get("default")
	if def == nil || goja.IsUndefined(def) || goja.IsNull(def) {
		return nil, fmt.Errorf("no default export: use export default defineCheck({...})")
	}
	// esbuild declares the global with var, which cannot be deleted.
	if err := vm.Set(globalName, goja.Undefined()); err != nil {
		return nil, err
	}
	return def.ToObject(vm), nil
}

// setParams applies overrides. Values are JSON (numbers, booleans, arrays)
// or plain strings. An override must have the type of the default.
func (d *Definition) setParams(overrides map[string]string) error {
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		def, ok := d.Params[k]
		if !ok {
			return fmt.Errorf("check %s has no param %q", d.Name, k)
		}
		var v any
		if err := json.Unmarshal([]byte(overrides[k]), &v); err != nil {
			v = overrides[k]
		}
		if def != nil && fmt.Sprintf("%T", def) != fmt.Sprintf("%T", v) {
			if _, isString := def.(string); isString {
				v = overrides[k] // e.g. "30" for a string param
			} else {
				return fmt.Errorf("param %s.%s: want a value like %v, got %q", d.Name, k, def, overrides[k])
			}
		}
		d.Params[k] = v
	}
	return nil
}

func validSeverity(s string) bool {
	return s == "critical" || s == "warning" || s == "info"
}

// globMatch reports whether name matches any of the patterns (path.Match).
func globMatch(name string, patterns []string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}
