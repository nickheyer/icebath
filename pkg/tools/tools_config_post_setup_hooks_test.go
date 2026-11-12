package tools

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
)

func TestLocalSTTModelSummaryMarksTheDefault(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	installSTTCatalogDoubles(t)
	if got := localSTTModelSummary(); got != "base (default), tiny, small, medium, large-v3, turbo" {
		t.Fatalf("summary %q", got)
	}
	s, out := newTestToolsSetup(t, "")
	swapHook(t, &postSetupSyncVenv, func(context.Context, pm.SyncVenvOptions) error { return nil })
	if err := s.runPostSetup("faster_whisper"); err != nil {
		t.Fatal(err)
	}
	want := "      Installing faster-whisper (model ~150MB downloads on first use)...\n" +
		"✓     faster-whisper dependencies ready. Restart Icebath to use them.\n" +
		"      Model sizes: base (default), tiny, small, medium, large-v3, turbo\n" +
		"      Change via stt.local.model in config.yaml\n"
	if out.String() != want {
		t.Fatalf("output %q", out.String())
	}
}

func TestPythonHooksPrintTheirClosingLines(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cases := map[string]string{
		"kittentts": "      Installing kittentts (~25-80MB model, CPU-only)...\n✓     kittentts dependencies ready. Restart Icebath to use them.\n" +
			"      Voices: Jasper, Bella, Luna, Bruno, Rosie, Hugo, Kiki, Leo\n" +
			"      Models: KittenML/kitten-tts-nano-0.8-int8 (25MB), micro (41MB), mini (80MB)\n",
		"piper": "      Installing piper-tts (~14MB wheel, voices downloaded on first use)...\n✓     piper-tts dependencies ready. Restart Icebath to use them.\n" +
			"      Default voice: en_US-lessac-medium (downloaded on first TTS call)\n" +
			"      Full voice list: https://github.com/OHF-Voice/piper1-gpl/blob/main/docs/VOICES.md\n" +
			"      Switch voices by setting tts.piper.voice in config.yaml\n",
	}
	for key, want := range cases {
		s, out := newTestToolsSetup(t, "")
		swapHook(t, &postSetupSyncVenv, func(context.Context, pm.SyncVenvOptions) error { return nil })
		if err := s.runPostSetup(key); err != nil {
			t.Fatal(err)
		}
		if out.String() != want {
			t.Fatalf("%s output %q", key, out.String())
		}
	}
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &postSetupSyncVenv, func(context.Context, pm.SyncVenvOptions) error { return errors.New("worker crashed") })
	if err := s.runPostSetup("piper"); err == nil || err.Error() != "worker crashed" {
		t.Fatalf("unhandled sync failure %v", err)
	}
	if err := s.runPostSetup("no_such_hook"); err != nil {
		t.Fatalf("unknown hook %v", err)
	}
}

func TestEnsureBrowserUseCLIReportsTheHarness(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, out := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.FindBrowserUseCLI, func(*Env) []string { return []string{"python", "-m", "browser_harness.run"} })
	s.ensureBrowserUseCLI()
	swapHook(t, &ToolsConfigHooks.FindBrowserUseCLI, func(*Env) []string { return nil })
	s.ensureBrowserUseCLI()
	want := "✓     browser_exec ready (browser-harness, bundled with Icebath)\n" +
		"⚠     browser-harness is missing from Icebath's Python environment\n" +
		"      Re-sync it with: icebath update\n"
	if out.String() != want {
		t.Fatalf("output %q", out.String())
	}
}

func TestPostSetupLightpandaReportsTheBinary(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, c := range []struct {
		bin, goos, want string
	}{
		{"/usr/local/bin/lightpanda", "linux", "✓     Lightpanda found: /usr/local/bin/lightpanda\n"},
		{"", "linux", "⚠     lightpanda binary not found on PATH, ~/.lightpanda or ~/.local/bin\n      Install it\n"},
		{"", "windows", "⚠     lightpanda binary not found on PATH, ~/.lightpanda or ~/.local/bin\n      Install it\n" +
			"      Lightpanda has no native Windows build; run Icebath under WSL2.\n"},
	} {
		s, out := newTestToolsSetup(t, "")
		swapHook(t, &ToolsConfigHooks.FindBrowserUseCLI, func(*Env) []string { return []string{"harness"} })
		swapHook(t, &ToolsConfigHooks.LightpandaBinary, func(*Env) string { return c.bin })
		swapHook(t, &ToolsConfigHooks.LightpandaInstallHint, func() string { return "Install it" })
		swapHook(t, &postSetupGOOS, c.goos)
		if err := s.runPostSetup("lightpanda"); err != nil {
			t.Fatal(err)
		}
		want := "✓     browser_exec ready (browser-harness, bundled with Icebath)\n" + c.want
		if out.String() != want {
			t.Fatalf("%+v output %q", c, out.String())
		}
	}
}

func TestPostSetupChromiumInstallFailures(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	setup := func(t *testing.T, ensureErr error, termux bool) (*ToolsSetup, *strings.Builder, *int) {
		s, out := newTestToolsSetup(t, "")
		ensures := 0
		swapHook(t, &ToolsConfigHooks.FindBrowserUseCLI, func(*Env) []string { return []string{"harness"} })
		swapHook(t, &ToolsConfigHooks.BakedChromiumFile, func() string { return "/nonexistent/chromium-executable-path" })
		swapHook(t, &ToolsConfigHooks.ChromiumExecutable, func(*Env) string { return "" })
		swapHook(t, &ToolsConfigHooks.ChromiumMissingHint, func(*Env) string { return "No Chromium here." })
		swapHook(t, &postSetupIsTermux, func() bool { return termux })
		swapHook(t, &postSetupEnsure, func(context.Context, string, pm.EnsureOptions) error { ensures++; return ensureErr })
		b := &strings.Builder{}
		s.Con.Out = io.MultiWriter(out, b)
		return s, b, &ensures
	}
	s, out, ensures := setup(t, pm.NewInstallError("chromium", "download failed", ""), false)
	if err := s.runPostSetup("chromium"); err != nil {
		t.Fatal(err)
	}
	want := "✓     browser_exec ready (browser-harness, bundled with Icebath)\n" +
		"⚠     Chromium install failed: chromium: download failed — retry, or run `icebath pm doctor`\n" +
		"      Retry with: icebath tools post-setup chromium\n"
	if out.String() != want || *ensures != 1 {
		t.Fatalf("install error output %q ensures %d", out.String(), *ensures)
	}
	s, out, _ = setup(t, &pm.DownloadError{Message: "hash mismatch"}, false)
	if err := s.runPostSetup("chromium"); err != nil || !strings.Contains(out.String(), "Chromium install failed: hash mismatch") {
		t.Fatalf("download error %v %q", err, out.String())
	}
	s, out, _ = setup(t, &os.PathError{Op: "open", Path: "/x", Err: os.ErrPermission}, false)
	if err := s.runPostSetup("chromium"); err != nil || !strings.Contains(out.String(), "Chromium install failed: open /x") {
		t.Fatalf("os error %v %q", err, out.String())
	}
	s, _, _ = setup(t, &pm.UnknownPackageError{Name: "chromium"}, false)
	if err := s.runPostSetup("chromium"); err == nil || err.Error() != "unknown package: chromium" {
		t.Fatalf("key error not raised %v", err)
	}
	s, _, _ = setup(t, &pm.PyError{Class: "ValueError", Message: "bad pin"}, false)
	if err := s.runPostSetup("chromium"); err == nil {
		t.Fatal("value error swallowed")
	}
	s, out, ensures = setup(t, nil, true)
	if err := s.runPostSetup("chromium"); err != nil {
		t.Fatal(err)
	}
	if *ensures != 0 || out.String() != "✓     browser_exec ready (browser-harness, bundled with Icebath)\n⚠     No Chromium here.\n" {
		t.Fatalf("termux output %q ensures %d", out.String(), *ensures)
	}
}

func TestPostSetupChromiumSkipsHostLibrariesOffLinux(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, c := range []struct {
		goos   string
		termux bool
	}{{"darwin", false}, {"linux", true}} {
		s, out := newTestToolsSetup(t, "")
		swapHook(t, &ToolsConfigHooks.FindBrowserUseCLI, func(*Env) []string { return []string{"harness"} })
		swapHook(t, &ToolsConfigHooks.BakedChromiumFile, func() string { return "/nonexistent" })
		swapHook(t, &ToolsConfigHooks.ChromiumExecutable, func(*Env) string { return "/pm/chrome" })
		swapHook(t, &ToolsConfigHooks.ChromiumMissingHint, func(*Env) string { return "" })
		swapHook(t, &postSetupIsTermux, func() bool { return c.termux })
		swapHook(t, &postSetupGOOS, c.goos)
		if err := s.runPostSetup("chromium"); err != nil {
			t.Fatal(err)
		}
		if out.String() != "✓     browser_exec ready (browser-harness, bundled with Icebath)\n✓     Chromium ready: /pm/chrome\n" {
			t.Fatalf("%+v output %q", c, out.String())
		}
	}
}

// Setup prompt doubles recording the questions asked and answering from the scripts
func setupPromptDoubles(t *testing.T, choice int, answer string) *[]string {
	asked := &[]string{}
	swapHook(t, &ToolsConfigHooks.SetupPromptChoice, func(q string, choices []string, def int) int {
		*asked = append(*asked, q+" "+strings.Join(choices, " | "))
		return choice
	})
	swapHook(t, &ToolsConfigHooks.SetupPrompt, func(q, def string, password bool) string {
		*asked = append(*asked, q)
		if !password {
			t.Fatalf("api key prompt not masked")
		}
		return answer
	})
	return asked
}

func TestPostSetupXAIGrokCredentialPaths(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	unsetEnv(t, "XAI_API_KEY")
	s, out := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.XAIOAuthStatus, func(context.Context, string) map[string]any { return map[string]any{"logged_in": true} })
	if err := s.runPostSetup("xai_grok"); err != nil || out.String() != "✓     xAI will use your xAI Grok OAuth (SuperGrok / Premium+) credentials\n" {
		t.Fatalf("oauth %v %q", err, out.String())
	}
	s, out = newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.XAIOAuthStatus, func(context.Context, string) map[string]any { panic("store unreadable") })
	writeHomeEnv(t, s.home(), map[string]string{"XAI_API_KEY": "xai-123"})
	if err := s.runPostSetup("xai_grok"); err != nil || out.String() != "✓     xAI will use your existing XAI_API_KEY\n" {
		t.Fatalf("api key %v %q", err, out.String())
	}
	swapHook(t, &ToolsConfigHooks.XAIOAuthStatus, func(context.Context, string) map[string]any { return map[string]any{} })
	question := "    How do you want xAI to authenticate? Sign in with xAI Grok OAuth (SuperGrok / Premium+) — browser login | " +
		"Paste an xAI API key (console.x.ai) | Skip — configure later via `icebath auth add xai-oauth`"
	for _, c := range []struct {
		choice  int
		login   bool
		answer  string
		want    string
		saved   string
		prompts []string
	}{
		{0, true, "", "✓     Logged in — xAI will use these OAuth credentials\n", "", []string{question}},
		{0, false, "", "⚠     xAI Grok OAuth login did not complete. Run later: icebath auth add xai-oauth\n", "", []string{question}},
		{1, false, "xai-pasted", "✓     XAI_API_KEY saved\n", "xai-pasted", []string{question, "    xAI API key"}},
		{1, false, "", "⚠     No API key provided. Run later: icebath auth add xai-oauth\n", "", []string{question, "    xAI API key"}},
		{2, false, "", "      xAI will remain inactive until credentials are configured.\n", "", []string{question}},
	} {
		s, out := newTestToolsSetup(t, "")
		asked := setupPromptDoubles(t, c.choice, c.answer)
		logins := 0
		swapHook(t, &ToolsConfigHooks.XAIOAuthLoginFromSetup, func(context.Context, string) bool { logins++; return c.login })
		if err := s.runPostSetup("xai_grok"); err != nil {
			t.Fatal(err)
		}
		if out.String() != "      xAI needs credentials. Choose one:\n"+c.want || !slices.Equal(*asked, c.prompts) {
			t.Fatalf("choice %d output %q asked %q", c.choice, out.String(), *asked)
		}
		if (c.choice == 0) != (logins == 1) {
			t.Fatalf("choice %d ran %d logins", c.choice, logins)
		}
		if got, _ := s.getEnvValue("XAI_API_KEY"); got != c.saved {
			t.Fatalf("choice %d saved %q", c.choice, got)
		}
		os.Unsetenv("XAI_API_KEY")
	}
	s, out = newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.XAIOAuthLoginFromSetup, nil)
	if err := s.runPostSetup("xai_grok"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "      xAI needs credentials. Choose one:\n⚠     Could not load setup helpers: setup helpers are not installed\n"+
		"      Run later: icebath auth add xai-oauth   (or set XAI_API_KEY)\n" {
		t.Fatalf("unwired output %q", out.String())
	}
}

func TestPostSetupOpenAICodexSignIn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	relogin := "icebath auth add openai-codex"
	s, out := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.CodexAuthStatus, func(context.Context, string) map[string]any { return map[string]any{"logged_in": true} })
	if err := s.runPostSetup("openai_codex"); err != nil || out.String() != "✓     Image generation will use your existing Codex/ChatGPT OAuth credentials\n" {
		t.Fatalf("present %v %q", err, out.String())
	}
	swapHook(t, &ToolsConfigHooks.CodexAuthStatus, func(context.Context, string) map[string]any { panic("pool unreadable") })
	head := "      OpenAI (Codex auth) needs credentials.\n"
	s, out = newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.SetupIsNoninteractive, func() bool { return true })
	if err := s.runPostSetup("openai_codex"); err != nil || out.String() != head+"      No terminal to sign in from. Run: "+relogin+"\n" {
		t.Fatalf("noninteractive %v %q", err, out.String())
	}
	swapHook(t, &ToolsConfigHooks.SetupIsNoninteractive, func() bool { return false })
	type saveCall struct {
		tokens      map[string]any
		lastRefresh string
		setActive   bool
	}
	for _, c := range []struct {
		choice   int
		creds    map[string]any
		loginErr error
		saveErr  error
		want     string
		saves    int
	}{
		{1, nil, nil, nil, "      Codex image generation will remain inactive until you sign in.\n", 0},
		{0, nil, cliui.ErrInterrupt, nil, "⚠     Codex sign-in did not complete: . Run later: " + relogin + "\n", 0},
		{0, map[string]any{"last_refresh": "x"}, nil, nil, "⚠     Codex sign-in did not complete: 'tokens'. Run later: " + relogin + "\n", 0},
		{0, map[string]any{"tokens": map[string]any{"access_token": "a"}}, nil, errors.New("store locked"),
			"⚠     Codex sign-in did not complete: store locked. Run later: " + relogin + "\n", 1},
		{0, map[string]any{"tokens": map[string]any{"access_token": "a"}, "last_refresh": "2026-01-01T00:00:00Z"}, nil, nil,
			"✓     Logged in — image generation will use these Codex OAuth credentials\n", 1},
	} {
		s, out := newTestToolsSetup(t, "")
		var questions []string
		swapHook(t, &ToolsConfigHooks.SetupPromptChoice, func(q string, choices []string, def int) int {
			questions = append(questions, q+" "+strings.Join(choices, " | "))
			return c.choice
		})
		swapHook(t, &ToolsConfigHooks.CodexDeviceCodeLogin, func(_ context.Context, h string, w io.Writer) (map[string]any, error) {
			if h != s.home() || w != s.Con.Out {
				t.Fatalf("login home %q", h)
			}
			return c.creds, c.loginErr
		})
		var saves []saveCall
		swapHook(t, &ToolsConfigHooks.SaveCodexTokens, func(_ context.Context, _ string, tokens map[string]any, lastRefresh string, setActive bool) error {
			saves = append(saves, saveCall{tokens, lastRefresh, setActive})
			return c.saveErr
		})
		if err := s.runPostSetup("openai_codex"); err != nil {
			t.Fatal(err)
		}
		if out.String() != head+c.want || len(saves) != c.saves {
			t.Fatalf("choice %d output %q saves %v", c.choice, out.String(), saves)
		}
		if !slices.Equal(questions, []string{"    How do you want to sign in? Sign in with ChatGPT/Codex OAuth — browser login | Skip — configure later via `" + relogin + "`"}) {
			t.Fatalf("questions %q", questions)
		}
		if len(saves) == 1 && (saves[0].setActive || !pyjson.Equal(saves[0].tokens, map[string]any{"access_token": "a"}) || saves[0].lastRefresh != pyOrStr(c.creds["last_refresh"])) {
			t.Fatalf("save call %+v", saves[0])
		}
	}
	s, out = newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.CodexDeviceCodeLogin, nil)
	if err := s.runPostSetup("openai_codex"); err != nil || out.String() != head+"⚠     Could not load setup helpers: setup helpers are not installed\n      Run later: "+relogin+"\n" {
		t.Fatalf("unwired %v %q", err, out.String())
	}
	swapHook(t, &ToolsConfigHooks.CodexAuthStatus, nil)
	if s.codexCredentialsPresent() {
		t.Fatal("unwired codex status read as present")
	}
}

func TestPostSetupAuthReadyPredicates(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestToolsSetup(t, "")
	unsetEnv(t, "XAI_API_KEY")
	swapHook(t, &XAIOAuthTokensReader, func(context.Context, string) error { return errors.New("none") })
	if postSetupAuthReady["xai_grok"](s) {
		t.Fatal("xai ready without credentials")
	}
	writeHomeEnv(t, s.home(), map[string]string{"XAI_API_KEY": "k"})
	if !postSetupAuthReady["xai_grok"](s) {
		t.Fatal("xai key not read")
	}
	swapHook(t, &ToolsConfigHooks.CodexAuthStatus, func(context.Context, string) map[string]any { return map[string]any{"logged_in": "yes"} })
	if !postSetupAuthReady["openai_codex"](s) {
		t.Fatal("codex login not read")
	}
}

func TestCuaDriverHookRunsTheInstaller(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestToolsSetup(t, "")
	var cons []*cliui.Console
	swapHook(t, &ToolsConfigHooks.InstallCuaDriver, func(_ context.Context, env *Env, con *cliui.Console) (bool, error) {
		cons = append(cons, con)
		return false, nil
	})
	if err := s.runPostSetup("cua_driver"); err != nil || len(cons) != 1 || cons[0] != s.Con {
		t.Fatalf("installer calls %v %v", cons, err)
	}
	swapHook(t, &ToolsConfigHooks.InstallCuaDriver, func(context.Context, *Env, *cliui.Console) (bool, error) {
		return false, errors.New("pm offline")
	})
	if err := s.runPostSetup("cua_driver"); err == nil || err.Error() != "pm offline" {
		t.Fatalf("installer error %v", err)
	}
}

func TestValidPostSetupKeysCoverCatalogAndPluginRows(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.BrowserRegistry, fakePickerRegistry(nil,
		&fakePickerProvider{name: "steel", schema: map[string]any{"name": "Steel", "post_setup": "steel_cli"}}))
	swapHook(t, &ToolsConfigHooks.ImageGenRegistry, &PickerRegistry{List: func(context.Context, *Env) []PickerProvider { panic("image registry boom") }})
	swapHook(t, &ToolsConfigHooks.VideoGenRegistry, nil)
	got := s.ValidPostSetupKeys()
	want := []string{"browserbase", "chromium", "cua_driver", "faster_whisper", "kittentts", "lightpanda", "piper", "steel_cli", "xai_grok"}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, want) {
		t.Fatalf("keys %v", keys)
	}
}

func TestRunPostSetupCommandExitCodes(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swapHook(t, &ToolsConfigHooks.BrowserRegistry, fakePickerRegistry(nil))
	swapHook(t, &ToolsConfigHooks.ImageGenRegistry, fakePickerRegistry(nil))
	swapHook(t, &ToolsConfigHooks.VideoGenRegistry, fakePickerRegistry(nil))
	s, out := newTestToolsSetup(t, "")
	if code := s.RunPostSetupCommand(map[string]any{"post_setup_key": nil}); code != 2 || out.String() != "✗ Usage: icebath tools post-setup <key>\n" {
		t.Fatalf("usage %d %q", code, out.String())
	}
	s, out = newTestToolsSetup(t, "")
	if code := s.RunPostSetupCommand(map[string]any{"post_setup_key": "bogus"}); code != 2 ||
		out.String() != "✗ Unknown post-setup key: 'bogus'. Valid keys: browserbase, chromium, cua_driver, faster_whisper, kittentts, lightpanda, piper, xai_grok\n" {
		t.Fatalf("unknown %d %q", code, out.String())
	}
	s, out = newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.FindBrowserUseCLI, func(*Env) []string { return []string{"h"} })
	if code := s.RunPostSetupCommand(map[string]any{"post_setup_key": "browserbase"}); code != 0 ||
		out.String() != "  Running post-setup hook: browserbase\n✓     browser_exec ready (browser-harness, bundled with Icebath)\n✓ Post-setup 'browserbase' complete\n" {
		t.Fatalf("success %d %q", code, out.String())
	}
	s, out = newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.FindBrowserUseCLI, func(*Env) []string { panic("harness probe crashed") })
	if code := s.RunPostSetupCommand(map[string]any{"post_setup_key": "browserbase"}); code != 1 ||
		out.String() != "  Running post-setup hook: browserbase\n✗ Post-setup failed: harness probe crashed\n" {
		t.Fatalf("panic %d %q", code, out.String())
	}
	s, out = newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.InstallCuaDriver, func(context.Context, *Env, *cliui.Console) (bool, error) { return false, errors.New("pm offline") })
	if code := s.RunPostSetupCommand(map[string]any{"post_setup_key": "cua_driver"}); code != 1 ||
		out.String() != "  Running post-setup hook: cua_driver\n✗ Post-setup failed: pm offline\n" {
		t.Fatalf("error %d %q", code, out.String())
	}
	saved := ToolCategories
	t.Cleanup(func() { ToolCategories = saved })
	ToolCategories = pyjson.NewObject()
	savedWeb := webPickerRegistry
	t.Cleanup(func() { webPickerRegistry = savedWeb })
	webPickerRegistry = fakePickerRegistry(nil)
	s, out = newTestToolsSetup(t, "")
	if code := s.RunPostSetupCommand(map[string]any{"post_setup_key": 3}); code != 2 || out.String() != "✗ Unknown post-setup key: 3. Valid keys: (none)\n" {
		t.Fatalf("no keys %d %q", code, out.String())
	}
}

func TestPostSetupInstalledChecks(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestToolsSetup(t, "")
	if !s.PostSetupAlreadyInstalled("chromium") {
		t.Fatal("unchecked key read as missing")
	}
	for _, c := range []struct {
		ready bool
		err   error
		want  bool
	}{{true, nil, true}, {false, nil, false}, {false, errors.New("x"), true}} {
		swapHook(t, &ToolsConfigHooks.CuaDriverInstallReady, func(*Env) (bool, error) { return c.ready, c.err })
		if got := s.PostSetupAlreadyInstalled("cua_driver"); got != c.want {
			t.Fatalf("%+v got %v", c, got)
		}
	}
	swapHook(t, &ToolsConfigHooks.CuaDriverInstallReady, func(*Env) (bool, error) { panic("crash") })
	if !s.PostSetupAlreadyInstalled("cua_driver") {
		t.Fatal("crashing check read as missing")
	}
	swapHook(t, &postSetupModuleImportable, func(context.Context, string) bool { panic("probe crash") })
	if s.moduleInstalled("piper") {
		t.Fatal("crashing probe read as installed")
	}
	var probed []string
	swapHook(t, &postSetupModuleImportable, func(_ context.Context, m string) bool { probed = append(probed, m); return m == "piper" })
	for key, want := range map[string]bool{"faster_whisper": false, "kittentts": false, "piper": true} {
		if got, err := postSetupReady[key](s); err != nil || got != want {
			t.Fatalf("%s ready %v %v", key, got, err)
		}
	}
	slices.Sort(probed)
	if !slices.Equal(probed, []string{"faster_whisper", "kittentts", "piper"}) {
		t.Fatalf("probed %v", probed)
	}
	swapHook(t, &ToolsConfigHooks.LightpandaBinary, func(*Env) string { return "/bin/lightpanda" })
	if ok, _ := postSetupReady["lightpanda"](s); !ok {
		t.Fatal("lightpanda binary not read")
	}
	swapHook(t, &ToolsConfigHooks.LightpandaBinary, func(*Env) string { panic("crash") })
	if ok, _ := postSetupReady["lightpanda"](s); ok {
		t.Fatal("crashing lightpanda lookup read as installed")
	}
	swapHook(t, &ToolsConfigHooks.LightpandaBinary, nil)
	if ok, _ := postSetupReady["lightpanda"](s); ok {
		t.Fatal("unwired lightpanda lookup read as installed")
	}
	swapHook(t, &ToolsConfigHooks.CuaDriverInstallReady, func(*Env) (bool, error) { return true, nil })
	if ok, err := postSetupReady["cua_driver"](s); !ok || err != nil {
		t.Fatalf("cua ready %v %v", ok, err)
	}
}

func TestPMRuntimeOrOSErrorClasses(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, c := range []struct {
		err  error
		want bool
	}{
		{pm.NewInstallError("a", "b", ""), true},
		{&pm.ResolutionConflict{InstallError: pm.NewInstallError("a", "b", "")}, true},
		{&pm.DownloadError{Message: "x"}, true},
		{&pm.FeatureProbeError{Message: "x"}, true},
		{&pm.PyError{Class: "RuntimeError", Message: "x"}, true},
		{&pm.PyError{Class: "ConnectionResetError", Message: "x"}, true},
		{&pm.PyError{Class: "KeyError", Message: "x"}, false},
		{&pm.TarReadError{Class: "OSError", Message: "x"}, true},
		{&pm.TarReadError{Class: "ReadError", Message: "x"}, false},
		{os.ErrNotExist, true},
		{errors.New("plain"), false},
	} {
		if got := pmRuntimeOrOSError(c.err); got != c.want {
			t.Fatalf("%T %v got %v", c.err, c.err, got)
		}
	}
}

func TestSelectedEnvironmentPythonNamesAnInterpreter(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	python, err := selectedEnvironmentPython(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(python, "python") {
		t.Fatalf("interpreter %q", python)
	}
	venv, err := pm.SelectedVenv(context.Background(), pm.RepoRoot())
	if err != nil {
		t.Fatal(err)
	}
	command, err := pm.VenvCommand(context.Background(), pm.RepoRoot(), venv, nil)
	if err != nil || command[0] != python {
		t.Fatalf("venv command %v %v", command, err)
	}
	_ = config.Tree{}
}
