package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// issuesURL is where users report to the maintainers of konmon.
const issuesURL = "https://github.com/guettli/konmon/issues/new"

var reportOpts struct {
	open      bool
	check     string
	resource  string
	condition string
	status    string
	reason    string
	rule      string
	title     string
}

var reportCmd = &cobra.Command{
	Use:   "report",
	Short: "Tell the maintainers about a false positive, an idea or a bug",
	Long: `Print a link to a prefilled GitHub issue. Nothing is sent: you see and edit
everything in the browser before you submit it. Object names and condition
messages are not included.

Every false positive which is fixed in konmon itself is fixed for everybody.`,
}

var reportFalsePositiveCmd = &cobra.Command{
	Use:   "false-positive",
	Short: "Report a finding of a healthy state",
	Example: `  konmon report false-positive --check conditions --resource widgets.example.com \
    --condition Stalled --status True --reason Waiting`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		o := reportOpts
		if o.check == "" || o.resource == "" {
			return errors.New("--check and --resource are required")
		}
		return printReport(cmd.Context(), cmd.OutOrStdout(), falsePositiveURL(o.check, o.resource, o.condition, o.status, o.reason, o.rule), o.open)
	},
}

var reportCheckIdeaCmd = &cobra.Command{
	Use:   "check-idea",
	Short: "Suggest something konmon should detect",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return printReport(cmd.Context(), cmd.OutOrStdout(), issueURL("check-idea.yml", "Check idea: "+reportOpts.title, nil), reportOpts.open)
	},
}

var reportBugCmd = &cobra.Command{
	Use:   "bug",
	Short: "Report a bug",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return printReport(cmd.Context(), cmd.OutOrStdout(), issueURL("bug.yml", "Bug: "+reportOpts.title, nil), reportOpts.open)
	},
}

func init() {
	reportCmd.PersistentFlags().BoolVar(&reportOpts.open, "open", false, "Open the link in the browser.")
	f := reportFalsePositiveCmd.Flags()
	f.StringVar(&reportOpts.check, "check", "", "Name of the check, e.g. conditions.")
	f.StringVar(&reportOpts.resource, "resource", "", "Resource type: resource.group, e.g. widgets.example.com.")
	f.StringVar(&reportOpts.condition, "condition", "", "Condition type, e.g. Stalled.")
	f.StringVar(&reportOpts.status, "status", "", "Status of the condition, e.g. True.")
	f.StringVar(&reportOpts.reason, "reason", "", "Reason of the condition.")
	f.StringVar(&reportOpts.rule, "rule", "", "The config which fixes it (YAML).")
	for _, c := range []*cobra.Command{reportCheckIdeaCmd, reportBugCmd} {
		c.Flags().StringVar(&reportOpts.title, "title", "", "Title of the issue.")
	}
	reportCmd.AddCommand(reportFalsePositiveCmd, reportCheckIdeaCmd, reportBugCmd)
	rootCmd.AddCommand(reportCmd)
}

// issueURL builds a link to a new issue with a form, prefilled by the ids of
// its fields.
func issueURL(template, title string, fields map[string]string) string {
	q := url.Values{}
	q.Set("template", template)
	q.Set("title", title)
	q.Set("version", buildVersion())
	for k, v := range fields {
		if v != "" {
			q.Set(k, v)
		}
	}
	return issuesURL + "?" + q.Encode()
}

// falsePositiveURL links to a prefilled false positive report.
func falsePositiveURL(check, resource, condition, status, reason, rule string) string {
	finding := condition
	if status != "" {
		finding += "=" + status
	}
	if reason != "" {
		finding += " " + reason
	}
	title := fmt.Sprintf("False positive: %s %s", check, resource)
	if finding != "" {
		title += " " + finding
	}
	return issueURL("false-positive.yml", title, map[string]string{
		"check": check, "resource": resource, "finding": strings.TrimSpace(finding), "rule": rule,
	})
}

func printReport(ctx context.Context, w io.Writer, link string, open bool) error {
	_, _ = fmt.Fprintf(w, "Open this link, check and edit the text, then submit:\n\n  %s\n", link)
	if !open {
		return nil
	}
	name, args := "xdg-open", []string{link}
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", link}
	}
	if err := exec.CommandContext(ctx, name, args...).Start(); err != nil {
		return fmt.Errorf("open the browser: %w", err)
	}
	return nil
}
