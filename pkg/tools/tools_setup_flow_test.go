package tools

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/setupflow"
)

// Asker answering from a script, one function per question, recording what it said and asked
type flowScript struct {
	t       *testing.T
	said    []string
	asked   []string
	answers []func(question string, choices []string) (any, error)
}

func (a *flowScript) next(question string, choices []string) (any, error) {
	a.asked = append(a.asked, question)
	if len(a.answers) == 0 {
		a.t.Fatalf("unexpected question %q %v", question, choices)
	}
	answer := a.answers[0]
	a.answers = a.answers[1:]
	return answer(question, choices)
}

func (a *flowScript) Say(line string) { a.said = append(a.said, line) }

func (a *flowScript) Choose(question string, options []setupflow.Option, def int) (int, error) {
	titles := make([]string, len(options))
	for i, o := range options {
		titles[i] = o.Title
	}
	v, err := a.next(question, titles)
	if err != nil {
		return def, err
	}
	if label, isLabel := v.(string); isLabel {
		for i, title := range titles {
			if strings.Contains(title, label) {
				return i, nil
			}
		}
		a.t.Fatalf("no option %q in %v", label, titles)
	}
	return v.(int), nil
}

func (a *flowScript) Check(question string, items []setupflow.Item, selected map[int]bool, _ map[string]int) (map[int]bool, error) {
	_, err := a.next(question, nil)
	return selected, err
}

func (a *flowScript) Text(question, def string, _ bool) (string, error) {
	v, err := a.next(question, nil)
	if err != nil {
		return "", err
	}
	if text := v.(string); text != "" {
		return text, nil
	}
	return def, nil
}

// Answer of a fixed value
func flowPick(v any) func(string, []string) (any, error) {
	return func(string, []string) (any, error) { return v, nil }
}

// Isolated home with a minimal config for one flow run
func toolsFlowHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model:\n  default: m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "SLACK_BOT_TOKEN", "WHATSAPP_ENABLED", "QQ_APP_ID", "XAI_API_KEY"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	return h
}

// Hooks that fail the case when a step it did not expect reaches them
func strictFlowHooks(t *testing.T) ToolsFlowHooks {
	return ToolsFlowHooks{
		ConfigureMCP: func(context.Context, config.Tree, setupflow.Asker) error { t.Fatal("mcp asked"); return nil },
		XAILogin:     func(context.Context, string, io.Writer) bool { t.Fatal("xai login asked"); return false },
	}
}

func TestToolsFlowMenuOffersGlobalAndMCPRowsAndSavesOnDone(t *testing.T) {
	h := toolsFlowHome(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "123:abc")
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model:\n  default: m\nmcp_servers:\n  files:\n    command: files-mcp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hooks := strictFlowHooks(t)
	mcpRan := 0
	hooks.ConfigureMCP = func(_ context.Context, cfg config.Tree, _ setupflow.Asker) error {
		mcpRan++
		cfg["mcp_servers"].(map[string]any)["files"].(map[string]any)["enabled"] = false
		return nil
	}
	var menus [][]string
	menu := func(label string) func(string, []string) (any, error) {
		return func(_ string, choices []string) (any, error) { menus = append(menus, choices); return label, nil }
	}
	ask := &flowScript{t: t, answers: []func(string, []string) (any, error){menu("MCP"), menu("Done")}}
	if err := RunToolsFlow(context.Background(), &Env{Home: h}, ask, hooks); err != nil {
		t.Fatal(err)
	}
	first := strings.Join(menus[0], "|")
	if !strings.Contains(first, "Configure Telegram") || !strings.Contains(first, "Configure all platforms (global)") || !strings.Contains(first, "Configure MCP server tools") {
		t.Fatalf("menu %v", menus[0])
	}
	if mcpRan != 1 {
		t.Fatalf("mcp %d", mcpRan)
	}
	saved, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	if !strings.Contains(string(saved), "enabled: false") || !strings.Contains(strings.Join(ask.said, "\n"), "Tool configuration saved to") {
		t.Fatalf("config\n%s\nsaid %v", saved, ask.said)
	}
}

func TestToolsFlowXAICredentialStepAsksThroughTheFlow(t *testing.T) {
	h := toolsFlowHome(t)
	hooks := strictFlowHooks(t)
	logins := 0
	hooks.XAILogin = func(context.Context, string, io.Writer) bool { logins++; return true }
	prevStatus := ToolsConfigHooks.XAIOAuthStatus
	ToolsConfigHooks.XAIOAuthStatus = nil
	t.Cleanup(func() { ToolsConfigHooks.XAIOAuthStatus = prevStatus })
	run := func(answers ...func(string, []string) (any, error)) *flowScript {
		ask := &flowScript{t: t, answers: answers}
		f := &ToolsSetup{Ctx: context.Background(), Env: &Env{Home: h}, Con: flowConsole(ask), Ask: ask, Flow: hooks}
		if err := f.runPostSetup("xai_grok"); err != nil {
			t.Fatal(err)
		}
		return ask
	}
	run(flowPick("Paste an xAI API key"), flowPick("xai-key-9"))
	if v, _, _ := config.GetEnvValue(context.Background(), h, "XAI_API_KEY"); v != "xai-key-9" {
		t.Fatalf("XAI_API_KEY %q", v)
	}
	ask := run()
	if !strings.Contains(strings.Join(ask.said, "\n"), "existing XAI_API_KEY") {
		t.Fatalf("held key asked again %v", ask.said)
	}
	if _, err := config.RemoveHomeEnvValue(context.Background(), h, "XAI_API_KEY"); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("XAI_API_KEY")
	run(flowPick("Sign in with xAI Grok OAuth"))
	if logins != 1 {
		t.Fatalf("logins %d", logins)
	}
	ask = run(flowPick("Skip"))
	if !strings.Contains(strings.Join(ask.said, "\n"), "remain inactive") {
		t.Fatalf("said %v", ask.said)
	}
}

func TestToolsFlowCodexStepSignsInOrSkips(t *testing.T) {
	h := toolsFlowHome(t)
	prevStatus, prevLogin, prevSave := ToolsConfigHooks.CodexAuthStatus, ToolsConfigHooks.CodexDeviceCodeLogin, ToolsConfigHooks.SaveCodexTokens
	t.Cleanup(func() {
		ToolsConfigHooks.CodexAuthStatus, ToolsConfigHooks.CodexDeviceCodeLogin, ToolsConfigHooks.SaveCodexTokens = prevStatus, prevLogin, prevSave
	})
	ToolsConfigHooks.CodexAuthStatus = func(context.Context, string) map[string]any { return map[string]any{"logged_in": false} }
	saved := 0
	ToolsConfigHooks.CodexDeviceCodeLogin = func(_ context.Context, _ string, out io.Writer) (map[string]any, error) {
		io.WriteString(out, "open the device page\n")
		return map[string]any{"tokens": map[string]any{"access_token": "a"}}, nil
	}
	ToolsConfigHooks.SaveCodexTokens = func(context.Context, string, map[string]any, string, bool) error { saved++; return nil }
	for _, answer := range []string{"Sign in", "Skip"} {
		ask := &flowScript{t: t, answers: []func(string, []string) (any, error){flowPick(answer)}}
		f := &ToolsSetup{Ctx: context.Background(), Env: &Env{Home: h}, Con: flowConsole(ask), Ask: ask, Flow: strictFlowHooks(t)}
		if err := f.runPostSetup("openai_codex"); err != nil {
			t.Fatal(err)
		}
		said := strings.Join(ask.said, "\n")
		if answer == "Sign in" && (saved != 1 || !strings.Contains(said, "open the device page") || !strings.Contains(said, "Logged in")) ||
			answer == "Skip" && !strings.Contains(said, "remain inactive until you sign in") {
			t.Fatalf("%s said %v saved %d", answer, ask.said, saved)
		}
	}
}

func TestToolsFlowStopsWhenAPromptIsLeft(t *testing.T) {
	h := toolsFlowHome(t)
	ask := &flowScript{t: t, answers: []func(string, []string) (any, error){
		func(string, []string) (any, error) { return nil, setupflow.ErrCancelled },
	}}
	if err := RunToolsFlow(context.Background(), &Env{Home: h}, ask, strictFlowHooks(t)); err != setupflow.ErrCancelled {
		t.Fatalf("err %v", err)
	}
}
