package provider

import (
	"bytes"
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
)

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

// Installs a temp home and a setup console reading the answers, returning the captured output
func scriptedSetupConsole(t *testing.T, answers ...string) (context.Context, *bytes.Buffer) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_NONINTERACTIVE", "")
	for _, key := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL", "ICEBATH_INFERENCE_PROVIDER", "LLM_MODEL"} {
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
	state := newSetupNavigationState(1, 0)
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
	state := newSetupNavigationState(-1, 0)
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
	state := newSetupNavigationState(-1, 0)
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
