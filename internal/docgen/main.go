// Command docgen writes the generated parts of the documentation: the CLI
// reference (docs/cli) and the table of core checks in docs/checks.md. CI
// fails if the committed docs differ: run `make docs` after a change.
package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra/doc"

	"github.com/guettli/konmon/cmd"
	"github.com/guettli/konmon/internal/checks"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if err := cliReference("docs/cli"); err != nil {
		return err
	}
	return checksTable("docs/checks.md")
}

func cliReference(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	root := cmd.Root()
	root.DisableAutoGenTag = true // no date: the output must be stable
	for _, c := range root.Commands() {
		if c.Name() == "completion" {
			c.Hidden = true
		}
	}
	return doc.GenMarkdownTree(root, dir)
}

const (
	beginMarker = "<!-- BEGIN generated checks table: make docs -->"
	endMarker   = "<!-- END generated checks table -->"
)

// checksTable replaces the table between the markers in path.
func checksTable(path string) error {
	core := checks.CoreFiles()
	engine, err := checks.Load(core.Sources, nil, checks.Options{})
	if err != nil {
		return err
	}
	sources := map[string]string{}
	for _, s := range core.Sources {
		sources[s.Path] = s.Code
	}
	var b bytes.Buffer
	b.WriteString(beginMarker + "\n\n")
	b.WriteString("| Check | Category | Severity | Reports | Parameters (`--check-param`) | From |\n|---|---|---|---|---|---|\n")
	for _, category := range []string{checks.CategoryHealth, checks.CategoryHygiene} {
		for _, d := range engine.Checks() {
			if d.Category != category {
				continue
			}
			file := strings.TrimPrefix(d.Path, "core/")
			reports, from := describe(sources[d.Path])
			fmt.Fprintf(&b, "| [`%s`](../checks/%s) | %s | %s | %s | %s | %s |\n",
				d.Name, file, d.Category, d.Severity, reports, params(d.Params), from)
		}
	}
	b.WriteString("\n" + endMarker)

	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	text := string(data)
	start, end := strings.Index(text, beginMarker), strings.Index(text, endMarker)
	if start < 0 || end < start {
		return fmt.Errorf("%s: markers not found", path)
	}
	text = text[:start] + b.String() + text[end+len(endMarker):]
	return os.WriteFile(path, []byte(text), 0o644) //nolint:gosec // a doc in the repo, the path is a constant
}

var projects = regexp.MustCompile(`check-conditions|k8sgpt|Popeye`)

// describe takes the first sentence of the header comment of a check
// ("// Reports X: details.") and the projects it was ported from.
func describe(code string) (reports, from string) {
	var header []string
	for _, line := range strings.Split(code, "\n") {
		if !strings.HasPrefix(line, "//") {
			break
		}
		header = append(header, strings.TrimSpace(strings.TrimPrefix(line, "//")))
	}
	text := strings.Join(header, " ")
	first := strings.SplitN(text, "  ", 2)[0] // up to an empty comment line
	if i := strings.Index(first, ". "); i >= 0 {
		first = first[:i]
	}
	reports = strings.TrimSuffix(strings.TrimPrefix(first, "Reports "), ".")
	reports = strings.ReplaceAll(reports, "|", `\|`)
	seen := map[string]bool{}
	var names []string
	for _, p := range projects.FindAllString(text, -1) {
		if !seen[p] {
			seen[p] = true
			names = append(names, p)
		}
	}
	return reports, strings.Join(names, ", ")
}

// params lists the parameters with their defaults: "name=30"; objects and
// lists only by name.
func params(p map[string]any) string {
	names := make([]string, 0, len(p))
	for name := range p {
		names = append(names, name)
	}
	slices.Sort(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		switch v := p[name].(type) {
		case map[string]any, []any:
			parts = append(parts, "`"+name+"`")
		default:
			parts = append(parts, fmt.Sprintf("`%s=%v`", name, v))
		}
	}
	return strings.Join(parts, " ")
}
