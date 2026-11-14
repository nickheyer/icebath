package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Minimal anthropic_messages agent of tests/agent/test_ctx_halving_fix.py TestEphemeralMaxOutputTokens
func ephemeralMaxOutputAgent(t *testing.T) *AIAgent {
	t.Helper()
	a := kwargsTestAgent(t, "anthropic", "https://api.anthropic.com", "claude-opus-4-6", "anthropic_messages")
	a.tools = []any{}
	a.requestOverrides = map[string]any{}
	return a
}

func TestEphemeralMaxOutputTokensEphemeralOverrideIsUsedOnFirstCall(t *testing.T) {
	a := ephemeralMaxOutputAgent(t)
	a.ephemeralMaxOutputTokens = 5000
	kwargs, err := a.buildAPIKwargs([]map[string]any{{"role": "user", "content": "hi"}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if kwargs["max_tokens"] != 5000 {
		t.Fatalf("max_tokens %v", kwargs["max_tokens"])
	}
}

func TestEphemeralMaxOutputTokensEphemeralOverrideIsConsumedAfterOneCall(t *testing.T) {
	a := ephemeralMaxOutputAgent(t)
	a.ephemeralMaxOutputTokens = 5000
	if _, err := a.buildAPIKwargs([]map[string]any{{"role": "user", "content": "hi"}}, nil, false); err != nil {
		t.Fatal(err)
	}
	kwargs, err := a.buildAPIKwargs([]map[string]any{{"role": "user", "content": "hi"}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if kwargs["max_tokens"] == 5000 {
		t.Fatalf("override leaked into the second call: %v", kwargs["max_tokens"])
	}
}

func TestExplicitUserConfigOverridesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	for name, body := range map[string]string{".env": "", "config.yaml": "providers:\n  openai-codex:\n    stale_timeout_seconds: 1800\n"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(provider.APICallStaleTimeoutEnv, "")
	os.Unsetenv(provider.APICallStaleTimeoutEnv)
	a := &AIAgent{model: "gpt-5.5", provider: "openai-codex", apiKey: "sk-dummy", platform: "cli", apiMode: "codex_responses"}
	a.setBaseURL("https://chatgpt.com/backend-api/codex")
	got, err := a.ComputeNonStreamStaleTimeout(map[string]any{"input": "hi"})
	if err != nil || got != 1800.0 {
		t.Fatalf("stale %v %v", got, err)
	}
}
