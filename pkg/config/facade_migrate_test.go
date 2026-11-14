package config

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Facade hooks over the migration and first stamp test doubles, the caches cold and parse warnings silenced
func registerMigrationFacade(t *testing.T) {
	t.Helper()
	t.Setenv(EnvManagedDir, filepath.Join(t.TempDir(), "no-managed"))
	InvalidateManagedCache()
	quietParseFailures(t)
	ResetCache()
	t.Cleanup(ResetCache)
	prev, prevCollab := hooks(), migrateCollaborators()
	RegisterConfigFacade(FacadeHooks{
		Migration: func(h string) MigrationSources { return migrationTestSources(h, io.Discard) },
		Stamp: func() StampHooks {
			return StampHooks{
				WorkerSnapshotActive: func() bool { return WorkerConfigSnapshot() != nil },
				RecordCoreUse:        func(string, map[string]any, int) bool { return false },
				AdoptAgentBrowserEnv: func(h string, cfg map[string]any) []string {
					return AdoptAgentBrowserEnv(h, cfg, testBrowserSources())
				},
			}
		},
	})
	RegisterMigrateCollaborators(MigrateCollaborators{Personality: migrationTestSources("", io.Discard).Personality})
	t.Cleanup(func() {
		RegisterConfigFacade(prev)
		RegisterMigrateCollaborators(prevCollab)
	})
}

func TestMigrationFacadeSourcesCarryTheFacadeReadersAndWriters(t *testing.T) {
	h, path, _ := caseHome(t, "_config_version: 30\nagent:\n  max_turns: 60\n", "")
	src := MigrationFacadeSources(t.Context(), h)
	if src.Persist == nil || src.ReadRaw == nil || src.GetEnvValue == nil || src.GetEnvValuePreferDotenv == nil ||
		src.SaveEnvValue == nil || src.RemoveEnvValue == nil || src.Out != FacadeStdout {
		t.Fatalf("facade collaborators missing: %+v", src)
	}
	raw, err := src.ReadRaw(h)
	if err != nil || !pyEq(Tree(raw).Get("agent.max_turns"), 60) {
		t.Fatalf("raw read %v %v", raw, err)
	}
	raw["_config_version"] = 31
	if err := src.Persist(h, raw); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "_config_version: 31") {
		t.Fatalf("persisted %q", data)
	}
}

// Runs migrate_config non interactively for one home and returns its results and stdout
func migrateConfigHome(t *testing.T, h string, quiet bool) (*MigrationResults, string) {
	t.Helper()
	registerMigrationFacade(t)
	out, _ := captureFacade(t)
	res, err := MigrateConfig(context.Background(), h, false, quiet)
	if err != nil {
		t.Fatal(err)
	}
	return res, out.String()
}

func TestFullMigrationStampsTheCurrentVersion(t *testing.T) {
	h := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 42, "platform_toolsets": map[string]any{"cli": []any{"file", "terminal"}}}, "")
	migrateConfigHome(t, h, true)
	raw := rawMigrated(t, h)
	if !slices.Contains(AsStrings(AsMap(raw["platform_toolsets"])["cli"]), "connections") || !pyEq(raw["_config_version"], Defaults()["_config_version"]) {
		t.Fatalf("raw %v", raw)
	}
}

func TestUpdateKeepsUserValuesOfAnUnversionedConfigAndMigratesLegacyKeys(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	writeMigrationHome(t, h, map[string]any{}, "")
	t.Setenv("ICEBATH_HOME", h)
	registerMigrationFacade(t)
	captureFacade(t)
	os.WriteFile(home.ConfigPath(h), []byte("compression:\n  summary_model: google/gemini-3-flash-preview\n"), 0o644)
	plugin := filepath.Join(h, "plugins", "notes-helper")
	os.MkdirAll(plugin, 0o755)
	os.WriteFile(filepath.Join(plugin, "plugin.yaml"), []byte("name: notes-helper\nversion: 0.1.0\n"), 0o644)
	soul := "# Me\n\n## Messaging other agents\nmy own notes\n\n## Prefs\nkeep\n"
	os.WriteFile(filepath.Join(h, "SOUL.md"), []byte(soul), 0o644)
	choices := [][2]string{{"display.personality", "kawaii"}, {"delegation.max_concurrent_children", "3"}, {"delegation.max_iterations", "50"},
		{"display.background_process_notifications", "all"}, {"agent.verify_on_stop", "true"}, {"curator.stale_after_days", "30"},
		{"curator.archive_after_days", "90"}, {"model_catalog.ttl_hours", "24"}}
	for _, kv := range choices {
		if err := SetConfigValue(context.Background(), h, kv[0], kv[1], false, pathCommandSources()); err != nil {
			t.Fatalf("set %s: %v", kv[0], err)
		}
	}
	watched := func(raw map[string]any) map[string]any {
		out := map[string]any{}
		for _, key := range []string{"display.personality", "delegation.max_concurrent_children", "delegation.max_iterations",
			"display.background_process_notifications", "agent.verify_on_stop", "curator.stale_after_days",
			"curator.archive_after_days", "model_catalog.ttl_hours", "plugins.enabled"} {
			out[key], _ = CfgGet(raw, strings.Split(key, ".")...)
		}
		return out
	}
	before := rawMigrated(t, h)
	if _, err := MigrateConfig(context.Background(), h, false, true); err != nil {
		t.Fatal(err)
	}
	after := rawMigrated(t, h)
	if !reflect.DeepEqual(watched(after), watched(before)) {
		t.Fatalf("watched before %v after %v", watched(before), watched(after))
	}
	summary, _ := CfgGet(before, "compression", "summary_model")
	moved, _ := CfgGet(after, "auxiliary", "compression", "model")
	if _, kept := AsMap(after["compression"])["summary_model"]; kept || moved != summary || summary == nil {
		t.Fatalf("summary model %v moved to %v in %v", summary, moved, after)
	}
	if !pyEq(after["_config_version"], Defaults()["_config_version"]) {
		t.Fatalf("stamp %v", after["_config_version"])
	}
	if data, _ := os.ReadFile(filepath.Join(h, "SOUL.md")); string(data) != soul {
		t.Fatalf("SOUL.md rewritten to %q", data)
	}
}

func TestConfigSeededFromTheTemplateReadsAsCurrent(t *testing.T) {
	h, path := facadeHome(t, "")
	data, err := os.ReadFile(filepath.Join(GetProjectRoot(), "cli-config.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, data, 0o600)
	current, latest, err := CheckConfigVersion(context.Background(), h, true)
	if err != nil || current != latest {
		t.Fatalf("template reads %d of %d, %v", current, latest, err)
	}
}

const legacyBrowserEnv = "AGENT_BROWSER_ENGINE=lightpanda\nAGENT_BROWSER_PROFILE=/probe/chrome-profile\nAGENT_BROWSER_HEADED=true\n"

// Launch home holding the legacy browser .env and the config text, no config file when nil
func legacyBrowserHome(t *testing.T, text *string) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "home")
	os.MkdirAll(h, 0o755)
	cleanBrowserEnv(t, h)
	t.Setenv("CAMOFOX_URL", "")
	os.Unsetenv("CAMOFOX_URL")
	os.WriteFile(home.EnvPath(h), []byte(legacyBrowserEnv), 0o644)
	if text != nil {
		os.WriteFile(home.ConfigPath(h), []byte(*text), 0o644)
	}
	registerMigrationFacade(t)
	captureFacade(t)
	return h
}

// Browser engine, headed mode and profile directory the loaded config hands the browser launcher
func browserRuntime(t *testing.T, h string) map[string]any {
	t.Helper()
	ResetCache()
	cfg, err := Load(h, facadeLookup)
	if err != nil {
		t.Fatal(err)
	}
	b := AsMap(cfg["browser"])
	return map[string]any{"engine": b["engine"], "headed": b["headed"], "user_data_dir": b["user_data_dir"]}
}

var keptBrowser = map[string]any{"engine": "lightpanda", "headed": true, "user_data_dir": "/probe/chrome-profile"}

func firstSave(t *testing.T, h string) {
	t.Helper()
	cfg, err := Load(h, facadeLookup)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(context.Background(), h, map[string]any(cfg), SaveOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestASaveBeforeAnyMigrationKeepsTheLegacyBrowserSettings(t *testing.T) {
	unstamped := "{}\n"
	for _, text := range []*string{nil, &unstamped} {
		h := legacyBrowserHome(t, text)
		firstSave(t, h)
		if v := rawMigrated(t, h)["_config_version"]; !pyEq(v, latestConfigVersion()) {
			t.Fatalf("first save stamped %v", v)
		}
		if got := browserRuntime(t, h); !reflect.DeepEqual(got, keptBrowser) {
			t.Fatalf("browser %v", got)
		}
	}
}

func TestExplicitMigrationRecoversAHomeWithoutConfig(t *testing.T) {
	h := legacyBrowserHome(t, nil)
	if _, err := MigrateConfig(context.Background(), h, false, true); err != nil {
		t.Fatal(err)
	}
	if got := browserRuntime(t, h); !reflect.DeepEqual(got, keptBrowser) {
		t.Fatalf("browser %v", got)
	}
	if v := rawMigrated(t, h)["_config_version"]; !pyEq(v, latestConfigVersion()) {
		t.Fatalf("stamp %v", v)
	}
}

func TestTheRetainedEnvIsConvertedOnceNeverOverLaterEdits(t *testing.T) {
	h := legacyBrowserHome(t, nil)
	ctx := context.Background()
	if _, err := MigrateConfig(ctx, h, false, true); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(h, facadeLookup)
	if err != nil {
		t.Fatal(err)
	}
	b := AsMap(cfg["browser"])
	b["engine"], b["headed"], b["user_data_dir"] = "auto", false, "/mine"
	if err := SaveConfig(ctx, h, map[string]any(cfg), SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateConfig(ctx, h, false, true); err != nil {
		t.Fatal(err)
	}
	firstSave(t, h)
	if got := browserRuntime(t, h); !reflect.DeepEqual(got, map[string]any{"engine": "auto", "headed": false, "user_data_dir": "/mine"}) {
		t.Fatalf("browser %v", got)
	}
}
