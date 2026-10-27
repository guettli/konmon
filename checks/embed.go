// Package checks holds the core checks of konmon. They are written like
// user checks and embedded into the binary.
package checks

import (
	"embed"
	"io/fs"
	"path"
	"strings"
)

//go:embed *.ts *.test.yaml tsconfig.json
var files embed.FS

// FS returns the check files, their tests and the type definitions.
func FS() fs.FS { return files }

// Types returns konmon.d.ts.
func Types() string {
	data, err := files.ReadFile("konmon.d.ts")
	if err != nil {
		panic(err)
	}
	return string(data)
}

// IsCheck reports whether a file name is a check (not a .d.ts file).
func IsCheck(name string) bool {
	ext := path.Ext(name)
	return (ext == ".ts" || ext == ".js") && !strings.HasSuffix(name, ".d.ts")
}
