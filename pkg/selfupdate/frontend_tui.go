package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// Module path of the bundler this binary carries in place of the prepared esbuild package
const frontendEsbuildModule = "github.com/evanw/esbuild"

// Resolves react-devtools-core, imported only by Ink dev mode, to an inert module so the bundle never carries it
var frontendInertDevtools = api.Plugin{
	Name: "stub-react-devtools-core",
	Setup: func(b api.PluginBuild) {
		b.OnResolve(api.OnResolveOptions{Filter: `^react-devtools-core$`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
			return api.OnResolveResult{Path: args.Path, Namespace: "stub-devtools"}, nil
		})
		b.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: "stub-devtools"}, func(api.OnLoadArgs) (api.OnLoadResult, error) {
			contents := "export default { initialize() {}, connectToDevTools() {} }"
			return api.OnLoadResult{Contents: &contents, Loader: api.LoaderJS}, nil
		})
	},
}

// Fails unless the workspace prepared the esbuild release this binary bundles with
func frontendRequireBundler(source string) error {
	var prepared string
	for _, root := range []string{filepath.Join(source, "ui-tui"), source} {
		raw, err := os.ReadFile(filepath.Join(root, "node_modules", "esbuild", "package.json"))
		if err != nil {
			continue
		}
		var manifest struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return err
		}
		prepared = manifest.Version
		break
	}
	if prepared == "" {
		return fmt.Errorf("Missing prepared esbuild dependency in ui-tui or %s/node_modules", source)
	}
	bundled := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == frontendEsbuildModule {
				bundled = strings.TrimPrefix(dep.Version, "v")
			}
		}
	}
	if prepared != bundled {
		return fmt.Errorf("Prepared esbuild %s does not match the esbuild %s this binary bundles the TUI with", prepared, bundled)
	}
	return nil
}

// Bundles the TUI source into out as dist/entry.js with its package type and receipt, returning the entry
func frontendBuildTUI(ctx context.Context, st *frontendStep, source, out string, env map[string]string) (string, error) {
	src, dest, err := frontendProductOutput(source, out, []string{"ui-tui", "apps/shared", "node_modules"})
	if err != nil {
		return "", err
	}
	if err := frontendRequireBundler(src); err != nil {
		return "", err
	}
	node, err := lookPathIn("node", env["PATH"])
	if err != nil {
		return "", errors.New("node is required on PATH")
	}
	host, err := frontendNodeHost(node)
	if err != nil {
		return "", err
	}
	root := filepath.Join(src, "ui-tui")
	inputs, err := frontendBuildInputs(src, "tui", map[string]string{}, env)
	if err != nil {
		return "", err
	}
	err = frontendWithProduct(dest, "", func(product, scratch string) error {
		outfile := filepath.Join(product, "dist", "entry.js")
		result := api.Build(api.BuildOptions{
			AbsWorkingDir:   root,
			EntryPoints:     []string{filepath.Join(root, "src", "entry.tsx")},
			Bundle:          true,
			Platform:        api.PlatformNode,
			Format:          api.FormatESModule,
			Engines:         []api.Engine{{Name: api.EngineNode, Version: "20"}},
			Outfile:         outfile,
			JSX:             api.JSXAutomatic,
			JSXImportSource: "react",
			Alias:           map[string]string{"@icebath/ink": filepath.Join(root, "packages", "hermes-ink", "src", "entry-exports.ts")},
			Plugins:         []api.Plugin{frontendInertDevtools},
			Banner:          map[string]string{"js": "import { createRequire as __cr } from 'node:module'; const require = __cr(import.meta.url);"},
			LogLevel:        api.LogLevelSilent,
			Write:           true,
		})
		for _, text := range api.FormatMessages(result.Warnings, api.FormatMessagesOptions{Kind: api.WarningMessage}) {
			fmt.Fprint(st, text)
		}
		if len(result.Errors) > 0 {
			text := api.FormatMessages(result.Errors, api.FormatMessagesOptions{Kind: api.ErrorMessage})
			fmt.Fprint(st, strings.Join(text, ""))
			word := "errors"
			if len(result.Errors) == 1 {
				word = "error"
			}
			return fmt.Errorf("Build failed with %d %s:\n%s", len(result.Errors), word, strings.Join(text, ""))
		}
		for _, file := range result.OutputFiles {
			rel, _ := filepath.Rel(root, file.Path)
			st.say("  %s  %d bytes", filepath.ToSlash(rel), len(file.Contents))
		}
		body, err := os.ReadFile(outfile)
		if err != nil {
			return err
		}
		if strings.HasPrefix(string(body), "#!") {
			if err := os.WriteFile(outfile, body[strings.IndexByte(string(body), '\n')+1:], 0o666); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(product, "package.json"), []byte(`{"type":"module"}`+"\n"), 0o666); err != nil {
			return err
		}
		return frontendRecordProduct(host, src, "tui", filepath.Join(product, "dist"), inputs, env, nil, false)
	})
	return filepath.Join(dest, "dist", "entry.js"), err
}

// Builds the TUI of a checkout into ui-tui/dist through a scratch product, keeping the workspace itself in place
func frontendBuildDeveloperTUI(ctx context.Context, st *frontendStep, source string, env map[string]string) (string, error) {
	scratch, err := os.MkdirTemp(frontendNodeTmpdir(env), "icebath-tui-")
	if err != nil {
		return "", err
	}
	entry, err := func() (string, error) {
		if _, err := frontendBuildTUI(ctx, st, source, filepath.Join(scratch, "tui"), env); err != nil {
			return "", err
		}
		staged, err := os.MkdirTemp(filepath.Join(source, "ui-tui"), ".dist-")
		if err != nil {
			return "", err
		}
		err = os.CopyFS(staged, os.DirFS(filepath.Join(scratch, "tui", "dist")))
		if err == nil {
			err = frontendPublishDirectory(staged, filepath.Join(source, "ui-tui", "dist"), source)
		}
		if rmErr := os.RemoveAll(staged); rmErr != nil {
			return "", rmErr
		}
		return filepath.Join(source, "ui-tui", "dist", "entry.js"), err
	}()
	if rmErr := os.RemoveAll(scratch); rmErr != nil {
		return "", rmErr
	}
	if err != nil {
		return "", err
	}
	st.say("built %s", entry)
	return entry, nil
}
