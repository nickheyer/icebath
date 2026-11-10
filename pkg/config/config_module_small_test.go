package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestConfigFilePermissions(t *testing.T) {
	t.Run("test_save_config_sets_0600", func(t *testing.T) {
		h, path, _ := moduleHome(t, "")
		moduleSave(t, h, map[string]any{"model": "test/model"})
		if got := permOfFile(t, path); got != 0o600 {
			t.Fatalf("config.yaml mode %o", got)
		}
	})
	t.Run("test_save_env_value_sets_0600", func(t *testing.T) {
		h, _, _ := moduleHome(t, "")
		isolateProcessEnv(t, "TEST_KEY")
		if err := SaveEnvValueContext(t.Context(), h, "TEST_KEY", "test_value"); err != nil {
			t.Fatal(err)
		}
		if got := permOfFile(t, filepath.Join(h, ".env")); got != 0o600 {
			t.Fatalf(".env mode %o", got)
		}
	})
	t.Run("test_ensure_hermes_home_sets_0700", func(t *testing.T) {
		h := filepath.Join(t.TempDir(), ".icebath")
		t.Setenv("ICEBATH_HOME", h)
		t.Setenv(EnvManagedDir, filepath.Join(t.TempDir(), "no-managed"))
		InvalidateManagedCache()
		if err := EnsureIcebathHome(t.Context(), h); err != nil {
			t.Fatal(err)
		}
		if got := permOfFile(t, h); got != 0o700 {
			t.Fatalf("home mode %o", got)
		}
		for _, subdir := range []string{"cron", "sessions", "logs", "memories"} {
			if got := permOfFile(t, filepath.Join(h, subdir)); got != 0o700 {
				t.Fatalf("%s should be 0700, got %o", subdir, got)
			}
		}
	})
}

func permOfFile(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestCodeScopedStampWinsOverHomeStamp(t *testing.T) {
	tmp := t.TempDir()
	code, h := filepath.Join(tmp, "code"), filepath.Join(tmp, "home")
	for dir, stamp := range map[string]string{code: "git\n", h: "docker\n"} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".install_method"), []byte(stamp), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_MANAGED", "")
	os.Unsetenv("ICEBATH_MANAGED")
	if got, err := DetectInstallMethod(t.Context(), h, code); err != nil || got != "git" {
		t.Fatalf("method %q %v", got, err)
	}
}

func TestContainerWithoutStampIsNotDocker(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", tmp)
	t.Setenv("ICEBATH_MANAGED", "")
	os.Unsetenv("ICEBATH_MANAGED")
	prev := runningInContainer
	runningInContainer = func() bool { return true }
	t.Cleanup(func() { runningInContainer = prev })
	if got, err := DetectInstallMethod(t.Context(), tmp, tmp); err != nil || got != "git" {
		t.Fatalf("method %q %v", got, err)
	}
}

func TestSaveAnthropicOauthTokenUsesTokenSlotAndClearsApiKey(t *testing.T) {
	h, _, _ := moduleHome(t, "")
	isolateProcessEnv(t, "ANTHROPIC_TOKEN", "ANTHROPIC_API_KEY")
	if err := SaveAnthropicOAuthToken(t.Context(), h, "sk-ant-oat01-test-token", nil); err != nil {
		t.Fatal(err)
	}
	env := LoadEnv(h)
	if env["ANTHROPIC_TOKEN"] != "sk-ant-oat01-test-token" {
		t.Fatalf("token slot %q", env["ANTHROPIC_TOKEN"])
	}
	if v, has := env["ANTHROPIC_API_KEY"]; !has || v != "" {
		t.Fatalf("api key slot %q present %v", v, has)
	}
}

func TestUseAnthropicClaudeCodeCredentialsClearsEnvSlots(t *testing.T) {
	h, _, _ := moduleHome(t, "")
	isolateProcessEnv(t, "ANTHROPIC_TOKEN", "ANTHROPIC_API_KEY")
	if err := SaveAnthropicOAuthToken(t.Context(), h, "sk-ant-oat01-token", nil); err != nil {
		t.Fatal(err)
	}
	if err := UseAnthropicClaudeCodeCredentials(t.Context(), h, nil); err != nil {
		t.Fatal(err)
	}
	env := LoadEnv(h)
	for _, slot := range []string{"ANTHROPIC_TOKEN", "ANTHROPIC_API_KEY"} {
		if v, has := env[slot]; !has || v != "" {
			t.Fatalf("%s %q present %v", slot, v, has)
		}
	}
}

func TestDestructiveSlashConfirmDefaultDefaultIsTrue(t *testing.T) {
	if got := Defaults().Get("approvals.destructive_slash_confirm"); got != true {
		t.Fatalf("default %v", got)
	}
}

func TestUserConfigMergeExistingUserConfigWithoutKeyGetsDefault(t *testing.T) {
	h, path, _ := moduleHome(t, "")
	moduleDump(t, path, map[string]any{"approvals": map[string]any{"mode": "manual", "timeout": 60, "cron_mode": "deny"}})
	if got := moduleLoad(t, h).Get("approvals.destructive_slash_confirm"); got != true {
		t.Fatalf("merged value %v", got)
	}
}

func TestFreshnessAfterConfigEdit(t *testing.T) {
	h, path, _ := moduleHome(t, "")
	moduleDump(t, path, map[string]any{"display": map[string]any{"ephemeral_system_ttl": 1}})
	first, err := ReadRawConfigReadonly(t.Context(), h)
	if err != nil || !pyEq(Tree(first).Get("display.ephemeral_system_ttl"), 1) {
		t.Fatalf("first read %v %v", first, err)
	}
	moduleDump(t, path, map[string]any{"display": map[string]any{"ephemeral_system_ttl": 7}})
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, st.ModTime(), st.ModTime().Add(1_000_000)); err != nil {
		t.Fatal(err)
	}
	second, err := ReadRawConfigReadonly(t.Context(), h)
	if err != nil || !pyEq(Tree(second).Get("display.ephemeral_system_ttl"), 7) {
		t.Fatalf("second read %v %v", second, err)
	}
}

func TestMissingConfigReturnsEmpty(t *testing.T) {
	h, path, _ := moduleHome(t, "")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	got, err := ReadRawConfigReadonly(t.Context(), h)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("raw config %#v %v", got, err)
	}
}

// Replaces a file with same sized content through a later written sibling, restoring its access and modification times
func replacePinningMtime(t *testing.T, path, content string) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(path), "other.yaml")
	if err := os.WriteFile(other, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		st, err := os.Stat(other)
		if err != nil {
			t.Fatal(err)
		}
		_, ctime := statInoCtime(st)
		if _, was := statInoCtime(before); ctime > was {
			break
		}
		now := time.Now()
		if err := os.Chtimes(other, now, now); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigSeesReplacementWithPinnedMtimeAndSize(t *testing.T) {
	h, path, _ := moduleHome(t, "model:\n  default: aaaa-route\n")
	first, err := loadConfigImpl(t.Context(), h, facadeLookup, false)
	if err != nil {
		t.Fatal(err)
	}
	again, err := loadConfigImpl(t.Context(), h, facadeLookup, false)
	if err != nil || reflect.ValueOf(map[string]any(again)).Pointer() != reflect.ValueOf(map[string]any(first)).Pointer() {
		t.Fatalf("an unchanged file was parsed again: %v", err)
	}
	replacePinningMtime(t, path, "model:\n  default: bbbb-route\n")
	if got := moduleLoad(t, h).Get("model.default"); got != "bbbb-route" {
		t.Fatalf("model.default %v", got)
	}
}

func TestRemoveEnvValueInvalidatesCache(t *testing.T) {
	h, _, _ := moduleHome(t, "")
	isolateProcessEnv(t, "DOOMED_KEY")
	InvalidateEnvCache()
	t.Cleanup(InvalidateEnvCache)
	if err := SaveEnvValueContext(t.Context(), h, "DOOMED_KEY", "value"); err != nil {
		t.Fatal(err)
	}
	if got := LoadEnv(h)["DOOMED_KEY"]; got != "value" {
		t.Fatalf("saved value %q", got)
	}
	removed, err := RemoveHomeEnvValue(t.Context(), h, "DOOMED_KEY")
	if err != nil || !removed {
		t.Fatalf("removed %v %v", removed, err)
	}
	if _, has := LoadEnv(h)["DOOMED_KEY"]; has {
		t.Fatal("the removed key still loads from the memo")
	}
}

func TestCustomEndpointKeyEnvIsAValidPosixNameForIpEndpoints(t *testing.T) {
	for _, identity := range []string{"127.0.0.1_8080", "0.0.0.0", "10.0.0.7:11434", "", "-–-"} {
		if name := CustomEndpointKeyEnv(identity); !envVarNameRe.MatchString(name) {
			t.Fatalf("%q derives the invalid name %q", identity, name)
		}
	}
}
