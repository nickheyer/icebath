package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Hermes screen text with the icebath renames of the setup wizard applied
var setupRenames = strings.NewReplacer("~/.hermes/", "~/.icebath/", "`hermes ", "`icebath ", "'hermes ", "'icebath ")

// Swaps the setup menu for one answering through fn, returning the questions it was asked
func recordSetupMenus(t *testing.T, fn func(question string, choices []string, def int, description string) int) *[]string {
	t.Helper()
	asked := &[]string{}
	t.Cleanup(SwapSetupRadiolist(func(question string, choices []string, def int, description string) int {
		*asked = append(*asked, question)
		return fn(question, choices, def, description)
	}))
	return asked
}

// Recorded hermes output of one setup section scenario
type setupSectionRef struct {
	Out string          `json:"out"`
	Err *string         `json:"err"`
	Cfg json.RawMessage `json:"cfg"`
}

func loadSetupSectionRefs(t *testing.T) map[string]setupSectionRef {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "setup_sections.json"))
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]setupSectionRef{}
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatal(err)
	}
	return refs
}

// Installs a temp home and a setup console reading the answers, returning the captured output
func scriptedSetupConsole(t *testing.T, answers ...string) (context.Context, *bytes.Buffer) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_NONINTERACTIVE", "")
	for _, key := range []string{"ICEBATH_MAX_ITERATIONS", "OPENROUTER_API_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL", "ICEBATH_INFERENCE_PROVIDER", "NOUS_API_KEY", "LLM_MODEL"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	var in strings.Builder
	for _, a := range answers {
		in.WriteString(a + "\n")
	}
	var out bytes.Buffer
	t.Cleanup(WithSetupConsole(&cliui.Console{In: strings.NewReader(in.String()), Out: &out}))
	return home.WithOverride(context.Background(), h), &out
}

// JSON form of a config for comparison with a recorded Python dict
func normalizedJSON(t *testing.T, v any) string {
	t.Helper()
	var decoded any
	switch raw := v.(type) {
	case json.RawMessage:
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
	default:
		data, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := json.Marshal(decoded)
	return string(data)
}

func TestSetupAgentSettingsScreensMatchHermes(t *testing.T) {
	refs := loadSetupSectionRefs(t)
	cases := []struct {
		name    string
		cfg     config.Tree
		answers []string
	}{
		{"agent_test", config.Tree{"agent": map[string]any{"max_turns": 500}, "display": map[string]any{"tool_progress": "all"}, "compression": map[string]any{"threshold": 0.50}}, []string{"500", "all", "0.5"}},
		{"agent_fresh_enter", config.Tree{}, []string{"", "", ""}},
		{"agent_invalid", config.Tree{"display": map[string]any{"tool_progress": "new"}}, []string{"abc", "bogus", "2"}},
		{"agent_zero_verbose", config.Tree{"agent": map[string]any{"max_turns": 60}}, []string{"0", "VERBOSE", "0.95"}},
		{"agent_bool_mode", config.Tree{"display": map[string]any{"tool_progress": true}}, []string{"12", ""}},
		{"agent_big_int", config.Tree{}, []string{"1_000", "log", "nan"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref := refs[c.name]
			ctx, out := scriptedSetupConsole(t, c.answers...)
			err := SetupAgentSettings(ctx, c.cfg)
			if got, want := out.String(), setupRenames.Replace(ref.Out); got != want {
				t.Fatalf("screen\n%q\nwant\n%q", got, want)
			}
			if ref.Err == nil && err != nil || ref.Err != nil && (err == nil || err.Error() != *ref.Err) {
				t.Fatalf("error %v want %v", err, ref.Err)
			}
			if got, want := normalizedJSON(t, c.cfg), normalizedJSON(t, ref.Cfg); got != want {
				t.Fatalf("config %s want %s", got, want)
			}
		})
	}
}

func TestSetupAgentSettingsPrefersConfigOverStaleEnv(t *testing.T) {
	ctx, out := scriptedSetupConsole(t, "500", "all", "0.5")
	h := home.Dir(ctx)
	if err := os.WriteFile(home.EnvPath(h), []byte("ICEBATH_MAX_ITERATIONS=60\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Tree{
		"agent":       map[string]any{"max_turns": 500},
		"display":     map[string]any{"tool_progress": "all"},
		"compression": map[string]any{"threshold": 0.50},
	}
	menus := recordSetupMenus(t, func(string, []string, int, string) int { return 4 })
	if err := SetupAgentSettings(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Press Enter to keep 500.") || strings.Contains(out.String(), "Press Enter to keep 60.") {
		t.Fatalf("screen %q", out.String())
	}
	if env := config.LoadEnv(h); env["ICEBATH_MAX_ITERATIONS"] != "" {
		t.Fatalf("stale env entry kept: %v", env)
	}
	if len(*menus) != 0 {
		t.Fatalf("menus %v", *menus)
	}
}

func TestApplyDefaultAgentSettingsScreenMatchesHermes(t *testing.T) {
	ref := loadSetupSectionRefs(t)["apply_defaults"]
	ctx, out := scriptedSetupConsole(t)
	cfg := config.Tree{}
	if err := ApplyDefaultAgentSettings(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), setupRenames.Replace(ref.Out); got != want {
		t.Fatalf("screen\n%q\nwant\n%q", got, want)
	}
	if got := normalizedJSON(t, cfg); got != `{"agent":{"max_turns":150},"compression":{"enabled":true,"threshold":0.5}}` {
		t.Fatalf("config %s", got)
	}
	saved, err := config.LoadContext(ctx, home.Dir(ctx), nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := config.CfgGet(saved, "agent", "max_turns"); normalizedJSON(t, v) != "150" {
		t.Fatalf("saved max_turns %v", v)
	}
	bad := config.Tree{"agent": []any{"x"}}
	if err := ApplyDefaultAgentSettings(ctx, bad); err == nil || err.Error() != "TypeError: 'list' object does not support item assignment" {
		t.Fatalf("non mapping agent section %v", err)
	}
}

func TestPromptIntSettingStoresOnlyAcceptedInts(t *testing.T) {
	_, out := scriptedSetupConsole(t, "7", "-3", "x")
	section := map[string]any{}
	positive := func(v *big.Int) bool { return v.Sign() > 0 }
	promptIntSetting(section, "n", "Count", 5, positive)
	if section["n"] != 7 {
		t.Fatalf("accepted %v", section)
	}
	promptIntSetting(section, "n", "Count", 7, positive)
	promptIntSetting(section, "n", "Count", 7, positive)
	if section["n"] != 7 || out.String() != "Count [5]: Count [7]: Count [7]: " {
		t.Fatalf("section %v screen %q", section, out.String())
	}
}

// Swaps the provider picker the model section runs
func swapSetupPicker(t *testing.T, fn func(ctx context.Context, ns map[string]any) error) {
	t.Helper()
	prev := setupSelectProviderAndModel
	setupSelectProviderAndModel = fn
	t.Cleanup(func() { setupSelectProviderAndModel = prev })
}

// Writes what a model flow writes, the provider, base url and default model of the home config
func writeModelConfig(t *testing.T, ctx context.Context, providerName, baseURL, modelName string) {
	t.Helper()
	h := home.Dir(ctx)
	cfg, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := cfg["model"].(map[string]any)
	if !ok {
		m = map[string]any{}
		if cfg["model"] != nil && cfg["model"] != "" {
			m["default"] = cfg["model"]
		}
		cfg["model"] = m
	}
	m["provider"] = providerName
	if baseURL != "" {
		m["base_url"] = baseURL
	}
	if modelName != "" {
		m["default"] = modelName
	}
	if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestSetupDelegatesToSelectProviderAndModel(t *testing.T) {
	ctx, _ := scriptedSetupConsole(t)
	h := home.Dir(ctx)
	cfg, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	swapSetupPicker(t, func(ctx context.Context, ns map[string]any) error {
		calls++
		if ns != nil {
			t.Fatalf("picker namespace %v", ns)
		}
		writeModelConfig(t, ctx, "custom", "http://localhost:11434/v1", "qwen3.5:32b")
		return nil
	})
	if err := SetupModelProvider(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	model, ok := reloaded["model"].(map[string]any)
	if !ok || calls != 1 || model["provider"] != "custom" || model["base_url"] != "http://localhost:11434/v1" || model["default"] != "qwen3.5:32b" {
		t.Fatalf("calls %d model %v", calls, reloaded["model"])
	}
}

func TestSetupModelProviderPreservesAuxiliaryChoicesWrittenByPicker(t *testing.T) {
	ctx, _ := scriptedSetupConsole(t)
	h := home.Dir(ctx)
	cfg, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := config.CfgGet(cfg, "auxiliary", "compression", "provider"); v != "auto" {
		t.Fatalf("default aux provider %v", v)
	}
	swapSetupPicker(t, func(ctx context.Context, _ map[string]any) error {
		fresh, err := config.LoadContext(ctx, h, nil)
		if err != nil {
			return err
		}
		aux, _ := SetdefaultDict(fresh, "auxiliary")
		entry, _ := SetdefaultDict(aux, "compression")
		entry["provider"], entry["model"] = "gemini", "gemini-2.5-flash"
		return config.SaveConfig(ctx, h, fresh, config.SaveOptions{})
	})
	if err := SetupModelProvider(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := config.CfgGet(reloaded, "auxiliary", "compression", "provider")
	m, _ := config.CfgGet(reloaded, "auxiliary", "compression", "model")
	if p != "gemini" || m != "gemini-2.5-flash" {
		t.Fatalf("aux compression %v %v", p, m)
	}
}

func TestSetupModelProviderScreensMatchHermes(t *testing.T) {
	refs := loadSetupSectionRefs(t)
	for _, c := range []struct {
		name   string
		picker func(ctx context.Context, ns map[string]any) error
	}{
		{"model_ok", func(context.Context, map[string]any) error { return nil }},
		{"model_exit", func(context.Context, map[string]any) error { panic(SetupExit{Code: 1}) }},
		{"model_interrupt", func(context.Context, map[string]any) error { return cliui.ErrInterrupt }},
		{"model_error", func(context.Context, map[string]any) error { return errors.New("boom") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, out := scriptedSetupConsole(t)
			cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil)
			if err != nil {
				t.Fatal(err)
			}
			swapSetupPicker(t, c.picker)
			if err := SetupModelProvider(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			if got, want := out.String(), setupRenames.Replace(refs[c.name].Out); got != want {
				t.Fatalf("screen\n%q\nwant\n%q", got, want)
			}
		})
	}
	t.Run("login exits read as skipped", func(t *testing.T) {
		for _, exit := range []error{&NousLoginExitError{Code: 130}, &AuthExitError{Code: 1, Message: "x"}, &OAuthLoginExitError{Code: 1}, &MoAExitError{Message: "m"}, SetupExit{Code: 2}} {
			ctx, out := scriptedSetupConsole(t)
			swapSetupPicker(t, func(context.Context, map[string]any) error { return exit })
			if err := SetupModelProvider(ctx, config.Tree{}); err != nil || !strings.HasSuffix(out.String(), "\n\n  Provider setup skipped.\n") {
				t.Fatalf("%T err %v screen %q", exit, err, out.String())
			}
		}
	})
	t.Run("navigation keeps unwinding", func(t *testing.T) {
		ctx, _ := scriptedSetupConsole(t)
		swapSetupPicker(t, func(context.Context, map[string]any) error { panic(setupGoBack{promptIndex: 0}) })
		defer func() {
			if r := recover(); r != (setupGoBack{promptIndex: 0}) {
				t.Fatalf("recovered %v", r)
			}
		}()
		_ = SetupModelProvider(ctx, config.Tree{})
		t.Fatal("back was swallowed")
	})
}

func TestPromptChoiceEscapeKeepsDefaultWithoutNumberedFallback(t *testing.T) {
	recordSetupMenus(t, func(string, []string, int, string) int { return -1 })
	var out bytes.Buffer
	t.Cleanup(WithSetupConsole(&cliui.Console{In: failingReader{t}, Out: &out}))
	if got := PromptChoice("Pick one", []string{"a", "b"}, 1, ""); got != 1 || out.Len() != 0 {
		t.Fatalf("choice %d screen %q", got, out.String())
	}
}

// Reader failing the test when a prompt reads it
type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Error("Escape must not enter the numbered fallback")
	return 0, io.EOF
}

// Installs a navigation state as the running setup scope for the test
func installSetupNavigation(t *testing.T, state *setupNavigationState) {
	t.Helper()
	setupNavMu.Lock()
	prev := setupNavigation
	setupNavigation = state
	setupNavMu.Unlock()
	t.Cleanup(func() {
		setupNavMu.Lock()
		setupNavigation = prev
		setupNavMu.Unlock()
	})
}

// Recovered panic of fn, nil when it returned
func recovered(fn func()) (r any) {
	defer func() { r = recover() }()
	fn()
	return nil
}

func TestSetupNavigationEscapeCancelsAndLeftGoesBack(t *testing.T) {
	state := &setupNavigationState{}
	state.reset(1, nil)
	installSetupNavigation(t, state)
	start, _ := handleSetupMenuNavigation(cliui.NavEventBegin)
	if !start.AllowBack {
		t.Fatal("begin of a later section must allow back")
	}
	if r := recovered(func() { handleSetupMenuNavigation(cliui.NavEventCancel) }); r != (setupCancelled{}) {
		t.Fatalf("cancel raised %v", r)
	}
	start, _ = handleSetupMenuNavigation(cliui.NavEventBegin)
	if !start.AllowBack {
		t.Fatal("second begin must allow back")
	}
	if r := recovered(func() { handleSetupMenuNavigation(cliui.NavEventBack) }); r != (setupGoBack{promptIndex: 1}) {
		t.Fatalf("back raised %v", r)
	}
}

func TestSetupStepsMoveToPreviousSectionOrRestartCurrentSection(t *testing.T) {
	var calls []string
	terminalAttempts, gatewayAttempts := 0, 0
	state := &setupNavigationState{}
	state.reset(-1, nil)
	installSetupNavigation(t, state)
	captureSetupOutput(t)
	err := runSetupSteps([]SetupStep{
		{"Model", func() error { calls = append(calls, "model"); return nil }},
		{"Terminal", func() error {
			calls = append(calls, "terminal")
			terminalAttempts++
			if terminalAttempts == 1 {
				panic(setupGoBack{promptIndex: 0})
			}
			return nil
		}},
		{"Gateway", func() error {
			calls = append(calls, "gateway")
			gatewayAttempts++
			if gatewayAttempts == 1 {
				panic(setupGoBack{promptIndex: 1})
			}
			return nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"model", "terminal", "model", "terminal", "gateway", "gateway"}; !slices.Equal(calls, want) {
		t.Fatalf("calls %v", calls)
	}
}

func TestNestedBackReopensOnlyTheImmediatelyPreviousPrompt(t *testing.T) {
	var shown []string
	attempts := 0
	modelProviderFlow := func() error {
		attempts++
		for _, label := range []string{"provider", "auth method", "existing or reauthenticate"} {
			start, _ := handleSetupMenuNavigation(cliui.NavEventBegin)
			if start.Replay {
				handleSetupMenuNavigation(cliui.NavEventResolve, start.ReplayValue)
				continue
			}
			shown = append(shown, label)
			if label == "existing or reauthenticate" && attempts == 1 {
				handleSetupMenuNavigation(cliui.NavEventBack)
			}
			handleSetupMenuNavigation(cliui.NavEventResolve, label)
		}
		return nil
	}
	state := &setupNavigationState{}
	state.reset(-1, nil)
	installSetupNavigation(t, state)
	captureSetupOutput(t)
	if err := runSetupSteps([]SetupStep{{"Model & Provider", modelProviderFlow}}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"provider", "auth method", "existing or reauthenticate", "auth method"}; len(shown) < 4 || !slices.Equal(shown[:4], want) {
		t.Fatalf("shown %v", shown)
	}
}

func TestPromptStripsBracketedPasteMarkers(t *testing.T) {
	_, _ = scriptedSetupConsole(t, "\x1b[200~sk-ant-api-key\x1b[201~")
	if got := SetupPrompt("API key", "", false); got != "sk-ant-api-key" {
		t.Fatalf("value %q", got)
	}
}

func TestSetdefaultDictMirrorsDictSetdefault(t *testing.T) {
	cfg := map[string]any{"a": map[string]any{"x": 1}, "t": config.Tree{"y": 2}, "n": nil, "s": "text"}
	if m, err := SetdefaultDict(cfg, "a"); err != nil || m["x"] != 1 {
		t.Fatalf("existing %v %v", m, err)
	}
	if m, err := SetdefaultDict(cfg, "t"); err != nil || m["y"] != 2 {
		t.Fatalf("tree %v %v", m, err)
	}
	if m, err := SetdefaultDict(cfg, "new"); err != nil || m == nil || cfg["new"] == nil {
		t.Fatalf("missing %v %v", m, err)
	}
	for key, typ := range map[string]string{"n": "NoneType", "s": "str"} {
		if _, err := SetdefaultDict(cfg, key); err == nil || err.Error() != "TypeError: '"+typ+"' object does not support item assignment" {
			t.Fatalf("%s %v", key, err)
		}
	}
}

func TestSetupSystemExitReadsEveryExitShape(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
		msg  string
		ok   bool
	}{
		{SetupExit{Code: 3}, 3, "", true},
		{&AuthExitError{Code: 1, Message: "denied"}, 1, "denied", true},
		{&NousLoginExitError{Code: 130}, 130, "", true},
		{&OAuthLoginExitError{Code: 1}, 1, "", true},
		{&MoAExitError{Message: "nope"}, 1, "nope", true},
		{errors.New("other"), 0, "", false},
		{nil, 0, "", false},
	} {
		code, msg, ok := SetupSystemExit(c.err)
		if code != c.code || msg != c.msg || ok != c.ok {
			t.Fatalf("%v -> %d %q %v", c.err, code, msg, ok)
		}
	}
}

func TestSetupTerminalConsoleDrawsThroughTheSetupPrompts(t *testing.T) {
	_, out := scriptedSetupConsole(t, "typed", "y")
	recordSetupMenus(t, func(string, []string, int, string) int { return 2 })
	ui := setupTerminalUI{}
	if got := ui.Prompt("Image", "base", false); got != "typed" {
		t.Fatalf("prompt %q", got)
	}
	if !ui.PromptYesNo("Persist?", false) || ui.PromptChoice("Backend:", []string{"a", "b", "c"}, 0) != 2 {
		t.Fatal("answers")
	}
	ui.PrintHeader("Terminal Backend")
	ui.PrintInfo("info")
	ui.PrintSuccess("ok")
	ui.PrintWarning("warn")
	ui.Println()
	want := "Image [base]: Persist? [y/N]: \n\n◆ Terminal Backend\n  info\n✓ ok\n⚠ warn\n\n"
	if out.String() != want {
		t.Fatalf("screen %q", out.String())
	}
}
