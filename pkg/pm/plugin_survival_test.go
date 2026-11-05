package pm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Writes a home config enabling plugins and naming an optional memory provider
func svWriteEnabled(t *testing.T, homeDir string, enabled []string, provider string) {
	t.Helper()
	data, _ := json.Marshal(enabled)
	body := "plugins:\n  enabled: " + string(data) + "\n"
	if provider != "" {
		body += "memory:\n  provider: " + provider + "\n"
	}
	peWrite(t, filepath.Join(homeDir, "config.yaml"), body)
}

// Gives a plugin a pyproject installing one real package resolvable offline
func svInstallingMember(t *testing.T, plugin, name, wheels, requiresPython string) {
	t.Helper()
	if requiresPython == "" {
		requiresPython = ">=3.11"
	}
	wheel, _ := json.Marshal(filepath.ToSlash(peWheel(t, wheels, "member_dep", "", nil, nil)))
	peWrite(t, filepath.Join(plugin, "pyproject.toml"), "[project]\nname=\""+name+"\"\nversion=\"1\"\nrequires-python=\""+requiresPython+"\"\n"+
		"dependencies=[\"member-dep==1.0\"]\n[tool.uv]\npackage=false\n[tool.uv.sources]\nmember-dep = { path = "+string(wheel)+" }\n")
}

// Two plugins pinning one local project to different path sources, a union with no solution
func svConflictMembers(t *testing.T, homeDir string) (string, string) {
	t.Helper()
	var plugins []string
	for _, row := range [][2]string{{"plug-a", "1.0.0"}, {"plug-b", "2.0.0"}} {
		shared := filepath.Join(homeDir, "sidecars", "sharedlib-v"+row[1][:1])
		peWrite(t, filepath.Join(shared, "pyproject.toml"), "[project]\nname = \"sharedlib\"\nversion = \""+row[1]+"\"\nrequires-python = \">=3.11\"\n")
		plugin := filepath.Join(homeDir, "plugins", row[0])
		peWrite(t, filepath.Join(plugin, "plugin.yaml"), "name: "+row[0]+"\n")
		peWrite(t, filepath.Join(plugin, "pyproject.toml"), "[project]\nname = \""+row[0]+"\"\nversion = \"0.1.0\"\nrequires-python = \">=3.11\"\n"+
			"dependencies = [\"sharedlib=="+row[1]+"\"]\n\n[tool.uv.sources]\nsharedlib = { path = \""+filepath.ToSlash(shared)+"\" }\n")
		plugins = append(plugins, plugin)
	}
	return plugins[0], plugins[1]
}

// Fake core with a real lock, a temp home with nothing enabled, and the real toolchain
func svAdmissionEnv(t *testing.T) (string, string) {
	t.Helper()
	tmp := isolateHome(t)
	home := filepath.Join(tmp, ".icebath")
	core := filepath.Join(tmp, "core")
	peWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname = \"fake-core\"\nversion = \"0.1.0\"\nrequires-python = \">=3.11\"\ndependencies = []\n")
	uv, python := peUV(t), peHostPython(t)
	env := peIsolatedEnv(t, tmp)
	env["UV_CACHE_DIR"] = filepath.Join(tmp, "uv-cache")
	peRun(t, []string{uv, "lock", "--python", python}, core, env)
	svWriteEnabled(t, home, []string{}, "")
	peSwap(t, &repoRoot, func() string { return core })
	t.Setenv(EnvDisableLazyInstalls, "")
	t.Setenv(EnvRuntimeDir, filepath.Join(tmp, "tools"))
	peToolchain(t, uv, python)
	wsHost(t, "")
	return tmp, home
}

// Parsed YAML document of a config file
func svConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(peRead(t, path)), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// Recorded venv fact of the runtime facts
func svVenvFact(t *testing.T) map[string]any {
	t.Helper()
	facts, err := NewFacts(RuntimeFactsPath(context.Background()), true)
	if err != nil {
		t.Fatal(err)
	}
	fact, _ := facts.Get("venv")
	return fact
}

// Generated workspace pyproject text behind the recorded venv
func svWorkspaceManifest(t *testing.T) string {
	t.Helper()
	return peRead(t, filepath.Join(filepath.Dir(pyStr(svVenvFact(t)["resolved_lock"])), "pyproject.toml"))
}

// Selected dependency environment of the core
func svSelected(t *testing.T, core string) string {
	t.Helper()
	selected, err := SelectedVenv(context.Background(), core)
	if err != nil {
		t.Fatal(err)
	}
	return selected
}

// Reports the core venv current for the recorded selection
func svCurrent(t *testing.T, core string) bool {
	t.Helper()
	current, err := VenvIsCurrent(context.Background(), VenvIsCurrentOptions{ProjectRoot: core})
	if err != nil {
		t.Fatal(err)
	}
	return current
}

// Strings of a decoded YAML list
func svList(v any) []string {
	out := []string{}
	items, _ := v.([]any)
	for _, item := range items {
		out = append(out, pyStr(item))
	}
	return out
}

// Value at a path of nested YAML mappings
func svAt(doc any, path ...string) any {
	for _, key := range path {
		m, ok := doc.(map[string]any)
		if !ok {
			return nil
		}
		doc = m[key]
	}
	return doc
}

func TestEvictionUpdateSyncDisablesPluginExcludedByRequiresPython(t *testing.T) {
	tmp, home := svAdmissionEnv(t)
	ctx := context.Background()
	core := filepath.Join(tmp, "core")
	version := peRun(t, []string{peHostPython(t), "-c", "import sys; print('%d.%d' % sys.version_info[:2])"}, tmp, map[string]string{})
	wheels := filepath.Join(tmp, "wheels")
	svInstallingMember(t, filepath.Join(home, "plugins", "fits"), "fits", wheels, ">=3.11")
	svInstallingMember(t, filepath.Join(home, "plugins", "too-old"), "too-old", wheels, "<"+version)
	peWrite(t, filepath.Join(home, "config.yaml"), "# operator note\nplugins:\n  enabled: [fits, too-old]\n")
	profile := filepath.Join(home, "profiles", "work")
	svWriteEnabled(t, profile, []string{}, "too-old")
	peWrite(t, filepath.Join(profile, "plugins", "too-old", "pyproject.toml"),
		strings.Replace(peRead(t, filepath.Join(home, "plugins", "too-old", "pyproject.toml")), "name=\"too-old\"", "name=\"too-old-work\"", -1))

	err := syncVenv(ctx, SyncVenvOptions{Explicit: true})
	var ie *InstallError
	if !errors.As(err, &ie) || !strings.Contains(err.Error(), "Python requirement") {
		t.Fatalf("ordinary sync %v", err)
	}
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(svSelected(t, core)); err != nil || !info.IsDir() {
		t.Fatalf("selected venv %v", err)
	}
	if manifest := svWorkspaceManifest(t); !strings.Contains(manifest, "fits") || strings.Contains(manifest, "too-old") {
		t.Fatalf("workspace %s", manifest)
	}
	text := peRead(t, filepath.Join(home, "config.yaml"))
	root := svConfig(t, filepath.Join(home, "config.yaml"))
	if !strings.Contains(text, "# operator note") || !reflect.DeepEqual(svList(svAt(root, "plugins", "enabled")), []string{"fits"}) ||
		!slicesContain(svList(svAt(root, "plugins", "disabled")), "too-old") {
		t.Fatalf("root config %s", text)
	}
	work := svConfig(t, filepath.Join(profile, "config.yaml"))
	if svAt(work, "memory", "provider") != "too-old" || !reflect.DeepEqual(svList(svAt(work, "plugins", "disabled")), []string{"too-old"}) {
		t.Fatalf("work config %v", work)
	}
	warnings, _ := json.Marshal(LatestReceipt(ctx)["warnings"])
	if !strings.Contains(string(warnings), "too-old") || !strings.Contains(string(warnings), "icebath plugins enable too-old") {
		t.Fatalf("receipt warnings %s", warnings)
	}
	if !svCurrent(t, core) {
		t.Fatal("venv not current after the update")
	}

	for _, plugin := range []string{filepath.Join(home, "plugins", "too-old"), filepath.Join(profile, "plugins", "too-old")} {
		wsEdit(t, filepath.Join(plugin, "pyproject.toml"), func(s string) string {
			return strings.ReplaceAll(s, "\"<"+version+"\"", "\">=3.11\"")
		})
	}
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
		t.Fatal(err)
	}
	plugins, _ := svAt(svConfig(t, filepath.Join(home, "config.yaml")), "plugins").(map[string]any)
	if _, recorded := plugins["disabled_by_update"]; !reflect.DeepEqual(svList(plugins["enabled"]), []string{"fits", "too-old"}) || len(svList(plugins["disabled"])) != 0 || recorded {
		t.Fatalf("root plugins %v", plugins)
	}
	work = svConfig(t, filepath.Join(profile, "config.yaml"))
	workPlugins, _ := svAt(work, "plugins").(map[string]any)
	if _, recorded := workPlugins["disabled_by_update"]; svAt(work, "memory", "provider") != "too-old" || len(svList(workPlugins["enabled"])) != 0 ||
		len(svList(workPlugins["disabled"])) != 0 || recorded {
		t.Fatalf("work config %v", work)
	}
	if !strings.Contains(svWorkspaceManifest(t), "too-old") || !svCurrent(t, core) {
		t.Fatal("rejoined plugin missing from a current workspace")
	}
}

// Reports whether list holds item
func slicesContain(list []string, item string) bool {
	for _, v := range list {
		if v == item {
			return true
		}
	}
	return false
}
