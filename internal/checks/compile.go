package checks

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dop251/goja"
	"github.com/evanw/esbuild/pkg/api"
)

// Source is the code of one check file (TypeScript or JavaScript).
type Source struct {
	// Path is used for error messages and decides the loader: .ts or .js.
	Path string
	Code string
}

const globalName = "__konmonCheck"

// runtimeModule is the JavaScript side of `import ... from "konmon"`. The
// types are in konmon.d.ts.
const runtimeModule = `
export function defineCheck(check) { return check; }
const rfc3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/;
export function parseTime(s) {
  if (typeof s !== "string" || !rfc3339.test(s)) return undefined;
  const d = new Date(s);
  return isNaN(d.getTime()) ? undefined : d;
}
`

// compile bundles a check file with esbuild (TypeScript is stripped, the
// "konmon" import is resolved) and compiles the result for goja.
func compile(src Source) (*goja.Program, error) {
	loader := api.LoaderJS
	switch strings.ToLower(filepath.Ext(src.Path)) {
	case ".ts":
		loader = api.LoaderTS
	case ".js":
	default:
		return nil, fmt.Errorf("%s: check files must end in .ts or .js", src.Path)
	}
	res := api.Build(api.BuildOptions{
		Stdin: &api.StdinOptions{
			Contents:   src.Code,
			Loader:     loader,
			Sourcefile: src.Path,
		},
		Bundle:     true,
		Format:     api.FormatIIFE,
		GlobalName: globalName,
		Target:     api.ES2017,
		Platform:   api.PlatformNeutral,
		Write:      false,
		LogLevel:   api.LogLevelSilent,
		Plugins:    []api.Plugin{konmonModule},
	})
	if len(res.Errors) > 0 {
		return nil, buildError(src.Path, res.Errors)
	}
	if len(res.OutputFiles) != 1 {
		return nil, fmt.Errorf("%s: esbuild produced %d files", src.Path, len(res.OutputFiles))
	}
	// Strict mode: writes to the read-only objects throw instead of being
	// ignored silently.
	prg, err := goja.Compile(src.Path, string(res.OutputFiles[0].Contents), true)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src.Path, err)
	}
	return prg, nil
}

// konmonModule makes "konmon" the only module a check can import. Checks
// have no access to the filesystem or network.
var konmonModule = api.Plugin{
	Name: "konmon",
	Setup: func(b api.PluginBuild) {
		b.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(a api.OnResolveArgs) (api.OnResolveResult, error) {
			if a.Path == "konmon" {
				return api.OnResolveResult{Path: "konmon", Namespace: "konmon"}, nil
			}
			return api.OnResolveResult{}, fmt.Errorf("cannot import %q: checks may only import \"konmon\"", a.Path)
		})
		b.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: "konmon"}, func(api.OnLoadArgs) (api.OnLoadResult, error) {
			code := runtimeModule
			return api.OnLoadResult{Contents: &code, Loader: api.LoaderJS}, nil
		})
	},
}

func buildError(path string, msgs []api.Message) error {
	errs := make([]error, 0, len(msgs))
	for _, m := range msgs {
		if m.Location != nil {
			errs = append(errs, fmt.Errorf("%s:%d:%d: %s", path, m.Location.Line, m.Location.Column, m.Text))
		} else {
			errs = append(errs, fmt.Errorf("%s: %s", path, m.Text))
		}
	}
	return errors.Join(errs...)
}
