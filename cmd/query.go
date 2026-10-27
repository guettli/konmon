package cmd

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/guettli/konmon/internal/store"
)

var (
	findingsAt     string
	findingsFilter findingFilter
)

var findingsCmd = &cobra.Command{
	Use:   "findings",
	Short: "Show the findings which are open now, or were open at a given time",
	Long: `Show the findings which are open now, or with --at, which were open at that
time. The output has the same format as 'konmon check', with the time the
finding was first seen.

Examples:
  konmon findings
  konmon findings --at 2h -n my-app
  konmon findings --at 2026-10-24T03:00:00Z --category health -o json`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := findingsFilter.validate(); err != nil {
			return err
		}
		now := time.Now()
		at, err := parseTime(findingsAt, now)
		if err != nil {
			return fmt.Errorf("--at: %w", err)
		}
		if at.IsZero() {
			at = now
		}
		st, err := store.OpenExisting(cmd.Context(), dbPath)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		rows, err := st.FindingsAt(cmd.Context(), at)
		if err != nil {
			return err
		}
		return printFindings(cmd.OutOrStdout(), rows, at, &findingsFilter)
	},
}

func printFindings(w io.Writer, rows []store.FindingRow, at time.Time, filter *findingFilter) error {
	if filter.output == "json" {
		result := findingsResult{At: at, Findings: []jsonFinding{}}
		for i := range rows {
			if filter.match(&rows[i].Finding) {
				j := toJSONFinding(&rows[i].Finding)
				j.FirstSeen = rows[i].FirstSeen
				result.Findings = append(result.Findings, j)
			}
		}
		sortJSONFindings(result.Findings)
		return writeJSON(w, result)
	}
	var shown []shownFinding
	for i := range rows {
		f := &rows[i].Finding
		if filter.match(f) {
			shown = append(shown, shownFinding{f, fmt.Sprintf("%s  [since %s]", f.Line(at), formatTime(rows[i].FirstSeen))})
		}
	}
	health, hygiene := printFindingLines(w, shown)
	_, _ = fmt.Fprintf(w, "%s open at %s\n", filter.countText(health, hygiene), formatTime(at))
	return nil
}

// findingsResult is the output of `findings -o json`.
type findingsResult struct {
	At       time.Time     `json:"at"`
	Findings []jsonFinding `json:"findings"`
}

var (
	historyNamespace string
	historySince     string
)

var historyCmd = &cobra.Command{
	Use:   "history RESOURCE/NAME",
	Short: "Show the condition changes and findings of one object over time",
	Long: `Show what happened to one object: every recorded change of its conditions,
and when findings were opened and resolved. RESOURCE is the plural resource
name or the kind, optionally with group (deployments.apps/my-app).

Examples:
  konmon history -n kube-system pods/coredns-abc
  konmon history machines.cluster.x-k8s.io/my-machine --since 24h`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resource, group, name, err := splitResourceName(args[0])
		if err != nil {
			return err
		}
		since, err := parseTime(historySince, time.Now())
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		st, err := store.OpenExisting(cmd.Context(), dbPath)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		objs, err := st.FindObjects(cmd.Context(), resource, group, historyNamespace, name)
		if err != nil {
			return err
		}
		if len(objs) == 0 {
			return fmt.Errorf("no object %s found", args[0])
		}
		for i := range objs {
			if i > 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout())
			}
			if err := printHistory(cmd.Context(), cmd.OutOrStdout(), st, &objs[i], since); err != nil {
				return err
			}
		}
		return nil
	},
}

type historyLine struct {
	at   time.Time
	text string
}

func printHistory(ctx context.Context, w io.Writer, st *store.Store, obj *store.ObjectRow, since time.Time) error {
	gr := obj.GVR.GroupResource().String()
	_, _ = fmt.Fprintf(w, "%s %s/%s uid=%s first seen %s", gr, obj.Namespace, obj.Name, obj.UID, formatTime(obj.FirstSeen))
	if !obj.DeletedAt.IsZero() {
		_, _ = fmt.Fprintf(w, ", deleted %s", formatTime(obj.DeletedAt))
	}
	_, _ = fmt.Fprintln(w)

	history, err := st.ConditionHistory(ctx, obj.UID, since)
	if err != nil {
		return err
	}
	findings, err := st.FindingsOfObject(ctx, obj.UID, since)
	if err != nil {
		return err
	}
	var lines []historyLine
	for _, h := range history {
		text := fmt.Sprintf("condition %s=%s %s %q", h.Type, h.Status, h.Reason, h.Message)
		if t, err := time.Parse(time.RFC3339, h.LastTransitionTime); err == nil {
			text += " [lastTransitionTime " + formatTime(t) + "]"
		}
		if h.Change == "removed" {
			text = "condition removed: " + h.Type
		}
		lines = append(lines, historyLine{h.ObservedAt, text})
	}
	for i := range findings {
		f := &findings[i]
		if !f.FirstSeen.Before(since) {
			lines = append(lines, historyLine{f.FirstSeen, "finding opened:   " + strings.TrimSpace(f.Finding.Line(f.FirstSeen))})
		}
		if !f.ResolvedAt.IsZero() {
			lines = append(lines, historyLine{f.ResolvedAt, "finding resolved: " + strings.TrimSpace(f.Finding.Line(f.ResolvedAt))})
		}
	}
	if !obj.DeletedAt.IsZero() && !obj.DeletedAt.Before(since) {
		lines = append(lines, historyLine{obj.DeletedAt, "object deleted"})
	}
	slices.SortStableFunc(lines, func(a, b historyLine) int { return cmp.Compare(a.at.UnixMilli(), b.at.UnixMilli()) })
	for _, l := range lines {
		_, _ = fmt.Fprintf(w, "  %s  %s\n", formatTime(l.at), l.text)
	}
	if len(lines) == 0 {
		_, _ = fmt.Fprintln(w, "  no changes recorded")
	}
	return nil
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), buildVersion())
	},
}

func init() {
	findingsCmd.Flags().StringVar(&findingsAt, "at", "", "Point in time: a duration ago (2h) or an RFC3339 timestamp. Default: now.")
	findingsFilter.register(findingsCmd.Flags())
	historyCmd.Flags().StringVarP(&historyNamespace, "namespace", "n", "", "Namespace of the object. Default: all namespaces.")
	historyCmd.Flags().StringVar(&historySince, "since", "", "Only show changes after this time: a duration ago (24h) or an RFC3339 timestamp. Default: everything.")
	rootCmd.AddCommand(findingsCmd, historyCmd, versionCmd)
}
