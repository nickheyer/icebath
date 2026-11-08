package gateway

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Context bound to a fresh temp home with the YAML text written as its config.yaml
func yamlHome(t *testing.T, text string) (context.Context, string) {
	t.Helper()
	h := t.TempDir()
	if text != "" {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home.WithOverride(context.Background(), h), h
}

// The config.yaml and gateway.json layers read the way load_gateway_config reads them before env overrides
func loadLayers(t *testing.T, ctx context.Context, h string) *GatewayConfig {
	t.Helper()
	gw := LoadLegacyGatewayJSON(h)
	if err := LoadYAMLLayerAny(ctx, h, gw); err != nil {
		t.Fatal(err)
	}
	cfg, err := GatewayConfigFromDict(ctx, gw)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Resets the bundled platform scan and pseudo members so a test sees its own bundled tree
func resetPlatformMembers(t *testing.T, bundled string) {
	t.Helper()
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", bundled)
	reset := func() {
		platformMembers.Lock()
		platformMembers.scanned, platformMembers.pseudo, platformMembers.memberMap = false, nil, nil
		platformMembers.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func TestStreamingConfigNestedAndModeAlias(t *testing.T) {
	ctx, h := yamlHome(t, "streaming:\n  enabled: true\n  transport: draft\n")
	if s := loadLayers(t, ctx, h).Streaming; !s.Enabled || s.Transport != "draft" {
		t.Fatalf("top level %+v", s)
	}
	ctx, h = yamlHome(t, "streaming: {enabled: true, transport: edit}\ngateway:\n  streaming: {enabled: false, transport: draft}\n")
	if s := loadLayers(t, ctx, h).Streaming; !s.Enabled || s.Transport != "edit" {
		t.Fatalf("precedence %+v", s)
	}
	ctx, h = yamlHome(t, "streaming:\n  mode: off\n")
	if s := loadLayers(t, ctx, h).Streaming; s.Enabled || s.Transport != "off" {
		t.Fatalf("bare yaml off %+v", s)
	}
	cases := []struct {
		in        map[string]any
		enabled   bool
		transport string
	}{
		{map[string]any{"mode": "auto"}, true, "auto"}, {map[string]any{"mode": "edit"}, true, "edit"},
		{map[string]any{"mode": "auto", "enabled": false}, false, "auto"}, {map[string]any{"mode": false}, false, "off"},
		{map[string]any{"mode": true}, true, "auto"},
	}
	for _, c := range cases {
		if s := StreamingConfigFromDict(c.in); s.Enabled != c.enabled || s.Transport != c.transport {
			t.Fatalf("%v -> %+v", c.in, s)
		}
	}
	if s := StreamingConfigFromDict(map[string]any{"edit_interval": "x", "buffer_threshold": "7", "cursor": nil}); s.EditInterval != 0.8 || s.BufferThreshold.Int64() != 7 || s.Cursor != nil {
		t.Fatalf("coercion %+v", s)
	}
	on := StreamingConfig{Enabled: true, Transport: "auto"}
	if !on.EnabledFor(nil) || on.EnabledFor(false) || DefaultStreamingConfig().EnabledFor(true) {
		t.Fatal("a per platform value only narrows the switch")
	}
}

func TestPlatformMembersAndBundledAliases(t *testing.T) {
	bundled := t.TempDir()
	for dir, manifest := range map[string]string{"a2a": "name: a2a-platform\n", "irc": "name: IRC\n", "line": "name: a2a\n"} {
		os.MkdirAll(filepath.Join(bundled, "platforms", dir), 0o755)
		os.WriteFile(filepath.Join(bundled, "platforms", dir, "plugin.yaml"), []byte(manifest), 0o600)
	}
	resetPlatformMembers(t, bundled)
	ctx := context.Background()
	if p, err := ParsePlatform(ctx, "  IRC "); err != nil || p != "irc" {
		t.Fatalf("case normalised %q %v", p, err)
	}
	if p, err := ParsePlatform(ctx, "a2a-platform"); err != nil || p != "a2a" {
		t.Fatalf("manifest alias %q %v", p, err)
	}
	if p, _ := ParsePlatform(ctx, "a2a"); p != "a2a" {
		t.Fatal("an alias never shadows a directory name")
	}
	if p, err := ParsePlatform(ctx, PlatformTelegram); err != nil || p != PlatformTelegram {
		t.Fatal("a member parses to itself")
	}
	_, err := ParsePlatform(ctx, "nope")
	var pe *PyError
	if !errors.As(err, &pe) || pe.Kind != "ValueError" || pe.Msg != "'nope' is not a valid Platform" {
		t.Fatalf("unknown %v", err)
	}
	if _, err := ParsePlatform(ctx, 5); err == nil {
		t.Fatal("non string")
	}
	if Platform("my-plat").Name() != "MY_PLAT" || PlatformWhatsAppCloud.Name() != "WHATSAPP_CLOUD" {
		t.Fatal("member names")
	}
	if len(PlatformMembers()) != 23 || !CoreShipsPlatform("IRC") || CoreShipsPlatform("zzz") || !CoreShipsPlatform("Telegram") {
		t.Fatal("members and core ships")
	}
	ctx2, h := yamlHome(t, "platforms:\n  a2a-platform:\n    enabled: true\n    token: tok\n")
	cfg := loadLayers(t, ctx2, h)
	if pc := cfg.Platforms["a2a"]; pc == nil || !pc.Enabled {
		t.Fatalf("config keeps a platform written under its manifest name %v", cfg.Platforms)
	}
}

func TestHomeChannelAndPlatformConfig(t *testing.T) {
	ctx := context.Background()
	h := NewHomeChannel(PlatformDiscord, " https://discord.com/channels/1/22/ ", "Home")
	if h.ChatID != "22" {
		t.Fatalf("discord link %q", h.ChatID)
	}
	if _, err := HomeChannelFromDict(ctx, map[string]any{"platform": "telegram"}); err == nil || err.(*PyError).Kind != "KeyError" {
		t.Fatalf("missing chat id %v", err)
	}
	hc, _ := HomeChannelFromDict(ctx, map[string]any{"platform": "telegram", "chat_id": 42, "thread_id": 7, "user_id": ""})
	if hc.ChatID != "42" || *hc.ThreadID != "7" || hc.UserID != nil || hc.Name != "Home" {
		t.Fatalf("from dict %+v", hc)
	}
	if d := hc.ToDict(); d["thread_id"] != "7" || d["user_id"] != nil {
		t.Fatalf("to dict %v", d)
	}
	pc, err := PlatformConfigFromDict(ctx, map[string]any{"enabled": "yes", "port": 8080, "extra": map[string]any{"port": 9}, "typing_indicator": "off",
		"channel_overrides": map[string]any{"c1": map[string]any{"model": "m"}, "c2": "bad"}})
	if err != nil || !pc.Enabled || pc.Extra["port"] != 9 || pc.TypingIndicator || pc.ReplyToMode != "first" || len(pc.ChannelOverrides) != 1 {
		t.Fatalf("from dict %+v %v", pc, err)
	}
	if d := pc.ToDict(); d["token"] != nil || !reflect.DeepEqual(d["channel_overrides"], map[string]any{"c1": map[string]any{"model": "m"}}) {
		t.Fatalf("to dict %v", d)
	}
	gw, err := GatewayConfigFromDict(ctx, map[string]any{"platforms": map[string]any{"bogus": map[string]any{}, "telegram": map[string]any{
		"home_channel": map[string]any{"platform": "nope", "chat_id": "1"}}, "slack": map[string]any{"enabled": true}}})
	if err != nil || len(gw.Platforms) != 1 || gw.Platforms[PlatformSlack] == nil {
		t.Fatalf("unknown platforms skipped %v %v", gw.Platforms, err)
	}
	if _, err := GatewayConfigFromDict(ctx, map[string]any{"platforms": map[string]any{"telegram": map[string]any{"home_channel": map[string]any{"chat_id": "1"}}}}); err == nil {
		t.Fatal("a KeyError propagates")
	}
}

func TestGatewayConfigFromDictCoercion(t *testing.T) {
	ctx := home.WithOverride(context.Background(), "/srv/h")
	gw, err := GatewayConfigFromDict(ctx, map[string]any{
		"gateway":                        map[string]any{"systemd_watchdog_seconds": "30", "max_concurrent_sessions": 4.0, "loop_watchdog_max_strikes": 5000, "multiplex_profiles": "no"},
		"loop_watchdog_probe_interval_s": 0.5, "stt": map[string]any{"enabled": false}, "session_store_max_age_days": -3,
		"unauthorized_dm_behavior": " Decline ", "unauthorized_dm_decline_message": "  bye ", "room_link_url": 5,
		"sessions_dir": "/tmp//s/", "profile_routes": []any{map[string]any{"platform": "telegram", "profile": "default", "chat_id": 12},
			map[string]any{"platform": "telegram", "profile": "default", "chat_id": "1", "thread_id": "2"}, "junk"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gw.SystemdWatchdogSeconds != 30 || gw.MaxConcurrentSessions.Int64() != 4 || gw.LoopWatchdogMaxStrikes != 3 || *gw.MultiplexProfiles ||
		gw.LoopWatchdogProbeIntervalS != 30 || gw.STTEnabled || gw.SessionStoreMaxAgeDays.Sign() != 0 || gw.UnauthorizedDMBehavior != "decline" ||
		gw.UnauthorizedDMDeclineMessage != "bye" || gw.RoomLinkURL != nil || gw.SessionsDir != "/tmp/s" {
		t.Fatalf("coerced %+v", gw)
	}
	if len(gw.ProfileRoutes) != 2 || *gw.ProfileRoutes[0].ThreadID != "2" || *gw.ProfileRoutes[1].ChatID != "12" {
		t.Fatalf("routes most specific first %+v", gw.ProfileRoutes)
	}
	if m, _ := gw.ProfileRoutes[1].Matches(ctx, RouteQuery{Platform: "telegram", ChatID: ptr("12")}); !m {
		t.Fatal("route match")
	}
	if m, _ := gw.ProfileRoutes[1].Matches(ctx, RouteQuery{Platform: "discord", ChatID: ptr("12")}); m {
		t.Fatal("route matching")
	}
	if _, err := GatewayConfigFromDict(ctx, map[string]any{"session_store_max_age_days": math.Inf(1)}); err == nil || err.(*pyint.Error).Kind != "OverflowError" {
		t.Fatalf("int of infinity raises %v", err)
	}
	if _, err := GatewayConfigFromDict(ctx, map[string]any{"sessions_dir": 3}); err == nil {
		t.Fatal("Path of an int raises")
	}
	d := DefaultGatewayConfig(ctx)
	if d.SessionsDir != "/srv/h/sessions" || d.GetUnauthorizedDMBehavior(PlatformEmail) != "ignore" || d.GetNoticeDelivery("") != "public" {
		t.Fatal("defaults")
	}
	d.Platforms[PlatformEmail] = &PlatformConfig{Extra: map[string]any{"unauthorized_dm_behavior": "pair", "notice_delivery": "PRIVATE"}}
	if d.GetUnauthorizedDMBehavior(PlatformEmail) != "pair" || d.GetNoticeDelivery(PlatformEmail) != "private" {
		t.Fatal("per platform choices")
	}
	for in, want := range map[any]int{nil: 0, "12": 12, " 7 ": 7, "x": 0, true: 0, 3000000000: 0, -1: 0, "+5": 0} {
		if got := CoerceSystemdWatchdogSeconds(in, ""); got != want {
			t.Fatalf("watchdog %v = %d", in, got)
		}
	}
	for in, want := range map[any]int{"3": 3, 2.0: 2, 0: 0, 2.5: 0, true: 0, "x": 0} {
		got := coerceOptionalPositiveInt(in, "k")
		if (want == 0) != (got == nil) || (got != nil && got.Int64() != int64(want)) {
			t.Fatalf("positive int %v = %v", in, got)
		}
	}
}

func ptr(s string) *string { return &s }

func TestConnectedPlatformsAndValidation(t *testing.T) {
	ctx := context.Background()
	gw := DefaultGatewayConfig(ctx)
	gw.Platforms = map[Platform]*PlatformConfig{
		PlatformAPIServer:      {Enabled: true, Extra: map[string]any{"key": "short"}},
		PlatformWebhook:        {Enabled: true, Extra: map[string]any{}},
		PlatformMSGraphWebhook: {Enabled: true, Extra: map[string]any{"client_state": "  s "}},
		PlatformRelay:          {Enabled: true, Extra: map[string]any{}},
		PlatformTelegram:       {Enabled: true, Token: "123456:real-token-value", Extra: map[string]any{}},
		PlatformSlack:          {Enabled: false, Token: "x", Extra: map[string]any{}},
	}
	got := gw.GetConnectedPlatforms(ctx)
	if !reflect.DeepEqual(got, []Platform{PlatformMSGraphWebhook, PlatformTelegram, PlatformWebhook}) {
		t.Fatalf("connected %v", got)
	}
	gw.Platforms[PlatformDiscord] = &PlatformConfig{Enabled: true, Token: "your_token_here", Extra: map[string]any{}}
	gw.Platforms[PlatformMatrix] = &PlatformConfig{Enabled: true, Token: "  ", Extra: map[string]any{}}
	if err := validateGatewayConfig(gw); err != nil || gw.Platforms[PlatformDiscord].Enabled || !gw.Platforms[PlatformMatrix].Enabled || !gw.Platforms[PlatformTelegram].Enabled {
		t.Fatalf("placeholder disabled %v", err)
	}
	gw.Platforms[PlatformWeixin] = &PlatformConfig{Enabled: true, Token: 5}
	if err := validateGatewayConfig(gw); err == nil || err.(*PyError).Kind != "AttributeError" {
		t.Fatalf("int token %v", err)
	}
	if !provider.HasUsableSecret("abcdefghijklmnopq", 16) || provider.HasUsableSecret("abc", 16) || provider.HasUsableSecret(5, 1) {
		t.Fatal("usable secret")
	}
}

func TestMergePlatformSectionsOrder(t *testing.T) {
	ctx := context.Background()
	gw := map[string]any{"platforms": map[string]any{"telegram": map[string]any{"extra": map[string]any{"a": 1, "b": 1}}}}
	yamlCfg := map[string]any{
		"platforms": map[string]any{"telegram": map[string]any{"enabled": false, "extra": map[string]any{"b": 2}}},
		"gateway": map[string]any{"platforms": map[string]any{"telegram": map[string]any{"token": "t"}},
			"telegram": map[string]any{"enabled": true}, "custom": map[string]any{"enabled": true}, "streaming": map[string]any{}},
	}
	out, err := MergePlatformSections(ctx, yamlCfg, yamlCfg["gateway"], gw, map[string]bool{"custom": true})
	if err != nil {
		t.Fatal(err)
	}
	tg := out["telegram"].(map[string]any)
	if tg["enabled"] != true || tg["token"] != "t" || !reflect.DeepEqual(tg["extra"], map[string]any{"a": 1, "b": 2, "_enabled_explicit": true}) {
		t.Fatalf("merged %v", tg)
	}
	if _, ok := out["custom"]; !ok || out["streaming"] != nil {
		t.Fatalf("also and platform names only %v", out)
	}
	bad := map[string]any{"platforms": map[string]any{"slack": map[string]any{"extra": nil}}}
	if _, err := MergePlatformSections(ctx, bad, nil, map[string]any{}, nil); err == nil || err.Error() != "'NoneType' object is not a mapping" {
		t.Fatalf("extra None %v", err)
	}
	if enabled, token := providerPlatformEnabled(map[string]any{"token": 77}); enabled || token != "77" {
		t.Fatal("provider platform enabled hook")
	}
}

func TestAuthoredExtraWinsAndSharedKeys(t *testing.T) {
	ctx, h := yamlHome(t, "telegram:\n  reply_to_mode: all\n  require_mention: true\n  allowed_chats: [1]\n  extra:\n    reply_to_mode: off\n"+
		"platforms:\n  telegram:\n    extra:\n      require_mention: false\n  slack:\n    allowed_chats: [2]\n    channel_prompts: {c: p}\n    enabled: true\n")
	gw := map[string]any{}
	if err := LoadYAMLLayer(ctx, h, gw); err != nil {
		t.Fatal(err)
	}
	tg := gw["platforms"].(map[string]any)["telegram"].(map[string]any)["extra"].(map[string]any)
	if tg["require_mention"] != false || tg["allowed_chats"] == nil || tg["reply_to_mode"] != false {
		t.Fatalf("telegram extra %v", tg)
	}
	sl := gw["platforms"].(map[string]any)["slack"].(map[string]any)
	if _, ok := sl["extra"].(map[string]any)["allowed_chats"]; ok || !reflect.DeepEqual(sl["extra"].(map[string]any)["channel_prompts"], map[string]any{"c": "p"}) {
		t.Fatalf("slack extra %v", sl)
	}
	cfg, err := GatewayConfigFromDict(ctx, gw)
	if err != nil || cfg.Platforms[PlatformTelegram].ReplyToMode != "first" || cfg.Platforms[PlatformTelegram].Extra["reply_to_mode"] != false {
		t.Fatalf("a root block typed key reaches only extra through its own extra %v", err)
	}
}

func TestRegistryLifecycleAndAdapters(t *testing.T) {
	ctx, _ := yamlHome(t, "")
	r := NewPlatformRegistry()
	installs := 0
	entry := NewPlatformEntry("irc", "IRC", func(ctx context.Context, cfg *PlatformConfig) (any, error) { return "adapter", nil }, func() bool { return installs > 0 })
	entry.EnsureDepsFn = func(ctx context.Context) (bool, error) { installs++; return true, nil }
	entry.RequiredEnv = []string{"IRC_PASSWORD", "IRC_NICK"}
	if err := r.Register(ctx, entry, ""); err != nil {
		t.Fatal(err)
	}
	if r.Get(ctx, "irc") != entry || !r.IsRegistered(ctx, "irc") {
		t.Fatal("register and get")
	}
	if a := r.CreateAdapter(ctx, "irc", NewPlatformConfig()); a != "adapter" || installs != 1 {
		t.Fatalf("installer then create %v %d", a, installs)
	}
	if a := r.CreateAdapter(ctx, "irc", NewPlatformConfig()); a != "adapter" || installs != 1 {
		t.Fatal("deps present skips installer")
	}
	failing := NewPlatformEntry("x", "X", entry.AdapterFactory, func() bool { return false })
	failing.EnsureDepsFn = func(ctx context.Context) (bool, error) { return false, errors.New("boom") }
	r.Register(ctx, failing, "")
	if r.CreateAdapter(ctx, "x", nil) != nil {
		t.Fatal("installer error returns nil")
	}
	failing.EnsureDepsFn = nil
	if r.CreateAdapter(ctx, "x", nil) != nil || r.CreateAdapter(ctx, "missing", nil) != nil {
		t.Fatal("no installer and missing deps returns nil")
	}
	entry.ValidateConfig = func(cfg *PlatformConfig) (bool, error) { return false, nil }
	if r.CreateAdapter(ctx, "irc", nil) != nil {
		t.Fatal("validation failure returns nil")
	}
	if !r.requiredEnvNamesIn(CurrentScopeKey(ctx))["IRC_PASSWORD"] {
		t.Fatal("required env names")
	}
	loads := 0
	r.RegisterDeferred("lazy", &plugins.DeferredPlatformLoader{Load: func(ctx context.Context) {
		loads++
		r.register(NewPlatformEntry("lazy", "Lazy", nil, nil), "")
	}}, "")
	if !r.RegisteredNames(ctx)["lazy"] || loads != 0 || !r.IsRegistered(ctx, "lazy") {
		t.Fatal("deferred names without materializing")
	}
	if r.Get(ctx, "lazy") == nil || loads != 1 || r.Get(ctx, "lazy") == nil || loads != 1 {
		t.Fatal("deferred loader runs once on lookup")
	}
	if ok, _ := r.Unregister(ctx, "irc", ""); !ok || r.Get(ctx, "irc") != nil {
		t.Fatal("unregister")
	}
	scope := CurrentScopeKey(ctx)
	scoped := NewPlatformEntry("irc", "Scoped", nil, nil)
	r.register(scoped, scope)
	prevE, prevL := r.SnapshotRegistration("irc", scope)
	if prevE != scoped || r.Get(ctx, "irc") != scoped || r.Get(context.Background(), "irc") != nil {
		t.Fatal("scoped entries overlay only their home")
	}
	if !r.RestoreRegistration("irc", scoped, prevL, nil, nil, scope) || r.Get(ctx, "irc") != nil {
		t.Fatal("restore to the empty predecessor")
	}
}

func TestPluginYAMLHooksAndActivation(t *testing.T) {
	ctx, h := yamlHome(t, "irc:\n  server: a\nplatforms:\n  irc:\n    extra:\n      server: b\n")
	saved := Registry
	Registry = NewPlatformRegistry()
	t.Cleanup(func() { Registry = saved })
	seenBlock := map[string]any{}
	e := NewPlatformEntry("irc", "IRC", nil, func() bool { return true })
	e.ApplyYAMLConfigFn = func(yamlCfg, block map[string]any) (map[string]any, error) {
		seenBlock = block
		return map[string]any{"seeded": 1}, nil
	}
	Registry.register(e, "")
	boom := NewPlatformEntry("boom", "Boom", nil, nil)
	boom.ApplyYAMLConfigFn = func(yamlCfg, block map[string]any) (map[string]any, error) { return nil, errors.New("x") }
	Registry.register(boom, "")
	untouched := 0
	Registry.RegisterDeferred("cold", &plugins.DeferredPlatformLoader{Load: func(ctx context.Context) { untouched++ }}, "")
	gw := map[string]any{}
	if err := LoadYAMLLayer(ctx, h, gw); err != nil {
		t.Fatal(err)
	}
	if seenBlock["server"] != "b" || gw["platforms"].(map[string]any)["irc"].(map[string]any)["extra"].(map[string]any)["seeded"] != 1 {
		t.Fatalf("hook saw authored extra %v %v", seenBlock, gw)
	}
	if untouched != 0 {
		t.Fatal("an unconfigured deferred platform is never imported")
	}
	t.Setenv("COLD_TOKEN", "v")
	if !ActivationPredicate(ctx, nil, nil, h, Registry)("cold") || ActivationPredicate(ctx, nil, nil, h, Registry)("warm") {
		t.Fatal("env prefix evidence")
	}
	Registry.NoteActivationEnv("warm", []string{" OTHER_SECRET "})
	t.Setenv("OTHER_SECRET", "1")
	if !ActivationPredicate(ctx, nil, nil, h, Registry)("warm") {
		t.Fatal("declared activation env evidence")
	}
}

func TestToolsAndProviderHooksInstalled(t *testing.T) {
	if tools.RegisteredAdapterSecretEnv == nil || tools.RegisteredPlatformNames == nil ||
		provider.GatewayHooks.MergePlatformSections == nil || provider.GatewayHooks.PlatformEnabled == nil {
		t.Fatal("init installs the hooks")
	}
	h := t.TempDir()
	saved, savedKeys := Registry, tools.ConfigEnvTableKeys
	Registry = NewPlatformRegistry()
	tools.ConfigEnvTableKeys = func() []string { return nil }
	t.Cleanup(func() { Registry, tools.ConfigEnvTableKeys = saved, savedKeys })
	e := NewPlatformEntry("irc", "IRC", nil, nil)
	e.RequiredEnv = []string{"irc_password", "IRC_NICK"}
	Registry.register(e, "")
	got := registeredAdapterSecretEnv(&tools.Env{Home: h})
	if !got["IRC_PASSWORD"] || got["IRC_NICK"] {
		t.Fatalf("secret suffixed names only %v", got)
	}
	if names := registeredPlatformNames(); !reflect.DeepEqual(names, []string{"irc"}) {
		t.Fatalf("names %v", names)
	}
}

func TestCoerceIntKeepsPythonIntsPastTheGoIntRange(t *testing.T) {
	huge := "123456789012345678901234567890"
	s := StreamingConfigFromDict(map[string]any{"buffer_threshold": huge})
	if got := s.ToDict()["buffer_threshold"]; pyrepr.Repr(got) != huge {
		t.Fatalf("buffer_threshold %v", got)
	}
	if got := StreamingConfigFromDict(map[string]any{"buffer_threshold": 1e30}).BufferThreshold.String(); got != "1000000000000000019884624838656" {
		t.Fatalf("float buffer_threshold %s", got)
	}
	if got := coerceInt(math.Inf(1), 5); got.Int64() != 5 {
		t.Fatalf("inf %v", got)
	}
	if got := coerceOptionalPositiveInt("-"+huge, "k"); got != nil {
		t.Fatalf("negative huge %v", got)
	}
	if got := coerceOptionalPositiveInt(huge, "k"); got == nil || got.String() != huge {
		t.Fatalf("positive huge %v", got)
	}
	ctx := context.Background()
	gw, err := GatewayConfigFromDict(ctx, map[string]any{"session_store_max_age_days": huge, "max_concurrent_sessions": huge,
		"loop_watchdog_max_strikes": huge})
	if err != nil {
		t.Fatal(err)
	}
	d := gw.ToDict()
	if pyrepr.Repr(d["session_store_max_age_days"]) != huge || pyrepr.Repr(d["max_concurrent_sessions"]) != huge ||
		gw.LoopWatchdogMaxStrikes != DefaultLoopWatchdogMaxStrikes {
		t.Fatalf("gateway ints %v %v %d", d["session_store_max_age_days"], d["max_concurrent_sessions"], gw.LoopWatchdogMaxStrikes)
	}
	gw, _ = GatewayConfigFromDict(ctx, map[string]any{"session_store_max_age_days": "-" + huge})
	if gw.SessionStoreMaxAgeDays.Sign() != 0 {
		t.Fatalf("negative max age %v", gw.SessionStoreMaxAgeDays)
	}
}

func TestMessagingPlatformLabelsCoverMembersBundledDirsAndAliases(t *testing.T) {
	bundled := t.TempDir()
	for dir, manifest := range map[string]string{"a2a": "name: a2a-platform\n", "irc": "name: IRC\n"} {
		os.MkdirAll(filepath.Join(bundled, "platforms", dir), 0o755)
		os.WriteFile(filepath.Join(bundled, "platforms", dir, "plugin.yaml"), []byte(manifest), 0o600)
	}
	resetPlatformMembers(t, bundled)
	labels := MessagingPlatformLabels()
	set := map[string]bool{}
	for i, l := range labels {
		if set[l] {
			t.Fatalf("duplicate label %q", l)
		}
		set[l] = true
		if i > 0 && labels[i-1] > l {
			t.Fatalf("labels not sorted: %v", labels)
		}
	}
	for _, want := range []string{string(PlatformTelegram), string(PlatformLocal), string(PlatformWhatsAppCloud), "a2a", "irc", "a2a-platform"} {
		if !set[want] {
			t.Fatalf("missing %q in %v", want, labels)
		}
	}
}
