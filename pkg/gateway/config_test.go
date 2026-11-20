package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestBoolTokenAndCoerceBool(t *testing.T) {
	if v, ok := boolToken(" On "); !v || !ok {
		t.Fatal("on token")
	}
	if _, ok := boolToken(nil); ok {
		t.Fatal("None is not a token")
	}
	if v, ok := boolToken(true); !v || !ok {
		t.Fatal("str(True) is a truthy token")
	}
	if coerceBool(nil, true) != true || coerceBool("maybe", false) != false || coerceBool("off", true) != false {
		t.Fatal("string and nil coercion")
	}
	if coerceBool(0, true) != false || coerceBool([]any{1}, false) != true {
		t.Fatal("python bool of non strings")
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("GATEWAY_MULTIPLEX_PROFILES", "  ")
	if envMultiplexProfilesOverride() != nil {
		t.Fatal("blank keeps config")
	}
	t.Setenv("GATEWAY_MULTIPLEX_PROFILES", "sometimes")
	if envMultiplexProfilesOverride() != nil {
		t.Fatal("unrecognized keeps config")
	}
	t.Setenv("GATEWAY_MULTIPLEX_PROFILES", "No")
	if v := envMultiplexProfilesOverride(); v == nil || *v {
		t.Fatal("no disables")
	}
	t.Setenv("GATEWAY_ON_ALL_ADAPTERS_DOWN", " Stay_Alive ")
	if envOnAllAdaptersDownOverride() != "stay_alive" {
		t.Fatal("stay_alive")
	}
	t.Setenv("GATEWAY_ON_ALL_ADAPTERS_DOWN", "restart")
	if envOnAllAdaptersDownOverride() != "" {
		t.Fatal("unrecognized policy")
	}
}

func TestSmallHelpers(t *testing.T) {
	cases := map[any]string{nil: "auto", true: "auto", false: "off", " EDIT ": "edit", "": "auto"}
	for in, want := range cases {
		if got := normalizeTransportToken(in); got != want {
			t.Errorf("transport %v = %q", in, got)
		}
	}
	if normalizeChoice(" Decline ", UnauthorizedDMBehaviors, "pair") != "decline" || normalizeChoice(3, UnauthorizedDMBehaviors, "pair") != "pair" {
		t.Fatal("choice")
	}
	c := map[string]any{"a": "x"}
	slot := dictSlot(c, "a")
	slot["k"] = 1
	if !reflect.DeepEqual(c["a"], map[string]any{"k": 1}) {
		t.Fatal("dict slot replaces a non mapping")
	}
	if !PlatformBindsPort("webhook", nil) || PlatformBindsPort("telegram", nil) {
		t.Fatal("port binding table")
	}
	if PlatformBindsPort("feishu", nil) || !PlatformBindsPort("feishu", map[string]any{"connection_mode": " Webhook "}) {
		t.Fatal("feishu binds only in webhook mode")
	}
	if id, ok := DiscordChannelIDFromLink("https://canary.discord.com/channels/@me/42/"); !ok || id != "42" {
		t.Fatal("channel link")
	}
	if _, ok := DiscordChannelIDFromLink("https://discord.com/channels/1/2/3"); ok {
		t.Fatal("message link is left alone")
	}
}

func TestLoadLegacyGatewayJSON(t *testing.T) {
	h := t.TempDir()
	if got := LoadLegacyGatewayJSON(h); !reflect.DeepEqual(got, map[string]any{}) {
		t.Fatalf("absent %v", got)
	}
	write := func(s string) { os.WriteFile(filepath.Join(h, "gateway.json"), []byte(s), 0o600) }
	write("\xef\xbb\xbf{\"platforms\": {\"telegram\": {\"enabled\": true}}}")
	want := map[string]any{"platforms": map[string]any{"telegram": map[string]any{"enabled": true}}}
	if got := LoadLegacyGatewayJSON(h); !reflect.DeepEqual(got, want) {
		t.Fatalf("bom %v", got)
	}
	write("{not json")
	if got := LoadLegacyGatewayJSON(h); !reflect.DeepEqual(got, map[string]any{}) {
		t.Fatalf("malformed %v", got)
	}
	write("[]")
	if got := LoadLegacyGatewayJSON(h); !reflect.DeepEqual(got, map[string]any{}) {
		t.Fatalf("falsy %v", got)
	}
	write("[1]")
	if got := LoadLegacyGatewayJSON(h); !reflect.DeepEqual(got, []any{int64(1)}) {
		t.Fatalf("list passes through %#v", got)
	}
}

func TestBridgeToplevelKeysModes(t *testing.T) {
	yamlCfg := map[string]any{
		"quick_commands": nil, "stt": "bad", "streaming": "x", "profile_routes": []any{"r"},
		"unauthorized_dm_behavior": " IGNORE ",
	}
	nested := map[string]any{
		"quick_commands": map[string]any{"a": 1}, "stt": map[string]any{"p": 1}, "multiplex_profiles": true,
		"systemd_watchdog_seconds": 30, "streaming": map[string]any{"mode": "edit"}, "room_link_url": "u",
	}
	gw := map[string]any{"multiplex_profiles": false}
	BridgeToplevelKeys(yamlCfg, nested, gw)
	want := map[string]any{
		"quick_commands": map[string]any{"a": 1}, "multiplex_profiles": false, "room_link_url": "u",
		"profile_routes": []any{"r"}, "systemd_watchdog_seconds": 30, "streaming": map[string]any{"mode": "edit"},
		"unauthorized_dm_behavior": "ignore",
	}
	if !reflect.DeepEqual(gw, want) {
		t.Fatalf("bridged %v", gw)
	}
	gw = map[string]any{}
	BridgeToplevelKeys(map[string]any{"systemd_watchdog_seconds": 5, "stt": map[string]any{}}, "not a mapping", gw)
	if !reflect.DeepEqual(gw, map[string]any{"stt": map[string]any{}}) {
		t.Fatalf("nested only key ignores top level %v", gw)
	}
}

func TestBridgeCoreEnvSettings(t *testing.T) {
	for _, k := range []string{"GATEWAY_ALLOW_ALL_USERS", "TELEGRAM_REQUIRE_MENTION", "SIGNAL_REQUIRE_MENTION"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Cleanup(func() { bridgedAllowAll.set = false })
	platforms := map[string]any{}
	BridgeCoreEnvSettings(context.Background(), map[string]any{
		"gateway": map[string]any{"allow_all_users": true}, "require_mention": false,
		"signal": map[string]any{"require_mention": true},
	}, platforms)
	if v, ok := BridgedAllowAllUsers(); !ok || v != "true" {
		t.Fatalf("bridged grant %q %v", v, ok)
	}
	if os.Getenv("TELEGRAM_REQUIRE_MENTION") != "false" || os.Getenv("SIGNAL_REQUIRE_MENTION") != "true" {
		t.Fatal("mention env bridges")
	}
	want := map[string]any{
		"telegram": map[string]any{"extra": map[string]any{"require_mention": false}},
		"signal":   map[string]any{"extra": map[string]any{"require_mention": true}},
	}
	if !reflect.DeepEqual(platforms, want) {
		t.Fatalf("extra seeding %v", platforms)
	}
	if env := DropBridgedEnv(map[string]string{"GATEWAY_ALLOW_ALL_USERS": "true", "A": "b"}); !reflect.DeepEqual(env, map[string]string{"A": "b"}) {
		t.Fatal("child env drops the bridged grant")
	}
	BridgeCoreEnvSettings(context.Background(), map[string]any{"allow_all_users": "no"}, map[string]any{})
	if _, ok := os.LookupEnv("GATEWAY_ALLOW_ALL_USERS"); ok {
		t.Fatal("reload clears the bridge's own write")
	}
	os.Setenv("GATEWAY_ALLOW_ALL_USERS", "operator")
	BridgeCoreEnvSettings(context.Background(), map[string]any{"allow_all_users": true}, map[string]any{})
	if v, ok := BridgedAllowAllUsers(); ok || os.Getenv("GATEWAY_ALLOW_ALL_USERS") != "operator" {
		t.Fatalf("operator env wins %q %v", v, ok)
	}
	os.Unsetenv("GATEWAY_ALLOW_ALL_USERS")
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	ctx := provider.WithSecretScope(context.Background(), map[string]string{"X": "y"}, "")
	BridgeCoreEnvSettings(ctx, map[string]any{"allow_all_users": true, "require_mention": true}, map[string]any{})
	if _, ok := os.LookupEnv("GATEWAY_ALLOW_ALL_USERS"); ok || os.Getenv("TELEGRAM_REQUIRE_MENTION") != "false" {
		t.Fatal("secondary profile scope never writes the process env")
	}
}

func TestProviderHooksInstalled(t *testing.T) {
	if provider.GatewayHooks.LegacyJSON == nil || provider.GatewayHooks.BridgedAllowAllUsers == nil {
		t.Fatal("init installs the provider hooks")
	}
	h := t.TempDir()
	os.WriteFile(filepath.Join(h, "gateway.json"), []byte("[1]"), 0o600)
	if provider.GatewayHooks.LegacyJSON(h) != nil {
		t.Fatal("a non mapping reads as nil")
	}
}
