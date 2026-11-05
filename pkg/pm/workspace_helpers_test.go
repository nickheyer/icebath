package pm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"github.com/nickheyer/icebath/pkg/home"
)

// Installs a manifest host reporting version and judging requires_icebath as a PEP 440 specifier set
func wsHost(t *testing.T, version string) {
	t.Helper()
	original := pluginManifestHost
	SetPluginManifestHost(&PluginManifestHost{
		SupportedManifestVersion: 2,
		RunningVersion:           func() string { return version },
		RequiresError: func(manifest map[string]any) string {
			spec, _ := manifest["requires_icebath"].(string)
			if strings.TrimSpace(spec) == "" {
				return ""
			}
			specs, err := parseSpecifierSet(spec)
			if err == nil {
				if ok, err := specifierSetContains(specs, version); err == nil && ok {
					return ""
				}
			}
			return "requires icebath " + spec + ", running " + version
		},
		ReadAgentManifest: func(pluginDir string) (map[string]any, error) {
			data, err := os.ReadFile(filepath.Join(pluginDir, "plugin.json"))
			if err != nil {
				return nil, err
			}
			manifest, err := loadJSON(data)
			if err != nil {
				return nil, err
			}
			return manifest.(map[string]any), nil
		},
	})
	t.Cleanup(func() { pluginManifestHost = original })
}

// Default home and one live profile, each a home whose selection feeds the dependency union
func wsHomes(t *testing.T) (string, string) {
	t.Helper()
	isolateHome(t)
	wsHost(t, "")
	root := DependencyHomeRoot(context.Background())
	profile := filepath.Join(root, "profiles", "work")
	if err := os.MkdirAll(profile, 0o777); err != nil {
		t.Fatal(err)
	}
	wsWriteConfig(t, root)
	wsWriteConfig(t, profile)
	if home.Root() != root {
		t.Fatalf("home root %s is not the dependency root %s", home.Root(), root)
	}
	return root, profile
}

// Writes a home config naming these enabled plugins, an empty plugins table when none
func wsWriteConfig(t *testing.T, homeDir string, enabled ...string) {
	t.Helper()
	body := "plugins: {}\n"
	if len(enabled) > 0 {
		body = "plugins:\n  enabled:\n"
		for _, name := range enabled {
			body += "  - " + name + "\n"
		}
	}
	peWrite(t, filepath.Join(homeDir, "config.yaml"), body)
}

// Bytes of every regular file under root keyed by relative path
func wsSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			data, _ := os.ReadFile(path)
			rel, _ := filepath.Rel(root, path)
			out[rel] = string(data)
		}
		return nil
	})
	return out
}

// Parsed TOML document of a file
func wsTOML(t *testing.T, path string) *OrderedObject {
	t.Helper()
	text, err := readUTF8SigText(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := tomlLoads(text)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// Value at a dotted path of a TOML document, nil when any step is absent
func wsAt(doc any, path ...string) any {
	for _, key := range path {
		obj, ok := doc.(*OrderedObject)
		if !ok {
			return nil
		}
		doc = obj.Values[key]
	}
	return doc
}

// Strings of a decoded TOML array
func wsStrings(v any) []string {
	items, _ := v.([]any)
	out := []string{}
	for _, item := range items {
		out = append(out, pyrepr.Str(item))
	}
	return out
}

// Locked core with a member and the toolchain, repo root and offline engine env pointed at it
func wsLayout(t *testing.T) (string, string, string, map[string]string) {
	t.Helper()
	tmp := t.TempDir()
	core, uv, env := peLockedProject(t, tmp)
	manifest := filepath.Join(core, "pyproject.toml")
	text, _ := os.ReadFile(manifest)
	peWrite(t, manifest, strings.Replace(string(text), "[tool.uv.workspace]\nmembers=[\"member\"]\n", "", 1))
	peToolchain(t, uv, peHostPython(t))
	peSwap(t, &repoRoot, func() string { return core })
	wsHost(t, "")
	return tmp, core, filepath.Join(core, "member"), env
}

// Managed engine over the swapped toolchain with the layout's offline env
func wsEngine(t *testing.T, destination string, env map[string]string) *PythonEnvironment {
	t.Helper()
	e, err := ManagedEnvironment(context.Background(), destination, ManagedEnvironmentOptions{Env: env, Cache: env["UV_CACHE_DIR"], Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// Package name to version of a uv.lock
func wsLocked(t *testing.T, lock string) map[string]string {
	t.Helper()
	out := map[string]string{}
	packages, _ := wsAt(wsTOML(t, lock), "package").([]any)
	for _, p := range packages {
		out[pyrepr.Str(wsAt(p, "name"))] = pyrepr.Str(wsAt(p, "version"))
	}
	return out
}
