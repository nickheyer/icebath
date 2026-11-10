package config

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"gopkg.in/yaml.v3"
)

// Notices the browser sources deferred during a test
var deferredBrowserNotices []string

// Store roots the pm double reports
var testPMStoreRoots []string

// Saves a migrated config with default stripping over the explicit paths of the file on disk
func testPersistMigration(h string, cfg map[string]any) error {
	raw, err := ReadUser(h)
	if err != nil {
		return err
	}
	canon, err := Canonicalize(Clone(cfg).(map[string]any))
	if err != nil {
		return err
	}
	stripped := StripDefaults(canon, map[string]any(Defaults()), ExplicitPaths(raw))
	return SaveUser(context.Background(), h, stripped)
}

func testDotenv(h string) map[string]string {
	data, err := os.ReadFile(home.EnvPath(h))
	if err != nil {
		return map[string]string{}
	}
	return ParseEnv(string(data))
}

func testBrowserSources() BrowserEnvSources {
	return BrowserEnvSources{
		HomeDotenv:         testDotenv,
		IsLaunchHome:       func(h string) bool { return home.Key(h) == home.Key(os.Getenv("ICEBATH_HOME")) },
		PMStoreRoots:       func() ([]string, error) { return testPMStoreRoots, nil },
		DeferStartupNotice: func(message string) { deferredBrowserNotices = append(deferredBrowserNotices, message) },
	}
}

// Migration collaborators bound to one home, each a test double of the owning slice
func migrationTestSources(h string, out io.Writer) MigrationSources {
	envPath := home.EnvPath(h)
	return MigrationSources{
		Out:     out,
		Persist: testPersistMigration,
		GetEnvValue: func(name string) string {
			if v, ok := os.LookupEnv(name); ok {
				return v
			}
			return testDotenv(h)[name]
		},
		GetEnvValuePreferDotenv: func(name string) string {
			if v := testDotenv(h)[name]; v != "" {
				return v
			}
			return os.Getenv(name)
		},
		SaveEnvValue:   func(name, value string) error { return SetEnvValue(envPath, name, value) },
		RemoveEnvValue: func(name string) (bool, error) { return RemoveEnvValue(envPath, name) },
		CustomProviderEntryToProviderConfig: func(entry any, key string) map[string]any {
			m := entry.(map[string]any)
			out := map[string]any{"name": m["name"]}
			for _, field := range []string{"base_url", "url", "api"} {
				if PyTruthy(m[field]) {
					out["api"] = m[field]
					break
				}
			}
			if v, ok := m["api_key"]; ok {
				out["api_key"] = v
			}
			return out
		},
		Personality: PersonalitySources{
			Normalize:  func(v any) string { s, _ := v.(string); return strings.ToLower(strings.TrimSpace(s)) },
			PromptText: func(v any) string { s, _ := v.(string); return strings.TrimSpace(s) },
			Render:     func(d any) string { return PyStr(d) },
			Available:  func(map[string]any) map[string]any { return map[string]any{"kawaii": "You are kawaii."} },
		},
		LegacyRelayPluginKeys: func(enabled any) []string {
			var out []string
			for _, v := range AsStrings(enabled) {
				if v == "observability/nemo_relay" {
					out = append(out, v)
				}
			}
			return out
		},
		BotProtocolHeading: "## Messaging other agents",
		BotRoster:          func(h string) []BotProfile { return []BotProfile{{Name: "default", Dir: h}} },
		StripLegacyBotProtocol: func(text string) string {
			before, _, _ := strings.Cut(text, "## Messaging other agents")
			return before
		},
		ParseConfigStringList: AsStrings,
		ExplicitToolsetKeys: func() map[string]bool {
			return map[string]bool{"file": true, "terminal": true, "web": true, "browser": true, "memory": true, "skills": true, "todo": true, "connections": true}
		},
		ToolsetAllowedForPlatform: func(key, platform string) bool { return key != "discord" || platform == "discord" },
		ParseEnabledFlag: func(v any, def bool) bool {
			switch x := v.(type) {
			case bool:
				return x
			case int:
				return x != 0
			case string:
				switch strings.ToLower(strings.TrimSpace(x)) {
				case "true", "1", "yes", "on":
					return true
				case "false", "0", "no", "off":
					return false
				}
			}
			return def
		},
		RecordCoreUse: func(string, map[string]any, int) bool { return false },
		Browser:       testBrowserSources(),
	}
}

func writeMigrationHome(t *testing.T, h string, cfg map[string]any, env string) string {
	t.Helper()
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home.ConfigPath(h), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home.EnvPath(h), []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

func rawMigrated(t *testing.T, h string) map[string]any {
	t.Helper()
	raw, err := ReadUser(h)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Runs migrate_config for one home the way the update and migrate commands do
func migrateFromStamp(t *testing.T, h string, quiet bool) (*MigrationResults, string) {
	t.Helper()
	return migrateConfigHome(t, h, quiet)
}

func runFrom(t *testing.T, h string, ver int) *MigrationResults {
	t.Helper()
	res := &MigrationResults{}
	RunMigrations(h, ver, res, true, MigrationOptions{}, migrationTestSources(h, io.Discard))
	return res
}

func TestV50DropsTirithKeysAndEnablesNothing(t *testing.T) {
	h := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 49,
		"security": map[string]any{"redact_secrets": true, "tirith_enabled": true, "tirith_fail_open": false}}, "")
	runFrom(t, h, 49)
	raw := rawMigrated(t, h)
	if !reflect.DeepEqual(raw["security"], map[string]any{"redact_secrets": true}) {
		t.Fatalf("security = %v", raw["security"])
	}
	if len(AsStrings(AsMap(raw["plugins"])["enabled"])) != 0 {
		t.Fatalf("plugins enabled %v", raw["plugins"])
	}
	for key := range AsMap(Defaults()["security"]) {
		if strings.HasPrefix(key, "tirith") {
			t.Fatalf("default security carries %s", key)
		}
	}
}

func TestStaleDefaultIsDroppedAndAUserPinSurvives(t *testing.T) {
	h := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 46, "terminal": map[string]any{
		"backend": "docker", "docker_image": LegacySandboxImage, "daytona_image": LegacySandboxImages[1],
		"singularity_image": "docker://" + LegacySandboxImage, "modal_image": "ghcr.io/me/custom:1",
	}}, "")
	runFrom(t, h, 46)
	terminal := AsMap(rawMigrated(t, h)["terminal"])
	for _, key := range []string{"docker_image", "singularity_image", "daytona_image"} {
		if _, ok := terminal[key]; ok {
			t.Fatalf("%s kept: %v", key, terminal)
		}
	}
	if terminal["modal_image"] != "ghcr.io/me/custom:1" {
		t.Fatalf("user pin rewritten: %v", terminal)
	}
	ResetCache()
	merged, err := Load(h, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	if merged.String("terminal.docker_image", "") != DefaultSandboxImage {
		t.Fatalf("merged docker image %v", merged.Get("terminal.docker_image"))
	}
}

func TestSeededDefaultRuntimeIsDroppedEverywhereAndAUserPinSurvives(t *testing.T) {
	t.Setenv("TERMINAL_VERCEL_RUNTIME", "")
	os.Unsetenv("TERMINAL_VERCEL_RUNTIME")
	run := func(h, runtime string) (map[string]any, string) {
		writeMigrationHome(t, h, map[string]any{"_config_version": 48, "terminal": map[string]any{"backend": "vercel_sandbox", "vercel_runtime": runtime}},
			"VERCEL_TOKEN=tok\nTERMINAL_VERCEL_RUNTIME="+runtime+"\n")
		runFrom(t, h, 48)
		env, _ := os.ReadFile(home.EnvPath(h))
		return AsMap(rawMigrated(t, h)["terminal"]), string(env)
	}
	seeded := filepath.Join(t.TempDir(), "seeded")
	terminal, dotenv := run(seeded, LegacyVercelRuntime)
	if _, ok := terminal["vercel_runtime"]; ok {
		t.Fatalf("seeded runtime kept: %v", terminal)
	}
	if strings.Contains(dotenv, "TERMINAL_VERCEL_RUNTIME") || !strings.Contains(dotenv, "VERCEL_TOKEN=tok") {
		t.Fatalf("dotenv %q", dotenv)
	}
	ResetCache()
	merged, err := Load(seeded, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	if merged.String("terminal.vercel_image", "") != DefaultVercelImage || PyTruthy(merged.Get("terminal.vercel_runtime")) {
		t.Fatalf("merged terminal %v", merged.Get("terminal"))
	}
	terminal, dotenv = run(filepath.Join(t.TempDir(), "pinned"), "python3.13")
	if terminal["vercel_runtime"] != "python3.13" || !strings.Contains(dotenv, "TERMINAL_VERCEL_RUNTIME=python3.13") {
		t.Fatalf("user runtime rewritten: %v %q", terminal, dotenv)
	}
}

func TestMalformedNestedValueIsMigratedNotCrashed(t *testing.T) {
	cases := []struct {
		name   string
		ver    int
		cfg    map[string]any
		path   []string
		expect any
	}{
		{"v12-name-int", 11, map[string]any{"custom_providers": []any{map[string]any{"name": 5, "base_url": "https://api.example.com/v1"}}},
			[]string{"providers", "api-example-com", "api"}, "https://api.example.com/v1"},
		{"v14-provider-map", 13, map[string]any{"stt": map[string]any{"model": "tiny", "provider": map[string]any{"nested": true}}},
			[]string{"stt", "local", "model"}, "tiny"},
		{"v14-section-scalar", 13, map[string]any{"stt": map[string]any{"model": "base", "provider": "openai", "openai": 5}},
			[]string{"stt", "openai", "model"}, "base"},
		{"v16-platform-scalar", 15, map[string]any{"display": map[string]any{"tool_progress_overrides": map[string]any{"telegram": "all"}, "platforms": map[string]any{"telegram": 5}}},
			[]string{"display", "platforms", "telegram", "tool_progress"}, "all"},
		{"v17-auxiliary-scalar", 16, map[string]any{"compression": map[string]any{"summary_model": "fast-model"}, "auxiliary": 5},
			[]string{"auxiliary", "compression", "model"}, "fast-model"},
		{"v17-compression-scalar", 16, map[string]any{"compression": map[string]any{"summary_model": "fast-model"}, "auxiliary": map[string]any{"compression": "x"}},
			[]string{"auxiliary", "compression", "model"}, "fast-model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg["_config_version"] = tc.ver
			h := writeMigrationHome(t, t.TempDir(), tc.cfg, "")
			res := runFrom(t, h, tc.ver)
			got, ok := CfgGet(rawMigrated(t, h), tc.path...)
			if !ok || got != tc.expect {
				t.Fatalf("%v = %v", tc.path, got)
			}
			if len(res.Warnings) > 0 {
				t.Fatalf("warnings %v", res.Warnings)
			}
		})
	}
}

func TestFailingStepIsSkippedWithAWarningInResultsAndTheLog(t *testing.T) {
	h := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 12, "model": map[string]any{"default": "x/y"}}, "")
	saved := migrationLadder
	t.Cleanup(func() { migrationLadder = saved })
	migrationLadder = func() []migrationStep {
		steps := migrationSteps()
		for i := range steps {
			if steps[i].target == 13 {
				steps[i].run = func(*migrationRun) error { return errors.New("boom") }
			}
		}
		return steps
	}
	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	res, _ := migrateFromStamp(t, h, true)
	found := false
	for _, w := range res.Warnings {
		found = found || strings.HasPrefix(w, "config migration to v13 failed and was skipped")
	}
	if v := rawMigrated(t, h)["_config_version"]; !pyEq(v, MigrationVersions()[len(MigrationVersions())-1]) {
		t.Fatalf("stamp %v", v)
	}
	if !found || !strings.Contains(logged.String(), "level=WARN") || !strings.Contains(logged.String(), "config migration to v13 failed and was skipped") {
		t.Fatalf("warnings %v log %q", res.Warnings, logged.String())
	}
	versions := MigrationVersions()
	if versions[len(versions)-1] != Version {
		t.Fatalf("ladder ends at %d, schema at %d", versions[len(versions)-1], Version)
	}
}

func TestV43DropsMultiplexProfileAllowlistFromUserConfig(t *testing.T) {
	h := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 42,
		"gateway": map[string]any{"multiplex_profiles": true, "multiplex_profile_allowlist": []any{"worker"}}}, "")
	runFrom(t, h, 42)
	gw := AsMap(rawMigrated(t, h)["gateway"])
	if _, ok := gw["multiplex_profile_allowlist"]; ok || gw["multiplex_profiles"] != true {
		t.Fatalf("gateway %v", gw)
	}
	if _, ok := AsMap(Defaults()["gateway"])["multiplex_profile_allowlist"]; ok {
		t.Fatal("default gateway carries the allowlist")
	}
}

func TestV44RewritesOldCuratorDefaultsButKeepsUserValues(t *testing.T) {
	h := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 43,
		"curator": map[string]any{"stale_after_days": 30, "archive_after_days": 180}}, "")
	runFrom(t, h, 43)
	cur := AsMap(rawMigrated(t, h)["curator"])
	if !Equal(cur["stale_after_days"], AsMap(Defaults()["curator"])["stale_after_days"]) || cur["archive_after_days"] != 180 {
		t.Fatalf("curator %v", cur)
	}
}

func TestV51DropsBotChatDeliveryTimeoutWithANote(t *testing.T) {
	h := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 50,
		"cron": map[string]any{"bot_chat_delivery_timeout_seconds": 900, "max_parallel_jobs": 2}}, "")
	res := runFrom(t, h, 50)
	cron := AsMap(rawMigrated(t, h)["cron"])
	if _, ok := cron["bot_chat_delivery_timeout_seconds"]; ok || cron["max_parallel_jobs"] != 2 {
		t.Fatalf("cron %v", cron)
	}
	if !strings.Contains(strings.Join(res.ConfigAdded, "\n"), "bot_chat_delivery_timeout_seconds") {
		t.Fatalf("config added %v", res.ConfigAdded)
	}
	if _, ok := AsMap(Defaults()["cron"])["bot_chat_delivery_timeout_seconds"]; ok {
		t.Fatal("default cron carries the timeout")
	}
}

func TestUnversionedConfigDropsTheRetiredBotChatTimeout(t *testing.T) {
	h := writeMigrationHome(t, t.TempDir(), map[string]any{"cron": map[string]any{"bot_chat_delivery_timeout_seconds": 900, "max_parallel_jobs": 2}}, "")
	migrateFromStamp(t, h, true)
	if !reflect.DeepEqual(rawMigrated(t, h)["cron"], map[string]any{"max_parallel_jobs": 2}) {
		t.Fatalf("cron %v", rawMigrated(t, h)["cron"])
	}
}

func TestBranchStampedHomeStillGetsMainRetiredKeySteps(t *testing.T) {
	h := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 50,
		"mcp_servers": map[string]any{"old": map[string]any{"command": "x", "enabled": true, "disabled": true}},
		"security":    map[string]any{"redact_secrets": true, "tirith_enabled": true}}, "")
	migrateFromStamp(t, h, true)
	raw := rawMigrated(t, h)
	if !reflect.DeepEqual(AsMap(raw["mcp_servers"])["old"], map[string]any{"command": "x", "enabled": false}) ||
		!reflect.DeepEqual(raw["security"], map[string]any{"redact_secrets": true}) {
		t.Fatalf("raw %v", raw)
	}
	settled, _ := os.ReadFile(home.ConfigPath(h))
	res, _ := migrateFromStamp(t, h, true)
	again, _ := os.ReadFile(home.ConfigPath(h))
	if len(res.ConfigAdded) != 0 || !bytes.Equal(settled, again) {
		t.Fatalf("second pass added %v or rewrote the file", res.ConfigAdded)
	}
	for v := range StampIndependentSteps {
		if !LegacyKeySteps[v] {
			t.Fatalf("stamp independent step %d is not a legacy key step", v)
		}
	}
}
