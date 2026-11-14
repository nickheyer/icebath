package tools

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Tools setup run over a temp home with a plain console reading input and recording output
func newTestToolsSetup(t *testing.T, input string) (*ToolsSetup, *bytes.Buffer) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	out := &bytes.Buffer{}
	con := &cliui.Console{
		In: strings.NewReader(input), Out: out,
		Getenv:    func(string) (string, bool) { return "", false },
		StdinTTY:  func() bool { return true },
		StdoutTTY: func() bool { return false },
		MaskedSecretPrompt: func(prompt string) (string, error) {
			return "", cliui.ErrEOF
		},
	}
	guardInteractiveHooks(t)
	return &ToolsSetup{Ctx: context.Background(), Env: &Env{Home: h, Config: config.Tree{}}, Con: con}, out
}

// Fails the test on any login, install or setup prompt hook it did not double itself
func guardInteractiveHooks(t *testing.T) {
	t.Helper()
	fail := func(name string) { t.Fatalf("unexpected call to %s", name) }
	swapHook(t, &ToolsConfigHooks.SetupPromptChoice, func(string, []string, int) int { fail("SetupPromptChoice"); return 0 })
	swapHook(t, &ToolsConfigHooks.SetupPrompt, func(string, string, bool) string { fail("SetupPrompt"); return "" })
	swapHook(t, &ToolsConfigHooks.XAIOAuthLoginFromSetup, func(context.Context, string) bool { fail("XAIOAuthLoginFromSetup"); return false })
	swapHook(t, &ToolsConfigHooks.CodexDeviceCodeLogin, func(context.Context, string, io.Writer) (map[string]any, error) {
		fail("CodexDeviceCodeLogin")
		return nil, nil
	})
	swapHook(t, &ToolsConfigHooks.SaveCodexTokens, func(context.Context, string, map[string]any, string, bool) error { fail("SaveCodexTokens"); return nil })
	swapHook(t, &ToolsConfigHooks.EnsureNousPortalAccess, func(context.Context, string, io.Writer, string, string) bool {
		fail("EnsureNousPortalAccess")
		return false
	})
	swapHook(t, &ToolsConfigHooks.InstallCuaDriver, func(context.Context, *Env, *cliui.Console) (bool, error) { fail("InstallCuaDriver"); return false, nil })
	swapHook(t, &postSetupSyncVenv, func(context.Context, pm.SyncVenvOptions) error { fail("pm.SyncVenv"); return nil })
	swapHook(t, &postSetupEnsure, func(context.Context, string, pm.EnsureOptions) error { fail("pm.Ensure"); return nil })
	swapHook(t, &postSetupModuleImportable, func(context.Context, string) bool { return false })
}

// Replaces a hook or seam for one test, restoring the previous value at cleanup
func swapHook[T any](t *testing.T, slot *T, v T) {
	t.Helper()
	prev := *slot
	*slot = v
	t.Cleanup(func() { *slot = prev })
}

// Clears process env vars for one test the way monkeypatch.delenv does
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// Writes KEY=value lines into the home env file
func writeHomeEnv(t *testing.T, h string, values map[string]string) {
	t.Helper()
	var b strings.Builder
	for k, v := range values {
		b.WriteString(k + "=" + v + "\n")
	}
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
}

// Texts printed through print_success, the status mark stripped
func successLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if text, ok := strings.CutPrefix(line, "✓ "); ok {
			lines = append(lines, text)
		}
	}
	return lines
}

func TestToolsSetupHomeAndContext(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	if s.home() != s.Env.Home {
		t.Fatalf("home %q", s.home())
	}
	bare := &ToolsSetup{}
	if bare.home() != os.Getenv("ICEBATH_HOME") || bare.toolEnv().Home != bare.home() {
		t.Fatalf("bare home %q env %+v", bare.home(), bare.toolEnv())
	}
	if err := s.saveEnvValue("TOOLS_SETUP_PROBE", "on"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.getEnvValue("TOOLS_SETUP_PROBE"); err != nil || v != "on" {
		t.Fatalf("env value %q %v", v, err)
	}
	os.Unsetenv("TOOLS_SETUP_PROBE")
	cfg, err := s.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfgSection(cfg, "web")["backend"] = "exa"
	if err := s.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	again, err := s.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := config.CfgGet(again, "web", "backend"); v != "exa" {
		t.Fatalf("saved backend %v", v)
	}
}

func TestToolsConfigHookPanicsWhenUninstalled(t *testing.T) {
	swapHook(t, &ToolsConfigHooks.NousFeatures, nil)
	defer func() {
		if r := recover(); r != "tools.ToolsConfigHooks.NousFeatures is not installed" {
			t.Fatalf("recovered %v", r)
		}
	}()
	s, _ := newTestToolsSetup(t, "")
	s.nousFeatures(config.Tree{}, false)
}
