package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
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
	if err := config.SaveUser(h, cfg); err != nil {
		t.Fatal(err)
	}
	MigrateLeftCoreHome(ctx, h, reinstalled, func(string) {}, false)
	if got := haOn(); !sameBools(got, map[string]bool{"telegram": true, "acp": false, "webhook": false}) {
		t.Fatalf("after the user's later choice %v", got)
	}
}

func TestCatalogMissIsReportedNotInstalled(t *testing.T) {
	leftCoreFresh(t)
	withPluginHooks(t, func(hk *PluginManagerHooks) { hk.CatalogEntry = func(string) (string, bool) { return "", false } })
	h := leftCoreHome(t, t.TempDir(), "home", "HASS_TOKEN=abc\n", "")
	var said []string
	got := MigrateLeftCoreHome(context.Background(), h, func(string) (map[string]any, error) {
		t.Fatal("installed")
		return nil, nil
	}, func(m string) { said = append(said, m) }, false)
	if len(got) != 0 || !strings.Contains(said[0], "cannot find in the plugin catalog") {
		t.Fatalf("installed %v said %v", got, said)
	}
}

func TestMigrateAllHomesMigratesOnlyTheProfileThatUsedIt(t *testing.T) {
	leftCoreFresh(t)
	root := t.TempDir()
	a := leftCoreHome(t, root, "a", "HASS_TOKEN=abc\n", "")
	b := leftCoreHome(t, root, "b", "OPENAI_API_KEY=sk\n", "")
	withPluginHooks(t, func(hk *PluginManagerHooks) {
		hk.DependencyHomes = func() ([]string, error) { return []string{a, b}, nil }
	})
	var installedInto []string
	prev := leftCoreInstallInto
	leftCoreInstallInto = func(_ context.Context, h string, _ *bool) func(string) (map[string]any, error) {
		return func(name string) (map[string]any, error) {
			installedInto = append(installedInto, h+"|"+name)
			return map[string]any{"ok": true}, nil
		}
	}
	t.Cleanup(func() { leftCoreInstallInto = prev })
	var said []string
	got, err := MigrateAllLeftCoreHomes(context.Background(), func(m string) { said = append(said, m) })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "homeassistant" || len(installedInto) != 1 || installedInto[0] != a+"|homeassistant" {
		t.Fatalf("installed %v into %v", got, installedInto)
	}
	if len(said) != 1 || !strings.Contains(said[0], a) {
		t.Fatalf("said %v", said)
	}
}

func TestStartupHonoursLazyInstallOptOut(t *testing.T) {
	leftCoreFresh(t)
	h := leftCoreHome(t, t.TempDir(), "home", "HASS_TOKEN=abc\n", "")
	ctx := bindLeftCoreHome(t, h, false, func(string) (map[string]any, error) {
		t.Fatal("installed")
		return nil, nil
	})
	var said []string
	say := func(m string) { said = append(said, m) }
	if got := RecoverLeftCoreAtStartup(ctx, say, false); len(got) != 0 {
		t.Fatalf("installed %v", got)
	}
	if len(said) != 1 || !strings.Contains(said[0], "allow_lazy_installs is off") || !strings.Contains(said[0], "plugins install homeassistant") {
		t.Fatalf("said %v", said)
	}
	if got := RecoverLeftCoreAtStartup(ctx, say, false); len(got) != 0 || len(said) != 1 {
		t.Fatalf("second start installed %v said %v", got, said)
	}
}

func TestGatewayStartOutcomeWaitsForTheFirstAgent(t *testing.T) {
	leftCoreFresh(t)
	h := leftCoreHome(t, t.TempDir(), "home", "HASS_TOKEN=abc\n", "")
	ctx := bindLeftCoreHome(t, h, true, nil)
	var consents []bool
	leftCoreInstallInto = func(_ context.Context, _ string, consent *bool) func(string) (map[string]any, error) {
		consents = append(consents, *consent)
		return func(string) (map[string]any, error) { return map[string]any{"ok": true}, nil }
	}
	if got := RecoverLeftCoreAtStartup(ctx, nil, false); len(got) != 1 || got[0] != "homeassistant" {
		t.Fatalf("gateway start installed %v", got)
	}
	if len(consents) != 1 || !consents[0] {
		t.Fatalf("consents %v", consents)
	}
	var said []string
	say := func(m string) { said = append(said, m) }
	if got := RecoverLeftCoreAtStartup(ctx, say, false); len(got) != 0 || len(said) != 1 || !strings.HasPrefix(said[0], "✓ Home Assistant moved out of core") {
		t.Fatalf("first agent installed %v said %v", got, said)
	}
	if got := RecoverLeftCoreAtStartup(ctx, say, false); len(got) != 0 || len(said) != 1 {
		t.Fatalf("second agent installed %v said %v", got, said)
	}
}

func TestAnAgentStartInstallOfAPlatformKeepsTheRestartHint(t *testing.T) {
	for _, gatewayStart := range []bool{false, true} {
		leftCoreFresh(t)
		h := leftCoreHome(t, t.TempDir(), "home", "HASS_TOKEN=abc\n", "")
		ctx := bindLeftCoreHome(t, h, true, func(string) (map[string]any, error) { return map[string]any{"ok": true}, nil })
		leftCoreGatewayServes = func(string) bool { return true }
		if got := RecoverLeftCoreAtStartup(ctx, nil, gatewayStart); len(got) != 1 || got[0] != "homeassistant" {
			t.Fatalf("gateway_start %v installed %v", gatewayStart, got)
		}
		kept := claimLeftCoreNotices(h)
		if len(kept) == 0 || !strings.HasPrefix(kept[0], "✓ Home Assistant moved out of core") {
			t.Fatalf("gateway_start %v kept %v", gatewayStart, kept)
		}
		restart := false
		for _, m := range kept {
			if strings.Contains(m, "icebath gateway restart") {
				restart = true
			}
		}
		if restart == gatewayStart {
			t.Fatalf("gateway_start %v kept %v", gatewayStart, kept)
		}
	}
}

func TestAMalformedRouteURLDoesNotAbortTheHomeMigration(t *testing.T) {
	leftCoreFresh(t)
	h := leftCoreHome(t, t.TempDir(), "home", "HASS_TOKEN=abc\n", "model:\n  provider: custom\n  base_url: http://[fe80::1/v1\n")
	ctx := bindLeftCoreHome(t, h, true, func(string) (map[string]any, error) { return map[string]any{"ok": true}, nil })
	if got := RecoverLeftCoreAtStartup(ctx, nil, false); len(got) != 1 || got[0] != "homeassistant" {
		t.Fatalf("installed %v", got)
	}
}

func TestTheMigrationInstallsOnceSoARemovalSticks(t *testing.T) {
	leftCoreFresh(t)
	h := leftCoreHome(t, t.TempDir(), "home", "HASS_TOKEN=abc\n", "")
	plugin := filepath.Join(h, "plugins", "homeassistant")
	var calls []string
	install := func(name string) (map[string]any, error) {
		calls = append(calls, name)
		mkPluginDir(t, h, name)
		return map[string]any{"ok": true}, nil
	}
	quiet := func(string) {}
	withPluginHooks(t, func(hk *PluginManagerHooks) { hk.CatalogEntry = func(string) (string, bool) { return "", false } })
	if got := MigrateLeftCoreHome(context.Background(), h, install, quiet, false); len(got) != 0 {
		t.Fatalf("catalog miss installed %v", got)
	}
	withPluginHooks(t, func(*PluginManagerHooks) {})
	if got := MigrateLeftCoreHome(context.Background(), h, func(string) (map[string]any, error) {
		return map[string]any{"ok": false, "error": "network down"}, nil
	}, quiet, false); len(got) != 0 {
		t.Fatalf("failed install installed %v", got)
	}
	if got := MigrateLeftCoreHome(context.Background(), h, install, quiet, false); len(got) != 1 || got[0] != "homeassistant" {
		t.Fatalf("installed %v", got)
	}
	if err := os.RemoveAll(plugin); err != nil {
		t.Fatal(err)
	}
	if got := MigrateLeftCoreHome(context.Background(), h, install, quiet, false); len(got) != 0 {
		t.Fatalf("reinstalled after a removal %v", got)
	}
	ctx := bindLeftCoreHome(t, h, true, install)
	if got := RecoverLeftCoreAtStartup(ctx, quiet, false); len(got) != 0 {
		t.Fatalf("startup reinstalled %v", got)
	}
	if _, err := os.Stat(plugin); len(calls) != 1 || calls[0] != "homeassistant" || !os.IsNotExist(err) {
		t.Fatalf("calls %v stat %v", calls, err)
	}
}

func TestAFailedStartupInstallBacksOffAndReportsOneLine(t *testing.T) {
	leftCoreFresh(t)
	h := leftCoreHome(t, t.TempDir(), "home", "HASS_TOKEN=abc\n", "")
	var lookups []string
	withPluginHooks(t, func(hk *PluginManagerHooks) {
		hk.CatalogEntry = func(name string) (string, bool) {
			lookups = append(lookups, name)
			return name, true
		}
	})
	gitError := "Could not download the plugin from https://github.com/x/y. Check the address.\n" +
		"Details: Cloning into '/h/plugins/.install-1'...\n" +
		"fatal: unable to access 'https://github.com/x/y/': Could not resolve host: github.com\n"
	failing := func(string) (map[string]any, error) { return map[string]any{"ok": false, "error": gitError}, nil }
	ctx := bindLeftCoreHome(t, h, true, failing)
	var said []string
	say := func(m string) { said = append(said, m) }
	if got := RecoverLeftCoreAtStartup(ctx, say, false); len(got) != 0 {
		t.Fatalf("installed %v", got)
	}
	if len(said) != 1 || strings.Contains(said[0], "\n") || !strings.Contains(said[0], "Could not resolve host: github.com") ||
		!strings.Contains(said[0], "plugins install homeassistant") {
		t.Fatalf("said %q", said)
	}
	clearLeftCoreAttempted()
	if got := RecoverLeftCoreAtStartup(ctx, say, false); len(got) != 0 || len(lookups) != 1 || len(said) != 1 {
		t.Fatalf("backed off start installed %v lookups %v said %v", got, lookups, said)
	}
	if got := MigrateLeftCoreHome(ctx, h, failing, say, false); len(got) != 0 || len(lookups) != 2 {
		t.Fatalf("update retry installed %v lookups %v", got, lookups)
	}
}

func TestRepeatedStartupFailuresBackOffExponentially(t *testing.T) {
	leftCoreFresh(t)
	h := leftCoreHome(t, t.TempDir(), "home", "HASS_TOKEN=abc\n", "")
	var attempts []string
	ctx := bindLeftCoreHome(t, h, true, func(name string) (map[string]any, error) {
		attempts = append(attempts, name)
		return map[string]any{"ok": false, "error": "x"}, nil
	})
	stamp := leftCoreFailureStamp(h, "homeassistant")
	quiet := func(string) {}
	startAfter := func(hours float64) {
		then := time.Now().Add(-time.Duration(hours * float64(time.Hour)))
		if err := os.Chtimes(stamp, then, then); err != nil {
			t.Fatal(err)
		}
		clearLeftCoreAttempted()
		RecoverLeftCoreAtStartup(ctx, quiet, false)
	}
	RecoverLeftCoreAtStartup(ctx, quiet, false)
	startAfter(1.5)
	if len(attempts) != 2 {
		t.Fatalf("after 1.5h %d attempts", len(attempts))
	}
	startAfter(1.5)
	if len(attempts) != 2 {
		t.Fatalf("inside the 2h window %d attempts", len(attempts))
	}
	startAfter(2.5)
	startAfter(3.5)
	if len(attempts) != 3 {
		t.Fatalf("after the 4h window %d attempts", len(attempts))
	}
	writeLeftCoreFile(t, stamp, "40")
	startAfter(24 * 6.9)
	if len(attempts) != 3 {
		t.Fatalf("inside the week cap %d attempts", len(attempts))
	}
	startAfter(24 * 7.1)
	if len(attempts) != 4 {
		t.Fatalf("after the week cap %d attempts", len(attempts))
	}
	writeLeftCoreFile(t, stamp, "")
	startAfter(1.5)
	if len(attempts) != 5 {
		t.Fatalf("an empty stamp counts once, %d attempts", len(attempts))
	}
}

func TestOldLangfusePluginKeyKeepsNamingTheCatalogPlugin(t *testing.T) {
	leftCoreFresh(t)
	h := leftCoreHome(t, t.TempDir(), "home", "", "")
	bindLeftCoreHome(t, h, true, nil)
	for _, name := range []string{"observability/langfuse", "langfuse"} {
		message := plugins.UnknownPluginMessage(name, false)
		if !strings.Contains(message, "moved out of core into the 'langfuse' plugin") || !strings.Contains(message, "icebath plugins install langfuse") {
			t.Fatalf("%s: %q", name, message)
		}
	}
	if strings.Contains(plugins.UnknownPluginMessage("nonexistent", false), "moved out of core") {
		t.Fatal("an unknown plugin names no move")
	}
	dir := filepath.Join(h, "plugins", "langfuse")
	writeLeftCoreFile(t, filepath.Join(dir, "plugin.yaml"), "name: langfuse\nversion: 2.0.0\n")
	writeLeftCoreFile(t, filepath.Join(dir, "__init__.py"), "def register(ctx):\n    pass\n")
	entry, err := plugins.FindPluginEntry(h, "observability/langfuse")
	if err != nil || entry == nil || entry.Key != "langfuse" {
		t.Fatalf("entry %+v %v", entry, err)
	}
	manifest := &plugins.Manifest{Name: "langfuse", Source: "user", Path: dir, Key: "langfuse"}
	if got := plugins.GateManifest(h, manifest, map[string]bool{}, map[string]bool{"observability/langfuse": true}, true); got.Action != "load" {
		t.Fatalf("old key enabled %+v", got)
	}
	if got := plugins.GateManifest(h, manifest, map[string]bool{"observability/langfuse": true}, map[string]bool{"langfuse": true}, true); got.Error != plugins.InBothLists {
		t.Fatalf("both lists %+v", got)
	}
	if got := plugins.GateManifest(h, manifest, map[string]bool{"observability/langfuse": true}, nil, false); got.Error != "disabled via config" {
		t.Fatalf("old key disabled %+v", got)
	}
}

func TestOnlyAHomeWithRegisteredImportsGetsThePlugin(t *testing.T) {
	leftCoreFresh(t)
	root := t.TempDir()
	synced := leftCoreHome(t, root, "synced", "", "model: test\n")
	untouched := leftCoreHome(t, root, "untouched", "", "model: test\n")
	writeLeftCoreFile(t, filepath.Join(synced, "import-sync.json"), `{"version": 1, "agents": {}}`)
	var calls, said []string
	install := func(name string) (map[string]any, error) {
		calls = append(calls, name)
		mkPluginDir(t, synced, name)
		return map[string]any{"ok": true}, nil
	}
	say := func(m string) { said = append(said, m) }
	if got := MigrateLeftCoreHome(context.Background(), untouched, install, say, false); len(got) != 0 || len(calls) != 0 || len(said) != 0 {
		t.Fatalf("untouched installed %v calls %v said %v", got, calls, said)
	}
	if got := MigrateLeftCoreHome(context.Background(), synced, install, say, false); len(got) != 1 || got[0] != "agent-importers" {
		t.Fatalf("synced installed %v", got)
	}
	if strings.Join(calls, ",") != "agent-importers" || !strings.Contains(said[0], "✓ `icebath import-agent` moved out of core") {
		t.Fatalf("calls %v said %q", calls, said)
	}
}
