package gateway

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

func writeBytes(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func quickPrompt(t *testing.T, cfg *GatewayConfig, name string) any {
	t.Helper()
	cmd, ok := cfg.QuickCommands[name].(map[string]any)
	if !ok {
		t.Fatalf("quick command %s missing from %v", name, cfg.QuickCommands)
	}
	return cmd["prompt"]
}

func TestGatewayFileLayersPreserveUnicodeAndFallback(t *testing.T) {
	for _, bom := range []string{"", "\xef\xbb\xbf"} {
		ctx, h := yamlHome(t, "")
		t.Setenv("ICEBATH_HOME", h)
		legacy, yamlPath := filepath.Join(h, "gateway.json"), filepath.Join(h, "config.yaml")
		writeBytes(t, legacy, bom+`{"reset_triggers": ["/départ"], "quick_commands": {"salut": {"type": "prompt", "prompt": "héritage 世界"}}}`)
		cfg := loadFull(t, ctx)
		if !reflect.DeepEqual(cfg.ResetTriggers, []any{"/départ"}) || quickPrompt(t, cfg, "salut") != "héritage 世界" {
			t.Fatalf("legacy layer %v %v", cfg.ResetTriggers, cfg.QuickCommands)
		}
		writeBytes(t, yamlPath, bom+"quick_commands:\n  salut:\n    type: prompt\n    prompt: bonjour 世界\n")
		cfg = loadFull(t, ctx)
		if !reflect.DeepEqual(cfg.ResetTriggers, []any{"/départ"}) || quickPrompt(t, cfg, "salut") != "bonjour 世界" {
			t.Fatalf("yaml layer %v %v", cfg.ResetTriggers, cfg.QuickCommands)
		}
		writeBytes(t, yamlPath, bom+"quick_commands: [")
		if got := quickPrompt(t, loadFull(t, ctx), "salut"); got != "héritage 世界" {
			t.Fatalf("broken yaml falls back to legacy, got %v", got)
		}
		writeBytes(t, legacy, bom+"{broken")
		if got := loadFull(t, ctx).QuickCommands; len(got) != 0 {
			t.Fatalf("broken legacy %v", got)
		}
	}
}

func mustPlatformConfig(t *testing.T, raw any) *PlatformConfig {
	t.Helper()
	pc, err := PlatformConfigFromDict(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return pc
}

func mustGatewayConfig(t *testing.T, raw any) *GatewayConfig {
	t.Helper()
	cfg, err := GatewayConfigFromDict(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestHomeChannelRoundtrip(t *testing.T) {
	hc := NewHomeChannel(PlatformDiscord, "999", "general")
	hc.UserID, hc.ScopeID = ptr("user-123"), ptr("guild-456")
	restored, err := HomeChannelFromDict(context.Background(), hc.ToDict())
	if err != nil {
		t.Fatal(err)
	}
	if restored.Platform != PlatformDiscord || restored.ChatID != "999" || restored.Name != "general" ||
		*restored.UserID != "user-123" || *restored.ScopeID != "guild-456" {
		t.Fatalf("%+v", restored)
	}
}

func TestPlatformConfigRoundtrip(t *testing.T) {
	pc := mustPlatformConfig(t, map[string]any{
		"enabled": true, "reply_to_mode": "all", "typing_indicator": false,
		"port": 9100, "routes": map[string]any{"gh": map[string]any{"prompt": "x"}}, "extra": map[string]any{"port": 9999},
	})
	want := map[string]any{"port": 9999, "routes": map[string]any{"gh": map[string]any{"prompt": "x"}}}
	if !reflect.DeepEqual(pc.Extra, want) || pc.ReplyToMode != "all" || pc.TypingIndicator {
		t.Fatalf("toplevel keys %+v", pc)
	}
	if again := mustPlatformConfig(t, pc.ToDict()); !reflect.DeepEqual(again.Extra, pc.Extra) {
		t.Fatalf("extra roundtrip %v", again.Extra)
	}

	pc = NewPlatformConfig()
	pc.Enabled, pc.Token, pc.HomeChannel, pc.Extra = true, "tok_123", NewHomeChannel(PlatformTelegram, "555", "Home"), map[string]any{"foo": "bar"}
	restored := mustPlatformConfig(t, pc.ToDict())
	if !restored.Enabled || restored.Token != "tok_123" || restored.HomeChannel.ChatID != "555" || !reflect.DeepEqual(restored.Extra, map[string]any{"foo": "bar"}) {
		t.Fatalf("to_dict from_dict %+v", restored)
	}

	if mustPlatformConfig(t, map[string]any{"enabled": "false"}).Enabled {
		t.Fatal("quoted false enabled")
	}

	pc = NewPlatformConfig()
	pc.Enabled, pc.GatewayRestartNotification = true, false
	if mustPlatformConfig(t, pc.ToDict()).GatewayRestartNotification {
		t.Fatal("gateway_restart_notification false lost")
	}

	if got := mustPlatformConfig(t, map[string]any{"extra": map[string]any{"typing_status_text": "chasing yarn…"}}).TypingStatusText; got != "chasing yarn…" {
		t.Fatalf("typing status text %v", got)
	}

	pc = NewPlatformConfig()
	pc.Enabled = true
	pc.ChannelOverrides = map[string]*ChannelOverride{
		"1234567890": {Model: "openrouter/healer-alpha", Provider: "openrouter", SystemPrompt: "You are a daily news summarizer."},
		"9876543210": {Model: "anthropic/claude-opus-4.6", Provider: "anthropic", SystemPrompt: "You are a coding assistant."},
	}
	d := pc.ToDict()
	overrides, ok := d["channel_overrides"].(map[string]any)
	if !ok || overrides["1234567890"].(map[string]any)["model"] != "openrouter/healer-alpha" ||
		overrides["9876543210"].(map[string]any)["system_prompt"] != "You are a coding assistant." {
		t.Fatalf("channel overrides dict %v", d["channel_overrides"])
	}
	restored = mustPlatformConfig(t, d)
	if restored.ChannelOverrides["1234567890"].Model != "openrouter/healer-alpha" || restored.ChannelOverrides["9876543210"].Provider != "anthropic" {
		t.Fatalf("channel overrides restored %+v", restored.ChannelOverrides)
	}
}

func TestChannelOverrideFromEmptyDict(t *testing.T) {
	if ChannelOverrideFromDict(map[string]any{}).Model != nil || ChannelOverrideFromDict(nil).Model != nil {
		t.Fatal("empty override has a model")
	}
}

func TestPlatformConfigIgnoresMalformedNestedSections(t *testing.T) {
	pc := mustPlatformConfig(t, map[string]any{"enabled": true, "home_channel": "telegram:123", "extra": "oops"})
	if !pc.Enabled || pc.HomeChannel != nil || len(pc.Extra) != 0 {
		t.Fatalf("%+v", pc)
	}
}

func TestGetConnectedPlatformsNeedsEnabledAndToken(t *testing.T) {
	ctx, _ := yamlHome(t, "")
	cfg := DefaultGatewayConfig(ctx)
	tg, dc, sl := NewPlatformConfig(), NewPlatformConfig(), NewPlatformConfig()
	tg.Enabled, tg.Token = true, "t"
	dc.Token = "d"
	sl.Enabled = true
	cfg.Platforms = map[Platform]*PlatformConfig{PlatformTelegram: tg, PlatformDiscord: dc, PlatformSlack: sl}
	got := map[Platform]bool{}
	for _, p := range cfg.GetConnectedPlatforms(ctx) {
		got[p] = true
	}
	if !got[PlatformTelegram] || got[PlatformDiscord] || got[PlatformSlack] {
		t.Fatalf("%v", got)
	}
}

func TestStreamingConfigMalformedNumbersFallBackToDefaults(t *testing.T) {
	restored := StreamingConfigFromDict(map[string]any{"edit_interval": "oops", "buffer_threshold": "oops", "fresh_final_after_seconds": "oops"})
	defaults := DefaultStreamingConfig()
	if restored.EditInterval != defaults.EditInterval || restored.BufferThreshold.Cmp(defaults.BufferThreshold) != 0 ||
		restored.FreshFinalAfterSeconds != defaults.FreshFinalAfterSeconds {
		t.Fatalf("%+v", restored)
	}
}

func TestGatewayConfigDictRoundtrip(t *testing.T) {
	for _, raw := range []any{nil, 0, -1, true, 1.5, math.NaN(), math.Inf(1), "120.0", "1e3", "bad", new(big.Int).SetInt64(2_147_483_648)} {
		if got := mustGatewayConfig(t, map[string]any{"systemd_watchdog_seconds": raw}).SystemdWatchdogSeconds; got != 0 {
			t.Fatalf("watchdog %v -> %d", raw, got)
		}
	}
	if got := mustGatewayConfig(t, map[string]any{"max_concurrent_sessions": "many"}).MaxConcurrentSessions; got != nil {
		t.Fatalf("max concurrent sessions %v", got)
	}

	cfg := DefaultGatewayConfig(context.Background())
	wa := NewPlatformConfig()
	wa.Enabled, wa.Extra = true, map[string]any{"unauthorized_dm_behavior": "pair"}
	cfg.UnauthorizedDMBehavior, cfg.Platforms = "ignore", map[Platform]*PlatformConfig{PlatformWhatsApp: wa}
	restored := mustGatewayConfig(t, cfg.ToDict())
	if restored.UnauthorizedDMBehavior != "ignore" || restored.Platforms[PlatformWhatsApp].Extra["unauthorized_dm_behavior"] != "pair" {
		t.Fatalf("unauthorized dm behavior %+v", restored)
	}

	email := NewPlatformConfig()
	email.Enabled = true
	cfg = DefaultGatewayConfig(context.Background())
	cfg.Platforms = map[Platform]*PlatformConfig{PlatformEmail: email}
	if got := cfg.GetUnauthorizedDMBehavior(PlatformEmail); got != "ignore" {
		t.Fatalf("email default %s", got)
	}
	email.Extra = map[string]any{"unauthorized_dm_behavior": "pair"}
	if got := cfg.GetUnauthorizedDMBehavior(PlatformEmail); got != "pair" {
		t.Fatalf("email opt in %s", got)
	}
}

// Loads the gateway config of a fresh home whose config.yaml holds the text
func loadYAML(t *testing.T, text string) *GatewayConfig {
	t.Helper()
	ctx, h := yamlHome(t, text)
	t.Setenv("ICEBATH_HOME", h)
	return loadFull(t, ctx)
}

func clearEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

func clearAPIServerEnv(t *testing.T) {
	clearEnv(t, "API_SERVER_ENABLED", "API_SERVER_KEY", "API_SERVER_PORT", "API_SERVER_HOST", "API_SERVER_CORS_ORIGINS", "API_SERVER_MODEL_NAME")
}

func TestLoadGatewayConfigExpandsAndBridges(t *testing.T) {
	t.Setenv("WEBHOOK_SECRET", "whsec-expanded")
	t.Setenv("API_SERVER_KEY", "server-key-expanded")
	t.Setenv("HOOK_PREFIX_FOR_TEST", "/hooks/expanded")
	cfg := loadYAML(t, "platforms:\n  webhook:\n    enabled: true\n    port: 8089\n    secret: ${WEBHOOK_SECRET}\n    path_prefix: ${HOOK_PREFIX_FOR_TEST}\n  api_server:\n    enabled: true\n    key: ${env:API_SERVER_KEY}\n")
	if wh := cfg.Platforms[PlatformWebhook].Extra; wh["secret"] != "whsec-expanded" || wh["path_prefix"] != "/hooks/expanded" || cfg.Platforms[PlatformAPIServer].Extra["key"] != "server-key-expanded" {
		t.Fatalf("expanded refs %v %v", wh, cfg.Platforms[PlatformAPIServer].Extra)
	}

	clearEnv(t, "WEBHOOK_SECRET_UNSET_FOR_TEST")
	cfg = loadYAML(t, "platforms:\n  webhook:\n    enabled: true\n    secret: ${WEBHOOK_SECRET_UNSET_FOR_TEST}\n")
	if got := cfg.Platforms[PlatformWebhook].Extra["secret"]; got != "${WEBHOOK_SECRET_UNSET_FOR_TEST}" {
		t.Fatalf("unresolved ref %v", got)
	}

	cfg = loadYAML(t, "platforms:\n  slack:\n    enabled: true\n    typing_status_text: \"chasing yarn…\"\n")
	if got := cfg.Platforms[PlatformSlack].TypingStatusText; got != "chasing yarn…" {
		t.Fatalf("typing status text %v", got)
	}

}

func TestLoadGatewayConfigNestedGatewaySection(t *testing.T) {
	if cfg := loadYAML(t, "gateway:\n  multiplex_profiles: true\n"); cfg.MultiplexProfiles == nil || !*cfg.MultiplexProfiles {
		t.Fatal("nested multiplex_profiles")
	}
	if cfg := loadYAML(t, "gateway:\n  multiplex_profiles: true\n  multiplex_profile_allowlist:\n    - worker\n"); cfg.MultiplexProfiles == nil || !*cfg.MultiplexProfiles {
		t.Fatal("stale allowlist key")
	}
	if cfg := loadYAML(t, "gateway:\n  quick_commands:\n    limits:\n      type: exec\n      command: echo ok\n"); !reflect.DeepEqual(cfg.QuickCommands, map[string]any{"limits": map[string]any{"type": "exec", "command": "echo ok"}}) {
		t.Fatalf("quick commands %v", cfg.QuickCommands)
	}
	if cfg := loadYAML(t, "gateway:\n  stt:\n    enabled: false\n"); cfg.STTEnabled {
		t.Fatal("nested stt")
	}
	clearAPIServerEnv(t)
	if cfg := loadYAML(t, "gateway:\n  api_server:\n    enabled: true\n"); cfg.Platforms[PlatformAPIServer] == nil || !cfg.Platforms[PlatformAPIServer].Enabled {
		t.Fatal("nested api_server")
	}
	cfg := loadYAML(t, "gateway:\n  api_server:\n    enabled: true\n    port: 8642\n    host: 0.0.0.0\n    key: sekrit\n    model_name: my-hermes\n")
	if extra := cfg.Platforms[PlatformAPIServer].Extra; extra["port"] != 8642 || extra["host"] != "0.0.0.0" || extra["key"] != "sekrit" || extra["model_name"] != "my-hermes" {
		t.Fatalf("api server extra %v", extra)
	}
	cfg = loadYAML(t, "gateway:\n  room_link_url: https://peer.example.test/hermes\n")
	if cfg.RoomLinkURL == nil || *cfg.RoomLinkURL != "https://peer.example.test/hermes" {
		t.Fatalf("room link %v", cfg.RoomLinkURL)
	}
	if again := mustGatewayConfig(t, cfg.ToDict()); again.RoomLinkURL == nil || *again.RoomLinkURL != "https://peer.example.test/hermes" {
		t.Fatal("room link roundtrip")
	}
	cfg = loadYAML(t, "gateway:\n  streaming:\n    enabled: false\n  timeout: 120\n  api_server:\n    enabled: true\n")
	if cfg.Streaming.Enabled || !cfg.Platforms[PlatformAPIServer].Enabled {
		t.Fatalf("non platform keys %+v", cfg.Platforms)
	}
	for p := range cfg.Platforms {
		if _, err := ParsePlatform(context.Background(), string(p)); err != nil {
			t.Fatalf("phantom platform %s", p)
		}
	}
	if loadYAML(t, "gateway:\n  group_sessions_per_user: false\n").GroupSessionsPerUser {
		t.Fatal("group_sessions_per_user")
	}
	if got := loadYAML(t, "gateway:\n  reset_triggers:\n    - /new\n    - /clear\n").ResetTriggers; !reflect.DeepEqual(got, []any{"/new", "/clear"}) {
		t.Fatalf("reset triggers %v", got)
	}
	if loadYAML(t, "gateway:\n  always_log_local: false\n").AlwaysLogLocal {
		t.Fatal("always_log_local")
	}
	if got := loadYAML(t, "gateway:\n  unauthorized_dm_behavior: ignore\n").UnauthorizedDMBehavior; got != "ignore" {
		t.Fatalf("unauthorized dm behavior %s", got)
	}
}

// Loads the config with the process marked a multiplexer and the secret scope bound on the context
func loadScoped(t *testing.T, ctx context.Context, secrets map[string]string) *GatewayConfig {
	t.Helper()
	provider.SetMultiplexActive(true)
	defer provider.SetMultiplexActive(false)
	return loadFull(t, provider.WithSecretScope(ctx, secrets, ""))
}

func TestRelayExclusiveReadsProfileScopedEnv(t *testing.T) {
	ctx, h := yamlHome(t, telegramYAML)
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("GATEWAY_RELAY_URL", "https://deploy.example/relay")
	profileDir := t.TempDir()
	writeBytes(t, filepath.Join(profileDir, ".env"), "")
	cfg := loadScoped(t, ctx, provider.BuildProfileSecretScope(profileDir))
	if relay := cfg.Platforms[PlatformRelay]; relay == nil || !relay.Enabled || cfg.Platforms[PlatformTelegram].Enabled {
		t.Fatalf("process stamp %+v %+v", relay, cfg.Platforms[PlatformTelegram])
	}
	clearEnv(t, "GATEWAY_RELAY_URL")
	writeBytes(t, filepath.Join(profileDir, ".env"), "GATEWAY_RELAY_URL=https://profile.example/relay\n")
	cfg = loadScoped(t, ctx, provider.BuildProfileSecretScope(profileDir))
	if relay := cfg.Platforms[PlatformRelay]; (relay != nil && relay.Enabled) || !cfg.Platforms[PlatformTelegram].Enabled {
		t.Fatalf("profile stamp %+v %+v", relay, cfg.Platforms[PlatformTelegram])
	}
}

func TestLoadGatewayConfigPlatformLayers(t *testing.T) {
	cfg := loadYAML(t, "gateway:\n  platforms:\n    telegram:\n      enabled: false\n      token: nested-token\n      extra:\n        reply_prefix: nested\nplatforms:\n  telegram:\n    enabled: true\n    token: top-token\n    extra:\n      reply_prefix: top\n")
	if tg := cfg.Platforms[PlatformTelegram]; !tg.Enabled || tg.Token != "top-token" || tg.Extra["reply_prefix"] != "top" {
		t.Fatalf("top level wins %+v", tg)
	}
	cfg = loadYAML(t, "platforms:\n  telegram:\n    allow_from:\n      - \"111222333\"\n      - \"444555666\"\n    require_mention: true\n")
	if tg := cfg.Platforms[PlatformTelegram]; !reflect.DeepEqual(tg.Extra["allow_from"], []any{"111222333", "444555666"}) || tg.Extra["require_mention"] != true {
		t.Fatalf("shared key loop %v", tg.Extra)
	}
	cfg = loadYAML(t, "unauthorized_dm_behavior: ignore\nwhatsapp:\n  unauthorized_dm_behavior: pair\n")
	if cfg.UnauthorizedDMBehavior != "ignore" || cfg.Platforms[PlatformWhatsApp].Extra["unauthorized_dm_behavior"] != "pair" {
		t.Fatalf("unauthorized dm bridge %s %v", cfg.UnauthorizedDMBehavior, cfg.Platforms[PlatformWhatsApp])
	}
	cfg = loadYAML(t, "gateway:\n  platforms:\n    telegram:\n      extra:\n        rich_messages: false\n")
	if got, ok := cfg.Platforms[PlatformTelegram].Extra["rich_messages"]; !ok || got != false {
		t.Fatalf("rich messages %v", cfg.Platforms[PlatformTelegram].Extra)
	}
}

func TestProfileScopedEnvOverridesDoNotFallBackToDefaultProfileEnv(t *testing.T) {
	defaultHome, secondary := t.TempDir(), t.TempDir()
	writeBytes(t, filepath.Join(defaultHome, "config.yaml"), "multiplex_profiles: true\n")
	writeBytes(t, filepath.Join(secondary, "config.yaml"), "multiplex_profiles: true\n")
	t.Setenv("ICEBATH_HOME", defaultHome)
	t.Setenv("API_SERVER_ENABLED", "true")
	t.Setenv("DISCORD_BOT_TOKEN", "default-token")
	cfg := loadScoped(t, home.WithOverride(context.Background(), secondary), map[string]string{"DISCORD_BOT_TOKEN": "worker-token"})
	if cfg.MultiplexProfiles == nil || !*cfg.MultiplexProfiles || cfg.Platforms[PlatformDiscord].Token != "worker-token" {
		t.Fatalf("scoped token %+v", cfg.Platforms[PlatformDiscord])
	}
	if _, ok := cfg.Platforms[PlatformAPIServer]; ok {
		t.Fatal("api_server enabled from the default profile env")
	}
}

func TestMultiplexProfilesEnvAndConfigPrecedence(t *testing.T) {
	multiplexed := func(cfg *GatewayConfig) any {
		if cfg.MultiplexProfiles == nil {
			return nil
		}
		return *cfg.MultiplexProfiles
	}
	t.Setenv("GATEWAY_MULTIPLEX_PROFILES", "1")
	if got := multiplexed(loadYAML(t, "multiplex_profiles: false\n")); got != true {
		t.Fatalf("env true over config false: %v", got)
	}
	clearEnv(t, "GATEWAY_MULTIPLEX_PROFILES")
	if got := multiplexed(loadYAML(t, "multiplex_profiles: true\n")); got != true {
		t.Fatalf("config true with env unset: %v", got)
	}
	t.Setenv("GATEWAY_MULTIPLEX_PROFILES", "")
	if got := multiplexed(loadYAML(t, "multiplex_profiles: true\n")); got != true {
		t.Fatalf("empty env shadows config: %v", got)
	}
	clearEnv(t, "GATEWAY_MULTIPLEX_PROFILES")
	if got := multiplexed(loadYAML(t, "gateway:\n  multiplex_profiles: true\n")); got != true {
		t.Fatalf("nested under gateway: %v", got)
	}
	if got := multiplexed(loadYAML(t, "multiplex_profiles: false\ngateway:\n  multiplex_profiles: true\n")); got != false {
		t.Fatalf("explicit top level false: %v", got)
	}
}

func TestOnAllAdaptersDown(t *testing.T) {
	clearEnv(t, "GATEWAY_ON_ALL_ADAPTERS_DOWN")
	if got := mustGatewayConfig(t, map[string]any{}).OnAllAdaptersDown; got != "exit" {
		t.Fatalf("default %s", got)
	}
	cfg := mustGatewayConfig(t, map[string]any{"gateway": map[string]any{"on_all_adapters_down": "stay_alive"}})
	if cfg.OnAllAdaptersDown != "stay_alive" || mustGatewayConfig(t, cfg.ToDict()).OnAllAdaptersDown != "stay_alive" {
		t.Fatalf("yaml value %s", cfg.OnAllAdaptersDown)
	}
	if got := mustGatewayConfig(t, map[string]any{"gateway": map[string]any{"on_all_adapters_down": "yolo"}}).OnAllAdaptersDown; got != "exit" {
		t.Fatalf("unrecognized %s", got)
	}
	exitYAML := map[string]any{"gateway": map[string]any{"on_all_adapters_down": "exit"}}
	t.Setenv("GATEWAY_ON_ALL_ADAPTERS_DOWN", "stay_alive")
	if got := mustGatewayConfig(t, exitYAML).OnAllAdaptersDown; got != "stay_alive" {
		t.Fatalf("env wins %s", got)
	}
	t.Setenv("GATEWAY_ON_ALL_ADAPTERS_DOWN", "whatever")
	if got := mustGatewayConfig(t, exitYAML).OnAllAdaptersDown; got != "exit" {
		t.Fatalf("invalid env %s", got)
	}
	clearEnv(t, "GATEWAY_ON_ALL_ADAPTERS_DOWN")
	if got := mustGatewayConfig(t, map[string]any{}).OnAllAdaptersDown; got != "exit" {
		t.Fatalf("env removed %s", got)
	}
}

func TestPlatformBlockWithoutBridgedKeysStaysOutOfPlatforms(t *testing.T) {
	ctx, h := yamlHome(t, `{"slack": {"reply_to_mode": "all"}}`)
	t.Setenv("ICEBATH_HOME", h)
	data := map[string]any{}
	if err := LoadYAMLLayer(ctx, h, data); err != nil {
		t.Fatal(err)
	}
	if platforms, _ := data["platforms"].(map[string]any); platforms != nil {
		if _, ok := platforms["slack"]; ok {
			t.Fatalf("slack bridged %v", platforms)
		}
	}
}

func TestManagedDictExtraMergesWithAuthoredDict(t *testing.T) {
	for _, overlap := range []bool{false, true} {
		h, managed := t.TempDir(), t.TempDir()
		user := `{"platforms": {"slack": {"enabled": true, "extra": {"channel_prompts": {"C_USER": "user prompt"}}}}}`
		if overlap {
			user = `{"platforms": {"slack": {"enabled": true, "extra": {"channel_prompts": {"C_USER": "user prompt", "C_ADMIN": "user override"}}}}}`
		}
		writeBytes(t, filepath.Join(h, "config.yaml"), user)
		writeBytes(t, filepath.Join(managed, "config.yaml"), `{"platforms": {"slack": {"extra": {"channel_prompts": {"C_ADMIN": "admin prompt"}}}}}`)
		t.Setenv("ICEBATH_HOME", h)
		t.Setenv("ICEBATH_MANAGED_DIR", managed)
		config.InvalidateManagedCache()
		cfg := loadFull(t, home.WithOverride(context.Background(), h))
		want := map[string]any{"C_USER": "user prompt", "C_ADMIN": "admin prompt"}
		if got := cfg.Platforms[PlatformSlack].Extra["channel_prompts"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("overlap %v: %v", overlap, got)
		}
	}
	config.InvalidateManagedCache()
}

func TestExplicitDisableWithoutEnvCredentialsIsSilent(t *testing.T) {
	logs := captureLogs(t)
	clearEnv(t, "WEIXIN_TOKEN", "WEIXIN_ACCOUNT_ID")
	resetExplicitDisableWarned(t)
	cfg := loadYAML(t, "platforms:\n  weixin:\n    enabled: false\n")
	if cfg.Platforms[PlatformWeixin] == nil || cfg.Platforms[PlatformWeixin].Enabled {
		t.Fatalf("weixin %+v", cfg.Platforms[PlatformWeixin])
	}
	if strings.Contains(logs.String(), "explicitly disabled") {
		t.Fatalf("warned %q", logs.String())
	}
}

// Forgets the once per process explicit disable notices for the length of the test
func resetExplicitDisableWarned(t *testing.T) {
	t.Helper()
	explicitDisableWarned.Lock()
	saved := explicitDisableWarned.set
	explicitDisableWarned.set = map[Platform]bool{}
	explicitDisableWarned.Unlock()
	t.Cleanup(func() {
		explicitDisableWarned.Lock()
		explicitDisableWarned.set = saved
		explicitDisableWarned.Unlock()
	})
}

func status(t *testing.T, ctx context.Context, id string, cfg map[string]any) [2]bool {
	t.Helper()
	enabled, connected, ok, err := BuiltinPlatformStatus(ctx, id, cfg)
	if err != nil || !ok {
		t.Fatalf("%s status ok=%v err=%v", id, ok, err)
	}
	return [2]bool{enabled, connected}
}

func TestBuiltinPlatformStatus(t *testing.T) {
	clearEnv(t, "GATEWAY_RELAY_URL", "GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS", "API_SERVER_KEY", "API_SERVER_ENABLED")
	config.InvalidateManagedCache()
	t.Cleanup(config.InvalidateManagedCache)
	ctx, h := yamlHome(t, "")
	t.Setenv("ICEBATH_HOME", h)
	if _, _, ok, err := BuiltinPlatformStatus(ctx, "workchat", map[string]any{"platforms": map[string]any{"workchat": map[string]any{"enabled": true}}}); ok || err != nil {
		t.Fatal("plugin only id judged")
	}
	callers := map[string]any{"platforms": map[string]any{"webhook": map[string]any{"extra": map[string]any{}}}, "webhook": map[string]any{"enabled": true, "port": 9}}
	before := pyrepr.Repr(callers)
	BuiltinPlatformStatus(ctx, "webhook", callers)
	if after := pyrepr.Repr(callers); after != before {
		t.Fatalf("caller config changed %s -> %s", before, after)
	}
	key := strings.Repeat("k", 32)
	t.Setenv("API_SERVER_KEY", key)
	off := map[string]any{"enabled": false}
	for _, cfg := range []map[string]any{
		{"platforms": map[string]any{"api_server": off}},
		{"gateway": map[string]any{"platforms": map[string]any{"api_server": off}}},
		{"api_server": off},
	} {
		if got := status(t, ctx, "api_server", cfg); got != [2]bool{false, true} {
			t.Fatalf("%v -> %v", cfg, got)
		}
		if got := status(t, ctx, "api_server", map[string]any{}); got != [2]bool{true, true} {
			t.Fatalf("env key alone -> %v", got)
		}
	}
	clearEnv(t, "API_SERVER_KEY")

	managed := t.TempDir()
	writeBytes(t, filepath.Join(managed, "config.yaml"), `{"api_server": {"key": "`+key+`"}}`)
	t.Setenv("ICEBATH_MANAGED_DIR", managed)
	config.InvalidateManagedCache()
	user := `{"platforms": {"api_server": {"enabled": true, "extra": {"key": ""}}}}`
	var userDoc map[string]any
	if err := json.Unmarshal([]byte(user), &userDoc); err != nil {
		t.Fatal(err)
	}
	if got := status(t, ctx, "api_server", config.ApplyManagedOverlay(userDoc)); got != [2]bool{true, true} {
		t.Fatalf("pinned key -> %v", got)
	}
	writeBytes(t, filepath.Join(h, "config.yaml"), user)
	cfg := loadFull(t, ctx)
	api := cfg.Platforms[PlatformAPIServer]
	if !api.Enabled || !cfg.isPlatformConnected(ctx, PlatformAPIServer, api) {
		t.Fatalf("loaded api_server %+v", api)
	}
}
