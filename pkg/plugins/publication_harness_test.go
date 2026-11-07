package plugins

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Real uv from PATH, else the pinned uv the pm suite caches under the user cache
func pubUV(t *testing.T) string {
	t.Helper()
	if found, err := exec.LookPath("uv"); err == nil {
		return found
	}
	cache, err := os.UserCacheDir()
	if err == nil {
		matches, _ := filepath.Glob(filepath.Join(cache, "icebath-pm-tests", "uv-*"))
		for _, root := range matches {
			found := ""
			_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() && (d.Name() == "uv" || d.Name() == "uv.exe") && found == "" {
					found = path
				}
				return nil
			})
			if found != "" {
				return found
			}
		}
	}
	t.Fatal("publication tests need real uv on PATH or the pinned uv the pm suite caches")
	return ""
}

// Host interpreter uv locks the fake core project with
func pubPython(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		if found, err := exec.LookPath(name); err == nil {
			return found
		}
	}
	t.Fatal("publication tests need a host Python")
	return ""
}

// Isolated home and a uv locked fake core project that real PM publications run against
func pubWorld(t *testing.T) (string, string) {
	t.Helper()
	uv, python := pubUV(t), pubPython(t)
	tmp := t.TempDir()
	h, core := filepath.Join(tmp, "home"), filepath.Join(tmp, "core")
	for _, key := range []string{"ICEBATH_MANAGED_MODE", "ICEBATH_INSTALL_ROOT", "VIRTUAL_ENV"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	for key, value := range map[string]string{"HOME": tmp, "USERPROFILE": tmp, home.EnvHome: h,
		pm.EnvRuntimeDir: filepath.Join(tmp, "store"), "UV_CACHE_DIR": filepath.Join(tmp, "cache"),
		"XDG_CONFIG_HOME": filepath.Join(tmp, "config"), "XDG_CONFIG_DIRS": filepath.Join(tmp, "config"),
		pm.EnvDisableLazyInstalls: "1", "UV_OFFLINE": "1"} {
		t.Setenv(key, value)
	}
	t.Cleanup(home.PatchUserHome(tmp))
	config.ResetCache()
	vtWrite(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: []\n  disabled: []\n")
	vtWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname=\"plugin-proof-core\"\nversion=\"1\"\n"+
		"requires-python=\">=3.11\"\ndependencies=[]\n[tool.uv]\npackage=false\n")
	lock := exec.Command(uv, "lock", "--python", python)
	lock.Dir = core
	if out, err := lock.CombinedOutput(); err != nil {
		t.Fatalf("uv lock: %v\n%s", err, out)
	}
	t.Cleanup(pm.OverrideCoreRepo(core))
	t.Cleanup(pm.OverrideUVToolchain(uv, python))
	return tmp, h
}

// Plugin source tree with a manifest and a register function
func pubPluginTree(t *testing.T, dir, manifest string) {
	t.Helper()
	vtWrite(t, filepath.Join(dir, "plugin.yaml"), manifest)
	vtWrite(t, filepath.Join(dir, "__init__.py"), "def register(ctx):\n    pass\n")
}

// Plugins section of a home config.yaml
func pubPlugins(t *testing.T, h string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	plugins, _ := doc["plugins"].(map[string]any)
	return plugins
}

// Names of one plugins list as strings
func pubList(plugins map[string]any, key string) []string {
	out := []string{}
	list, _ := plugins[key].([]any)
	for _, v := range list {
		out = append(out, scalarString(v))
	}
	return out
}

func TestNestedEnableDisableUseTheCanonicalKey(t *testing.T) {
	for _, query := range []string{"trace-leaf", "trace-manifest", "observability/trace-leaf"} {
		t.Run(query, func(t *testing.T) {
			_, h := pubWorld(t)
			ctx := context.Background()
			key := "observability/trace-leaf"
			pubPluginTree(t, filepath.Join(h, "plugins", key), "name: trace-manifest\nversion: '1.0'\n")
			vtWrite(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: []\n  disabled: ["+key+", trace-leaf, trace-manifest]\n")
			resolved, err := resolveNamedPluginKey(ctx, query)
			if err != nil || resolved != key {
				t.Fatalf("resolve %s = %q %v", query, resolved, err)
			}
			changed, err := activateKey(ctx, h, resolved, true, nil)
			if err != nil || !changed {
				t.Fatalf("enable: %v %v", changed, err)
			}
			plugins := pubPlugins(t, h)
			if got := pubList(plugins, "enabled"); strings.Join(got, ",") != key {
				t.Fatalf("enabled = %v", got)
			}
			if got := pubList(plugins, "disabled"); len(got) != 0 {
				t.Fatalf("disabled = %v", got)
			}
			if changed, err := activateKey(ctx, h, resolved, true, nil); err != nil || changed {
				t.Fatalf("second enable must be a no-op: %v %v", changed, err)
			}
			if changed, err := activateKey(ctx, h, resolved, false, nil); err != nil || !changed {
				t.Fatalf("disable: %v %v", changed, err)
			}
			plugins = pubPlugins(t, h)
			if got := pubList(plugins, "enabled"); len(got) != 0 {
				t.Fatalf("enabled after disable = %v", got)
			}
			if got := pubList(plugins, "disabled"); strings.Join(got, ",") != key {
				t.Fatalf("disabled after disable = %v", got)
			}
		})
	}
}

func TestAmbiguousAndUnknownNamesCannotChangeConfig(t *testing.T) {
	_, h := pubWorld(t)
	ctx := context.Background()
	for _, category := range []string{"image_gen", "model-providers"} {
		vtWrite(t, filepath.Join(h, "plugins", category, "same-leaf", "plugin.yaml"), "name: "+category+"-fixture\n")
	}
	before, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"same-leaf", "not-installed"} {
		var out strings.Builder
		err := CmdEnable(ctx, query, nil, &out)
		var exit *CommandExit
		if !errors.As(err, &exit) || exit.Code != 1 {
			t.Fatalf("enable %s: %v\n%s", query, err, out.String())
		}
	}
	if key, err := resolveNamedPluginKey(ctx, "image_gen/same-leaf"); err != nil || key != "image_gen/same-leaf" {
		t.Fatalf("resolve = %q %v", key, err)
	}
	after, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	if string(after) != string(before) {
		t.Fatalf("config changed:\n%s", after)
	}
}

// test_plugins_cmd_activation_keys.py test_dashboard_toggle_writes_canonical_key_and_clears_stale_aliases
func TestDashboardToggleWritesCanonicalKeyAndClearsStaleAliases(t *testing.T) {
	_, h := pubWorld(t)
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return liveInterpreters(t)["system"], nil }
	t.Cleanup(func() { hostInterpreter = previous })
	ctx := homeCtx(h)
	pubPluginTree(t, filepath.Join(h, "plugins", "obs", "zzprobe"), "name: zz-probe-manifest\nversion: '1.0'\n")
	vtWrite(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: []\n  disabled: [obs/zzprobe]\n")
	result, err := DashboardSetAgentPluginEnabled(ctx, "zzprobe", true)
	if err != nil || result["ok"] != true || result["name"] != "obs/zzprobe" || result["unchanged"] != false || result["restart_required"] != true ||
		result["gateway_reloaded"] != false {
		t.Fatalf("enable by leaf = %v %v", result, err)
	}
	if m := GetPluginManager(ctx); m.pluginHostInstance != nil {
		t.Cleanup(m.pluginHostInstance.Shutdown)
	}
	plugins := pubPlugins(t, h)
	enabled, disabled := pubList(plugins, "enabled"), pubList(plugins, "disabled")
	if strings.Join(enabled, ",") != "obs/zzprobe" || len(disabled) != 0 {
		t.Fatalf("lists = %v %v", enabled, disabled)
	}
	var manifest *Manifest
	for _, mf := range CollectDirectoryManifests(h) {
		if mf.Name == "zz-probe-manifest" {
			manifest = mf
		}
	}
	if manifest == nil {
		t.Fatal("zz-probe-manifest is not discovered")
	}
	if g := GateManifest(h, manifest, map[string]bool{}, map[string]bool{"obs/zzprobe": true}, true); g.Action != "load" {
		t.Fatalf("gate = %+v", g)
	}
	again, err := DashboardSetAgentPluginEnabled(ctx, "zz-probe-manifest", true)
	if err != nil || again["unchanged"] != true || again["restart_required"] != false {
		t.Fatalf("enable by manifest name again = %v %v", again, err)
	}
}
