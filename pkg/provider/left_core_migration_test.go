package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Plugin manager stand in with no plugins, every catalog lookup found and every install refused
func testPluginManagerHooks() PluginManagerHooks {
	return PluginManagerHooks{
		CatalogEntry: func(name string) (string, bool) { return name, true },
		Install: func(context.Context, string, bool) (map[string]any, error) {
			return nil, errors.New("provider tests install no plugins")
		},
		LoadError:       func(string) string { return "" },
		DependencyHomes: func() ([]string, error) { return nil, nil },
	}
}

func init() { InstallPluginManagerHooks(testPluginManagerHooks()) }

// Swaps the plugin manager hooks for one test and restores the stand in afterwards
func withPluginHooks(t *testing.T, mutate func(*PluginManagerHooks)) {
	t.Helper()
	hooks := testPluginManagerHooks()
	mutate(&hooks)
	InstallPluginManagerHooks(hooks)
	t.Cleanup(func() { InstallPluginManagerHooks(testPluginManagerHooks()) })
}

// Home directory with optional .env and config.yaml content
func leftCoreHome(t *testing.T, root, name, env, cfg string) string {
	t.Helper()
	h := filepath.Join(root, name)
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if env != "" {
		writeLeftCoreFile(t, filepath.Join(h, ".env"), env)
	}
	if cfg != "" {
		writeLeftCoreFile(t, filepath.Join(h, "config.yaml"), cfg)
	}
	return h
}

func writeLeftCoreFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mkPluginDir(t *testing.T, h, plugin string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(h, "plugins", plugin), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestLeftCoreMigrationKeepsAToolsetOffWhereCoreHadItOff(t *testing.T) {
	h := leftCoreHome(t, t.TempDir(), "home", "HASS_TOKEN=abc\n",
		"platform_toolsets:\n  telegram: [web, terminal, file]\n  discord: [icebath-discord]\n  slack: [web, homeassistant]\n")
	MigrateLeftCoreHome(context.Background(), h, func(string) (map[string]any, error) { return map[string]any{"ok": true}, nil }, func(string) {}, false)
	known := anyMap(leftCoreReadConfig(h)["known_plugin_toolsets"])
	off := map[string]bool{}
	for platform, list := range known {
		if containsString(pyStrList(list.([]any)), "homeassistant") {
			off[platform] = true
		}
	}
	want := map[string]bool{"telegram": true, "webhook": true, "acp": true}
	if len(off) != len(want) || !off["telegram"] || !off["webhook"] || !off["acp"] {
		t.Fatalf("homeassistant off for %v, want %v", off, want)
	}
}

func TestHomeWithoutHomeassistantGetsNothingAndNoNotice(t *testing.T) {
	h := leftCoreHome(t, t.TempDir(), "home", "OPENAI_API_KEY=sk\n", "")
	var said []string
	got := MigrateLeftCoreHome(context.Background(), h, func(string) (map[string]any, error) {
		t.Fatal("installed")
		return nil, nil
	}, func(m string) { said = append(said, m) }, false)
	if len(got) != 0 || len(said) != 0 {
		t.Fatalf("installed %v said %v", got, said)
	}
}

func TestADisabledOrPresentPluginIsLeftAlone(t *testing.T) {
	h := leftCoreHome(t, t.TempDir(), "home", "HASS_TOKEN=abc\n", "")
	mkPluginDir(t, h, "homeassistant")
	got := MigrateLeftCoreHome(context.Background(), h, func(string) (map[string]any, error) {
		t.Fatal("reinstalled")
		return nil, nil
	}, func(string) {}, false)
	if len(got) != 0 {
		t.Fatalf("installed %v", got)
	}
}

func TestMCPServeIsNeverInstalledAutomatically(t *testing.T) {
	h := leftCoreHome(t, t.TempDir(), "home", "", "mcp_servers:\n  icebath:\n    command: icebath\n    args: [mcp, serve]\n")
	var installs []string
	got := MigrateLeftCoreHome(context.Background(), h, func(name string) (map[string]any, error) {
		installs = append(installs, name)
		return map[string]any{"ok": true}, nil
	}, func(string) {}, false)
	if len(got) != 0 || len(installs) != 0 {
		t.Fatalf("installed %v %v", got, installs)
	}
}

func TestLeftCoreRecordMigrationConvertsTheScopeOnceAndMarksTheInstall(t *testing.T) {
	h := leftCoreHome(t, t.TempDir(), "home", "", "platform_toolsets:\n  telegram: [web]\n")
	mkPluginDir(t, h, "homeassistant")
	f := leftCoreFeatureByPlugin("homeassistant")
	if err := leftCoreRecordMigration(context.Background(), h, f); err != nil {
		t.Fatal(err)
	}
	cfg := leftCoreReadConfig(h)
	known := anyMap(cfg["known_plugin_toolsets"])
	for _, platform := range []string{"telegram", "acp", "webhook"} {
		if list, _ := known[platform].([]any); !containsString(pyStrList(list), "homeassistant") {
			t.Fatalf("%s keeps homeassistant on: %v", platform, known)
		}
	}
	if !leftCoreMarked(cfg, leftCoreScopedKey, f) || !leftCoreMarked(cfg, leftCoreInstalledKey, f) {
		t.Fatalf("markers missing: %v", cfg)
	}
	cfg["platform_toolsets"] = map[string]any{"telegram": []any{"web", "homeassistant"}}
	cfg["known_plugin_toolsets"] = map[string]any{"acp": []any{"homeassistant"}, "webhook": []any{"homeassistant"}}
	if err := config.SaveUser(context.Background(), h, cfg); err != nil {
		t.Fatal(err)
	}
	if err := leftCoreRecordMigration(context.Background(), h, f); err != nil {
		t.Fatal(err)
	}
	if _, ok := anyMap(leftCoreReadConfig(h)["known_plugin_toolsets"])["telegram"]; ok {
		t.Fatal("a second run undid the user's later choice")
	}
}

func TestLeftCoreReplacedToolsetsTurnOffEverywhere(t *testing.T) {
	cfg := map[string]any{
		"platform_toolsets": map[string]any{"cli": []any{"browser", "web"}, "telegram": `["web", 'browser']`, "slack": []any{"web"}},
		"agent":             map[string]any{"disabled_toolsets": "[\"memory\"]"},
	}
	f := leftCoreFeatureByPlugin("camofox")
	leftCoreTurnOffReplaced(cfg, f)
	saved := anyMap(cfg["platform_toolsets"])
	if got := pyStrList(saved["cli"].([]any)); strings.Join(got, ",") != "web,camofox" {
		t.Fatalf("cli %v", got)
	}
	if got := pyStrList(saved["telegram"].([]any)); strings.Join(got, ",") != "web,camofox" {
		t.Fatalf("telegram %v", got)
	}
	if got := pyStrList(anyMap(cfg["agent"])["disabled_toolsets"].([]any)); strings.Join(got, ",") != "memory,browser" {
		t.Fatalf("disabled %v", got)
	}
	cfg = map[string]any{"agent": map[string]any{"disabled_toolsets": []any{"browser"}}}
	leftCoreTurnOffReplaced(cfg, f)
	if got := pyStrList(anyMap(cfg["agent"])["disabled_toolsets"].([]any)); strings.Join(got, ",") != "browser,camofox" {
		t.Fatalf("an already disabled browser disables camofox too, got %v", got)
	}
}

func TestLeftCoreNoticesAreKeptOnceAndClaimedOnce(t *testing.T) {
	h := t.TempDir()
	ctx := home.WithOverride(context.Background(), h)
	DeferStartupNotice(ctx, "\u26a0 one")
	DeferStartupNotice(ctx, "\u26a0 one")
	DeferStartupNotice(ctx, "two \U0001F600")
	data, err := os.ReadFile(leftCoreNoticeFile(h))
	if err != nil || string(data) != "\"\\u26a0 one\"\n\"two \\ud83d\\ude00\"\n" {
		t.Fatalf("kept %q %v", data, err)
	}
	writeLeftCoreFile(t, leftCoreNoticeFile(h), string(data)+"not json\n\"two \\ud83d\\ude00\"\n")
	got := ClaimKeptNotices(ctx)
	if len(got) != 2 || got[0] != "\u26a0 one" || got[1] != "two \U0001F600" {
		t.Fatalf("claimed %q", got)
	}
	if again := ClaimKeptNotices(ctx); len(again) != 0 {
		t.Fatalf("claimed twice %q", again)
	}
}

func TestLeftCoreFailedInstallsBackOffExponentially(t *testing.T) {
	h := t.TempDir()
	stamp := leftCoreFailureStamp(h, "homeassistant")
	age := func(hours float64) {
		at := time.Now().Add(-time.Duration(hours * float64(time.Hour)))
		if err := os.Chtimes(stamp, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if LeftCoreFailedRecently(h, "homeassistant") {
		t.Fatal("no stamp, no backoff")
	}
	NoteLeftCoreFailure(h, "homeassistant")
	if !LeftCoreFailedRecently(h, "homeassistant") {
		t.Fatal("fresh failure backs off")
	}
	age(1.1)
	if LeftCoreFailedRecently(h, "homeassistant") {
		t.Fatal("one failure waits an hour")
	}
	NoteLeftCoreFailure(h, "homeassistant")
	age(1.5)
	if !LeftCoreFailedRecently(h, "homeassistant") {
		t.Fatal("two failures wait two hours")
	}
	for i := 0; i < 20; i++ {
		NoteLeftCoreFailure(h, "homeassistant")
	}
	age(24 * 7.1)
	if LeftCoreFailedRecently(h, "homeassistant") {
		t.Fatal("the backoff caps at a week")
	}
	writeLeftCoreFile(t, stamp, "")
	age(1.5)
	if LeftCoreFailedRecently(h, "homeassistant") {
		t.Fatal("an empty stamp counts once")
	}
}

func TestLeftCoreFirstCauseIsGitsOwnLine(t *testing.T) {
	cases := map[string]string{
		"clone failed\nfatal: repository 'x' not found.\n": "repository 'x' not found",
		"\n  network down.  \nmore":                        "network down",
		"":                                                 "unknown error",
		"Cloning...\nerror: RPC failed":                    "RPC failed",
	}
	for in, want := range cases {
		if got := leftCoreFirstCause(in); got != want {
			t.Fatalf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestLeftCoreToolsetValuesParseLikeLiteralEval(t *testing.T) {
	cases := map[string]string{
		`["browser", 'terminal']`:         "browser|terminal",
		`[1, True, None, 2.50, 1e16]`:     "1|True|None|2.5|1e+16",
		`['a' 'b', u"c\n", [1, 'x'], ()]`: "ab|c\n|[1, 'x']|()",
		`[(1,), {'k': 'v'}, {1, 2}]`:      "(1,)|{'k': 'v'}|{1, 2}",
		`["it's"]`:                        "it's",
	}
	for in, want := range cases {
		got, ok := parsePlatformToolsetsValue(in)
		if !ok || strings.Join(pyStrList(got), "|") != want {
			t.Fatalf("%s -> %v %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"browser", "[browser]", "['a',", "['a'] x", "{'a': 1}"} {
		if got, ok := parsePlatformToolsetsValue(bad); ok {
			t.Fatalf("%s parsed as %v", bad, got)
		}
	}
	if got := parseConfigStringList("browser"); len(got) != 1 || got[0] != "browser" {
		t.Fatalf("scalar %v", got)
	}
	if got := parseConfigStringList("[broken"); len(got) != 1 || got[0] != "[broken" {
		t.Fatalf("unparsable list string is one name, got %v", got)
	}
}

func TestLeftCoreRecoverProviderStopsBeforeAnyReadForOtherNames(t *testing.T) {
	h := leftCoreHome(t, t.TempDir(), "home", "", "")
	withPluginHooks(t, func(hk *PluginManagerHooks) {
		hk.Install = func(context.Context, string, bool) (map[string]any, error) {
			t.Fatal("installed")
			return nil, nil
		}
	})
	recoverLeftCoreProvider(context.Background(), h, "openrouter")
	writeLeftCoreFile(t, filepath.Join(h, "auth.json"), `{"active_provider": "openai-codex"}`)
	recoverLeftCoreProvider(context.Background(), h, "auto")
	recoverLeftCoreProvider(WithLeftCoreInstallsHeld(context.Background()), h, "copilot")
	mkPluginDir(t, h, "github-copilot")
	recoverLeftCoreProvider(context.Background(), h, "copilot")
	if _, err := os.Stat(leftCoreNoticeFile(h)); !os.IsNotExist(err) {
		t.Fatal("no notice is kept for a provider that needs no recovery")
	}
}

func TestAnAppendRacingAClaimIsDeliveredNotLost(t *testing.T) {
	h := t.TempDir()
	queueLeftCoreNotice(h, "first")
	opened, goOn := make(chan struct{}), make(chan struct{})
	open := leftCoreOpenAppend
	leftCoreOpenAppend = func(path string) (*os.File, error) {
		fh, err := open(path)
		close(opened)
		<-goOn
		return fh, err
	}
	t.Cleanup(func() { leftCoreOpenAppend = open })
	writerDone := make(chan struct{})
	go func() {
		queueLeftCoreNotice(h, "second")
		close(writerDone)
	}()
	<-opened
	claimedCh := make(chan []string, 1)
	go func() { claimedCh <- claimLeftCoreNotices(h) }()
	var claimed []string
	select {
	case claimed = <-claimedCh:
		t.Fatal("a claim ran while the writer held the notice lock")
	case <-time.After(500 * time.Millisecond):
	}
	close(goOn)
	<-writerDone
	claimed = <-claimedCh
	all := append(claimed, claimLeftCoreNotices(h)...)
	if strings.Join(all, ",") != "first,second" {
		t.Fatalf("delivered %v", all)
	}
}

func TestPluginHooksReturnsInstalledHooks(t *testing.T) {
	withPluginHooks(t, func(h *PluginManagerHooks) {
		h.DependencyHomes = func() ([]string, error) { return []string{"/profiles/coder"}, nil }
	})
	if got, err := PluginHooks().DependencyHomes(); err != nil || len(got) != 1 || got[0] != "/profiles/coder" {
		t.Fatal(got)
	}
}

func TestIsTruthyValueAcceptsTheFlagSpellings(t *testing.T) {
	for _, v := range []any{"1", "true", "yes", "on", true} {
		if !IsTruthyValue(v) {
			t.Fatalf("%v should be truthy", v)
		}
	}
	for _, v := range []any{"0", "false", "", nil, "off"} {
		if IsTruthyValue(v) {
			t.Fatalf("%v should be falsy", v)
		}
	}
}

// MigrateLeftCoreHome installs every left core plugin the home uses and lacks, returning the installed names
func MigrateLeftCoreHome(ctx context.Context, h string, install func(string) (map[string]any, error), say func(string), processEnv bool) []string {
	var installed []string
	for _, f := range leftCorePending(ctx, h, say, processEnv, false) {
		if leftCoreInstallOne(ctx, h, f, install, say, processEnv) {
			installed = append(installed, f.Plugin)
		}
	}
	return installed
}
