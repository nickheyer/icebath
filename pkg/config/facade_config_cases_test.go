package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Migration facade home holding the config text and .env text, stdout and stderr captured
func caseHome(t *testing.T, configText, envText string) (string, string, func() (string, string)) {
	t.Helper()
	h := filepath.Join(t.TempDir(), "home")
	os.MkdirAll(h, 0o755)
	t.Setenv("ICEBATH_HOME", h)
	registerMigrationFacade(t)
	out, errOut := captureFacade(t)
	path := home.ConfigPath(h)
	if configText != "" {
		os.WriteFile(path, []byte(configText), 0o644)
	}
	os.WriteFile(home.EnvPath(h), []byte(envText), 0o644)
	return h, path, func() (string, string) {
		o, e := out.String(), errOut.String()
		out.Reset()
		errOut.Reset()
		return o, e
	}
}

func migrateCase(t *testing.T, h string, interactive, quiet bool) *MigrationResults {
	t.Helper()
	res, err := MigrateConfig(context.Background(), h, interactive, quiet)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func loadCase(t *testing.T, h string) Tree {
	t.Helper()
	ResetCache()
	cfg, err := Load(h, facadeLookup)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestMigrateReportsNormalizedLineFormatting(t *testing.T) {
	h, _, output := caseHome(t, fmt.Sprintf("_config_version: %d\n", latestConfigVersion()), "A=1\r\n  B=2  \n# keep  \nC=3")
	migrateCase(t, h, false, false)
	if out, _ := output(); out != "  ✓ Normalized .env line formatting (2 line(s) changed)\n" {
		t.Fatalf("stdout %q", out)
	}
}

func TestGLMSuffixCollisionNotSplit(t *testing.T) {
	lines := []string{"GLM_API_KEY=glm-secret\n", "GLM_BASE_URL=https://api.z.ai/api/paas/v4\n"}
	if got := SanitizeEnvLines(lines); !reflect.DeepEqual(got, lines) {
		t.Fatalf("GLM lines corrupted %q", got)
	}
}

func TestSanitizeEnvFileKeepsValueSemanticsAndCleanFiles(t *testing.T) {
	for _, text := range []string{"FAL_KEY=good\nOPENROUTER_API_KEY=valFIRECRAWL_API_KEY=val2\n", "GOOD_KEY=good\nOTHER_KEY=other\n"} {
		h, _, _ := caseHome(t, "", text)
		fixes, err := SanitizeEnvFile(context.Background(), h)
		data, _ := os.ReadFile(home.EnvPath(h))
		if err != nil || fixes != 0 || string(data) != text {
			t.Fatalf("fixes %d err %v content %q", fixes, err, data)
		}
	}
}

func TestRequiredSecretEnvPromptUsesMaskedPrompt(t *testing.T) {
	h, _, _ := caseHome(t, fmt.Sprintf("_config_version: %d\n", latestConfigVersion()), "")
	prevRequired := RequiredEnvVars
	RequiredEnvVars = map[string]EnvVar{"TEST_API_KEY": {Description: "Test key", Prompt: "Test API key", Password: true}}
	t.Cleanup(func() { RequiredEnvVars = prevRequired })
	var prompted string
	withHooks := hooks()
	withHooks.MaskedSecretPrompt = func(prompt string) (string, error) { prompted = prompt; return "secret", nil }
	RegisterConfigFacade(withHooks)
	t.Setenv("TEST_API_KEY", "")
	os.Unsetenv("TEST_API_KEY")
	res := migrateCase(t, h, true, true)
	if prompted != "  Test API key: " || LoadEnv(h)["TEST_API_KEY"] != "secret" || !reflect.DeepEqual(res.EnvAdded, []string{"TEST_API_KEY"}) {
		t.Fatalf("prompt %q env %v added %v", prompted, LoadEnv(h), res.EnvAdded)
	}
}

func TestLoadedVersionIsLatestWhileTheFileReadsLegacy(t *testing.T) {
	h, _, _ := caseHome(t, "model: {}\n", "")
	if v := loadCase(t, h)["_config_version"]; !pyEq(v, latestConfigVersion()) {
		t.Fatalf("loaded version %v", v)
	}
	if cur, latest, err := CheckConfigVersion(context.Background(), h, false); err != nil || cur != 0 || latest != latestConfigVersion() {
		t.Fatalf("check %d %d %v", cur, latest, err)
	}
}

func TestMigrationRejectsInvalidConfigBeforeSanitizingEnv(t *testing.T) {
	for _, tc := range []struct{ text, match string }{
		{"model: [unterminated\n", "not valid YAML"}, {"- just_a_list\n", "must be a mapping"}, {"[]\n", "must be a mapping"},
	} {
		h, path, _ := caseHome(t, tc.text, "OPENAI_API_KEY=test-without-final-newline")
		_, err := MigrateConfig(context.Background(), h, false, true)
		var invalid *InvalidUserConfigError
		if !errors.As(err, &invalid) || !strings.Contains(err.Error(), tc.match) {
			t.Fatalf("%q migrated with %v", tc.text, err)
		}
		cfgData, _ := os.ReadFile(path)
		envData, _ := os.ReadFile(home.EnvPath(h))
		if string(cfgData) != tc.text || string(envData) != "OPENAI_API_KEY=test-without-final-newline" {
			t.Fatalf("files touched %q %q", cfgData, envData)
		}
	}
}

func TestV11ConfigIsRefusedAndUntouched(t *testing.T) {
	original := "_config_version: 11\ncustom_providers:\n- base_url: http://localhost:1234/v1\n  name: Old\n"
	h, path, output := caseHome(t, original, "ANTHROPIC_TOKEN=old-token\n")
	res := migrateCase(t, h, false, false)
	data, _ := os.ReadFile(path)
	if string(data) != original || LoadEnv(h)["ANTHROPIC_TOKEN"] != "old-token" {
		t.Fatalf("refused home touched %q %v", data, LoadEnv(h))
	}
	out, errOut := output()
	fragment := "This config predates version 12 (~2 years old) and can no longer be auto-migrated."
	if !strings.Contains(out, fragment) || !strings.Contains(errOut, fragment) || !strings.Contains(out, "run `icebath setup` to regenerate") ||
		!strings.Contains(out, "_config_version: 12") || strings.Contains(out, "Config version:") {
		t.Fatalf("stdout %q stderr %q", out, errOut)
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], fragment) {
		t.Fatalf("warnings %v", res.Warnings)
	}
}

func TestV11QuietStillWarnsOnStderrOnly(t *testing.T) {
	h, path, output := caseHome(t, "_config_version: 11\n", "")
	res := migrateCase(t, h, false, true)
	data, _ := os.ReadFile(path)
	out, errOut := output()
	if string(data) != "_config_version: 11\n" || out != "" || !strings.Contains(errOut, "can no longer be auto-migrated") || len(res.Warnings) == 0 {
		t.Fatalf("file %q stdout %q stderr %q warnings %v", data, out, errOut, res.Warnings)
	}
}

func TestFloorMessageUsesDisplayHome(t *testing.T) {
	h := t.TempDir()
	if msg := SupportFloorMessage(h); !strings.Contains(msg, home.Display(h)+"/config.yaml") {
		t.Fatalf("message %q", msg)
	}
}

func TestRegistryHasNoTargetsBelowFloor(t *testing.T) {
	versions := MigrationVersions()
	if SupportFloorVersion != 12 || versions[0] != 12 {
		t.Fatalf("floor %d first target %d", SupportFloorVersion, versions[0])
	}
	targets := map[int]bool{}
	for _, v := range versions {
		if v < SupportFloorVersion {
			t.Fatalf("target %d below the floor", v)
		}
		targets[v] = true
	}
	for v := range LegacyKeySteps {
		if !targets[v] {
			t.Fatalf("legacy key step %d is not a ladder target", v)
		}
	}
}

func TestAtOrAboveFloorMigratesIdenticallyToPreFloor(t *testing.T) {
	envFixture := "LLM_MODEL=old-model\nOPENAI_MODEL=old-openai\nOPENROUTER_API_KEY=test\n"
	for _, tc := range []struct {
		fixture, expected map[string]any
		env               string
	}{
		{map[string]any{"_config_version": 12, "model": map[string]any{"default": "openai/gpt-5.4", "provider": "openrouter"},
			"display": map[string]any{"tool_progress_overrides": map[string]any{"telegram": "verbose"}},
			"stt":     map[string]any{"model": "base", "provider": "local"}, "compression": map[string]any{"summary_model": "gpt-x", "summary_provider": "auto"},
			"model_catalog": map[string]any{"ttl_hours": 24}, "memory": map[string]any{"write_mode": "approve"},
			"delegation": map[string]any{"max_async_children": 8}, "agent": map[string]any{"verify_on_stop": true}},
			map[string]any{"agent": map[string]any{"verify_on_stop": false}, "auxiliary": map[string]any{"compression": map[string]any{"model": "gpt-x"}},
				"compression": map[string]any{}, "delegation": map[string]any{"max_concurrent_children": 8},
				"display": map[string]any{"platforms": map[string]any{"telegram": map[string]any{"tool_progress": "verbose"}},
					"tool_progress_overrides": map[string]any{"telegram": "verbose"}},
				"memory": map[string]any{"write_approval": true}, "model": map[string]any{"default": "openai/gpt-5.4", "provider": "openrouter"},
				"model_catalog": map[string]any{}, "plugins": map[string]any{"enabled": []any{}}, "stt": map[string]any{"provider": "local"}},
			"LLM_MODEL=\nOPENAI_MODEL=\nOPENROUTER_API_KEY=test\n"},
		{map[string]any{"_config_version": 20, "model": map[string]any{"default": "anthropic/claude-fable-5", "provider": "nous"},
			"plugins": map[string]any{"disabled": []any{"foo"}}, "skills": map[string]any{"write_mode": "on"},
			"model_catalog": map[string]any{"ttl_hours": 24}, "agent": map[string]any{}},
			map[string]any{"model": map[string]any{"default": "anthropic/claude-fable-5", "provider": "nous"}, "model_catalog": map[string]any{},
				"plugins": map[string]any{"disabled": []any{"foo"}, "enabled": []any{}}},
			envFixture},
	} {
		dir := t.TempDir()
		h := writeMigrationHome(t, dir, tc.fixture, envFixture)
		t.Setenv("ICEBATH_HOME", h)
		migrateConfigHome(t, h, true)
		raw := rawMigrated(t, h)
		if !pyEq(raw["_config_version"], latestConfigVersion()) {
			t.Fatalf("stamp %v", raw["_config_version"])
		}
		for key, want := range tc.expected {
			if !Equal(raw[key], want) {
				t.Fatalf("parity drift on %s: %#v want %#v", key, raw[key], want)
			}
		}
		if data, _ := os.ReadFile(home.EnvPath(h)); string(data) != tc.env {
			t.Fatalf("env %q", data)
		}
	}
}

func TestWriteApprovalMigration(t *testing.T) {
	h, path, _ := caseHome(t, "_config_version: 28\nmemory:\n  write_mode: approve\nskills:\n  write_mode: approve\n", "")
	migrateCase(t, h, false, true)
	for _, section := range []string{"memory", "skills"} {
		if v, _ := readSaved(t, path, section, "write_approval"); v != true {
			t.Fatalf("%s.write_approval %v", section, v)
		}
		if _, ok := readSaved(t, path, section, "write_mode"); ok {
			t.Fatalf("%s.write_mode survived", section)
		}
	}
	h, path, _ = caseHome(t, "_config_version: 28\nmemory:\n  write_mode: 'on'\nskills:\n  write_mode: 'off'\n", "")
	migrateCase(t, h, false, true)
	loaded := loadCase(t, h)
	for _, section := range []string{"memory", "skills"} {
		if _, ok := readSaved(t, path, section, "write_mode"); ok {
			t.Fatalf("%s.write_mode survived", section)
		}
		if v, _ := CfgGet(loaded, section, "write_approval"); v != false {
			t.Fatalf("loaded %s.write_approval %v", section, v)
		}
	}
}

func TestVersionBumpKeepsConfigLean(t *testing.T) {
	latest := latestConfigVersion()
	for _, start := range []int{12, latest - 1} {
		h, path, _ := caseHome(t, fmt.Sprintf("_config_version: %d\nmodel:\n  default: test-model\n  provider: openrouter\nmatrix:\n  require_mention: false\n", start), "")
		migrateCase(t, h, false, true)
		raw, err := ReadDocument(path)
		if err != nil {
			t.Fatal(err)
		}
		loaded := loadCase(t, h)
		if !pyEq(raw["_config_version"], latest) {
			t.Fatalf("start %d stamp %v", start, raw["_config_version"])
		}
		if v, _ := CfgGet(raw, "matrix", "require_mention"); v != false {
			t.Fatalf("raw matrix %v", raw["matrix"])
		}
		if v, _ := CfgGet(loaded, "matrix", "require_mention"); v != false {
			t.Fatalf("loaded matrix %v", loaded["matrix"])
		}
		for _, key := range []string{"timezone", "curator", "auxiliary", "tts", "compression", "whatsapp", "bedrock"} {
			if _, ok := raw[key]; ok {
				t.Fatalf("start %d materialised %s into a lean config", start, key)
			}
		}
		if v, _ := CfgGet(loaded, "curator", "enabled"); !Equal(v, AsMap(Defaults()["curator"])["enabled"]) {
			t.Fatalf("curator.enabled %v", v)
		}
		if v, _ := CfgGet(loaded, "display", "compact"); !Equal(v, AsMap(Defaults()["display"])["compact"]) {
			t.Fatalf("display.compact %v", v)
		}
	}
}

func TestDelegationMigrationWithoutSectionIsANoop(t *testing.T) {
	h, path, _ := caseHome(t, "_config_version: 32\nmodel:\n  provider: openrouter\n", "")
	migrateCase(t, h, false, true)
	if _, ok := readSaved(t, path, "delegation"); ok {
		t.Fatal("delegation section invented")
	}
}

func TestBackgroundNotificationsConciseMigration(t *testing.T) {
	for _, tc := range []struct {
		written string
		want    any
	}{{"all", "concise"}, {"off", false}, {"result", "result"}, {"error", "error"}, {"concise", "concise"}} {
		h, path, _ := caseHome(t, "_config_version: 34\ndisplay:\n  background_process_notifications: "+tc.written+"\n", "")
		migrateCase(t, h, false, true)
		if v, _ := readSaved(t, path, "display", "background_process_notifications"); v != tc.want {
			t.Fatalf("%s became %#v", tc.written, v)
		}
	}
	h, path, _ := caseHome(t, "_config_version: 34\nmodel:\n  provider: openrouter\n", "")
	migrateCase(t, h, false, true)
	if _, ok := readSaved(t, path, "display", "background_process_notifications"); ok {
		t.Fatal("unset notifications key materialised")
	}
}

func TestConfigCommandExitsCleanlyOnBrokenYAML(t *testing.T) {
	original := "model:\n  default: keep\nbroken: [unterminated\n"
	value := "gpt-4o"
	for _, args := range []ConfigArgs{{Command: "set", Key: "model.default", Value: &value}, {Command: "unset", Key: "model.default"}} {
		h, path, output := caseHome(t, original, "")
		err := ConfigCommand(context.Background(), h, args, pathCommandSources())
		_, errOut := output()
		data, _ := os.ReadFile(path)
		if exitCode(err) != 1 || !strings.Contains(errOut, "formatting error") || string(data) != original {
			t.Fatalf("%s returned %v stderr %q file %q", args.Command, err, errOut, data)
		}
		if args.Command == "set" && !strings.Contains(errOut, "`icebath config edit`") {
			t.Fatalf("set stderr %q", errOut)
		}
	}
}
