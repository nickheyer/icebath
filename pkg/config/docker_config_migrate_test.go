package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"gopkg.in/yaml.v3"
)

// Volume home holding the config document and .env text for one boot migration
func dockerHome(t *testing.T, cfg any, env string) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "home")
	os.MkdirAll(h, 0o755)
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_SKIP_CONFIG_MIGRATION", "")
	registerMigrationFacade(t)
	text, ok := cfg.(string)
	if !ok {
		data, _ := yaml.Marshal(cfg)
		text = string(data)
	}
	os.WriteFile(home.ConfigPath(h), []byte(text), 0o644)
	if env != "" {
		os.WriteFile(home.EnvPath(h), []byte(env), 0o644)
	}
	return h
}

// Runs the boot step as a fresh container process would, returning status, stdout and stderr
func runDockerMigration(t *testing.T, h string) (int, string, string) {
	t.Helper()
	ResetCache()
	out, errOut := captureFacade(t)
	code := DockerConfigMigrateMain(context.Background(), h)
	return code, out.String(), errOut.String()
}

func dockerBackups(h, name string) []string {
	found, _ := filepath.Glob(filepath.Join(h, "backups", "config", name+".pre-docker-migrate.*"))
	return found
}

func noBakFiles(t *testing.T, h string) {
	t.Helper()
	if found, _ := filepath.Glob(filepath.Join(h, "*.bak-*")); len(found) > 0 {
		t.Fatalf("stray backups %v", found)
	}
}

func readText(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func TestDockerConfigMigrateBacksUpAndMigratesLegacyConfig(t *testing.T) {
	h := dockerHome(t, map[string]any{"_config_version": 12, "model_catalog": map[string]any{"ttl_hours": 24},
		"delegation": map[string]any{"max_async_children": 8}}, "OPENROUTER_API_KEY=test\n")
	code, out, errOut := runDockerMigration(t, h)
	if code != 0 || !strings.Contains(out, "Migrating config schema 12 ->") {
		t.Fatalf("status %d stdout %q stderr %q", code, out, errOut)
	}
	raw := rawMigrated(t, h)
	if !pyEq(raw["_config_version"], latestConfigVersion()) || !Equal(raw["delegation"], map[string]any{"max_concurrent_children": 8}) {
		t.Fatalf("raw %v", raw)
	}
	if _, ok := AsMap(raw["model_catalog"])["ttl_hours"]; ok {
		t.Fatalf("ttl_hours survived %v", raw["model_catalog"])
	}
	if len(dockerBackups(h, "config.yaml")) == 0 || len(dockerBackups(h, ".env")) == 0 {
		t.Fatal("pre migration backups missing")
	}
}

func TestDockerConfigMigrateSkipsBelowFloorConfigUntouched(t *testing.T) {
	h := dockerHome(t, map[string]any{"_config_version": 11, "custom_providers": []any{map[string]any{"name": "Local API",
		"base_url": "http://localhost:8080/v1", "api_key": "test-key"}}}, "")
	original := readText(home.ConfigPath(h))
	code, out, errOut := runDockerMigration(t, h)
	if code != 0 || strings.Contains(out, "Migrating config schema") || !strings.Contains(errOut, "can no longer be auto-migrated") {
		t.Fatalf("status %d stdout %q stderr %q", code, out, errOut)
	}
	if readText(home.ConfigPath(h)) != original {
		t.Fatal("below floor config rewritten")
	}
	noBakFiles(t, h)
}

func TestDockerConfigMigrateStampsUnversionedConfig(t *testing.T) {
	model := map[string]any{"default": "m", "provider": "openrouter"}
	h := dockerHome(t, map[string]any{"model": model}, "")
	code, _, errOut := runDockerMigration(t, h)
	if code != 0 || strings.Contains(errOut, "can no longer be auto-migrated") {
		t.Fatalf("status %d stderr %q", code, errOut)
	}
	raw := rawMigrated(t, h)
	if !pyEq(raw["_config_version"], latestConfigVersion()) || !reflect.DeepEqual(raw["model"], model) {
		t.Fatalf("raw %v", raw)
	}
}

func TestDockerConfigMigrateDoesNotRewriteInvalidYAML(t *testing.T) {
	for _, original := range []string{"model: [unterminated\n", "- a\n- b\n"} {
		h := dockerHome(t, original, "")
		code, out, errOut := runDockerMigration(t, h)
		if code != 0 || strings.Contains(out, "Migrating config schema") || !strings.Contains(errOut, "leaving config.yaml untouched") {
			t.Fatalf("%q status %d stdout %q stderr %q", original, code, out, errOut)
		}
		if readText(home.ConfigPath(h)) != original {
			t.Fatalf("%q rewritten", original)
		}
		noBakFiles(t, h)
	}
}

func TestDockerConfigMigrateSkipEnvLeavesConfigUnchanged(t *testing.T) {
	h := dockerHome(t, map[string]any{"_config_version": 11}, "")
	original := readText(home.ConfigPath(h))
	t.Setenv("ICEBATH_SKIP_CONFIG_MIGRATION", "1")
	code, out, _ := runDockerMigration(t, h)
	if code != 0 || !strings.Contains(out, "skipping config migration") || readText(home.ConfigPath(h)) != original {
		t.Fatalf("status %d stdout %q", code, out)
	}
	noBakFiles(t, h)
}

// Replaces the stamp reader with a v12 home and the migration pass with one that clobbers both files
func stalledDockerMigration(t *testing.T, fail error) {
	t.Helper()
	prevStamp, prevMigrate := dockerReadStamp, dockerMigrateConfig
	t.Cleanup(func() { dockerReadStamp, dockerMigrateConfig = prevStamp, prevMigrate })
	dockerReadStamp = func(string, bool) (*int, int, error) { v := 12; return &v, latestConfigVersion(), nil }
	dockerMigrateConfig = func(_ context.Context, h string, _, _ bool) (*MigrationResults, error) {
		os.WriteFile(home.ConfigPath(h), []byte("gateway: {}\n"), 0o644)
		os.WriteFile(home.EnvPath(h), nil, 0o644)
		return nil, fail
	}
}

func TestDockerConfigMigrateRestoresBackupsAfterFailedMigration(t *testing.T) {
	h := dockerHome(t, map[string]any{"_config_version": 12, "gateway": map[string]any{"provider": "telegram"}}, "TELEGRAM_BOT_TOKEN=test-token\n")
	original := readText(home.ConfigPath(h))
	stalledDockerMigration(t, errors.New("boom"))
	ResetCache()
	captureFacade(t)
	if err := DockerConfigMigrate(context.Background(), h); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("failed migration returned %v", err)
	}
	if readText(home.ConfigPath(h)) != original || readText(home.EnvPath(h)) != "TELEGRAM_BOT_TOKEN=test-token\n" {
		t.Fatal("backups not restored")
	}
	if len(dockerBackups(h, "config.yaml")) == 0 || len(dockerBackups(h, ".env")) == 0 {
		t.Fatal("pre migration backups missing")
	}
}

func TestDockerConfigMigrateRestoresBackupsWhenVersionDoesNotAdvance(t *testing.T) {
	h := dockerHome(t, map[string]any{"_config_version": 12, "gateway": map[string]any{"provider": "telegram"}}, "TELEGRAM_BOT_TOKEN=test-token\n")
	original := readText(home.ConfigPath(h))
	stalledDockerMigration(t, nil)
	ResetCache()
	captureFacade(t)
	if err := DockerConfigMigrate(context.Background(), h); err == nil || !strings.Contains(err.Error(), "did not advance config version") {
		t.Fatalf("stalled migration returned %v", err)
	}
	if readText(home.ConfigPath(h)) != original || readText(home.EnvPath(h)) != "TELEGRAM_BOT_TOKEN=test-token\n" {
		t.Fatal("backups not restored")
	}
}

func TestDockerConfigMigrateSecondBootPreservesEnvByteForByte(t *testing.T) {
	env := "TELEGRAM_BOT_TOKEN=secret-bot-token\nTELEGRAM_ALLOWED_USERS=123456789\nOPENROUTER_API_KEY=sk-test-provider-key\n"
	h := dockerHome(t, map[string]any{"_config_version": 12, "gateway": map[string]any{"provider": "telegram"}}, env)
	code, out, errOut := runDockerMigration(t, h)
	if code != 0 || !strings.Contains(out, "Migrating config schema 12 ->") || !pyEq(rawMigrated(t, h)["_config_version"], latestConfigVersion()) {
		t.Fatalf("first boot status %d stdout %q stderr %q", code, out, errOut)
	}
	if readText(home.EnvPath(h)) != env {
		t.Fatalf("first boot rewrote .env %q", readText(home.EnvPath(h)))
	}
	configAfter, backups := readText(home.ConfigPath(h)), dockerBackups(h, "config.yaml")
	code, out, _ = runDockerMigration(t, h)
	if code != 0 || strings.Contains(out, "Migrating config schema") || readText(home.EnvPath(h)) != env ||
		readText(home.ConfigPath(h)) != configAfter || !reflect.DeepEqual(dockerBackups(h, "config.yaml"), backups) {
		t.Fatalf("second boot status %d stdout %q", code, out)
	}
}
