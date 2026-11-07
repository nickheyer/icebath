package provider

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestDropsReplayItemIDsExportReadsTheProfileFlag(t *testing.T) {
	snapshotProviderRegistry(t)
	registerTestProfile(t, Profile{Name: "pinned-responses", BaseURL: "https://pinned.example/v1", Hostnames: []string{"pinned.example"}, ResponsesDropReplayItemIDs: true})
	if !DropsReplayItemIDs("https://pinned.example/v1") || DropsReplayItemIDs("https://generic.example/v1") {
		t.Fatal("drops replay item ids")
	}
}

func TestResponsesRouteIsCodexBackendExportClassifiesTheRoute(t *testing.T) {
	for _, c := range []struct {
		provider, base string
		want           bool
	}{{"openai-codex", "", true}, {"OpenAI-Codex", "", false}, {"", "https://chatgpt.com/backend-api/codex", true},
		{"", "https://evil.com/chatgpt.com/backend-api/codex", false}, {"custom", "https://sub.chatgpt.com/backend-api/codex/responses", true}} {
		if got := ResponsesRouteIsCodexBackend(c.provider, c.base); got != c.want {
			t.Fatalf("%q %q got %v", c.provider, c.base, got)
		}
	}
}

// Responses API object holding one completed function call whose arguments are given verbatim
func codexFunctionCallResponse(arguments any) map[string]any {
	return map[string]any{"status": "completed", "output": []any{map[string]any{
		"type": "function_call", "status": "completed", "name": "web_search", "arguments": arguments, "call_id": "call_abc123", "id": "fc_abc123",
	}}}
}

func TestFunctionCallDictArgumentsProduceValidJSON(t *testing.T) {
	args := map[string]any{"query": "weather in NYC", "units": "celsius"}
	msg, _, err := normalizeCodexResponse(codexFunctionCallResponse(args), "codex_backend", "", true)
	if err != nil || len(msg.ToolCalls) != 1 {
		t.Fatalf("msg %+v err %v", msg, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(msg.ToolCalls[0].Arguments), &parsed); err != nil || !reflect.DeepEqual(parsed, args) {
		t.Fatalf("arguments %q parsed %v err %v", msg.ToolCalls[0].Arguments, parsed, err)
	}
}

func TestStringArgumentsUnchanged(t *testing.T) {
	msg, _, err := normalizeCodexResponse(codexFunctionCallResponse(`{"query": "test"}`), "codex_backend", "", true)
	if err != nil || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Arguments != `{"query": "test"}` {
		t.Fatalf("msg %+v err %v", msg, err)
	}
}
