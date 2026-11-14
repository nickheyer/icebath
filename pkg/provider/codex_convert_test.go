package provider

import (
	"encoding/json"
	"github.com/nickheyer/icebath/pkg/state"
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

func TestExportedResponsesRouteAndCompactionGate(t *testing.T) {
	r := ClassifyResponsesRoute("openai-codex", "https://chatgpt.com/backend-api/codex")
	if !r.IsCodexBackend() || r.IsXAI() || ClassifyResponsesRoute("", "https://api.x.ai/v1").IsXAI() != true {
		t.Fatal("route flags")
	}
	g := NativeCompactionGate{RuntimeCapability: true, Configured: true, CompressionEnabled: true, Model: "gpt-5.6", Provider: "openai", BaseURL: "https://api.openai.com/v1"}
	if got := NativeCompactionContextManagement(g, ClassifyResponsesRoute("openai", g.BaseURL)); len(got) != 1 || got[0]["type"] != "compaction" {
		t.Fatalf("payload %v", got)
	}
	g.Configured = false
	if NativeCompactionContextManagement(g, ClassifyResponsesRoute("openai", g.BaseURL)) != nil {
		t.Fatal("unconfigured route sent compaction")
	}
	if c, i := SplitResponsesToolID("call_a|fc_b"); c != "call_a" || i != "fc_b" {
		t.Fatal("split")
	}
	if DeriveResponsesFunctionCallID("call_x", "") != "fc_x" {
		t.Fatal("derive")
	}
	if OpencodeFamilyForCustom("opencode-go", "") != opencodeFamilyForCustom("opencode-go", "") {
		t.Fatal("opencode family")
	}
}

func TestNativeResponsesReplayInputKeepsTheCompactionCheckpoint(t *testing.T) {
	msgs := []*state.Message{
		state.MessageFromDict(map[string]any{"role": "user", "content": "hi"}),
		state.MessageFromDict(map[string]any{"role": "assistant", "content": "ok", "codex_reasoning_items": []any{
			map[string]any{"type": "compaction", "id": "cmp_1", "encrypted_content": "enc"}}}),
	}
	route := ClassifyResponsesRoute("openai-codex", "https://chatgpt.com/backend-api/codex")
	items, err := NativeResponsesReplayInput(msgs, route, "https://chatgpt.com/backend-api/codex", "gpt-5.5", true)
	if err != nil {
		t.Fatal(err)
	}
	want, err := chatMessagesToResponsesInput(codexMsgsOf(msgs), codexConvertOptions{replayEncryptedReasoning: true,
		currentIssuerKind:  classifyResponsesIssuer(false, true, "https://chatgpt.com/backend-api/codex", &route.flags.endpoint),
		currentIssuerModel: wireModelIdentity("gpt-5.5"), nativeCompactionEligible: true, dropReplayItemIDs: !route.flags.endpoint.replayItemIDs})
	if err != nil || !reflect.DeepEqual(items, want) {
		t.Fatalf("items %v want %v err %v", items, want, err)
	}
	if !HasCompactionCheckpoint(items) {
		t.Fatalf("checkpoint dropped %v", items)
	}
}
