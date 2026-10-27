// Package cmd implements the konmon command line.
package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "dev"
	}
	return info.Main.Version
}

var (
	dbPath   string
	logLevel string
)

var rootCmd = &cobra.Command{
	Use:     "konmon",
	Version: buildVersion(),
	Short:   "Kubernetes Condition Monitor",
	Long: `konmon checks the resources of a Kubernetes cluster: status.conditions
(the rules of check-conditions) and more checks, written in TypeScript or
JavaScript, and your own ones.

Ad hoc:      konmon check --context prod
Monitoring:  konmon run, which keeps a local copy with history in SQLite
             and sends alerts to Alertmanager; query it with 'konmon
             findings' and 'konmon history'.
False positives: 'konmon triage' fixes them in your config file, and
'konmon report' tells the maintainers.`,
	SilenceUsage: true,
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		var exit exitError
		if errors.As(err, &exit) {
			os.Exit(int(exit))
		}
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&dbPath, "db", "konmon.db", "Path of the SQLite database.")
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "Log level: debug, info, warn, error.")
}

func newLogger() (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(logLevel)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q: %w", logLevel, err)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})), nil
}

// parseTime accepts an RFC3339 timestamp or a duration which is subtracted
// from now. An empty string yields the zero time.
func parseTime(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is neither a duration (e.g. 2h) nor an RFC3339 timestamp (e.g. 2026-10-24T03:00:00Z)", s)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// splitResourceName splits "pods/foo" or "machines.cluster.x-k8s.io/foo"
// into resource, group and name.
func splitResourceName(s string) (resource, group, name string, err error) {
	resource, name, ok := strings.Cut(s, "/")
	if !ok || resource == "" || name == "" {
		return "", "", "", fmt.Errorf("expected RESOURCE/NAME, e.g. pods/my-pod or deployments.apps/my-app, got %q", s)
	}
	resource, group, _ = strings.Cut(resource, ".")
	return resource, group, name, nil
}
