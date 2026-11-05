package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Strict runtime facts venv record of the core
func evRecorded(t *testing.T) map[string]any {
	t.Helper()
	facts, err := NewFacts(RuntimeFactsPath(context.Background()), true)
	if err != nil {
		t.Fatal(err)
	}
	fact, _ := facts.Get("venv")
	return fact
}

// Reports the core venv current through the engine probe
func evCurrent(t *testing.T, core string) bool {
	t.Helper()
	current, err := venvIsCurrent(context.Background(), nil, nil, core)
	if err != nil {
		t.Fatal(err)
	}
	return current
}

// Plugins table of a home config
func evPlugins(t *testing.T, homeDir string) map[string]any {
	t.Helper()
	plugins, _ := svConfig(t, filepath.Join(homeDir, "config.yaml"))["plugins"].(map[string]any)
	return plugins
}

// Fails unless err is a Python ValueError naming fragment
func evWantValueError(t *testing.T, err error, fragment string) {
	t.Helper()
	pubWantValueError(t, err, fragment)
}

// Captures slog output until the test ends
func evCaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// Base names of the plugin member identities a prepare call builds
func evNames(dirs []MemberDir) []string {
	names := []string{}
	for _, dir := range dirs {
		names = append(names, filepath.Base(dir.Identity))
	}
	return names
}

func TestEvictionMalformedSecondaryCannotEvictRecordedMember(t *testing.T) {
	tmp, homeA := svAdmissionEnv(t)
	ctx := context.Background()
	core := filepath.Join(tmp, "core")
	logs := evCaptureLog(t)
	profile := filepath.Join(homeA, "profiles", "work")
	svWriteEnabled(t, profile, []string{"profile-dep"}, "")
	svInstallingMember(t, filepath.Join(profile, "plugins", "profile-dep"), "profile-dep", filepath.Join(tmp, "wheels"), "")
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	recorded := evRecorded(t)
	selected := svSelected(t, core)
	if pyStr(recorded["stamp"]) == "" || !isDirectory(selected) || !strings.Contains(svWorkspaceManifest(t), "profile-dep") {
		t.Fatalf("recorded %v", recorded)
	}
	bad := filepath.Join(profile, "config.yaml")
	peWrite(t, bad, "plugins: [broken]\n")
	svInstallingMember(t, filepath.Join(homeA, "plugins", "new-dep"), "new-dep", filepath.Join(tmp, "wheels"), "")
	svWriteEnabled(t, homeA, []string{"new-dep"}, "")
	if evCurrent(t, core) {
		t.Fatal("a broken secondary reads as current")
	}
	if !strings.Contains(logs.String(), bad) {
		t.Fatalf("passive inspection did not report %s: %s", bad, logs.String())
	}
	evWantValueError(t, syncVenv(ctx, SyncVenvOptions{Explicit: true}), "config.yaml")
	if !reflect.DeepEqual(evRecorded(t), recorded) || svSelected(t, core) != selected {
		t.Fatal("refused sync changed the recorded graph")
	}
	evWantValueError(t, syncVenv(ctx, SyncVenvOptions{Explicit: true, Plugins: &Members{Dirs: []MemberDir{}}}), "config.yaml")
	if !reflect.DeepEqual(evRecorded(t), recorded) {
		t.Fatal("a precomputed member list bypassed the broken profile")
	}

	homeB := filepath.Join(tmp, "home-b")
	svWriteEnabled(t, homeB, []string{}, "")
	t.Setenv("ICEBATH_HOME", homeB)
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	if !isDirectory(svSelected(t, core)) || evRecorded(t)["stamp"] == recorded["stamp"] {
		t.Fatal("home b did not get its own selection")
	}

	t.Setenv("ICEBATH_HOME", homeA)
	evWantValueError(t, syncVenv(ctx, SyncVenvOptions{Explicit: true}), "config.yaml")
	if svSelected(t, core) != selected {
		t.Fatal("refused sync moved the selection")
	}
	svWriteEnabled(t, profile, []string{"profile-dep"}, "")
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	restored := evRecorded(t)
	if !isDirectory(svSelected(t, core)) || restored["stamp"] == recorded["stamp"] {
		t.Fatalf("restored %v", restored)
	}
	if text := peRead(t, filepath.Join(filepath.Dir(pyStr(restored["resolved_lock"])), "pyproject.toml")); !strings.Contains(text, "profile-dep") {
		t.Fatalf("workspace %s", text)
	}
}

func TestEvictionPluginTheUserDisablesAfterAnUpdateStaysDisabled(t *testing.T) {
	_, homeDir := svAdmissionEnv(t)
	ctx := context.Background()
	svConflictMembers(t, homeDir)
	svWriteEnabled(t, homeDir, []string{"plug-a", "plug-b"}, "")
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
		t.Fatal(err)
	}
	if record, _ := evPlugins(t, homeDir)["disabled_by_update"].(map[string]any); record["plug-b"] == nil {
		t.Fatalf("plugins %v", evPlugins(t, homeDir))
	}
	err := syncVenv(ctx, SyncVenvOptions{Explicit: true, Plugins: &Selection{Data: map[string]any{
		"home": resolvePath(homeDir), "enabled": pubList("plug-a"), "disabled": pubList("plug-b"), "settled": pubList("plug-b"),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := svConfig(t, filepath.Join(homeDir, "config.yaml"))
	cfg["plugins"].(map[string]any)["enabled"] = []any{}
	data, _ := yaml.Marshal(cfg)
	peWrite(t, filepath.Join(homeDir, "config.yaml"), string(data))
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
		t.Fatal(err)
	}
	plugins := evPlugins(t, homeDir)
	if _, recorded := plugins["disabled_by_update"]; !reflect.DeepEqual(svList(plugins["disabled"]), []string{"plug-b"}) || recorded {
		t.Fatalf("plugins %v", plugins)
	}
}

func TestEvictionProviderOnlyRecordIsRetriedOnlyWhileItIsStillTheProvider(t *testing.T) {
	_, homeDir := svAdmissionEnv(t)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(homeDir, "plugins", "parked"), 0o777); err != nil {
		t.Fatal(err)
	}
	record := "plugins:\n  disabled: [parked]\n  disabled_by_update:\n    parked: {enabled: false, reason: r}\n"
	peWrite(t, filepath.Join(homeDir, "config.yaml"), record+"memory:\n  provider: parked\n")
	evictions, err := recordedEvictions(ctx)
	if err != nil || len(evictions) != 1 || evictions[0].Entry.Name != "parked" {
		t.Fatalf("evictions %+v %v", evictions, err)
	}
	peWrite(t, filepath.Join(homeDir, "config.yaml"), record+"memory:\n  provider: other\n")
	if evictions, err := recordedEvictions(ctx); err != nil || len(evictions) != 0 {
		t.Fatalf("evictions %+v %v", evictions, err)
	}
}

func TestEvictionRecordedPluginIsRetriedOnlyWhenChangedAndNeverDisplacesAnEnabledOne(t *testing.T) {
	tmp, homeDir := svAdmissionEnv(t)
	ctx := context.Background()
	plugA, plugB := svConflictMembers(t, homeDir)
	svWriteEnabled(t, homeDir, []string{"plug-a", "plug-b"}, "")
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
		t.Fatal(err)
	}
	var builds [][]string
	offline := false
	v := pubSwapVenv(t, &pubVenv{})
	v.prepare = func(ctx context.Context, extras []string, opts VenvApplyOptions) (*VenvGeneration, error) {
		names := evNames(opts.PluginDirs)
		builds = append(builds, names)
		if offline && reflect.DeepEqual(names, []string{"plug-a"}) {
			return nil, NewInstallError("plug-a", "network unreachable", "")
		}
		return v.Venv.Prepare(ctx, extras, opts)
	}
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
		t.Fatal(err)
	}
	for _, names := range builds {
		if slicesContain(names, "plug-b") {
			t.Fatalf("unchanged recorded plugin rebuilt %v", builds)
		}
	}
	manifest := filepath.Join(plugB, "pyproject.toml")
	peWrite(t, manifest, peRead(t, manifest)+"# 0.1.1, still sharedlib 2\n")
	offline = true
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
		t.Fatal(err)
	}
	plugins := evPlugins(t, homeDir)
	if slicesContain(svList(plugins["enabled"]), "plug-b") || !slicesContain(svList(plugins["disabled"]), "plug-b") {
		t.Fatalf("plugins %v", plugins)
	}
	offline = false
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
		t.Fatal(err)
	}
	plugins = evPlugins(t, homeDir)
	if !reflect.DeepEqual(svList(plugins["enabled"]), []string{"plug-a"}) || !reflect.DeepEqual(svList(plugins["disabled"]), []string{"plug-b"}) {
		t.Fatalf("plugins %v", plugins)
	}
	if !evCurrent(t, filepath.Join(tmp, "core")) {
		t.Fatal("venv not current")
	}
	manifest = filepath.Join(plugA, "pyproject.toml")
	peWrite(t, manifest, strings.ReplaceAll(strings.ReplaceAll(peRead(t, manifest), "\"sharedlib==1.0.0\"", "\"sharedlib>=1\""), "sharedlib-v1", "sharedlib-v2"))
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
		t.Fatal(err)
	}
	plugins = evPlugins(t, homeDir)
	if !reflect.DeepEqual(svList(plugins["enabled"]), []string{"plug-a", "plug-b"}) || len(svList(plugins["disabled"])) != 0 {
		t.Fatalf("plugins %v", plugins)
	}
}

func TestEvictionUpdateSyncDisablesLaterPluginOfUnresolvableUnion(t *testing.T) {
	tmp, homeDir := svAdmissionEnv(t)
	svConflictMembers(t, homeDir)
	svWriteEnabled(t, homeDir, []string{"plug-a", "plug-b"}, "")
	if err := syncVenv(context.Background(), SyncVenvOptions{Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
		t.Fatal(err)
	}
	plugins := evPlugins(t, homeDir)
	if !reflect.DeepEqual(svList(plugins["disabled"]), []string{"plug-b"}) || !reflect.DeepEqual(svList(plugins["enabled"]), []string{"plug-a"}) {
		t.Fatalf("plugins %v", plugins)
	}
	if !evCurrent(t, filepath.Join(tmp, "core")) {
		t.Fatal("venv not current")
	}
}

func TestEvictionUpdateSyncSurvivesUnreadableSecondaryProfile(t *testing.T) {
	tmp, homeDir := svAdmissionEnv(t)
	ctx := context.Background()
	core := filepath.Join(tmp, "core")
	svInstallingMember(t, filepath.Join(homeDir, "plugins", "primary-dep"), "primary-dep", filepath.Join(tmp, "wheels"), "")
	svWriteEnabled(t, homeDir, []string{"primary-dep"}, "")
	broken := filepath.Join(homeDir, "profiles", "work", "config.yaml")
	peWrite(t, broken, "plugins: [broken]\n")
	evWantValueError(t, syncVenv(ctx, SyncVenvOptions{Explicit: true}), "config.yaml")
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(svWorkspaceManifest(t), "primary-dep") || peRead(t, broken) != "plugins: [broken]\n" {
		t.Fatal("primary member missing or broken config rewritten")
	}
	warnings, _ := json.Marshal(LatestReceipt(ctx)["warnings"])
	if !strings.Contains(string(warnings), strings.ReplaceAll(filepath.Dir(broken), `\`, `\\`)) {
		t.Fatalf("warnings %s", warnings)
	}
	if !evCurrent(t, core) {
		t.Fatal("venv not current")
	}
}

func TestEvictionUpdateSyncRetriesAFetchFailureOnceBeforeDisabling(t *testing.T) {
	var trials []string
	tmp, homeDir := svAdmissionEnv(t)
	v := pubSwapVenv(t, &pubVenv{})
	v.prepare = func(ctx context.Context, extras []string, opts VenvApplyOptions) (*VenvGeneration, error) {
		if len(opts.PluginDirs) > 0 {
			trials = append(trials, filepath.Base(opts.PluginDirs[len(opts.PluginDirs)-1].Identity))
		}
		return v.Venv.Prepare(ctx, extras, opts)
	}
	t.Setenv("UV_HTTP_RETRIES", "0")
	broken := filepath.Join(tmp, "broken-lib")
	peWrite(t, filepath.Join(broken, "pyproject.toml"), "[project]\nname=\"broken-lib\"\ndynamic=[\"version\"]\n"+
		"[build-system]\nrequires=[]\nbuild-backend=\"backend\"\nbackend-path=[\".\"]\n")
	peWrite(t, filepath.Join(broken, "backend.py"), "def get_requires_for_build_wheel(config=None): return []\n"+
		"def prepare_metadata_for_build_wheel(directory, config=None): raise SystemExit('no build')\n"+
		"def build_wheel(directory, config=None, metadata=None): raise SystemExit('no build')\n")
	for _, row := range [][3]string{{"wont-build", "broken-lib", "{ path = \"" + filepath.ToSlash(broken) + "\" }"},
		{"offline-dep", "gone", "{ url = \"https://127.0.0.1:9/gone-1.0-py3-none-any.whl\" }"}} {
		peWrite(t, filepath.Join(homeDir, "plugins", row[0], "pyproject.toml"), "[project]\nname=\""+row[0]+"\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n"+
			"dependencies=[\""+row[1]+"\"]\n[tool.uv]\npackage=false\n[tool.uv.sources]\n"+row[1]+" = "+row[2]+"\n")
	}
	svWriteEnabled(t, homeDir, []string{"wont-build", "offline-dep"}, "")
	ctx := context.Background()
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
		t.Fatal(err)
	}
	if disabled := svList(evPlugins(t, homeDir)["disabled"]); !reflect.DeepEqual(disabled, []string{"wont-build", "offline-dep"}) {
		t.Fatalf("disabled %v", disabled)
	}
	if len(trials) < 1 || !reflect.DeepEqual(trials[1:], []string{"wont-build", "offline-dep", "offline-dep"}) {
		t.Fatalf("trials %v", trials)
	}
	if warnings, _ := json.Marshal(LatestReceipt(ctx)["warnings"]); !strings.Contains(string(warnings), "could not be prepared, twice") {
		t.Fatalf("warnings %s", warnings)
	}
	if !evCurrent(t, filepath.Join(tmp, "core")) {
		t.Fatal("venv not current")
	}
}
