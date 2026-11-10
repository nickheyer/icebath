package provider

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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

func configModuleMigrate(t *testing.T, h string, quiet bool) *config.MigrationResults {
	t.Helper()
	res, err := config.MigrateConfig(t.Context(), h, false, quiet)
	if err != nil {
		t.Fatal(err)
	}
	return res
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

// Drives the migration ladder from one version with the production collaborators
func runConfigLadder(t *testing.T, h string, from int) *config.MigrationResults {
	t.Helper()
	res := &config.MigrationResults{}
	config.RunMigrations(h, from, res, true, config.MigrationOptions{}, config.MigrationFacadeSources(t.Context(), h))
	return res
}

func configNumber(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return -1
}

func TestCustomProviderCompatibilityV11UpgradeMovesCustomProvidersIntoProviders(t *testing.T) {
	h, path, _ := configModuleCase(t, "")
	configModuleDump(t, path, map[string]any{
		"_config_version": 11,
		"model":           map[string]any{"default": "openai/gpt-5.4", "provider": "openrouter"},
		"custom_providers": []any{map[string]any{
			"name": "OpenAI Direct", "base_url": "https://api.openai.com/v1", "api_key": "test-key",
			"api_mode": "codex_responses", "model": "gpt-5-mini",
		}},
		"fallback_providers": []any{map[string]any{"provider": "openai-direct", "model": "gpt-5-mini"}},
	})
	runConfigLadder(t, h, 11)
	raw := configModuleRaw(t, path)
	want := map[string]any{
		"api": "https://api.openai.com/v1", "api_key": "test-key", "default_model": "gpt-5-mini",
		"name": "OpenAI Direct", "transport": "codex_responses",
	}
	if got := raw.Get("providers.openai-direct"); !reflect.DeepEqual(got, want) {
		t.Fatalf("providers.openai-direct %v", got)
	}
	if _, has := raw["custom_providers"]; has {
		t.Fatal("custom_providers kept")
	}
}

func TestCustomProviderCompatibilityV11UpgradePreservesCustomProviderModelMetadata(t *testing.T) {
	h, path, _ := configModuleCase(t, "")
	modelMap := func() map[string]any {
		return map[string]any{
			"kimi-k2.6":                 map[string]any{"context_length": 262144},
			"moonshotai/Kimi-K2.6-ACED": map[string]any{"context_length": 131072},
		}
	}
	configModuleDump(t, path, map[string]any{
		"_config_version": 11,
		"custom_providers": []any{
			map[string]any{
				"name": "Kimi Coding Plan", "base_url": "https://api.kimi.example.com/coding",
				"api_key_env": "KIMI_CODING_API_KEY", "api_mode": "anthropic_messages", "model": "kimi-k2.6",
				"models": modelMap(), "context_length": 262144, "rate_limit_delay": 0.25, "discover_models": false,
				"extra_body": map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}},
			},
			map[string]any{"name": "List Models", "base_url": "https://list.example.com/v1", "models": []any{"alpha", "beta"}},
		},
	})
	runConfigLadder(t, h, 11)
	raw := configModuleRaw(t, path)
	compatible := CompatibleCustomProviders(raw)
	if _, has := raw["custom_providers"]; has {
		t.Fatal("custom_providers kept")
	}
	provider, _ := raw.Get("providers.kimi-coding-plan").(map[string]any)
	sameModels := func(got any) bool {
		m, ok := got.(map[string]any)
		if !ok || len(m) != 2 {
			return false
		}
		for name, want := range map[string]float64{"kimi-k2.6": 262144, "moonshotai/Kimi-K2.6-ACED": 131072} {
			entry, ok := m[name].(map[string]any)
			if !ok || len(entry) != 1 || configNumber(entry["context_length"]) != want {
				return false
			}
		}
		return true
	}
	if provider["api"] != "https://api.kimi.example.com/coding" || provider["key_env"] != "KIMI_CODING_API_KEY" ||
		provider["transport"] != "anthropic_messages" || provider["default_model"] != "kimi-k2.6" {
		t.Fatalf("provider %v", provider)
	}
	if !sameModels(provider["models"]) || configNumber(provider["context_length"]) != 262144 || configNumber(provider["rate_limit_delay"]) != 0.25 {
		t.Fatalf("provider metadata %v", provider)
	}
	if provider["discover_models"] != false {
		t.Fatalf("discover_models %v", provider["discover_models"])
	}
	if !reflect.DeepEqual(provider["extra_body"], map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}) {
		t.Fatalf("extra_body %v", provider["extra_body"])
	}
	if got := raw.Get("providers.list-models.models"); !reflect.DeepEqual(got, map[string]any{"alpha": map[string]any{}, "beta": map[string]any{}}) {
		t.Fatalf("list models %v", got)
	}
	var kimi map[string]any
	for _, entry := range compatible {
		if entry["provider_key"] == "kimi-coding-plan" {
			kimi = entry
			break
		}
	}
	if kimi == nil || !sameModels(kimi["models"]) || kimi["key_env"] != "KIMI_CODING_API_KEY" {
		t.Fatalf("compatible entry %v", kimi)
	}
}

func TestCustomProviderCompatibilityProvidersDictResolvesAtRuntime(t *testing.T) {
	h, path, _ := configModuleCase(t, "")
	configModuleDump(t, path, map[string]any{
		"_config_version": 17,
		"providers": map[string]any{"openai-direct": map[string]any{
			"api": "https://api.openai.com/v1", "api_key": "test-key", "default_model": "gpt-5-mini",
			"name": "OpenAI Direct", "transport": "codex_responses",
		}},
	})
	compatible := CompatibleCustomProviders(configModuleLoad(t, h))
	if len(compatible) != 1 {
		t.Fatalf("compatible %v", compatible)
	}
	if compatible[0]["name"] != "OpenAI Direct" || compatible[0]["base_url"] != "https://api.openai.com/v1" ||
		compatible[0]["provider_key"] != "openai-direct" || compatible[0]["api_mode"] != "codex_responses" {
		t.Fatalf("entry %v", compatible[0])
	}
}

func TestCustomProviderCompatibilityCompatibleCustomProvidersPrefersBaseUrlThenUrlThenApi(t *testing.T) {
	h, path, _ := configModuleCase(t, "")
	configModuleDump(t, path, map[string]any{
		"_config_version": 17,
		"providers": map[string]any{"my-provider": map[string]any{
			"name": "My Provider", "api": "https://api.example.com/v1", "url": "https://url.example.com/v1",
			"base_url": "https://base.example.com/v1",
		}},
	})
	compatible := CompatibleCustomProviders(configModuleLoad(t, h))
	want := []map[string]any{{"name": "My Provider", "base_url": "https://base.example.com/v1", "provider_key": "my-provider"}}
	if !reflect.DeepEqual(compatible, want) {
		t.Fatalf("compatible %v", compatible)
	}
}

func TestInterimAssistantMessageConfigMigrateToV15SuppliesInterimMessageGateAtReadTime(t *testing.T) {
	h, path, out := configModuleCase(t, "")
	configModuleDump(t, path, map[string]any{"_config_version": 14, "display": map[string]any{"tool_progress": "off"}})
	results := configModuleMigrate(t, h, false)
	raw := configModuleRaw(t, path)
	loaded := configModuleLoad(t, h)
	if configNumber(raw["_config_version"]) != configNumber(config.Defaults()["_config_version"]) {
		t.Fatalf("version %v", raw["_config_version"])
	}
	if raw.Get("display.tool_progress") != "off" {
		t.Fatalf("tool_progress %v", raw.Get("display.tool_progress"))
	}
	if _, has := raw.Map("display")["interim_assistant_messages"]; has {
		t.Fatal("interim_assistant_messages written to disk")
	}
	if loaded.Get("display.interim_assistant_messages") != true {
		t.Fatalf("loaded gate %v", loaded.Get("display.interim_assistant_messages"))
	}
	for _, item := range results.ConfigAdded {
		if strings.Contains(item, "interim_assistant_messages") {
			t.Fatalf("config_added %v", results.ConfigAdded)
		}
	}
	if strings.Contains(out.String(), "Added display.interim_assistant_messages") {
		t.Fatalf("stdout %q", out.String())
	}
}

func TestDiscordChannelPromptsConfigMigratePreservesCustomProvidersAndNoDefaultsDump(t *testing.T) {
	h, path, _ := configModuleCase(t, "")
	configModuleDump(t, path, map[string]any{
		"_config_version": 11,
		"model":           map[string]any{"default": "test-model", "provider": "openrouter"},
		"custom_providers": []any{map[string]any{
			"name": "local-llm", "base_url": "http://localhost:8080/v1", "models": map[string]any{"test": map[string]any{}},
		}},
	})
	runConfigLadder(t, h, 11)
	raw := configModuleRaw(t, path)
	if _, has := raw["custom_providers"]; has {
		t.Fatal("custom_providers kept")
	}
	if raw.Get("providers.local-llm.api") != "http://localhost:8080/v1" {
		t.Fatalf("providers %v", raw["providers"])
	}
	for _, key := range []string{"tts", "compression", "security", "whatsapp", "bedrock"} {
		if _, has := raw[key]; has {
			t.Fatalf("%s written into the migrated config", key)
		}
	}
}

func TestEnvWriteDenylistHermesIntegrationKeysStillWritable(t *testing.T) {
	for _, key := range []string{"ICEBATH_LANGFUSE_PUBLIC_KEY", "ICEBATH_SPOTIFY_CLIENT_ID", "ICEBATH_QWEN_BASE_URL", "ICEBATH_MAX_ITERATIONS"} {
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

func TestEnvWriteDenylistHermesSecurityControlKeysAreNotWritable(t *testing.T) {
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

func TestWriteApprovalMigrationApproveMapsToTrue(t *testing.T) {
	h, path, _ := configModuleCase(t, "_config_version: 28\nmemory:\n  write_mode: approve\nskills:\n  write_mode: approve\n")
	configModuleMigrate(t, h, true)
	raw := configModuleRaw(t, path)
	for _, section := range []string{"memory", "skills"} {
		if raw.Get(section+".write_approval") != true {
			t.Fatalf("%s.write_approval %v", section, raw.Get(section+".write_approval"))
		}
		if _, has := raw.Map(section)["write_mode"]; has {
			t.Fatalf("%s.write_mode kept", section)
		}
	}
}

func TestWriteApprovalMigrationOnAndOffMapToFalse(t *testing.T) {
	h, path, _ := configModuleCase(t, "_config_version: 28\nmemory:\n  write_mode: 'on'\nskills:\n  write_mode: 'off'\n")
	configModuleMigrate(t, h, true)
	raw := configModuleRaw(t, path)
	loaded := configModuleLoad(t, h)
	for _, section := range []string{"memory", "skills"} {
		if _, has := raw.Map(section)["write_mode"]; has {
			t.Fatalf("%s.write_mode kept", section)
		}
		if loaded.Get(section+".write_approval") != false {
			t.Fatalf("%s.write_approval %v", section, loaded.Get(section+".write_approval"))
		}
	}
}

const feishuV30Body = "_config_version: 30\nmodel:\n  default: deepseek-v4-pro\n  provider: deepseek\nagent:\n  max_turns: 60\n" +
	"platforms:\n  feishu:\n    enabled: true\n    extra:\n      app_id: cli_xxx\n      app_secret: xxx\nfeishu:\n  require_mention: true\n"

func TestSaveConfigPartialWritePreservationMergeExistingPreservesPlatformsOnPartialWrite(t *testing.T) {
	h, path, _ := configModuleCase(t, feishuV30Body)
	err := config.SaveConfig(t.Context(), h, map[string]any{
		"_config_version": 30,
		"model":           map[string]any{"default": "deepseek-v4-pro", "provider": "deepseek"},
		"agent":           map[string]any{"max_turns": 60, "verify_on_stop": false},
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
	if merged.Get("agent.verify_on_stop") != false {
		t.Fatalf("merged verify_on_stop %v", merged.Get("agent.verify_on_stop"))
	}
}

func TestSaveConfigPartialWritePreservationV30ToLatestMigrationKeepsPlatforms(t *testing.T) {
	h, path, _ := configModuleCase(t, feishuV30Body)
	configModuleMigrate(t, h, true)
	raw := configModuleRaw(t, path)
	if raw.Get("platforms.feishu.extra.app_id") != "cli_xxx" || raw.Get("feishu.require_mention") != true {
		t.Fatalf("raw %v", raw)
	}
}

func TestDelegationCapUnificationMigrationNoDelegationSectionIsNoop(t *testing.T) {
	h, path, _ := configModuleCase(t, "_config_version: 32\nmodel:\n  provider: openrouter\n")
	configModuleMigrate(t, h, true)
	if _, has := configModuleRaw(t, path)["delegation"]; has {
		t.Fatal("delegation section created")
	}
}

func TestBackgroundNotificationsConciseMigrationAllBecomesConcise(t *testing.T) {
	h, path, _ := configModuleCase(t, "_config_version: 34\ndisplay:\n  background_process_notifications: all\n")
	configModuleMigrate(t, h, true)
	if got := configModuleRaw(t, path).Get("display.background_process_notifications"); got != "concise" {
		t.Fatalf("notifications %v", got)
	}
}

func TestBackgroundNotificationsConciseMigrationExplicitChoicesPreserved(t *testing.T) {
	h, path, _ := configModuleCase(t, "")
	for _, tc := range []struct {
		written string
		want    any
	}{{"off", false}, {"result", "result"}, {"error", "error"}, {"concise", "concise"}} {
		if err := os.WriteFile(path, []byte("_config_version: 34\ndisplay:\n  background_process_notifications: "+tc.written+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		config.ResetCache()
		configModuleMigrate(t, h, true)
		if got := configModuleRaw(t, path).Get("display.background_process_notifications"); got != tc.want {
			t.Fatalf("%s migrated to %v, want %v", tc.written, got, tc.want)
		}
	}
}

func TestBackgroundNotificationsConciseMigrationUnsetKeyIsNotMaterialized(t *testing.T) {
	h, path, _ := configModuleCase(t, "_config_version: 34\nmodel:\n  provider: openrouter\n")
	configModuleMigrate(t, h, true)
	if _, has := configModuleRaw(t, path).Map("display")["background_process_notifications"]; has {
		t.Fatal("background_process_notifications written")
	}
}

func TestCodexAppServerAutoConfigPreservesExistingCodexAppServerAutoValue(t *testing.T) {
	h, path, _ := configModuleCase(t, "_config_version: 31\ncompression:\n  codex_app_server_auto: hermes\n")
	configModuleMigrate(t, h, true)
	if got := configModuleRaw(t, path).Get("compression.codex_app_server_auto"); got != "hermes" {
		t.Fatalf("codex_app_server_auto %v", got)
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

func compatibleNames(cfg map[string]any) []any {
	names := []any{}
	for _, e := range CompatibleCustomProviders(cfg) {
		names = append(names, e["name"])
	}
	return names
}

func TestCompatibleProvidersMalformedLegacyKeyStringCustomProvidersKeepsProvidersViewAndWarns(t *testing.T) {
	log := captureDefaultLog(t)
	names := compatibleNames(map[string]any{
		"custom_providers": "- name: broken",
		"providers":        map[string]any{"exl3": map[string]any{"api": "http://127.0.0.1:8290/v1", "default_model": "m"}},
	})
	if !reflect.DeepEqual(names, []any{"exl3"}) {
		t.Fatalf("names %v", names)
	}
	if !strings.Contains(log.String(), "custom_providers is a str") {
		t.Fatalf("log %q", log.String())
	}
}

func TestCompatibleProvidersMalformedLegacyKeyListCustomProvidersIsSilent(t *testing.T) {
	log := captureDefaultLog(t)
	names := compatibleNames(map[string]any{
		"custom_providers": []any{map[string]any{"name": "legacy", "base_url": "http://h/v1"}},
		"providers":        map[string]any{},
	})
	if !reflect.DeepEqual(names, []any{"legacy"}) {
		t.Fatalf("names %v", names)
	}
	if strings.Contains(log.String(), "custom_providers is a") {
		t.Fatalf("log %q", log.String())
	}
}
