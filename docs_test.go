package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var mdLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// TestMarkdownLinks checks that relative links in the Markdown files point
// to existing files.
func TestMarkdownLinks(t *testing.T) {
	var files []string
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 5 {
		t.Fatalf("found only %d Markdown files", len(files))
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range mdLink.FindAllStringSubmatch(string(data), -1) {
			target, _, _ := strings.Cut(m[1], "#")
			if target == "" || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(file), target)); err != nil { //nolint:gosec // links of our own docs
				t.Errorf("%s: broken link %s", file, m[1])
			}
		}
	}
}
