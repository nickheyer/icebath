package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Points the home at a fresh directory and clears the cached CLI config
func tempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	SetCLIConfig(nil)
	t.Cleanup(func() { SetCLIConfig(nil) })
	return dir
}

func TestResolvePrefillMessagesFileUsesTopLevel(t *testing.T) {
	t.Setenv("ICEBATH_PREFILL_MESSAGES_FILE", "")
	os.Unsetenv("ICEBATH_PREFILL_MESSAGES_FILE")
	got := resolvePrefillMessagesFile(context.Background(), map[string]any{
		"prefill_messages_file": "top.json",
		"agent":                 map[string]any{"prefill_messages_file": "legacy.json"},
	})
	if got != "top.json" {
		t.Fatalf("got %q", got)
	}
}

func TestResolvePrefillMessagesFileAcceptsLegacyAgentKey(t *testing.T) {
	t.Setenv("ICEBATH_PREFILL_MESSAGES_FILE", "")
	os.Unsetenv("ICEBATH_PREFILL_MESSAGES_FILE")
	got := resolvePrefillMessagesFile(context.Background(), map[string]any{"agent": map[string]any{"prefill_messages_file": "legacy.json"}})
	if got != "legacy.json" {
		t.Fatalf("got %q", got)
	}
}

func TestResolvePrefillMessagesFilePrefersEnv(t *testing.T) {
	t.Setenv("ICEBATH_PREFILL_MESSAGES_FILE", "env.json")
	got := resolvePrefillMessagesFile(context.Background(), map[string]any{
		"prefill_messages_file": "top.json",
		"agent":                 map[string]any{"prefill_messages_file": "legacy.json"},
	})
	if got != "env.json" {
		t.Fatalf("got %q", got)
	}
}

func TestLoadPrefillMessagesReadsAJSONArrayUnderTheHome(t *testing.T) {
	home := tempHome(t)
	os.WriteFile(filepath.Join(home, "prefill.json"), []byte("\ufeff[{\"role\": \"user\", \"content\": \"hi\"}]"), 0o600)
	os.WriteFile(filepath.Join(home, "object.json"), []byte(`{"role": "user"}`), 0o600)
	ctx := context.Background()
	if got := loadPrefillMessages(ctx, "prefill.json", ""); len(got) != 1 || got[0].(map[string]any)["role"] != "user" {
		t.Fatalf("got %v", got)
	}
	if got := loadPrefillMessages(ctx, "object.json", ""); len(got) != 0 {
		t.Fatalf("object accepted %v", got)
	}
	if got := loadPrefillMessages(ctx, "missing.json", ""); len(got) != 0 {
		t.Fatalf("missing accepted %v", got)
	}
	if got := loadPrefillMessages(ctx, "", ""); len(got) != 0 {
		t.Fatalf("empty path gave %v", got)
	}
}

// Writes the home and managed config files and loads the CLI config fresh
func loadWithManaged(t *testing.T, userYAML, managedYAML string) map[string]any {
	t.Helper()
	home := tempHome(t)
	managed := t.TempDir()
	t.Setenv("ICEBATH_MANAGED_DIR", managed)
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte(userYAML), 0o600)
	os.WriteFile(filepath.Join(managed, "config.yaml"), []byte(managedYAML), 0o600)
	config.InvalidateManagedCache()
	return LoadCLIConfig(context.Background())
}

func TestCliConfigHonorsManagedSkin(t *testing.T) {
	cfg := loadWithManaged(t, "display:\n  skin: user_skin\n", "display:\n  skin: charizard\n")
	if sectionOf(cfg, "display")["skin"] != "charizard" {
		t.Fatalf("display %v", cfg["display"])
	}
}

func TestCliConfigManagedLeafPreservesUserSiblings(t *testing.T) {
	cfg := loadWithManaged(t, "display:\n  skin: user_skin\n  show_reasoning: true\n", "display:\n  skin: charizard\n")
	display := sectionOf(cfg, "display")
	if display["skin"] != "charizard" || display["show_reasoning"] != true {
		t.Fatalf("display %v", display)
	}
}

// Writes the sentinel user config the ignore flag tests read
func writeUserConfig(t *testing.T, home, modelDefault string) {
	t.Helper()
	yaml := "model:\n  default: " + modelDefault + "\n  provider: openrouter\nagent:\n  system_prompt: \"from user config\"\n"
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte(yaml), 0o600)
}

func TestIgnoreUserConfigEnvGateUserConfigLoadedWhenFlagUnset(t *testing.T) {
	home := tempHome(t)
	os.Unsetenv("ICEBATH_IGNORE_USER_CONFIG")
	writeUserConfig(t, home, "test-vendor/ignore-user-config-sentinel")
	cfg := LoadCLIConfig(context.Background())
	if sectionOf(cfg, "model")["default"] != "test-vendor/ignore-user-config-sentinel" || sectionOf(cfg, "agent")["system_prompt"] != "from user config" {
		t.Fatalf("cfg %v", cfg)
	}
}

func TestIgnoreUserConfigEnvGateUserConfigSkippedWhenFlagSet(t *testing.T) {
	home := tempHome(t)
	writeUserConfig(t, home, "test-vendor/ignore-user-config-sentinel")
	t.Setenv("ICEBATH_IGNORE_USER_CONFIG", "1")
	cfg := LoadCLIConfig(context.Background())
	if sectionOf(cfg, "agent")["system_prompt"] == "from user config" {
		t.Fatalf("user system prompt leaked %v", cfg)
	}
	if sectionOf(cfg, "model")["default"] == "test-vendor/ignore-user-config-sentinel" {
		t.Fatalf("user model leaked %v", cfg)
	}
}

func TestIgnoreUserConfigEnvGateFlagIgnoredWhenSetToOtherValue(t *testing.T) {
	home := tempHome(t)
	writeUserConfig(t, home, "test-vendor/ignore-user-config-sentinel")
	t.Setenv("ICEBATH_IGNORE_USER_CONFIG", "true")
	cfg := LoadCLIConfig(context.Background())
	if sectionOf(cfg, "model")["default"] != "test-vendor/ignore-user-config-sentinel" {
		t.Fatalf("cfg %v", cfg)
	}
}

func TestSaveConfigSetBridgesEveryCliTerminalKey(t *testing.T) {
	saveKeys := map[string]bool{}
	for _, pair := range config.TerminalConfigEnvMap {
		if pair[0] != "cwd" {
			saveKeys[pair[0]] = true
		}
	}
	exempt := map[string]bool{"env_type": true, "sudo_password": true, "cwd": true, "home_mode": true}
	var missing []string
	for _, pair := range terminalEnvMappings() {
		if !exempt[pair[0]] && !saveKeys[pair[0]] {
			missing = append(missing, pair[0])
		}
	}
	if len(missing) > 0 {
		t.Fatalf("config set does not sync %v", missing)
	}
}

func TestMirrorConfigToEnvBridgesTerminalBrowserAuxiliaryAndSessionKeys(t *testing.T) {
	for _, name := range []string{"TERMINAL_ENV", "TERMINAL_CWD", "TERMINAL_TIMEOUT", "TERMINAL_DOCKER_VOLUMES", "BROWSER_INACTIVITY_TIMEOUT",
		"AUXILIARY_VISION_MODEL", "AUXILIARY_VISION_PROVIDER", "ICEBATH_REDACT_SECRETS", "ICEBATH_CJK_FTS", "SUDO_PASSWORD"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Setenv("TERMINAL_TIMEOUT", "7")
	defaults := map[string]any{
		"terminal":  map[string]any{"backend": "docker", "cwd": "auto", "timeout": 30, "docker_volumes": []any{"a:b"}, "sudo_password": "pw"},
		"browser":   map[string]any{"inactivity_timeout": 15},
		"auxiliary": map[string]any{"vision": map[string]any{"provider": "auto", "model": "gpt-4o", "base_url": "", "api_key": ""}},
		"security":  map[string]any{"redact_secrets": true},
		"sessions":  map[string]any{"cjk_fts": false},
	}
	mirrorConfigToEnv(defaults, false)
	if os.Getenv("TERMINAL_ENV") != "docker" || os.Getenv("TERMINAL_TIMEOUT") != "7" || os.Getenv("TERMINAL_DOCKER_VOLUMES") != `["a:b"]` {
		t.Fatalf("terminal env %q %q %q", os.Getenv("TERMINAL_ENV"), os.Getenv("TERMINAL_TIMEOUT"), os.Getenv("TERMINAL_DOCKER_VOLUMES"))
	}
	if _, set := os.LookupEnv("TERMINAL_CWD"); set {
		t.Fatal("stand in cwd bridged for a non local backend")
	}
	if os.Getenv("BROWSER_INACTIVITY_TIMEOUT") != "15" || os.Getenv("AUXILIARY_VISION_MODEL") != "gpt-4o" || os.Getenv("AUXILIARY_VISION_PROVIDER") != "" {
		t.Fatalf("browser or auxiliary env wrong")
	}
	if os.Getenv("ICEBATH_REDACT_SECRETS") != "true" || os.Getenv("ICEBATH_CJK_FTS") != "False" || os.Getenv("SUDO_PASSWORD") != "pw" {
		t.Fatalf("security or session env wrong %q %q", os.Getenv("ICEBATH_REDACT_SECRETS"), os.Getenv("ICEBATH_CJK_FTS"))
	}
	mirrorConfigToEnv(map[string]any{"terminal": map[string]any{"timeout": 30}}, true)
	cwd, _ := os.Getwd()
	if os.Getenv("TERMINAL_TIMEOUT") != "30" || os.Getenv("TERMINAL_CWD") != cwd {
		t.Fatalf("file terminal config did not win %q %q", os.Getenv("TERMINAL_TIMEOUT"), os.Getenv("TERMINAL_CWD"))
	}
}

func TestMergeFileConfigPromotesModelAndLegacyMaxTurns(t *testing.T) {
	defaults := cliConfigDefaults()
	mergeFileConfig(defaults, map[string]any{"model": map[string]any{"model": "m1", "base_url": "http://x"}, "max_turns": 9,
		"display": map[string]any{"compact": true}, "memory": map[string]any{"provider": "p"}, "agent": nil})
	model := sectionOf(defaults, "model")
	if model["default"] != "m1" || model["base_url"] != "http://x" || sectionOf(defaults, "agent")["max_turns"] != 9 {
		t.Fatalf("defaults %v", defaults)
	}
	if sectionOf(defaults, "display")["compact"] != true || sectionOf(defaults, "display")["skin"] != "default" || defaults["memory"] == nil {
		t.Fatalf("merge %v", defaults)
	}
	plain := cliConfigDefaults()
	mergeFileConfig(plain, map[string]any{"model": "m2", "agent": map[string]any{"max_turns": 3}, "max_turns": 9})
	if sectionOf(plain, "model")["default"] != "m2" || sectionOf(plain, "agent")["max_turns"] != 3 {
		t.Fatalf("plain %v", plain)
	}
}

func TestIntOrParsesPythonIntInputs(t *testing.T) {
	cases := map[any]int{"12": 12, " 7 ": 7, 3.9: 3, true: 1, int64(5): 5, "x": -1, nil: -1, "1.5": -1}
	for in, want := range cases {
		if got := intOr(in, -1); got != want {
			t.Fatalf("intOr(%v) = %d want %d", in, got, want)
		}
	}
}

// Mirrors a local terminal config from a fresh working directory with the inherited gateway marker set
func mirrorFromGatewayShell(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("_ICEBATH_GATEWAY", "1")
	t.Setenv("TERMINAL_CWD", "/gateway/terminal/cwd")
	mirrorConfigToEnv(map[string]any{"terminal": map[string]any{"backend": "local", "cwd": "/configured/cwd"}}, true)
	cwd, _ := os.Getwd()
	return os.Getenv("TERMINAL_CWD"), cwd
}

func TestInheritedGatewayMarkerDoesNotKeepTheGatewayCwd(t *testing.T) {
	gatewayProcess.Store(false)
	if got, want := mirrorFromGatewayShell(t); got != want {
		t.Fatalf("TERMINAL_CWD %q want %q", got, want)
	}
}

func TestGatewayProcessKeepsItsBridgedCwd(t *testing.T) {
	MarkGatewayProcess()
	t.Cleanup(func() { gatewayProcess.Store(false) })
	if got, _ := mirrorFromGatewayShell(t); got != "/gateway/terminal/cwd" {
		t.Fatalf("TERMINAL_CWD %q", got)
	}
	if !InGatewayProcess() {
		t.Fatal("marker lost")
	}
}

// Writes a home config.yaml and loads the CLI config fresh from it
func loadFromYAML(t *testing.T, yaml string) map[string]any {
	t.Helper()
	home := tempHome(t)
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte(yaml), 0o600)
	return LoadCLIConfig(context.Background())
}

func TestRootLevelProviderOverrideModelProviderWinsOverRootProvider(t *testing.T) {
	cfg := loadFromYAML(t, "provider: opencode-go\nmodel:\n  default: google/gemini-3-flash-preview\n  provider: openrouter\n")
	if sectionOf(cfg, "model")["provider"] != "openrouter" {
		t.Fatalf("model %v", cfg["model"])
	}
}

func TestRootLevelProviderOverrideRootProviderUsedAsFallbackWhenModelProviderMissing(t *testing.T) {
	cfg := loadFromYAML(t, "provider: opencode-go\nmodel:\n  default: google/gemini-3-flash-preview\n")
	if sectionOf(cfg, "model")["provider"] != "opencode-go" {
		t.Fatalf("model %v", cfg["model"])
	}
}

func TestRootLevelProviderOverrideRootBaseUrlUsedAsFallbackWhenModelBaseUrlMissing(t *testing.T) {
	cfg := loadFromYAML(t, "base_url: https://example.com/v1\nmodel:\n  default: google/gemini-3-flash-preview\n")
	if sectionOf(cfg, "model")["base_url"] != "https://example.com/v1" {
		t.Fatalf("model %v", cfg["model"])
	}
}

func TestRootLevelProviderOverrideTerminalVercelRuntimeBridgedToEnv(t *testing.T) {
	t.Setenv("TERMINAL_VERCEL_RUNTIME", "")
	os.Unsetenv("TERMINAL_VERCEL_RUNTIME")
	cfg := loadFromYAML(t, "terminal:\n  backend: vercel_sandbox\n  vercel_runtime: python3.13\n")
	if sectionOf(cfg, "terminal")["vercel_runtime"] != "python3.13" || os.Getenv("TERMINAL_VERCEL_RUNTIME") != "python3.13" {
		t.Fatalf("terminal %v env %q", cfg["terminal"], os.Getenv("TERMINAL_VERCEL_RUNTIME"))
	}
}
