package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/guettli/konmon/internal/checks"
	"github.com/guettli/konmon/internal/store"
)

func TestAssess(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	old := now.Add(-72 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-5 * time.Minute).Format(time.RFC3339)
	// obj has condition typ=status since since, and Ready (if not "").
	obj := func(uid, typ, status, since, ready string) objectConditions {
		o := objectConditions{uid: uid, conditions: map[string]store.Condition{
			typ: {Type: typ, Status: status, LastTransitionTime: since},
		}}
		if ready != "" {
			o.conditions["Ready"] = store.Condition{Type: "Ready", Status: ready}
		}
		return o
	}
	row := func(at time.Duration, change, typ, status string) store.HistoryRow {
		return store.HistoryRow{ObservedAt: now.Add(-at), Change: change, Condition: store.Condition{Type: typ, Status: status}}
	}
	flips := func(typ string) []store.HistoryRow {
		rows := make([]store.HistoryRow, 0, 8)
		for i := range 8 {
			st := "True"
			if i%2 == 1 {
				st = "False"
			}
			rows = append(rows, row(time.Duration(8-i)*time.Hour, "set", typ, st))
		}
		return rows
	}

	cases := []struct {
		name      string
		condition string
		status    string
		group     []string // uids of the findings
		objects   []objectConditions
		history   map[string][]store.HistoryRow
		verdict   string
		reason    string
	}{
		{
			name: "constant on all objects while Ready", condition: "Stalled", status: "True", group: []string{"a", "b"},
			objects: []objectConditions{obj("a", "Stalled", "True", old, "True"), obj("b", "Stalled", "True", old, "True")},
			verdict: verdictFalsePositive, reason: "Stalled=True on all 2 objects of widgets.example.com for at least 3d, all of them Ready=True",
		},
		{
			name: "a single object", condition: "Stalled", status: "True", group: []string{"a"},
			objects: []objectConditions{obj("a", "Stalled", "True", old, "True")},
		},
		{
			name: "no Ready condition (Deployment Available=False, Job Failed)", condition: "Available", status: "False", group: []string{"a", "b"},
			objects: []objectConditions{obj("a", "Available", "False", old, ""), obj("b", "Available", "False", old, "")},
		},
		{
			name: "one fresh: the newest transition counts", condition: "Stalled", status: "True", group: []string{"a", "b"},
			objects: []objectConditions{obj("a", "Stalled", "True", old, "True"), obj("b", "Stalled", "True", recent, "True")},
		},
		{
			name: "another object of the type is healthy", condition: "Stalled", status: "True", group: []string{"a", "b"},
			objects: []objectConditions{obj("a", "Stalled", "True", old, "True"), obj("b", "Stalled", "True", old, "True"), obj("c", "Stalled", "False", old, "True")},
		},
		{
			name: "Unknown", condition: "Synced", status: "Unknown", group: []string{"a", "b"},
			objects: []objectConditions{obj("a", "Synced", "Unknown", old, "True"), obj("b", "Synced", "Unknown", old, "True")},
		},
		{
			name: "Ready itself", condition: "Ready", status: "False", group: []string{"a", "b"},
			objects: []objectConditions{obj("a", "Ready", "False", old, ""), obj("b", "Ready", "False", old, "")},
		},
		{
			name: "objects outside the group do not count", condition: "Stalled", status: "True", group: []string{"a", "b"},
			objects: []objectConditions{obj("a", "Stalled", "True", old, "True"), obj("b", "Stalled", "True", old, "False")},
		},
		{
			name: "Ready went bad after it", condition: "Stalled", status: "True", group: []string{"a"},
			objects: []objectConditions{obj("a", "Stalled", "True", old, "True")},
			history: map[string][]store.HistoryRow{"a": {row(30*time.Minute, "set", "Stalled", "True"), row(25*time.Minute, "set", "Ready", "False"), row(5*time.Minute, "set", "Ready", "True")}},
			verdict: verdictReal, reason: "Ready became unhealthy within 10m after Stalled=True on 1 of 1 object",
		},
		{
			name: "two types on one object count as one object", condition: "Failed/Stalled", status: "True", group: []string{"a"},
			objects: []objectConditions{{uid: "a", conditions: map[string]store.Condition{"Failed": {Status: "True", LastTransitionTime: old}, "Stalled": {Status: "True", LastTransitionTime: old}, "Ready": {Status: "True"}}}},
			history: map[string][]store.HistoryRow{"a": {row(30*time.Minute, "set", "Failed", "True"), row(30*time.Minute, "set", "Stalled", "True"), row(28*time.Minute, "set", "Ready", "False"), row(5*time.Minute, "set", "Ready", "True")}},
			verdict: verdictReal, reason: "Ready became unhealthy within 10m after Failed/Stalled=True on 1 of 1 object",
		},
		{
			name: "Ready was unhealthy before, only its reason changed", condition: "Stalled", status: "True", group: []string{"a"},
			objects: []objectConditions{obj("a", "Stalled", "True", old, "False")},
			history: map[string][]store.HistoryRow{"a": {row(20*time.Hour, "set", "Ready", "False"), row(60*time.Minute, "set", "Stalled", "True"), row(58*time.Minute, "set", "Ready", "False")}},
		},
		{
			name: "objects the store does not know: no verdict about all of them", condition: "Stalled", status: "True", group: []string{"a", "b", "c"},
			objects: []objectConditions{obj("a", "Stalled", "True", old, "True"), obj("b", "Stalled", "True", old, "True")},
		},
		{
			name: "a removed row is not the bad state", condition: "Stalled", status: "True", group: []string{"a"},
			objects: []objectConditions{obj("a", "Stalled", "True", recent, "True")},
			history: map[string][]store.HistoryRow{"a": {row(30*time.Minute, "removed", "Stalled", "True"), row(25*time.Minute, "set", "Ready", "False"), row(5*time.Minute, "set", "Ready", "True")}},
		},
		{
			name: "flapping while Ready stays True", condition: "Stalled", status: "True", group: []string{"a"},
			objects: []objectConditions{obj("a", "Stalled", "True", recent, "True")},
			history: map[string][]store.HistoryRow{"a": flips("Stalled")},
			verdict: verdictInformational, reason: "Stalled changed at least 6 times on each of 1 object in 24h while Ready stayed True",
		},
		{
			name: "flapping, but Ready was False in between (later than 10m)", condition: "Stalled", status: "True", group: []string{"a"},
			objects: []objectConditions{obj("a", "Stalled", "True", recent, "True")},
			history: map[string][]store.HistoryRow{"a": append(flips("Stalled"), row(20*time.Hour, "set", "Ready", "False"))},
		},
		{
			name: "multi-type group: every type must agree", condition: "Failed/Stalled", status: "True", group: []string{"a", "b"},
			objects: []objectConditions{
				{uid: "a", conditions: map[string]store.Condition{"Failed": {Status: "True", LastTransitionTime: old}, "Stalled": {Status: "True", LastTransitionTime: recent}, "Ready": {Status: "True"}}},
				{uid: "b", conditions: map[string]store.Condition{"Failed": {Status: "True", LastTransitionTime: old}, "Stalled": {Status: "True", LastTransitionTime: old}, "Ready": {Status: "True"}}},
			},
		},
	}
	for _, c := range cases {
		g := &triageGroup{check: "conditions", resource: "widgets.example.com", condition: c.condition, status: c.status}
		for _, uid := range c.group {
			g.findings = append(g.findings, &checks.Finding{UID: uid})
		}
		h := &historyData{objects: map[string][]objectConditions{g.resource: c.objects}, history: c.history, since: now.Add(-24 * time.Hour)}
		ev := h.assess(g, now)
		if ev.verdict != c.verdict || (c.reason != "" && ev.reason != c.reason) {
			t.Errorf("%s: got %q %q", c.name, ev.verdict, ev.reason)
		}
	}
	var nilHist *historyData
	if nilHist.assess(&triageGroup{check: "conditions", condition: "X"}, now).verdict != verdictNone {
		t.Error("no history, no verdict")
	}
}

func TestTriageRanksByHistory(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	realProblem := conditionFinding("a", "w1", "Degraded", "True", "X")
	fp1 := conditionFinding("a", "w2", "Stalled", "True", "Waiting")
	fp2 := conditionFinding("a", "w3", "Stalled", "True", "Waiting")
	since := now.Add(-48 * time.Hour).Format(time.RFC3339)
	stalled := func(uid string) objectConditions {
		return objectConditions{uid: uid, conditions: map[string]store.Condition{
			"Stalled": {Status: "True", LastTransitionTime: since}, "Ready": {Status: "True"},
		}}
	}
	h := &historyData{objects: map[string][]objectConditions{"widgets.example.com": {stalled(fp1.UID), stalled(fp2.UID)}}}
	groups := groupFindings([]*checks.Finding{realProblem, realProblem, realProblem, fp1, fp2}, h, now)
	if groups[0].condition != "Stalled" || groups[0].ev.verdict != verdictFalsePositive {
		t.Errorf("the possibly normal group comes first: %+v", groups[0])
	}
	if !strings.Contains(groups[0].ev.reason, "2 objects") {
		t.Errorf("reason: %s", groups[0].ev.reason)
	}
}
