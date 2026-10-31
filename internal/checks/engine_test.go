package checks

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestCoreChecks(t *testing.T) {
	core := CoreFiles()
	if len(core.Sources) < 3 {
		t.Fatalf("want at least 3 core checks, got %d", len(core.Sources))
	}
	e, err := Load(core.Sources, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tested := map[string]bool{}
	for _, p := range core.TestPaths() {
		tf := core.Tests[p]
		tested[tf.Check] = true
		for _, r := range e.RunTests(tf) {
			if r.Err != nil {
				t.Errorf("%s: %s: %v", p, r.Case, r.Err)
			}
		}
	}
	for _, d := range e.Checks() {
		if !tested[d.Name] {
			t.Errorf("core check %s has no test file", d.Name)
		}
	}
}

var (
	podsGVR     = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	servicesGVR = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	secretsGVR  = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
)

func pod(name string) Item {
	obj := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": "default", "uid": "uid-" + name},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Zeta", "status": "False"},
			map[string]any{"type": "Alpha", "status": "False"},
		}},
	}
	return Item{GVR: podsGVR, Obj: &unstructured.Unstructured{Object: obj}}
}

func load(t *testing.T, code string) *Engine {
	t.Helper()
	e, err := Load(nil, []Source{{Path: "test.ts", Code: code}}, Options{Timeout: 200 * time.Millisecond, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func evaluate(t *testing.T, e *Engine, items []Item, available ...schema.GroupVersionResource) *Result {
	t.Helper()
	if len(available) == 0 {
		available = []schema.GroupVersionResource{podsGVR}
	}
	res, err := e.Evaluate(context.Background(), NewSnapshot(items, available), nil, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestObjectsAreReadOnly(t *testing.T) {
	e := load(t, `
import { defineCheck } from "konmon";
export default defineCheck({
  name: "mutate",
  match: { resources: ["pods"] },
  check(obj) {
    const errors: string[] = [];
    const attempts: Array<() => void> = [
      () => { (obj.status.conditions as any[]).sort((a, b) => a.type < b.type ? -1 : 1); },
      () => { (obj.status as any).mutated = true; },
      () => { (obj.status.conditions as any[]).push({}); },
      () => { delete (obj.metadata as any).name; },
    ];
    for (const a of attempts) {
      try { a(); errors.push("no error"); } catch (e) { errors.push(String(e).split(":")[0]); }
    }
    return [{ message: errors.join(",") }];
  },
});`)
	items := []Item{pod("a")}
	res := evaluate(t, e, items)
	if len(res.Failures) != 0 {
		t.Fatalf("failures: %v", res.Failures)
	}
	if got := res.Findings[0].Message; strings.Contains(got, "no error") {
		t.Errorf("a write did not fail: %s", got)
	}
	conds := items[0].Obj.Object["status"].(map[string]any)["conditions"].([]any)
	if conds[0].(map[string]any)["type"] != "Zeta" || len(conds) != 2 {
		t.Errorf("object was changed: %v", conds)
	}
	if _, ok := items[0].Obj.Object["status"].(map[string]any)["mutated"]; ok {
		t.Error("object was changed")
	}
}

func TestFindingsMayHoldPartsOfObjects(t *testing.T) {
	e := load(t, `
import { defineCheck } from "konmon";
export default defineCheck({
  name: "parts",
  match: { resources: ["pods"] },
  check(obj) {
    return [
      { key: "a", message: "m", details: obj.metadata as any },
      { key: "b", message: "m", details: { first: obj.status.conditions[0] as any } },
    ];
  },
});`)
	res := evaluate(t, e, []Item{pod("a")})
	if len(res.Failures) != 0 {
		t.Fatalf("failures: %v", res.Failures)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("findings: %+v", res.Findings)
	}
	if got := res.Findings[0].Details["name"]; got != "a" {
		t.Errorf("details from an object: %v", res.Findings[0].Details)
	}
	if got := res.Findings[1].Details["first"]; got != "map[status:False type:Zeta]" {
		t.Errorf("a nested object: %q", got)
	}
}

func TestGetListAndReads(t *testing.T) {
	e := load(t, `
import { defineCheck } from "konmon";
export default defineCheck({
  name: "svc",
  match: { resources: ["pods"] },
  reads: ["services"],
  check(pod, ctx) {
    const svc = ctx.get("services", pod.metadata.namespace ?? null, "web");
    const all = ctx.list("services", { namespace: "default", labels: { app: "web" } });
    const owned = ctx.list("pods", { owner: "owner-1" });
    let secret = "";
    try { ctx.list("secrets"); } catch (e) { secret = String(e); }
    return [{ message: [svc ? svc.metadata.name : "none", all.length, owned.length, ctx.resource.resource, secret.includes("may not read")].join(" ") }];
  },
});`)
	svc := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "web", "namespace": "default", "uid": "s1", "labels": map[string]any{"app": "web"}},
	}}
	owned := pod("b")
	owned.Obj.SetOwnerReferences(nil)
	owned.Obj.Object["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"uid": "owner-1", "name": "x", "kind": "ReplicaSet", "apiVersion": "apps/v1"}}
	res := evaluate(t, e, []Item{pod("a"), owned, {GVR: servicesGVR, Obj: svc}}, podsGVR, servicesGVR)
	if len(res.Failures) != 0 {
		t.Fatalf("failures: %v", res.Failures)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("want 2 findings, got %d", len(res.Findings))
	}
	if got, want := res.Findings[0].Message, "web 1 1 pods true"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadingUnavailableTypeFails(t *testing.T) {
	e := load(t, `
import { defineCheck } from "konmon";
export default defineCheck({
  name: "svc",
  match: { resources: ["pods"] },
  reads: ["services"],
  check(pod, ctx) {
    if (!ctx.has("services")) return [{ message: "no services" }];
    return [];
  },
});`)
	res := evaluate(t, e, []Item{pod("a")})
	if len(res.Findings) != 1 || res.Findings[0].Message != "no services" {
		t.Fatalf("findings: %+v", res.Findings)
	}

	e = load(t, `
import { defineCheck } from "konmon";
export default defineCheck({
  name: "svc",
  match: { resources: ["pods"] },
  reads: ["services"],
  check(pod, ctx) { ctx.list("services"); return []; },
});`)
	res = evaluate(t, e, []Item{pod("a")})
	if len(res.Failures) != 1 || !strings.Contains(res.Failures[0].Err.Error(), "not available") {
		t.Fatalf("want a failure for an unavailable type, got %+v", res.Failures)
	}
	if res.Findings[0].Check != ErrorCheck || res.Findings[0].Key != "svc" {
		t.Errorf("want an error finding, got %+v", res.Findings[0])
	}
}

func TestTimeoutAndErrors(t *testing.T) {
	e := load(t, `
import { defineCheck } from "konmon";
export default defineCheck({
  name: "bad",
  match: { resources: ["pods"] },
  check(pod) {
    if (pod.metadata.name === "loop") { for (;;) {} }
    if (pod.metadata.name === "throw") throw new Error("boom");
    if (pod.metadata.name === "dup") return [{ message: "a" }, { message: "b" }];
    if (pod.metadata.name === "nomsg") return [{ key: "x" }];
    return [{ message: "ok", since: "2025-12-31T23:00:00Z" }];
  },
});`)
	res := evaluate(t, e, []Item{pod("dup"), pod("fine"), pod("loop"), pod("nomsg"), pod("throw")})
	want := map[string]string{
		"uid-dup": "two findings", "uid-loop": "longer than", "uid-nomsg": "message", "uid-throw": "boom",
	}
	if len(res.Failures) != len(want) {
		t.Fatalf("want %d failures, got %+v", len(want), res.Failures)
	}
	for _, f := range res.Failures {
		if !strings.Contains(f.Err.Error(), want[f.UID]) {
			t.Errorf("%s: %v does not contain %q", f.UID, f.Err, want[f.UID])
		}
	}
	var ok *Finding
	for i := range res.Findings {
		if res.Findings[i].Check == "bad" {
			ok = &res.Findings[i]
		}
	}
	if ok == nil || ok.UID != "uid-fine" || !ok.Since.Equal(time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("finding of the good object: %+v", ok)
	}
}

func TestDeterministicTimeAndRandom(t *testing.T) {
	e := load(t, `
import { defineCheck } from "konmon";
export default defineCheck({
  name: "det",
  match: { resources: ["pods"] },
  check(pod, ctx) {
    return [{ message: [new Date().toISOString(), Date.now() === ctx.now.getTime(), Math.random()].join(" ") }];
  },
});`)
	res := evaluate(t, e, []Item{pod("a"), pod("b")})
	if res.Findings[0].Message != res.Findings[1].Message {
		t.Errorf("not deterministic: %q vs %q", res.Findings[0].Message, res.Findings[1].Message)
	}
	if !strings.HasPrefix(res.Findings[0].Message, "2026-01-01T00:00:00.000Z true ") {
		t.Errorf("got %q", res.Findings[0].Message)
	}
}

func TestLoadErrorsAndOverrides(t *testing.T) {
	core := CoreFiles()
	cases := map[string]string{
		"syntax":  `export default {`,
		"import":  `import fs from "fs"; export default {};`,
		"no fn":   `export default { name: "x", match: { resources: ["pods"] } };`,
		"name":    `export default { name: "Bad Name", match: { resources: ["pods"] }, check() {} };`,
		"match":   `export default { name: "x", match: { resources: [] }, check() {} };`,
		"reads *": `export default { name: "x", match: { resources: ["pods"] }, reads: ["*"], check() {} };`,
	}
	for name, code := range cases {
		if _, err := Load(nil, []Source{{Path: "x.ts", Code: code}}, Options{}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}

	override := `export default { name: "conditions", match: { resources: ["pods"] }, check() { return [{ message: "mine" }]; } };`
	e, err := Load(core.Sources, []Source{{Path: "mine.js", Code: override}}, Options{Disabled: []string{"pod-*"}})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(e.Checks()))
	for _, d := range e.Checks() {
		names = append(names, d.Name)
		if d.Name == "conditions" && d.Core {
			t.Error("conditions was not overridden")
		}
	}
	joined := "," + strings.Join(names, ",") + ","
	if !strings.Contains(joined, ",conditions,") || !strings.Contains(joined, ",deletion-timestamp,") || strings.Contains(joined, ",pod-") {
		t.Errorf("checks: %v", names)
	}

	if _, err := Load(core.Sources, nil, Options{Params: map[string]map[string]string{"conditions": {"nope": "1"}}}); err == nil {
		t.Error("want an error for an unknown param")
	}
	if _, err := Load(core.Sources, nil, Options{Params: map[string]map[string]string{"conditions": {"podStartGraceSeconds": "abc"}}}); err == nil {
		t.Error("want an error for a param of the wrong type")
	}
	e, err = Load(core.Sources, nil, Options{Params: map[string]map[string]string{"conditions": {"podStartGraceSeconds": "60"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range e.Checks() {
		if d.Name == "conditions" && d.Params["podStartGraceSeconds"] != float64(60) {
			t.Errorf("param not set: %v", d.Params)
		}
	}
}

func TestLoadOnly(t *testing.T) {
	core := CoreFiles()
	e, err := Load(core.Sources, nil, Options{Only: []string{"pv-*", "conditions"}, Disabled: []string{"pv-released"}})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(e.Checks()))
	for _, d := range e.Checks() {
		names = append(names, d.Name)
	}
	if got := strings.Join(names, ","); got != "conditions,pv-failed" {
		t.Errorf("checks: %s", got)
	}
	e, err = Load(core.Sources, nil, Options{Only: []string{"pv-*"}, Categories: []string{CategoryHygiene}})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Checks()) != 1 || e.Checks()[0].Name != "pv-released" {
		t.Errorf("want only pv-released, got %d checks", len(e.Checks()))
	}
	if _, err := Load(core.Sources, nil, Options{Only: []string{"pv-released"}, Categories: []string{CategoryHealth}}); err == nil {
		t.Error("want an error when no check is left")
	}
	if _, err := Load(core.Sources, nil, Options{Only: []string{"conditoins"}}); err == nil || !strings.Contains(err.Error(), "conditoins") {
		t.Errorf("want an error for a pattern which matches no check, got %v", err)
	}
}

func TestWatchAndStore(t *testing.T) {
	core := CoreFiles()
	e, err := Load(core.Sources, []Source{{Path: "x.ts", Code: `export default { name: "x", match: { resources: ["ingresses.networking.k8s.io"] }, reads: ["endpointslices.discovery.k8s.io"], check() {} };`}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	gr := func(s string) schema.GroupResource {
		g, err := ParseResource(s)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	for _, tc := range []struct {
		resource      string
		watch, stored bool
	}{
		{"pods", true, true},
		{"secrets", false, false},
		{"configmaps", true, false},                     // read by configmap-key-missing only
		{"endpointslices.discovery.k8s.io", true, true}, // conditions matches "*"
		{"cronjobs.batch", true, true},
		{"events", false, false},
	} {
		if got := e.Watch(gr(tc.resource)); got != tc.watch {
			t.Errorf("Watch(%s) = %v", tc.resource, got)
		}
		if got := e.Store(gr(tc.resource)); got != tc.stored {
			t.Errorf("Store(%s) = %v", tc.resource, got)
		}
	}
	e, err = Load(nil, []Source{{Path: "x.ts", Code: `export default { name: "x", match: { resources: ["pods"] }, reads: ["services"], check() {} };`}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !e.Watch(gr("services")) || e.Store(gr("services")) {
		t.Error("read-only types are watched but not stored")
	}
	if e.Watch(secretsGVR.GroupResource()) {
		t.Error("secrets must never be watched")
	}
}

func TestContextIsFrozenAndFreshPerCall(t *testing.T) {
	e, err := Load(nil, []Source{{Path: "test.ts", Code: `
import { defineCheck } from "konmon";
export default defineCheck({
  name: "state",
  match: { resources: ["pods"] },
  check(pod, ctx) {
    const before = ctx.now.toISOString();
    let frozen = false;
    try { (ctx as any).leak = 1; } catch (e) { frozen = true; }
    ctx.now.setFullYear(2000);
    return [{ message: before + " " + frozen }];
  },
});`}}, Options{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	res := evaluate(t, e, []Item{pod("a"), pod("b"), pod("c")})
	for _, f := range res.Findings {
		if f.Message != "2026-01-01T00:00:00.000Z true" {
			t.Errorf("%s: %q", f.Name, f.Message)
		}
	}
}

func TestSameErrorForManyObjectsIsOneFinding(t *testing.T) {
	e := load(t, `
import { defineCheck } from "konmon";
export default defineCheck({
  name: "svc",
  match: { resources: ["pods"] },
  reads: ["services"],
  check(pod, ctx) { ctx.list("services"); return []; },
});`)
	res := evaluate(t, e, []Item{pod("a"), pod("b"), pod("c")})
	if len(res.Failures) != 3 {
		t.Errorf("want 3 failures (findings of all objects stay open), got %d", len(res.Failures))
	}
	if len(res.Findings) != 1 || res.Findings[0].UID != "" ||
		!strings.Contains(res.Findings[0].Message, "failed for 3 objects, e.g. default/a") {
		t.Errorf("want one check-error finding independent of the objects, got %+v", res.Findings)
	}
}

func TestCoreTestsOfReplacedChecksAreSkipped(t *testing.T) {
	core := CoreFiles()
	override := `export default { name: "conditions", match: { resources: ["pods"] }, check() { return []; } };`
	e, err := Load(core.Sources, []Source{{Path: "mine.js", Code: override}}, Options{Disabled: []string{"deletion-timestamp"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range core.TestPaths() {
		tf := core.Tests[p]
		want := tf.Check != "conditions" && tf.Check != "deletion-timestamp"
		if got := e.Tested(tf, true); got != want {
			t.Errorf("Tested(%s) = %v, want %v", tf.Check, got, want)
		}
	}
}

func TestUserTestsOfDisabledChecksAreSkipped(t *testing.T) {
	mine := `export default { name: "mine", match: { resources: ["pods"] }, check() { return []; } };`
	e, err := Load(nil, []Source{{Path: "mine.js", Code: mine}}, Options{Disabled: []string{"mine"}})
	if err != nil {
		t.Fatal(err)
	}
	if e.Tested(&TestFile{Check: "mine"}, false) {
		t.Error("the test of a disabled user check runs")
	}
	if !e.Tested(&TestFile{Check: "typo"}, false) {
		t.Error("the test of a check which does not exist must run and fail")
	}
}

func TestCheckReplacingGlobalsCannotCrash(t *testing.T) {
	e, err := Load(nil, []Source{
		{Path: "evil.js", Code: `
Object.freeze = () => { throw new Error("no freeze"); };
globalThis.Date = 1;
Object.defineProperty(Object.prototype, "now", { set() { throw new Error("setter"); } });
export default { name: "evil", match: { resources: ["pods"] }, check() { return [{ message: "still here" }]; } };`},
		{Path: "good.js", Code: `export default { name: "good", match: { resources: ["pods"] }, check(o, ctx) { return [{ message: String(ctx.now.getTime()) }]; } };`},
	}, Options{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	res := evaluate(t, e, []Item{pod("a"), pod("b")})
	if len(res.Failures) != 0 {
		t.Fatalf("failures: %+v", res.Failures)
	}
	for _, f := range res.Findings {
		if f.Check == "good" && f.Message != "1767225600000" {
			t.Errorf("good check: %q", f.Message)
		}
	}
	if len(res.Findings) != 4 {
		t.Errorf("want 4 findings, got %+v", res.Findings)
	}
}
