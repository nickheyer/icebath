package provider

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Home of one config case with the given config.yaml text and every facade stream captured
func configModuleCase(t *testing.T, text string, restore ...string) (string, string, *bytes.Buffer) {
	t.Helper()
	h := configModuleHome(t, restore...)
	config.ResetConfigParseFailures()
	out := &bytes.Buffer{}
	prevOut, prevErr, prevRefusal, prevParse := config.FacadeStdout, config.FacadeNoticeOut, config.EnvRefusalOut, config.ParseFailureOut
	config.FacadeStdout, config.FacadeNoticeOut, config.EnvRefusalOut, config.ParseFailureOut = out, &bytes.Buffer{}, &bytes.Buffer{}, &bytes.Buffer{}
	t.Cleanup(func() {
		config.FacadeStdout, config.FacadeNoticeOut, config.EnvRefusalOut, config.ParseFailureOut = prevOut, prevErr, prevRefusal, prevParse
		config.ResetConfigParseFailures()
	})
	path := filepath.Join(h, "config.yaml")
	if text != "" {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return h, path, out
}

// Writes the safe dump of a mapping as the config of a home
func configModuleDump(t *testing.T, path string, data map[string]any) {
	t.Helper()
	out, err := config.SafeDump(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Parsed mapping of the config file on disk
func configModuleRaw(t *testing.T, path string) config.Tree {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := config.SafeLoad(data)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := doc.(map[string]any)
	return config.Tree(m)
}

func configModuleLoad(t *testing.T, h string) config.Tree {
	t.Helper()
	config.ResetCache()
	cfg, err := config.LoadContext(t.Context(), h, config.EnvRefLookup)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCustomProviderCompatibilityProvidersDictResolvesAtRuntime(t *testing.T) {
	h, path, _ := configModuleCase(t, "")
	configModuleDump(t, path, map[string]any{
		"providers": map[string]any{"openai-direct": map[string]any{
			"base_url": "https://api.openai.com/v1", "api_key": "test-key", "default_model": "gpt-5-mini",
			"name": "OpenAI Direct", "api_mode": "codex_responses",
		}},
	})
	compatible := ProviderEntries(configModuleLoad(t, h))
	if len(compatible) != 1 {
		t.Fatalf("compatible %v", compatible)
	}
	if compatible[0]["name"] != "OpenAI Direct" || compatible[0]["base_url"] != "https://api.openai.com/v1" ||
		compatible[0]["provider_key"] != "openai-direct" || compatible[0]["api_mode"] != "codex_responses" {
		t.Fatalf("entry %v", compatible[0])
	}
}

func TestEnvWriteDenylistIcebathIntegrationKeysStillWritable(t *testing.T) {
	for _, key := range []string{"ICEBATH_LANGFUSE_PUBLIC_KEY", "ICEBATH_SPOTIFY_CLIENT_ID", "ICEBATH_QWEN_BASE_URL"} {
		t.Run(key, func(t *testing.T) {
			h := configModuleHome(t, key)
			if err := config.EnsureIcebathHome(t.Context(), h); err != nil {
				t.Fatal(err)
			}
			if err := config.SaveEnvValueContext(t.Context(), h, key, "test-value-123"); err != nil {
				t.Fatal(err)
			}
			if got := config.LoadEnv(h)[key]; got != "test-value-123" {
				t.Fatalf("%s = %q", key, got)
			}
		})
	}
}

// Asserts the generic writer refuses a name with the denylist ValueError and persists nothing
func refuseEnvWrite(t *testing.T, key string) {
	t.Helper()
	h := configModuleHome(t, key)
	if err := config.EnsureIcebathHome(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	err := config.SaveEnvValueContext(t.Context(), h, key, "1")
	if err == nil || !strings.Contains(err.Error(), "denylist") {
		t.Fatalf("%s written: %v", key, err)
	}
	if _, has := config.LoadEnv(h)[key]; has {
		t.Fatalf("%s persisted", key)
	}
}

func TestEnvWriteDenylistIcebathSecurityControlKeysAreNotWritable(t *testing.T) {
	for _, key := range []string{
		"ICEBATH_CONFIG_PATH", "ICEBATH_ENV_PATH", "ICEBATH_OPTIONAL_MCPS", "ICEBATH_COPILOT_ACP_COMMAND",
		"ICEBATH_COPILOT_ACP_ARGS", "ICEBATH_YOLO_MODE", "ICEBATH_ACCEPT_HOOKS", "ICEBATH_REDACT_SECRETS",
		"ICEBATH_INTERACTIVE", "ICEBATH_EXEC_ASK", "ICEBATH_GATEWAY_SESSION", "ICEBATH_CRON_SESSION",
		"ICEBATH_SINGLE_QUERY_SESSION", "ICEBATH_SESSION_KEY", "ICEBATH_SESSION_PLATFORM",
	} {
		t.Run(key, func(t *testing.T) { refuseEnvWrite(t, key) })
	}
}

func TestEnvWriteDenylistExecInfluenceKeysAreNotWritable(t *testing.T) {
	for _, key := range []string{
		"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM",
		"GIT_CONFIG_KEY_17", "GIT_CONFIG_VALUE_17",
		"GIT_SSH", "GIT_ASKPASS", "GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "GIT_PAGER", "GIT_EXTERNAL_DIFF", "GIT_PROXY_COMMAND",
		"GIT_TEMPLATE_DIR", "GIT_DIR",
		"SSH_ASKPASS", "SUDO_ASKPASS",
		"LD_PROFILE", "DYLD_PRINT_LIBRARIES",
		"BASH_ENV", "ENV", "ZDOTDIR", "PROMPT_COMMAND", "VIMINIT", "EXINIT",
		"MANPAGER",
		"PERL5OPT", "PERL5LIB", "PERLLIB", "RUBYOPT", "RUBYLIB",
		"PYTHONBREAKPOINT", "PYTHONCASEOK", "CLASSPATH",
		"JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS", "JDK_JAVA_OPTIONS",
		"GOFLAGS", "RUSTFLAGS",
	} {
		t.Run(key, func(t *testing.T) { refuseEnvWrite(t, key) })
	}
}

func TestEnvWriteDenylistNonExecNearMissesStillWritable(t *testing.T) {
	for _, key := range []string{"GIT_COMMITTER_NAME", "GIT_AUTHOR_NAME", "GIT_TERMINAL_PROMPT", "GIT_EDITOR_WIDE", "git_config_parameters", "ld_preload"} {
		t.Run(key, func(t *testing.T) {
			h := configModuleHome(t, key)
			if err := config.EnsureIcebathHome(t.Context(), h); err != nil {
				t.Fatal(err)
			}
			if err := config.SaveEnvValueContext(t.Context(), h, key, "test-value-123"); err != nil {
				t.Fatal(err)
			}
			if got := config.LoadEnv(h)[key]; got != "test-value-123" {
				t.Fatalf("%s = %q", key, got)
			}
		})
	}
}

func TestEnvWriteDenylistPreexistingOptionalMcpsOverrideStillLoads(t *testing.T) {
	h := configModuleHome(t)
	if err := config.EnsureIcebathHome(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(h, "custom-mcp-catalog")
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("ICEBATH_OPTIONAL_MCPS="+catalog+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.InvalidateEnvCache()
	if got := config.LoadEnv(h)["ICEBATH_OPTIONAL_MCPS"]; got != catalog {
		t.Fatalf("override %q", got)
	}
}

func TestEnvWriteDenylistSaveEnvValueSecureInheritsDenylist(t *testing.T) {
	h := configModuleHome(t, "LD_PRELOAD")
	if err := config.EnsureIcebathHome(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	_, err := config.SaveEnvValueSecure(t.Context(), h, "LD_PRELOAD", "/tmp/evil.so")
	if err == nil || !strings.Contains(err.Error(), "denylist") {
		t.Fatalf("secure save wrote LD_PRELOAD: %v", err)
	}
}

const feishuV30Body = "_config_version: 30\nmodel:\n  default: deepseek-v4-pro\n  provider: deepseek\nagent:\n  max_turns: 60\n" +
	"platforms:\n  feishu:\n    enabled: true\n    extra:\n      app_id: cli_xxx\n      app_secret: xxx\nfeishu:\n  require_mention: true\n"

func TestSaveConfigPartialWritePreservationMergeExistingPreservesPlatformsOnPartialWrite(t *testing.T) {
	h, path, _ := configModuleCase(t, feishuV30Body)
	err := config.SaveConfig(t.Context(), h, map[string]any{
		"_config_version": 30,
		"model":           map[string]any{"default": "deepseek-v4-pro", "provider": "deepseek"},
		"agent":           map[string]any{"max_turns": 60, "verify_on_stop": "off"},
	}, config.SaveOptions{MergeExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	raw := configModuleRaw(t, path)
	merged := configModuleLoad(t, h)
	if raw.Get("platforms.feishu.extra.app_id") != "cli_xxx" || raw.Get("feishu.require_mention") != true {
		t.Fatalf("raw %v", raw)
	}
	if _, has := raw.Map("agent")["verify_on_stop"]; has {
		t.Fatalf("default written: %v", raw["agent"])
	}
	if merged.Get("agent.verify_on_stop") != "off" {
		t.Fatalf("merged verify_on_stop %v", merged.Get("agent.verify_on_stop"))
	}
}

func TestIsProviderEnabledMissingFlagDefaultsToEnabled(t *testing.T) {
	if !IsProviderEnabled(map[string]any{"name": "Anthropic"}) {
		t.Fatal("a block without the flag is disabled")
	}
}

func TestIsProviderEnabledYamlStringTruthyValuesKeepItEnabled(t *testing.T) {
	for _, raw := range []string{"true", "True", "yes", "on", "1", "anything-else"} {
		if !IsProviderEnabled(map[string]any{"enabled": raw}) {
			t.Fatalf("enabled %q reads as disabled", raw)
		}
	}
}

func TestIsProviderEnabledNonDictInputDefaultsToEnabled(t *testing.T) {
	for _, input := range []any{nil, []any{}, "oops"} {
		if !IsProviderEnabled(input) {
			t.Fatalf("%#v reads as disabled", input)
		}
	}
}

func TestProviderEnabledRuntimeGateDisabledCustomProviderRaisesValueerror(t *testing.T) {
	h, path, _ := configModuleCase(t, "")
	configModuleDump(t, path, map[string]any{
		"model": map[string]any{"default": "claude-sonnet-4-6", "provider": "claude-agent-sdk"},
		"providers": map[string]any{"my-fork": map[string]any{
			"name": "my-fork", "base_url": "http://127.0.0.1:9999", "api_key": "not-needed", "enabled": false,
		}},
	})
	config.ResetCache()
	_, err := ResolveRuntimeProvider(t.Context(), h, "my-fork", "", "", "")
	var valueErr *ValueError
	if !errors.As(err, &valueErr) || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("resolution error %v", err)
	}
	if err.Error() != "provider 'my-fork' is disabled in config (providers.my-fork.enabled: false)" {
		t.Fatalf("message %q", err.Error())
	}
}

// Routes the default logger into a buffer for one test and returns the buffer
func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}
