package checks

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	core "github.com/guettli/konmon/checks"
)

// Files are the check sources and test files of one directory.
type Files struct {
	Sources []Source
	Tests   map[string]*TestFile // by path
}

// ReadFiles reads *.ts and *.js checks (not *.d.ts) and *.test.yaml files
// of a directory. prefix is put in front of the paths in error messages.
func ReadFiles(fsys fs.FS, prefix string) (*Files, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	files := &Files{Tests: map[string]*TestFile{}}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		p := path.Join(prefix, name)
		switch {
		case core.IsCheck(name):
			data, err := fs.ReadFile(fsys, name)
			if err != nil {
				return nil, err
			}
			files.Sources = append(files.Sources, Source{Path: p, Code: string(data)})
		case strings.HasSuffix(name, ".test.yaml") || strings.HasSuffix(name, ".test.yml"):
			data, err := fs.ReadFile(fsys, name)
			if err != nil {
				return nil, err
			}
			tf, err := ParseTestFile(data)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			files.Tests[p] = tf
		}
	}
	sort.Slice(files.Sources, func(i, j int) bool { return files.Sources[i].Path < files.Sources[j].Path })
	return files, nil
}

// CoreFiles returns the embedded core checks and their tests.
func CoreFiles() *Files {
	files, err := ReadFiles(core.FS(), "core")
	if err != nil {
		panic(fmt.Sprintf("embedded core checks: %v", err))
	}
	return files
}

// TestPaths returns the test file paths, sorted.
func (f *Files) TestPaths() []string {
	paths := make([]string, 0, len(f.Tests))
	for p := range f.Tests {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}
