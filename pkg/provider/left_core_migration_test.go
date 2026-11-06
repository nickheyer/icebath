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
		LoadError:        func(string) string { return "" },
		RefreshBoundHome: func() {},
		DependencyHomes:  func() []string { return nil },
		AuxiliaryTasks:   func() []map[string]any { return nil },
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

// Named profile home under a root bound as the process home so install commands carry the profile
func leftCoreProfileHome(t *testing.T) (string, context.Context) {
	t.Helper()
	root := t.TempDir()
	t.Setenv(home.EnvHome, root)
	home.ResetKeyCache()
	h := leftCoreHome(t, root, filepath.Join("profiles", "work"), "", "")
	return h, home.WithOverride(context.Background(), h)
}

func mkPluginDir(t *testing.T, h, plugin string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(h, "plugins", plugin), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestLeftCoreMigrateHomeInstallsTheCatalogPluginOnce(t *testing.T) {
	h := leftCoreHome(t, t.TempDir(), "home", "HASS_TOKEN=abc\n", "")
	var calls, said []string
	install := func(name string) (map[string]any, error) {
		calls = append(calls, name)
		mkPluginDir(t, h, name)
		return map[string]any{"ok": true}, nil
	}
	say := func(m string) { said = append(said, m) }
	if got := MigrateLeftCoreHome(context.Background(), h, install, say, false); len(got) != 1 || got[0] != "homeassistant" {
		t.Fatalf("installed %v", got)
	}
	if len(calls) != 1 || !strings.Contains(said[0], "\u2713 Home Assistant moved out of core") {
		t.Fatalf("calls %v said %v", calls, said)
	}
	said = nil
	if got := MigrateLeftCoreHome(context.Background(), h, install, say, false); len(got) != 0 || len(calls) != 1 || len(said) != 0 {
		t.Fatalf("second pass installed %v calls %v said %v", got, calls, said)
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

func TestLeftCoreMigrateHomeReportsAFailedInstallWithTheCommand(t *testing.T) {
	h := leftCoreHome(t, t.TempDir(), "home", "HASS_TOKEN=abc\n", "")
	var said []string
	got := MigrateLeftCoreHome(context.Background(), h, func(string) (map[string]any, error) {
		return map[string]any{"ok": false, "error": "network down"}, nil
	}, func(m string) { said = append(said, m) }, false)
	if len(got) != 0 || !strings.Contains(said[0], "could not be installed automatically: network down") ||
		!strings.Contains(said[0], "plugins install homeassistant") {
		t.Fatalf("installed %v said %v", got, said)
	}
}

func TestLeftCoreHomeWithoutHomeassistantGetsNothingAndNoNotice(t *testing.T) {
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

func TestLeftCoreAPresentPluginIsLeftAlone(t *testing.T) {
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

func TestLeftCoreMCPServeIsNeverInstalledAutomatically(t *testing.T) {
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
	if err := leftCoreRecordMigration(h, f); err != nil {
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
	if err := config.SaveUser(h, cfg); err != nil {
		t.Fatal(err)
	}
	if err := leftCoreRecordMigration(h, f); err != nil {
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

func TestLeftCoreSpotifyInUseIsAStoredLoginOrTheToolset(t *testing.T) {
	cases := []struct {
		auth, cfg string
		inUse     bool
	}{
		{`{"providers": {"spotify": {"refresh_token": "r"}}}`, "", true},
		{`{"providers": {"spotify": {"access_token": "a"}}}`, "", true},
		{"", "platform_toolsets:\n  cli: [icebath-cli, spotify]\n", true},
		{`{"providers": {"spotify": {"client_id": "only-the-app"}}}`, "", false},
		{`{"providers": {"nous": {"access_token": "a"}}}`, "platform_toolsets:\n  cli: [icebath-cli]\n", false},
	}
	for i, c := range cases {
		h := leftCoreHome(t, t.TempDir(), "home", "", c.cfg)
		if c.auth != "" {
			writeLeftCoreFile(t, filepath.Join(h, "auth.json"), c.auth)
		}
		if got := spotifyInUse(h, false); got != c.inUse {
			t.Fatalf("case %d in use %v", i, got)
		}
	}
}

func TestLeftCoreOldSpotifyCommandsPointAtThePlugin(t *testing.T) {
	h, ctx := leftCoreProfileHome(t)
	hint := MovedCommandHint(ctx, "icebath auth", "spotify", "status")
	if !strings.Contains(hint, "`icebath auth status spotify` is now `icebath spotify status`") || !strings.Contains(hint, "plugins install spotify") {
		t.Fatalf("auth hint %q", hint)
	}
	if !strings.Contains(MovedCommandHint(ctx, "icebath", "spotify", ""), "plugins install spotify") {
		t.Fatal("bare command names the install")
	}
	mkPluginDir(t, h, "spotify")
	if !strings.Contains(MovedCommandHint(ctx, "icebath", "spotify", ""), "plugins enable spotify") {
		t.Fatal("installed but disabled names the enable command")
	}
	if MovedCommandHint(ctx, "icebath", "honcho", "") != "" {
		t.Fatal("honcho never left core through this table")
	}
}

func TestLeftCoreOldMCPServeSpellingNamesThePluginCommand(t *testing.T) {
	h, ctx := leftCoreProfileHome(t)
	hint := MovedCommandHint(ctx, "icebath mcp", "serve", "")
	if !strings.Contains(hint, "`icebath mcp serve` is now `icebath mcp-serve`") || !strings.Contains(hint, "plugins install mcp-serve") {
		t.Fatalf("hint %q", hint)
	}
	mkPluginDir(t, h, "mcp-serve")
	hint = MovedCommandHint(ctx, "icebath mcp", "serve", "")
	if !strings.Contains(hint, "is now `icebath mcp-serve`") || strings.Contains(hint, "install") {
		t.Fatalf("installed hint %q", hint)
	}
	if got := MovedCommandHint(ctx, "icebath", "claw", ""); !strings.HasPrefix(got, "`icebath claw` moved out of core into the 'agent-importers' plugin") {
		t.Fatalf("old cli hint %q", got)
	}
}

func TestLeftCoreLangfuseInUseIsTheBundledPluginEnabled(t *testing.T) {
	cases := []struct {
		cfg, managed string
		inUse        bool
	}{
		{"plugins:\n  enabled: [observability/langfuse]\n", "", true},
		{"plugins:\n  enabled: [langfuse]\n", "", true},
		{"plugins:\n  enabled: [observability/langfuse]\n  disabled: [langfuse]\n", "", false},
		{"plugins:\n  enabled: [other]\n", "", false},
		{"", "", false},
		{"", "plugins:\n  enabled: [observability/langfuse]\n", true},
	}
	for i, c := range cases {
		root := t.TempDir()
		h := leftCoreHome(t, root, "home", "ICEBATH_LANGFUSE_PUBLIC_KEY=pk-lf-x\nICEBATH_LANGFUSE_SECRET_KEY=sk-lf-x\n", c.cfg)
		managed := filepath.Join(root, "managed")
		if err := os.MkdirAll(managed, 0o755); err != nil {
			t.Fatal(err)
		}
		if c.managed != "" {
			writeLeftCoreFile(t, filepath.Join(managed, "config.yaml"), c.managed)
		}
		t.Setenv(config.EnvManagedDir, managed)
		config.InvalidateManagedCache()
		if got := langfuseInUse(h, false); got != c.inUse {
			t.Fatalf("case %d in use %v", i, got)
		}
	}
	config.InvalidateManagedCache()
}

func TestLeftCoreOldLangfusePluginKeyKeepsNamingTheCatalogPlugin(t *testing.T) {
	_, ctx := leftCoreProfileHome(t)
	for _, name := range []string{"observability/langfuse", "langfuse"} {
		msg := MovedPluginHint(ctx, name)
		if !strings.Contains(msg, "moved out of core into the 'langfuse' plugin") || !strings.Contains(msg, "icebath -p work plugins install langfuse") {
			t.Fatalf("%s hint %q", name, msg)
		}
	}
	if MovedPluginHint(ctx, "nonexistent") != "" {
		t.Fatal("unknown plugin names nothing")
	}
	if LegacyPluginAlias("observability/langfuse") != "langfuse" || !LegacyPluginKeys("langfuse")["observability/langfuse"] {
		t.Fatal("bundled key aliases the catalog plugin")
	}
}

func TestLeftCoreProviderInstallHintNamesInstallEnableOrUpdate(t *testing.T) {
	h, ctx := leftCoreProfileHome(t)
	if ProviderInstallHint(ctx, "openrouter") != "" {
		t.Fatal("openrouter never left core")
	}
	want := "GitHub Copilot moved out of core into the 'github-copilot' plugin: install it with `icebath -p work plugins install github-copilot`."
	if got := ProviderInstallHint(ctx, " Copilot "); got != want {
		t.Fatalf("absent %q", got)
	}
	mkPluginDir(t, h, "github-copilot")
	writeLeftCoreFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  disabled: [github-copilot]\n")
	if got := ProviderInstallHint(ctx, "copilot-acp"); !strings.HasSuffix(got, "installed but disabled: run `icebath -p work plugins enable github-copilot`.") {
		t.Fatalf("disabled %q", got)
	}
	writeLeftCoreFile(t, filepath.Join(h, "config.yaml"), "{}\n")
	withPluginHooks(t, func(hk *PluginManagerHooks) {
		hk.LoadError = func(plugin string) string { return "ImportError: no module named x" }
	})
	got := ProviderInstallHint(ctx, "github")
	if !strings.Contains(got, "installed but failed to load (ImportError: no module named x): run `icebath -p work plugins update github-copilot`") {
		t.Fatalf("failed load %q", got)
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
	NoteLeftCoreSuccess(h, "homeassistant")
	if _, err := os.Stat(stamp); !os.IsNotExist(err) {
		t.Fatal("success clears the stamp")
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

func TestLeftCoreEnvNameSets(t *testing.T) {
	launch := leftCoreLaunchEnvNames()
	if len(launch) != 3 || !launch["ICEBATH_COPILOT_ACP_COMMAND"] || !launch["COPILOT_CLI_PATH"] || launch["COPILOT_ACP_BASE_URL"] {
		t.Fatalf("launch %v", launch)
	}
	profile := leftCoreProfileEnv()
	if strings.Join(profile, ",") != "ICEBATH_COPILOT_ACP_COMMAND,ICEBATH_COPILOT_ACP_ARGS,COPILOT_CLI_PATH,COPILOT_ACP_BASE_URL" {
		t.Fatalf("profile %v", profile)
	}
	if !EnvWriterLaunchDenylist()["ICEBATH_COPILOT_ACP_ARGS"] {
		t.Fatal("the env writers refuse a left core launch name")
	}
	if LeftCoreTerminalKeys["terminal.singularity_image"] != "TERMINAL_SINGULARITY_IMAGE" || len(LeftCoreVersions()) != 1 || LeftCoreVersions()[0] != 52 {
		t.Fatal("terminal keys and versions")
	}
	if PlatformInstallHint(context.Background(), "telegram") != "" || TerminalBackendInstallHint(context.Background(), "docker") != "" {
		t.Fatal("core platforms and backends name nothing")
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
	writeLeftCoreFile(t, filepath.Join(h, "auth.json"), `{"active_provider": "nous"}`)
	recoverLeftCoreProvider(context.Background(), h, "auto")
	recoverLeftCoreProvider(WithLeftCoreInstallsHeld(context.Background()), h, "copilot")
	mkPluginDir(t, h, "github-copilot")
	recoverLeftCoreProvider(context.Background(), h, "copilot")
	if _, err := os.Stat(leftCoreNoticeFile(h)); !os.IsNotExist(err) {
		t.Fatal("no notice is kept for a provider that needs no recovery")
	}
}

func TestLeftCoreSingularityAndImportsInUseFromTheirConfig(t *testing.T) {
	h := leftCoreHome(t, t.TempDir(), "home", "", "terminal:\n  backend: Singularity\n")
	if !singularityInUse(h, false) {
		t.Fatal("terminal.backend selects singularity")
	}
	h = leftCoreHome(t, t.TempDir(), "home", "", "terminal:\n  backend: ''\n  env_type: singularity\n")
	if !singularityInUse(h, false) {
		t.Fatal("legacy env_type selects singularity")
	}
	if agentImportsInUse(h, false) {
		t.Fatal("no import-sync.json, no use")
	}
	writeLeftCoreFile(t, filepath.Join(h, "import-sync.json"), "{}")
	if !agentImportsInUse(h, false) {
		t.Fatal("a registered sync source is use")
	}
}

func TestLeftCoreAnAppendRacingAClaimIsDeliveredNotLost(t *testing.T) {
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
		h.DependencyHomes = func() []string { return []string{"/profiles/coder"} }
	})
	if got := PluginHooks().DependencyHomes(); len(got) != 1 || got[0] != "/profiles/coder" {
		t.Fatal(got)
	}
}
