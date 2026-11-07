package gateway

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Captures every record at INFO and above for the length of the test
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func loadFull(t *testing.T, ctx context.Context) *GatewayConfig {
	t.Helper()
	cfg, err := LoadGatewayConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

const telegramYAML = "gateway:\n  platforms:\n    telegram:\n      enabled: true\n      bot_token: '123:abc'\n"

func TestRelayPlatformEnabledFromEnvURL(t *testing.T) {
	ctx, _ := yamlHome(t, "")
	t.Setenv("GATEWAY_RELAY_URL", "https://connector.example/relay/")
	cfg := loadFull(t, ctx)
	relay := cfg.Platforms[PlatformRelay]
	if relay == nil || !relay.Enabled || relay.Extra["relay_url"] != "https://connector.example/relay" {
		t.Fatalf("relay %+v", relay)
	}
	found := false
	for _, p := range cfg.GetConnectedPlatforms(ctx) {
		found = found || p == PlatformRelay
	}
	if !found {
		t.Fatal("relay connected")
	}
}

func TestRelayEnvURLDisablesOtherMessagingPlatforms(t *testing.T) {
	ctx, _ := yamlHome(t, telegramYAML+"    api_server:\n      enabled: true\n")
	t.Setenv("GATEWAY_RELAY_URL", "https://connector.example/relay")
	t.Setenv("DISCORD_BOT_TOKEN", "fake-token-for-test")
	cfg := loadFull(t, ctx)
	if !cfg.Platforms[PlatformRelay].Enabled || cfg.Platforms[PlatformTelegram].Enabled || !cfg.Platforms[PlatformAPIServer].Enabled {
		t.Fatal("relay exclusive sweep")
	}
	if d := cfg.Platforms[PlatformDiscord]; d != nil && d.Enabled {
		t.Fatal("discord stays off")
	}
}

func TestRelayYAMLURLKeepsOtherPlatformsEnabled(t *testing.T) {
	ctx, _ := yamlHome(t, "gateway:\n  platforms:\n    relay:\n      enabled: true\n      extra:\n        relay_url: https://connector.example/relay\n"+
		"    telegram:\n      enabled: true\n      bot_token: '123:abc'\n")
	os.Unsetenv("GATEWAY_RELAY_URL")
	cfg := loadFull(t, ctx)
	if !cfg.Platforms[PlatformRelay].Enabled || !cfg.Platforms[PlatformTelegram].Enabled {
		t.Fatal("yaml relay is additive")
	}
}

func TestRelayEnvURLOptOutKeepsDirectPlatforms(t *testing.T) {
	ctx, _ := yamlHome(t, telegramYAML)
	t.Setenv("GATEWAY_RELAY_URL", "https://connector.example/relay")
	t.Setenv("GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS", "true")
	cfg := loadFull(t, ctx)
	if !cfg.Platforms[PlatformRelay].Enabled || !cfg.Platforms[PlatformTelegram].Enabled {
		t.Fatal("opt out keeps direct platforms")
	}
}

func TestRelayExclusiveSweepLogLevelsAndMarkerCleanup(t *testing.T) {
	ctx, h := yamlHome(t, telegramYAML+"    slack:\n      enabled: true\n      bot_token: x\n")
	t.Setenv("GATEWAY_RELAY_URL", "https://connector.example/relay")
	logs := captureLogs(t)
	cfg := loadLayers(t, ctx, h)
	cfg.Platforms[PlatformSlack].Extra = map[string]any{}
	applyEnvOverrides(ctx, cfg, "")
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "platform 'telegram' even though") ||
		!strings.Contains(out, "level=INFO msg=\"Relay connector is configured via GATEWAY_RELAY_URL; disabling directly-connected platform 'slack'.\"") {
		t.Fatalf("sweep logs %s", out)
	}
	for p, pc := range cfg.Platforms {
		if _, ok := pc.Extra["_enabled_explicit"]; ok {
			t.Fatalf("marker survived on %s", p)
		}
	}
}

func TestEnvKeyDoesNotReenableExplicitlyDisabledListeners(t *testing.T) {
	ctx, _ := yamlHome(t, "")
	explicitOff := func() *PlatformConfig {
		pc := NewPlatformConfig()
		pc.Extra["_enabled_explicit"] = true
		return pc
	}
	cfg := DefaultGatewayConfig(ctx)
	cfg.Platforms = map[Platform]*PlatformConfig{PlatformAPIServer: explicitOff(), PlatformWebhook: explicitOff(), PlatformMSGraphWebhook: explicitOff()}
	t.Setenv("API_SERVER_KEY", "secret-key-at-least-16")
	t.Setenv("WEBHOOK_ENABLED", "true")
	t.Setenv("WEBHOOK_PORT", "9999")
	t.Setenv("WEBHOOK_SECRET", "shared-secret")
	t.Setenv("MSGRAPH_WEBHOOK_ENABLED", "true")
	t.Setenv("MSGRAPH_WEBHOOK_PORT", "9998")
	applyEnvOverrides(ctx, cfg, "")
	api, wh, mg := cfg.Platforms[PlatformAPIServer], cfg.Platforms[PlatformWebhook], cfg.Platforms[PlatformMSGraphWebhook]
	if api.Enabled || api.Extra["key"] != "secret-key-at-least-16" || wh.Enabled || mg.Enabled {
		t.Fatalf("explicit disables hold %+v %+v %+v", api, wh, mg)
	}
	if wh.Extra["port"] != 9999 || wh.Extra["secret"] != "shared-secret" || mg.Extra["port"] != 9998 {
		t.Fatalf("listener extras %v %v", wh.Extra, mg.Extra)
	}
}

func TestConfigEnabledWebhookReadsEnvPortAndSecret(t *testing.T) {
	ctx, _ := yamlHome(t, "platforms:\n  webhook:\n    enabled: true\n    extra:\n      secret: yaml-secret\n")
	t.Setenv("WEBHOOK_PORT", "8650")
	t.Setenv("WEBHOOK_SECRET", "")
	t.Setenv("API_SERVER_CORS_ORIGINS", " a , ,b ")
	t.Setenv("API_SERVER_KEY", "short")
	cfg := loadFull(t, ctx)
	wh := cfg.Platforms[PlatformWebhook]
	if !wh.Enabled || wh.Extra["port"] != 8650 || wh.Extra["secret"] != "yaml-secret" {
		t.Fatalf("webhook %+v", wh.Extra)
	}
	if _, ok := cfg.Platforms[PlatformAPIServer]; ok {
		t.Fatal("an unusable api server key never enables the listener")
	}
}

// Registers a plugin entry into the process registry for the test
func registerPluginEntry(t *testing.T, e *PlatformEntry) {
	t.Helper()
	e.Source = "plugin"
	Registry.register(e, "")
	t.Cleanup(func() { Registry.unregister(context.Background(), e.Name, "") })
}

func TestPluginEnablePassGates(t *testing.T) {
	ctx, _ := yamlHome(t, "")
	resetPlatformMembers(t, t.TempDir())
	notConfigured := NewPlatformEntry("myunconfiguredplat", "U", nil, func() bool { return true })
	notConfigured.IsConnected = func(_ context.Context, cfg *PlatformConfig) (bool, error) { return false, nil }
	notConfigured.EnvEnablementFn = func(ctx context.Context) map[string]any { return map[string]any{"some_key": "should-not-leak"} }
	raising := NewPlatformEntry("myraisingplat", "R", nil, func() bool { return true })
	raising.IsConnected = func(_ context.Context, cfg *PlatformConfig) (bool, error) { return false, errors.New("boom") }
	installable := NewPlatformEntry("myinstallableplat", "I", nil, func() bool { return false })
	installs := 0
	installable.EnsureDepsFn = func(ctx context.Context) (bool, error) { installs++; return true, nil }
	installable.IsConnected = func(_ context.Context, cfg *PlatformConfig) (bool, error) { return cfg.Extra["token"] == "t", nil }
	installable.EnvEnablementFn = func(ctx context.Context) map[string]any {
		return map[string]any{"token": "t", "home_channel": map[string]any{"chat_id": 42, "thread_id": "7"}}
	}
	missing := NewPlatformEntry("mymissingplat", "M", nil, func() bool { return false })
	panicking := NewPlatformEntry("mypanickingplat", "P", nil, func() bool { panic("probe") })
	panicking.EnsureDepsFn = func(ctx context.Context) (bool, error) { return true, nil }
	for _, e := range []*PlatformEntry{notConfigured, raising, installable, missing, panicking} {
		registerPluginEntry(t, e)
	}
	cfg := loadFull(t, ctx)
	for _, name := range []string{"myunconfiguredplat", "myraisingplat", "mymissingplat"} {
		if pc, ok := cfg.Platforms[Platform(name)]; ok {
			t.Fatalf("%s must stay out of config %+v", name, pc)
		}
	}
	pc := cfg.Platforms["myinstallableplat"]
	if pc == nil || !pc.Enabled || pc.Extra["token"] != "t" || installs != 0 || pc.HomeChannel == nil ||
		pc.HomeChannel.ChatID != "42" || pc.HomeChannel.Name != "Home" || *pc.HomeChannel.ThreadID != "7" {
		t.Fatalf("installable enabled with its seed and no install %+v", pc)
	}
	if p := cfg.Platforms["mypanickingplat"]; p == nil || !p.Enabled {
		t.Fatal("a raising check falls through to the installer")
	}
}

func TestBundledEnvRowsRunWithoutTheirPlugin(t *testing.T) {
	ctx, _ := yamlHome(t, "")
	bundled := t.TempDir()
	os.MkdirAll(filepath.Join(bundled, "platforms", "zzchat"), 0o755)
	os.WriteFile(filepath.Join(bundled, "platforms", "zzchat", "plugin.yaml"), []byte("name: zzchat\n"), 0o644)
	resetPlatformMembers(t, bundled)
	RegisterBundledEnv("zzchat", "", BundledEnvRows{
		Overrides: []EnvStep{
			&EnvCred{Platform: "zzchat", Creds: [][]string{{"ZZCHAT_TOKEN"}, {"ZZCHAT_A", "ZZCHAT_B"}}, Token: "ZZCHAT_TOKEN",
				Fixed: []EnvSpec{{Key: "url", Env: "ZZCHAT_URL", Default: "https://x/", Fn: StripSlash}, {Key: "id", AnyOf: []string{"ZZCHAT_A", "ZZCHAT_B"}, Default: "none"},
					{Key: "port", Env: "ZZCHAT_PORT", Default: "", Fn: IntOr(8645)}},
				Optional: []EnvSpec{{Key: "retries", Env: "ZZCHAT_RETRIES", Int: true}}, OptionalStripped: []EnvSpec{{Key: "allow", Env: "ZZCHAT_ALLOW"}},
				Home: "ZZCHAT_HOME"},
			ReplyModeStep{"zzchat", "ZZCHAT_REPLY"},
			HomeStep{"zzchat", "ZZCHAT_HOME2", true},
		},
		EnableCredentials: []string{"ZZCHAT_TOKEN"},
		Authz:             map[string]any{"allowed_users_env": "ZZCHAT_ALLOWED_USERS"},
	})
	t.Cleanup(func() { RegisterBundledEnv("zzchat", "", BundledEnvRows{}) })
	for k, v := range map[string]string{"ZZCHAT_TOKEN": "tok", "ZZCHAT_B": "bee", "ZZCHAT_RETRIES": "x", "ZZCHAT_ALLOW": "  a  ",
		"ZZCHAT_HOME": "h1", "ZZCHAT_HOME_NAME": "Main", "ZZCHAT_REPLY": "ALL", "ZZCHAT_PORT": "nope", "ZZCHAT_HOME2_THREAD_ID": "9"} {
		t.Setenv(k, v)
	}
	cfg := loadFull(t, ctx)
	pc := cfg.Platforms["zzchat"]
	if pc == nil || !pc.Enabled || pc.Token != "tok" || pc.ReplyToMode != "all" || pc.Extra["url"] != "https://x" ||
		pc.Extra["id"] != "bee" || pc.Extra["port"] != 8645 || pc.Extra["allow"] != "a" || pc.HomeChannel.Name != "Main" {
		t.Fatalf("bundled rows %+v %+v", pc, pc.Extra)
	}
	if _, ok := pc.Extra["retries"]; ok {
		t.Fatal("a malformed int is skipped")
	}
	if AuthzField("ZZChat ", "allowed_users_env", nil) != "ZZCHAT_ALLOWED_USERS" || len(enableCredentialNames(ctx, "zzchat")) != 1 {
		t.Fatal("bundled authz and credentials")
	}
}

func TestExplicitDisableWarnsOnceAndStatusReadsStaySilent(t *testing.T) {
	ctx, _ := yamlHome(t, "platforms:\n  relay:\n    enabled: false\n")
	t.Setenv("GATEWAY_RELAY_URL", "https://r")
	logs := captureLogs(t)
	explicitDisableWarned.Lock()
	delete(explicitDisableWarned.set, PlatformRelay)
	explicitDisableWarned.Unlock()
	enabled, connected, ok, err := BuiltinPlatformStatus(ctx, "relay", map[string]any{"platforms": map[string]any{"relay": map[string]any{"enabled": false}}})
	if err != nil || !ok || enabled || !connected || logs.Len() != 0 {
		t.Fatalf("status read %v %v %v %v %q", enabled, connected, ok, err, logs.String())
	}
	if _, _, ok, _ := BuiltinPlatformStatus(ctx, "myplugin", map[string]any{}); ok {
		t.Fatal("plugin only id")
	}
	for range 2 {
		if cfg := loadFull(t, ctx); cfg.Platforms[PlatformRelay].Enabled {
			t.Fatal("explicit disable wins")
		}
	}
	if n := strings.Count(logs.String(), "explicitly disabled by platforms.relay.enabled"); n != 1 || !strings.Contains(logs.String(), "(GATEWAY_RELAY_URL)") {
		t.Fatalf("one warning naming the present credential %q", logs.String())
	}
}

func TestWhatsAppIdentityHelpers(t *testing.T) {
	ctx, h := yamlHome(t, "")
	for in, want := range map[string]string{"6012:47@s.whatsapp.net": "6012", "6012@lid": "6012", "+6012": "6012", " ": ""} {
		if got := NormalizeWhatsAppIdentifier(in); got != want {
			t.Fatalf("normalize %q = %q", in, got)
		}
	}
	for in, want := range map[string]string{"+1 (555) 010-2000": "15550102000@s.whatsapp.net", "12:3@lid": "12@lid", "abc": "abc", "": "", "١٢": "١٢@s.whatsapp.net"} {
		if got := ToWhatsAppJID(in); got != want {
			t.Fatalf("jid %q = %q", in, got)
		}
	}
	if NormalizeWhatsAppMentionJID("15550102000") != "15550102000@s.whatsapp.net" || NormalizeWhatsAppMentionJID("1@g.us") != "" || NormalizeWhatsAppMentionJID("١٢") != "" {
		t.Fatal("mention jid")
	}
	session := filepath.Join(h, "platforms", "whatsapp", "session")
	os.MkdirAll(session, 0o755)
	os.WriteFile(filepath.Join(session, "lid-mapping-15550102000.json"), []byte("\xEF\xBB\xBF\"999@lid\""), 0o644)
	os.WriteFile(filepath.Join(session, "lid-mapping-999_reverse.json"), []byte("\"+15550102000\""), 0o644)
	os.WriteFile(filepath.Join(session, "lid-mapping-12.json"), []byte("not json"), 0o644)
	got, err := expandWhatsAppAliases(ctx, "15550102000@s.whatsapp.net")
	if err != nil || len(got) != 2 || !got["999"] || !got["15550102000"] {
		t.Fatalf("aliases %v %v", got, err)
	}
	if c, _ := CanonicalWhatsAppIdentifier(ctx, "999@lid"); c != "999" {
		t.Fatalf("canonical %q", c)
	}
	if got, err := expandWhatsAppAliases(ctx, "12"); err != nil || len(got) != 1 {
		t.Fatalf("bad json is skipped %v %v", got, err)
	}
	os.WriteFile(filepath.Join(session, "lid-mapping-13.json"), []byte{0xff}, 0o644)
	if _, err := expandWhatsAppAliases(ctx, "13"); err == nil || !strings.Contains(err.Error(), "can't decode byte 0xff") {
		t.Fatalf("invalid utf-8 raises %v", err)
	}
	r := &ProfileRoute{Enabled: true, Platform: "whatsapp", ChatID: ptr("999@lid")}
	if ok, err := r.Matches(ctx, RouteQuery{Platform: "whatsapp", ChatID: ptr("15550102000@s.whatsapp.net")}); !ok || err != nil {
		t.Fatalf("route matches across lid and phone %v", err)
	}
}
