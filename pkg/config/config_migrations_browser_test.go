package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Clears the AGENT_BROWSER process env and points the launch home away from the test homes
func cleanBrowserEnv(t *testing.T, launch string) {
	t.Helper()
	for _, pair := range agentBrowserEnvKeys {
		t.Setenv(pair[0], "")
		os.Unsetenv(pair[0])
	}
	t.Setenv("ICEBATH_HOME", launch)
	deferredBrowserNotices = nil
	testPMStoreRoots = nil
}

func browserOf(t *testing.T, h string) map[string]any {
	return AsMap(rawMigrated(t, h)["browser"])
}

func TestQuietMigrationHandsTheBackendOffNoticeToTheCaller(t *testing.T) {
	dir := t.TempDir()
	cleanBrowserEnv(t, filepath.Join(dir, "launch"))
	h := writeMigrationHome(t, filepath.Join(dir, "a"), map[string]any{"_config_version": 51, "browser": map[string]any{"backend": "off"}}, "")
	res, out := migrateFromStamp(t, h, true)
	if out != "" {
		t.Fatalf("quiet pass printed %q", out)
	}
	found := false
	for _, note := range res.ConfigAdded {
		found = found || (strings.Contains(note, "browser_exec is the browser now") && strings.Contains(note, "`browser` toolset"))
	}
	if !found || len(res.Notices) != 1 {
		t.Fatalf("config added %v notices %v", res.ConfigAdded, res.Notices)
	}
}

func TestBackendOffNoticePrintsOnceAndUnquotedOffCounts(t *testing.T) {
	dir := t.TempDir()
	cleanBrowserEnv(t, filepath.Join(dir, "launch"))
	h := filepath.Join(dir, "a")
	os.MkdirAll(h, 0o755)
	os.WriteFile(home.ConfigPath(h), []byte("_config_version: 51\nbrowser:\n  backend: off\n  inactivity_timeout: 60\n"), 0o644)
	res, out := migrateFromStamp(t, h, false)
	if !reflect.DeepEqual(browserOf(t, h), map[string]any{"inactivity_timeout": 60}) {
		t.Fatalf("browser %v", browserOf(t, h))
	}
	if !strings.Contains(out, "browser_exec is the browser now") || !strings.Contains(out, "`browser` toolset in `icebath tools`") {
		t.Fatalf("output %q", out)
	}
	if !strings.Contains(strings.Join(res.ConfigAdded, "\n"), "browser.backend: off") {
		t.Fatalf("config added %v", res.ConfigAdded)
	}
	if v := rawMigrated(t, h)["_config_version"]; !pyEq(v, latestConfigVersion()) {
		t.Fatalf("stamp %v", v)
	}
	if _, out = migrateFromStamp(t, h, false); strings.Contains(out, "browser_exec") {
		t.Fatalf("second pass printed %q", out)
	}
}

func TestARemovedBackendValueIsNamedWithItsReason(t *testing.T) {
	for value, reason := range map[string]string{
		"browserbase":   "a cloud browser is chosen with browser.cloud_provider",
		"Browser-Use":   "browser_exec (Browser Use mode) is the only browser now",
		"agent-browser": "agent-browser left icebath",
		"lynx":          "browser_exec is the only browser now",
	} {
		dir := t.TempDir()
		cleanBrowserEnv(t, filepath.Join(dir, "launch"))
		h := writeMigrationHome(t, filepath.Join(dir, "a"), map[string]any{"_config_version": 51, "browser": map[string]any{"backend": value}}, "")
		res, _ := migrateFromStamp(t, h, true)
		if _, ok := browserOf(t, h)["backend"]; ok {
			t.Fatalf("%s kept", value)
		}
		var note string
		for _, n := range res.ConfigAdded {
			if strings.Contains(n, "browser.backend") {
				note = n
				break
			}
		}
		if !strings.HasPrefix(note, "removed browser.backend: "+value+" — ") || !strings.Contains(note, reason) {
			t.Fatalf("%s note %q", value, note)
		}
	}
}

func TestOtherBackendValuesAreDroppedSilently(t *testing.T) {
	for _, value := range []any{"", "browser-use", "Browser-Use", nil} {
		dir := t.TempDir()
		cleanBrowserEnv(t, filepath.Join(dir, "launch"))
		h := writeMigrationHome(t, filepath.Join(dir, "a"), map[string]any{"_config_version": 51, "browser": map[string]any{"backend": value, "headed": true}}, "")
		_, out := migrateFromStamp(t, h, false)
		if !reflect.DeepEqual(browserOf(t, h), map[string]any{"headed": true}) || strings.Contains(out, "browser_exec") {
			t.Fatalf("%v: browser %v out %q", value, browserOf(t, h), out)
		}
	}
}

func TestUnversionedConfigStillLosesBackend(t *testing.T) {
	dir := t.TempDir()
	cleanBrowserEnv(t, filepath.Join(dir, "launch"))
	h := writeMigrationHome(t, filepath.Join(dir, "a"), map[string]any{"browser": map[string]any{"backend": "off"}}, "")
	migrateFromStamp(t, h, true)
	if _, ok := browserOf(t, h)["backend"]; ok {
		t.Fatal("backend kept")
	}
}

func TestDotenvSettingsMoveToUnsetKeysOnly(t *testing.T) {
	dir := t.TempDir()
	cleanBrowserEnv(t, filepath.Join(dir, "launch"))
	env := "AGENT_BROWSER_EXECUTABLE_PATH=/opt/chrome/chrome\nAGENT_BROWSER_PROFILE=/data/profile\nAGENT_BROWSER_ARGS=--no-sandbox, --lang=de\n" +
		"AGENT_BROWSER_HEADED=true\nAGENT_BROWSER_ENGINE=lightpanda\n"
	h := writeMigrationHome(t, filepath.Join(dir, "a"), map[string]any{"_config_version": 51, "browser": map[string]any{"user_data_dir": "/mine"}}, env)
	res, _ := migrateFromStamp(t, h, true)
	want := map[string]any{"executable_path": "/opt/chrome/chrome", "user_data_dir": "/mine", "chromium_args": []any{"--no-sandbox", "--lang=de"},
		"headed": true, "engine": "lightpanda"}
	if !reflect.DeepEqual(browserOf(t, h), want) {
		t.Fatalf("browser %v", browserOf(t, h))
	}
	if !strings.Contains(strings.Join(res.ConfigAdded, "\n"), "AGENT_BROWSER_ARGS → browser.chromium_args") {
		t.Fatalf("config added %v", res.ConfigAdded)
	}
	if data, _ := os.ReadFile(home.EnvPath(h)); string(data) != env {
		t.Fatalf("env rewritten %q", data)
	}
}

func TestEnvWinsOverTheDefaultsCoreWrote(t *testing.T) {
	dir := t.TempDir()
	cleanBrowserEnv(t, filepath.Join(dir, "launch"))
	env := "AGENT_BROWSER_ENGINE=lightpanda\nAGENT_BROWSER_HEADED=true\n"
	defaults := writeMigrationHome(t, filepath.Join(dir, "a"), map[string]any{"_config_version": 51, "browser": map[string]any{"engine": "auto", "headed": false}}, env)
	chosen := writeMigrationHome(t, filepath.Join(dir, "b"), map[string]any{"_config_version": 51, "browser": map[string]any{"engine": "chrome", "headed": "false"}}, env)
	migrateFromStamp(t, defaults, true)
	migrateFromStamp(t, chosen, true)
	if !reflect.DeepEqual(browserOf(t, defaults), map[string]any{"engine": "lightpanda", "headed": true}) ||
		!reflect.DeepEqual(browserOf(t, chosen), map[string]any{"engine": "chrome", "headed": true}) {
		t.Fatalf("defaults %v chosen %v", browserOf(t, defaults), browserOf(t, chosen))
	}
}

func TestRelativeProfileKeepsTheDirectoryCoreUsedAndNamesThePin(t *testing.T) {
	dir := t.TempDir()
	cleanBrowserEnv(t, filepath.Join(dir, "launch"))
	cwd := filepath.Join(dir, "work")
	os.MkdirAll(cwd, 0o755)
	t.Chdir(cwd)
	h := writeMigrationHome(t, filepath.Join(dir, "a"), map[string]any{"_config_version": 51}, "AGENT_BROWSER_PROFILE=prof/rel\n")
	res, _ := migrateFromStamp(t, h, true)
	pinned := filepath.Join(cwd, "prof", "rel")
	if browserOf(t, h)["user_data_dir"] != pinned {
		t.Fatalf("browser %v", browserOf(t, h))
	}
	found := false
	for _, note := range res.ConfigAdded {
		found = found || (strings.Contains(note, pinned) && strings.Contains(note, "prof/rel"))
	}
	if !found || len(res.Notices) != 1 || !strings.HasPrefix(res.Notices[0], "Pinned browser.user_data_dir to ") {
		t.Fatalf("config added %v notices %v", res.ConfigAdded, res.Notices)
	}
}

func TestFalsyHeadedAndUnknownEngineCarryNothing(t *testing.T) {
	dir := t.TempDir()
	cleanBrowserEnv(t, filepath.Join(dir, "launch"))
	h := writeMigrationHome(t, filepath.Join(dir, "a"), map[string]any{"_config_version": 51}, "AGENT_BROWSER_HEADED=0\nAGENT_BROWSER_ENGINE=webkit\n")
	migrateFromStamp(t, h, true)
	if PyTruthy(rawMigrated(t, h)["browser"]) {
		t.Fatalf("browser %v", rawMigrated(t, h)["browser"])
	}
}

func TestPMStoreChromiumPathIsNotPinned(t *testing.T) {
	dir := t.TempDir()
	h := filepath.Join(dir, "a")
	cleanBrowserEnv(t, h)
	store := filepath.Join(dir, "tools", "store")
	testPMStoreRoots = []string{store, filepath.Join(dir, "tools", "writable")}
	writeMigrationHome(t, h, map[string]any{"_config_version": 51}, "")
	t.Setenv("AGENT_BROWSER_EXECUTABLE_PATH", filepath.Join(store, "chromium-1208", "chrome-linux64", "chrome"))
	migrateFromStamp(t, h, true)
	if _, ok := browserOf(t, h)["executable_path"]; ok {
		t.Fatalf("browser %v", browserOf(t, h))
	}
}

func TestProfilesAreIslandsABA(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	cleanBrowserEnv(t, a)
	writeMigrationHome(t, a, map[string]any{"_config_version": 51, "browser": map[string]any{"backend": "off"}}, "AGENT_BROWSER_EXECUTABLE_PATH=/opt/a/chrome\n")
	b := writeMigrationHome(t, filepath.Join(dir, "b"), map[string]any{"_config_version": 51, "browser": map[string]any{"inactivity_timeout": 300}}, "")
	t.Setenv("AGENT_BROWSER_HEADED", "1")
	_, outA := migrateFromStamp(t, a, false)
	_, outB := migrateFromStamp(t, b, false)
	_, outA2 := migrateFromStamp(t, a, false)
	if !reflect.DeepEqual(browserOf(t, a), map[string]any{"executable_path": "/opt/a/chrome", "headed": true}) || !strings.Contains(outA, "browser_exec is the browser now") {
		t.Fatalf("a %v out %q", browserOf(t, a), outA)
	}
	if !reflect.DeepEqual(browserOf(t, b), map[string]any{"inactivity_timeout": 300}) || strings.Contains(outB, "browser_exec") || strings.Contains(outB, "agent-browser") {
		t.Fatalf("b %v out %q", browserOf(t, b), outB)
	}
	if strings.Contains(outA2, "browser_exec") || strings.Contains(outA2, "agent-browser") {
		t.Fatalf("second a pass printed %q", outA2)
	}
}

func TestExtensionControlBlockIsDropped(t *testing.T) {
	for _, stamped := range []bool{true, false} {
		dir := t.TempDir()
		cleanBrowserEnv(t, filepath.Join(dir, "launch"))
		cfg := map[string]any{"browser": map[string]any{"extension_control": map[string]any{"enabled": true, "developer_mode": false}, "headed": true}}
		if stamped {
			cfg["_config_version"] = 51
		}
		h := writeMigrationHome(t, filepath.Join(dir, "a"), cfg, "")
		res, _ := migrateFromStamp(t, h, true)
		if !reflect.DeepEqual(browserOf(t, h), map[string]any{"headed": true}) || !strings.Contains(strings.Join(res.ConfigAdded, "\n"), "removed browser.extension_control") {
			t.Fatalf("stamped %v browser %v added %v", stamped, browserOf(t, h), res.ConfigAdded)
		}
	}
}

func TestAdoptMovesEnvOnceAndDefersThePinnedNotice(t *testing.T) {
	dir := t.TempDir()
	cleanBrowserEnv(t, filepath.Join(dir, "launch"))
	cwd := filepath.Join(dir, "work")
	os.MkdirAll(cwd, 0o755)
	t.Chdir(cwd)
	h := writeMigrationHome(t, filepath.Join(dir, "a"), map[string]any{"browser": map[string]any{"headed": true}}, "AGENT_BROWSER_PROFILE=prof/rel\nAGENT_BROWSER_ENGINE=chrome\n")
	if !AgentBrowserEnvPending(h, testBrowserSources()) {
		t.Fatal("pending should report the unmoved env")
	}
	cfg := map[string]any{"_config_version": 52}
	moved := AdoptAgentBrowserEnv(h, cfg, testBrowserSources())
	want := []string{"AGENT_BROWSER_PROFILE → browser.user_data_dir", "AGENT_BROWSER_ENGINE → browser.engine"}
	if !reflect.DeepEqual(moved, want) || AsMap(cfg["browser"])["user_data_dir"] != filepath.Join(cwd, "prof", "rel") {
		t.Fatalf("moved %v cfg %v", moved, cfg)
	}
	if len(deferredBrowserNotices) != 1 || !strings.HasPrefix(deferredBrowserNotices[0], "Pinned browser.user_data_dir to ") || !strings.HasSuffix(deferredBrowserNotices[0], ".") {
		t.Fatalf("notices %v", deferredBrowserNotices)
	}
	untouched := map[string]any{}
	if AdoptAgentBrowserEnv(filepath.Join(dir, "empty"), untouched, testBrowserSources()) != nil || len(untouched) != 0 {
		t.Fatalf("a home without env changed %v", untouched)
	}
}
