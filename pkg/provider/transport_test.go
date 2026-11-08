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

func TestChatTransportReversesOnlyRecordedWireAliases(t *testing.T) {
	tr := GetTransport(string(ModeChatCompletions))
	tools, aliases := RenameToolSearchBridgeForXAI([]map[string]any{{"type": "function", "function": map[string]any{"name": "tool_search"}}})
	if tools[0]["function"].(map[string]any)["name"] != "icebath_tool_search" || aliases["icebath_tool_search"] != "tool_search" {
		t.Fatal("rename")
	}
	tr.(WireAliasTransport).SetLastWireAliases(map[string]string{})
	payload := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{
		map[string]any{"id": "c", "type": "function", "function": map[string]any{"name": "icebath_tool_search", "arguments": "{}"}}}}}}}
	resp, err := tr.NormalizeResponse(payload, nil)
	if err != nil || resp.Message.ToolCalls[0].Function.Name != "icebath_tool_search" {
		t.Fatal("empty alias map must keep the wire name")
	}
}
