package provider

import "testing"

func TestPreflightKwargsRunsOnlyOnTheResponsesTransport(t *testing.T) {
	out, err := PreflightKwargs(GetTransport(string(ModeCodexResponses)), map[string]any{"model": "gpt-5.4", "instructions": "be brief",
		"input": []any{map[string]any{"role": "user", "content": "hi"}}, "store": false}, false, false, false)
	if err != nil || out["model"] != "gpt-5.4" {
		t.Fatal(out, err)
	}
	for mode, class := range map[APIMode]string{ModeChatCompletions: "ChatCompletionsTransport", ModeAnthropicMessages: "AnthropicTransport", modeBedrockConverse: "BedrockTransport"} {
		_, err := PreflightKwargs(GetTransport(string(mode)), map[string]any{}, false, false, false)
		if err == nil || err.Error() != "AttributeError: '"+class+"' object has no attribute 'preflight_kwargs'" {
			t.Fatalf("%s: %v", mode, err)
		}
	}
}
