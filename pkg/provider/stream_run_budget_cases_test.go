package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type budgetChatAgent struct {
	*fakeChatAgent
	budget, started float64
}

func (a *budgetChatAgent) RunBudget() (float64, float64) { return a.budget, a.started }

func streamBudgetHome(t *testing.T, configBody string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	for _, name := range []string{"ICEBATH_API_CALL_STALE_TIMEOUT", StreamStaleTimeoutEnv, LocalStreamStaleTimeoutEnv} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	if configBody == "" {
		configBody = "{}\n"
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
}

func resolvedStreamStale(agent ChatCompletionAgent, kwargs map[string]any) float64 {
	c := newStreamingCall(context.Background(), agent, kwargs, nil)
	c.resolveStaleTimeout()
	return c.staleTimeout
}

func TestRunBudgetStreamStaleTimeout(t *testing.T) {
	now := func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	t.Run("test_cloud_stream_budget_preserves_explicit_deadlines", func(t *testing.T) {
		for _, tc := range []struct {
			configBody, envValue string
			want                 float64
		}{{"", "", 60.0}, {"providers:\n  openai:\n    stale_timeout_seconds: 1200\n", "", 1200.0}, {"", "900", 900.0}} {
			streamBudgetHome(t, tc.configBody)
			if tc.envValue != "" {
				t.Setenv(StreamStaleTimeoutEnv, tc.envValue)
			}
			agent := &budgetChatAgent{fakeChatAgent: &fakeChatAgent{provider: "openai", apiMode: "chat_completions", baseURL: "https://api.openai.com/v1",
				model: "deepseek/deepseek-v4-pro"}, budget: 900, started: now() - 800}
			payload := map[string]any{"model": agent.model, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			if got := resolvedStreamStale(agent, payload); got != tc.want {
				t.Fatalf("config %q env %q: stale timeout %v want %v", tc.configBody, tc.envValue, got, tc.want)
			}
		}
	})
	t.Run("test_local_stream_patience_is_independent_of_run_budget", func(t *testing.T) {
		streamBudgetHome(t, "")
		agent := &budgetChatAgent{fakeChatAgent: &fakeChatAgent{provider: "openai", apiMode: "chat_completions", baseURL: "http://127.0.0.1:11434/v1",
			model: "gpt-5.5"}, budget: 900}
		withoutClock := resolvedStreamStale(agent, map[string]any{"model": agent.model})
		agent.started = now() - 800
		if got := resolvedStreamStale(agent, map[string]any{"model": agent.model}); got != withoutClock || got != 900.0 {
			t.Fatalf("local patience %v without clock %v", got, withoutClock)
		}
	})
}
