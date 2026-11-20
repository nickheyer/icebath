package provider

import (
	"testing"
)

// Switch with OpenCode fakes, the runtime base URL defaulting to the session one
func runOpencodeSwitch(t *testing.T, raw, currentProvider, currentBaseURL, explicitProvider, runtimeBaseURL string) ModelSwitchResult {
	t.Helper()
	ctx := switchValidateHome(t, "")
	if runtimeBaseURL == "" {
		runtimeBaseURL = currentBaseURL
	}
	fakeSwitchPipeline(t, true)
	fakeSwitchRouting(t, []string{}, [2]string{})
	fakeSwitchRuntime(t, &Runtime{APIKey: "sk-opencode-fake", BaseURL: runtimeBaseURL, APIMode: ModeChatCompletions}, nil)
	return mustSwitch(t, ctx, SwitchModelOptions{RawInput: raw, CurrentProvider: currentProvider, CurrentBaseURL: currentBaseURL,
		CurrentAPIKey: "sk-opencode-fake", ExplicitProvider: explicitProvider})
}

func TestSwitchToMinimaxM27StripsV1(t *testing.T) {
	res := runOpencodeSwitch(t, "minimax-m2.7", "opencode-go", "https://opencode.ai/zen/go/v1", "", "")
	if !res.Success {
		t.Fatalf("switch_model failed: %s", res.ErrorMessage)
	}
	if res.APIMode != "anthropic_messages" || res.BaseURL != "https://opencode.ai/zen/go" {
		t.Fatalf("Expected /v1 stripped for anthropic_messages; got %+v", res)
	}
}

func TestSwitchToGptUsesCodexResponsesKeepsV1(t *testing.T) {
	res := runOpencodeSwitch(t, "gpt-5.4", "opencode-zen", "https://opencode.ai/zen", "", "https://opencode.ai/zen/v1")
	if !res.Success || res.APIMode != "codex_responses" || res.BaseURL != "https://opencode.ai/zen/v1" {
		t.Fatalf("result %+v", res)
	}
}

func TestKimiSwitchKeepsV1DespiteClaudeConfigDefault(t *testing.T) {
	ctx := switchValidateHome(t, "model:\n  provider: opencode-zen\n  default: claude-sonnet-4-6\n")
	t.Setenv("OPENCODE_ZEN_API_KEY", "test-key")
	fakeSwitchPipeline(t, true)
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "kimi-k2.6", CurrentProvider: "opencode-zen", CurrentBaseURL: "https://opencode.ai/zen",
		CurrentAPIKey: "test-key", ExplicitProvider: "opencode-zen"})
	if !res.Success {
		t.Fatalf("switch failed: %s", res.ErrorMessage)
	}
	if res.BaseURL != "https://opencode.ai/zen/v1" {
		t.Fatalf("base_url wedged at %q - stale Claude config.default caused api_mode to be computed as anthropic_messages, stripping /v1, and chat_completions override never re-added it.", res.BaseURL)
	}
	if res.APIMode != "chat_completions" {
		t.Fatalf("api_mode %q", res.APIMode)
	}
}
