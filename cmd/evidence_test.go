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
	recent := now.Add(-time.Hour).Format(time.RFC3339)
	obj := func(uid, stalled, since, ready string) objectConditions {
		o := objectConditions{uid: uid, conditions: map[string]store.Condition{
			"Stalled": {Type: "Stalled", Status: stalled, LastTransitionTime: since},
		}}
		if ready != "" {
			o.conditions["Ready"] = store.Condition{Type: "Ready", Status: ready}
		}
		return o
	}
	g := &triageGroup{check: "conditions", resource: "widgets.example.com", condition: "Stalled", status: "True"}
	row := func(at time.Duration, typ, status string) store.HistoryRow {
		return store.HistoryRow{ObservedAt: now.Add(-at), Change: "set", Condition: store.Condition{Type: typ, Status: status}}
	}

	cases := []struct {
		name    string
		objects []objectConditions
		history map[string][]store.HistoryRow
		verdict string
		reason  string
	}{
		{
			name:    "constant on all objects while Ready",
			objects: []objectConditions{obj("a", "True", old, "True"), obj("b", "True", old, "True")},
			verdict: verdictFalsePositive, reason: "Stalled=True on 2/2 widgets.example.com for 3d, all of them Ready",
		},
		{
			name:    "not on all objects",
			objects: []objectConditions{obj("a", "True", old, "True"), obj("b", "False", old, "True")},
			verdict: verdictNone,
		},
		{
			name:    "too recent",
			objects: []objectConditions{obj("a", "True", recent, "True")},
			verdict: verdictNone,
		},
		{
			name:    "not Ready",
			objects: []objectConditions{obj("a", "True", old, "False")},
			verdict: verdictNone,
		},
		{
			name:    "Ready went bad after it",
			objects: []objectConditions{obj("a", "True", old, "True")},
			history: map[string][]store.HistoryRow{"a": {row(30*time.Minute, "Stalled", "True"), row(25*time.Minute, "Ready", "False"), row(5*time.Minute, "Ready", "True")}},
			verdict: verdictReal, reason: "Ready became unhealthy within 10m after Stalled=True on 1 of 1 object",
		},
		{
			name:    "flapping while Ready",
			objects: []objectConditions{obj("a", "True", recent, "True"), obj("b", "False", recent, "True")},
			history: map[string][]store.HistoryRow{"a": {
				row(7*time.Hour, "Stalled", "True"), row(6*time.Hour, "Stalled", "False"), row(5*time.Hour, "Stalled", "True"),
				row(4*time.Hour, "Stalled", "False"), row(3*time.Hour, "Stalled", "True"), row(2*time.Hour, "Stalled", "False"),
				row(time.Hour, "Stalled", "True"),
			}},
			verdict: verdictInformational, reason: "Stalled changed 6 times on 1 object in 24h while Ready stayed True",
		},
	}
	for _, c := range cases {
		h := &historyData{objects: map[string][]objectConditions{g.resource: c.objects}, history: c.history, since: now.Add(-24 * time.Hour)}
		ev := h.assess(g, now)
		if ev.verdict != c.verdict || (c.reason != "" && !strings.HasPrefix(ev.reason, c.reason)) {
			t.Errorf("%s: got %q %q", c.name, ev.verdict, ev.reason)
		}
	}
	var nilHist *historyData
	if nilHist.assess(g, now).verdict != verdictNone {
		t.Error("no history, no verdict")
	}
}

func TestTriageRanksByHistory(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	realProblem := conditionFinding("a", "w1", "Degraded", "True", "X")
	fp := conditionFinding("a", "w2", "Stalled", "True", "Waiting")
	h := &historyData{objects: map[string][]objectConditions{"widgets.example.com": {
		{uid: "w2", conditions: map[string]store.Condition{"Stalled": {Status: "True", LastTransitionTime: now.Add(-48 * time.Hour).Format(time.RFC3339)}}},
	}}}
	groups := groupFindings([]*checks.Finding{realProblem, realProblem, fp}, h, now)
	if groups[0].condition != "Stalled" || groups[0].ev.verdict != verdictFalsePositive {
		t.Errorf("the likely false positive comes first: %+v", groups[0])
	}
}
