package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

var effortSmallPrompt = map[string]any{"model": "gpt-5.5", "input": []any{map[string]any{"role": "user", "content": strings.Repeat("x", 6000)}}}

// Hosted openai-codex agent in a fresh home with the watchdog env cleared and the given reasoning effort
func effortCodexAgent(t *testing.T, effort string, enabled bool) *AIAgent {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	for name, body := range map[string]string{".env": "", "config.yaml": "{}\n"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{provider.APICallStaleTimeoutEnv, provider.CodexTTFBTimeoutEnv, provider.CodexEventStaleTimeoutEnv, provider.CodexTTFBMaxEnv} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	a := &AIAgent{model: "gpt-5.5", provider: "openai-codex", apiKey: "sk-dummy", platform: "cli", apiMode: "codex_responses"}
	a.setBaseURL("https://chatgpt.com/backend-api/codex")
	a.reasoningConfig = map[string]any{"enabled": enabled, "effort": effort}
	return a
}

func resolveEffortWatchdogs(t *testing.T, a *AIAgent) provider.NonStreamWatchdogs {
	t.Helper()
	wd, err := provider.ResolveNonstreamWatchdogs(context.Background(), a, effortSmallPrompt)
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func TestHighEffortSmallPromptGetsTheSilenceFloorOnAllThreeFuses(t *testing.T) {
	for _, effort := range []string{"high", "xhigh"} {
		t.Run(effort, func(t *testing.T) {
			wd := resolveEffortWatchdogs(t, effortCodexAgent(t, effort, true))
			floor := provider.HighEffortSilenceFloorSeconds
			if !wd.IdleEnabled || !wd.TTFBEnabled {
				t.Fatalf("fuses disabled %+v", wd)
			}
			if wd.IdleTimeout < floor || wd.TTFBTimeout < floor || wd.StaleTimeout < floor {
				t.Fatalf("fuses under the floor %+v", wd)
			}
		})
	}
}

func TestDefaultEffortTiersAndExplicitOperatorValuesAreUntouched(t *testing.T) {
	floor := provider.HighEffortSilenceFloorSeconds
	medium := resolveEffortWatchdogs(t, effortCodexAgent(t, "medium", true))
	mediumFuses := [3]float64{medium.IdleTimeout, medium.TTFBTimeout, medium.StaleTimeout}
	for _, f := range mediumFuses {
		if f >= floor {
			t.Fatalf("medium fuse %v reached the floor", f)
		}
	}
	disabled := resolveEffortWatchdogs(t, effortCodexAgent(t, "high", false))
	if got := [3]float64{disabled.IdleTimeout, disabled.TTFBTimeout, disabled.StaleTimeout}; got != mediumFuses {
		t.Fatalf("disabled %v want %v", got, mediumFuses)
	}
	a := effortCodexAgent(t, "high", true)
	t.Setenv(provider.CodexEventStaleTimeoutEnv, "20")
	t.Setenv(provider.CodexTTFBTimeoutEnv, "45")
	t.Setenv(provider.APICallStaleTimeoutEnv, "75")
	explicit := resolveEffortWatchdogs(t, a)
	if got := [3]float64{explicit.IdleTimeout, explicit.TTFBTimeout, explicit.StaleTimeout}; got != [3]float64{20, 45, 75} {
		t.Fatalf("explicit %v", got)
	}
}

func TestEffortFloorNeverOutlivesTheRunBudgetCap(t *testing.T) {
	a := effortCodexAgent(t, "high", true)
	a.runBudgetSeconds, a.runBudgetStartedAt = 100, nowSeconds()-5
	if got := resolveEffortWatchdogs(t, a).StaleTimeout; got != 60.0 {
		t.Fatalf("stale %v want 60", got)
	}
}
