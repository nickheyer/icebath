package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Fresh attempt memo and no HASS_TOKEN in the process environment
func leftCoreFresh(t *testing.T) {
	t.Helper()
	leftCoreAttemptedMu.Lock()
	prev := leftCoreAttempted
	leftCoreAttempted = map[string]bool{}
	leftCoreAttemptedMu.Unlock()
	t.Cleanup(func() {
		leftCoreAttemptedMu.Lock()
		leftCoreAttempted = prev
		leftCoreAttemptedMu.Unlock()
	})
	t.Setenv("HASS_TOKEN", "")
	os.Unsetenv("HASS_TOKEN")
}

// Clears the once per process memo the way a fresh agent or gateway process starts
func clearLeftCoreAttempted() {
	leftCoreAttemptedMu.Lock()
	leftCoreAttempted = map[string]bool{}
	leftCoreAttemptedMu.Unlock()
}

// Binds the home as the process home with the lazy install and install seams restored afterwards
func bindLeftCoreHome(t *testing.T, h string, lazy bool, install func(string) (map[string]any, error)) context.Context {
	t.Helper()
	t.Setenv(home.EnvHome, h)
	home.ResetKeyCache()
	t.Cleanup(home.ResetKeyCache)
	prevLazy, prevInto, prevServes := leftCoreLazyInstallsAllowed, leftCoreInstallInto, leftCoreGatewayServes
	leftCoreLazyInstallsAllowed = func(context.Context) bool { return lazy }
	if install != nil {
		leftCoreInstallInto = func(context.Context, string, *bool) func(string) (map[string]any, error) { return install }
	}
	t.Cleanup(func() {
		leftCoreLazyInstallsAllowed, leftCoreInstallInto, leftCoreGatewayServes = prevLazy, prevInto, prevServes
	})
	return context.Background()
}

const examplePlatformToolsets = "platform_toolsets:\n  cli: [icebath-cli]\n  telegram: [icebath-telegram]\n  discord: [icebath-discord]\n" +
	"  whatsapp: [icebath-whatsapp]\n  slack: [icebath-slack]\n  signal: [icebath-signal]\n  homeassistant: [icebath-homeassistant]\n" +
	"  qqbot: [icebath-qqbot]\n  yuanbao: [icebath-yuanbao]\n  teams: [icebath-teams]\n  google_chat: [icebath-google_chat]\n"

func TestHomeassistantInUseMatchesWhatCoreActivated(t *testing.T) {
	leftCoreFresh(t)
	rows := []struct{ env, cfg string }{
		{"HASS_TOKEN=abc\n", ""},
		{"", "platforms:\n  homeassistant:\n    enabled: true\n"},
		{"", "gateway:\n  platforms:\n    homeassistant:\n      enabled: true\n"},
		{"", "platforms:\n  homeassistant:\n    token: inline-token\n"},
		{"", "platform_toolsets:\n  cli: [icebath-cli, homeassistant]\n"},
		{"", "toolsets: [icebath-cli, homeassistant]\n"},
		{"", "platforms:\n  homeassistant:\n    enabled: true\nplatform_toolsets:\n  homeassistant: [icebath-homeassistant]\n"},
	}
	for i, row := range rows {
		if !homeassistantInUse(leftCoreHome(t, t.TempDir(), "home", row.env, row.cfg), false) {
			t.Fatalf("row %d not in use", i)
		}
	}
}

func TestHomeassistantNotInUse(t *testing.T) {
	leftCoreFresh(t)
	rows := []struct{ env, cfg string }{
		{"", ""},
		{"HASS_TOKEN=\n", ""},
		{"", "platforms:\n  homeassistant:\n    enabled: false\n    token: t\n"},
		{"", "platform_toolsets:\n  cli: [icebath-cli]\n"},
		{"OPENAI_API_KEY=sk\n", "platforms:\n  telegram:\n    enabled: true\n"},
		{"", "platform_toolsets:\n  homeassistant: [icebath-homeassistant]\n"},
		{"", examplePlatformToolsets},
	}
	for i, row := range rows {
		if homeassistantInUse(leftCoreHome(t, t.TempDir(), "home", row.env, row.cfg), false) {
			t.Fatalf("row %d in use", i)
		}
	}
}

func TestInUseReadsThePlatformConfigTheGatewayLoaderMerged(t *testing.T) {
	leftCoreFresh(t)
	const jsonOn = `{"platforms": {"homeassistant": {"enabled": true, "token": "t"}}}`
	rows := []struct {
		gatewayJSON, cfg string
		inUse            bool
	}{
		{jsonOn, "", true},
		{"", "gateway:\n  homeassistant:\n    enabled: true\n    token: t\n", true},
		{"", "gateway:\n  platforms:\n    homeassistant:\n      enabled: true\n      token: t\nplatforms:\n  homeassistant:\n    extra:\n      url: http://ha.local\n", true},
		{jsonOn, "platforms:\n  homeassistant:\n    enabled: false\n", false},
	}
	for i, row := range rows {
		h := leftCoreHome(t, t.TempDir(), "home", "", row.cfg)
		if row.gatewayJSON != "" {
			writeLeftCoreFile(t, filepath.Join(h, "gateway.json"), row.gatewayJSON)
		}
		if got := homeassistantInUse(h, false); got != row.inUse {
			t.Fatalf("row %d in use %v", i, got)
		}
	}
}

func TestProcessEnvTokenCountsOnlyForTheActiveHome(t *testing.T) {
	leftCoreFresh(t)
	h := leftCoreHome(t, t.TempDir(), "home", "", "")
	bindLeftCoreHome(t, h, true, nil)
	t.Setenv("HASS_TOKEN", "from-systemd")
	if homeassistantInUse(h, false) || !homeassistantInUse(h, true) {
		t.Fatal(homeassistantInUse(h, false), homeassistantInUse(h, true))
	}
}

// Platforms where the homeassistant plugin toolset resolves on for the saved selections
func homeassistantOn(h string, selections map[string][]string) map[string]bool {
	cfg := leftCoreReadConfig(h)
	out := map[string]bool{}
	for p, sel := range selections {
		out[p] = len(tools.EnabledPluginToolsets(config.Tree(cfg), p, sel, map[string]bool{"homeassistant": true})) > 0
	}
	return out
}

func sameBools(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestMigrationKeepsAToolsetOffWhereCoreHadItOff(t *testing.T) {
	leftCoreFresh(t)
	h := leftCoreHome(t, t.TempDir(), "home", "HASS_TOKEN=abc\n",
		"platform_toolsets:\n  telegram: [web, terminal, file]\n  discord: [icebath-discord]\n  slack: [web, homeassistant]\n")
	MigrateLeftCoreHome(context.Background(), h, func(string) (map[string]any, error) { return map[string]any{"ok": true}, nil }, func(string) {}, false)
	got := homeassistantOn(h, map[string][]string{"telegram": {"web", "terminal", "file"}, "discord": {"icebath-discord"},
		"slack": {"web", "homeassistant"}, "cli": {"icebath-cli"}, "webhook": {"icebath-webhook"}, "acp": {"icebath-acp"}})
	want := map[string]bool{"telegram": false, "discord": true, "slack": true, "cli": true, "webhook": false, "acp": false}
	if !sameBools(got, want) {
		t.Fatalf("enabled %v, want %v", got, want)
	}
}

func TestAPreinstalledPluginGetsTheScopeConversionOnce(t *testing.T) {
	leftCoreFresh(t)
	h := leftCoreHome(t, t.TempDir(), "home", "HASS_TOKEN=abc\n", "platform_toolsets:\n  telegram: [web]\n")
	mkPluginDir(t, h, "homeassistant")
	ctx := home.WithOverride(context.Background(), h)
	selections := map[string][]string{"telegram": {"web"}, "acp": {"icebath-acp"}, "webhook": {"icebath-webhook"}}
	haOn := func() map[string]bool {
		selections["telegram"] = parseConfigStringList(anyMap(leftCoreReadConfig(h)["platform_toolsets"])["telegram"])
		return homeassistantOn(h, selections)
	}
	reinstalled := func(string) (map[string]any, error) {
		t.Fatal("reinstalled")
		return nil, nil
	}
	MigrateLeftCoreHome(ctx, h, reinstalled, func(string) {}, false)
	if got := haOn(); !sameBools(got, map[string]bool{"telegram": false, "acp": false, "webhook": false}) {
		t.Fatalf("after the conversion %v", got)
	}
	cfg, err := leftCoreReadUserConfigRaw(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := tools.Default.SavePlatformTools(ctx, "", config.Tree(cfg), "telegram", map[string]bool{"web": true, "homeassistant": true}, nil); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveUser(ctx, h, cfg); err != nil {
		t.Fatal(err)
	}
	MigrateLeftCoreHome(ctx, h, reinstalled, func(string) {}, false)
	if got := haOn(); !sameBools(got, map[string]bool{"telegram": true, "acp": false, "webhook": false}) {
		t.Fatalf("after the user's later choice %v", got)
	}
}
