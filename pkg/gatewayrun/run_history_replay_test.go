package gatewayrun

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func historyCtx(t *testing.T) context.Context {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return context.Background()
}

func TestUserMessageHasOnlyRoleAndContent(t *testing.T) {
	entry := buildReplayEntry("user", "hello", map[string]any{"role": "user", "content": "hello", "reasoning": "leak", "extra": "drop"}, false)
	if !reflect.DeepEqual(entry, map[string]any{"role": "user", "content": "hello"}) {
		t.Fatalf("entry %v", entry)
	}
}

func TestToolMessageHasOnlyRoleAndContent(t *testing.T) {
	entry := buildReplayEntry("tool", "result", map[string]any{"role": "tool", "content": "result", "reasoning": "leak"}, false)
	if !reflect.DeepEqual(entry, map[string]any{"role": "tool", "content": "result"}) {
		t.Fatalf("entry %v", entry)
	}
}

func TestAssistantDropsFalsyReasoning(t *testing.T) {
	msg := map[string]any{"role": "assistant", "content": "answer", "reasoning": "", "reasoning_details": []any{},
		"codex_reasoning_items": []any{}, "codex_message_items": []any{}, "finish_reason": ""}
	entry := buildReplayEntry("assistant", "answer", msg, false)
	if !reflect.DeepEqual(entry, map[string]any{"role": "assistant", "content": "answer"}) {
		t.Fatalf("entry %v", entry)
	}
}

func TestAssistantPreservesAllSixFieldsTogether(t *testing.T) {
	details := []any{map[string]any{"type": "reasoning.summary", "summary": "s"}}
	codexItems := []any{map[string]any{"type": "reasoning", "encrypted_content": "b"}}
	msgItems := []any{map[string]any{"type": "message", "role": "assistant", "phase": "final_answer",
		"content": []any{map[string]any{"type": "output_text", "text": "x"}}}}
	msg := map[string]any{"role": "assistant", "content": "answer", "reasoning": "thinking", "reasoning_content": "structured",
		"reasoning_details": details, "codex_reasoning_items": codexItems, "codex_message_items": msgItems, "finish_reason": "stop"}
	entry := buildReplayEntry("assistant", "answer", msg, false)
	want := map[string]any{"reasoning": "thinking", "reasoning_content": "structured", "reasoning_details": details,
		"codex_reasoning_items": codexItems, "codex_message_items": msgItems, "finish_reason": "stop"}
	for k, v := range want {
		if !reflect.DeepEqual(entry[k], v) {
			t.Errorf("%s = %v, want %v", k, entry[k], v)
		}
	}
}

func TestUnrelatedKeysAreIgnored(t *testing.T) {
	msg := map[string]any{"role": "assistant", "content": "answer", "timestamp": 12345.6, "internal_marker": "should not flow",
		"tool_call_id": "should not be set on simple-text branch"}
	entry := buildReplayEntry("assistant", "answer", msg, false)
	for _, k := range []string{"timestamp", "internal_marker", "tool_call_id"} {
		if _, ok := entry[k]; ok {
			t.Errorf("%s leaked", k)
		}
	}
}

func TestUserForwardsAPIContentWhenContentUnchanged(t *testing.T) {
	entry := buildReplayEntry("user", "hi", map[string]any{"role": "user", "content": "hi", "api_content": "hi\n\nCTX"}, false)
	if entry["api_content"] != "hi\n\nCTX" || entry["content"] != "hi" {
		t.Fatalf("entry %v", entry)
	}
}

func TestAssistantForwardsAPIContentWhenContentUnchanged(t *testing.T) {
	entry := buildReplayEntry("assistant", "a", map[string]any{"role": "assistant", "content": "a", "api_content": "a <memory-context>"}, false)
	if entry["api_content"] != "a <memory-context>" {
		t.Fatalf("entry %v", entry)
	}
}

func TestEndToEndHistoryBuildKeepsSidecar(t *testing.T) {
	history := []map[string]any{
		{"role": "user", "content": "hi", "api_content": "hi\n\nCTX", "timestamp": 123.0},
		{"role": "assistant", "content": "hello"},
	}
	agentHistory, _, err := buildGatewayAgentHistory(historyCtx(t), history, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if agentHistory[0]["api_content"] != "hi\n\nCTX" {
		t.Fatalf("history %v", agentHistory)
	}
}

func TestGatewayHistoryKeepsSidecarOnlyAssistantRow(t *testing.T) {
	history := []map[string]any{
		{"role": "user", "content": "2+2?"},
		{"role": "assistant", "content": "", "reasoning": "The answer is 4.", "api_content": "The answer is 4."},
		{"role": "user", "content": "thanks"},
	}
	agentHistory, _, err := buildGatewayAgentHistory(historyCtx(t), history, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range agentHistory {
		roles = append(roles, m["role"].(string))
	}
	if strings.Join(roles, ",") != "user,assistant,user" {
		t.Fatalf("roles %v", roles)
	}
	if agentHistory[1]["api_content"] != "The answer is 4." || agentHistory[1]["reasoning"] != "The answer is 4." {
		t.Fatalf("assistant row %v", agentHistory[1])
	}
}

func TestReplayRebuildsTheSameConversationSoEveryRoleKeepsItsIdentity(t *testing.T) {
	call := map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "t", "arguments": "{}"}}
	history := []map[string]any{
		{"role": "user", "content": "a\n\nb", "message_uid": strings.Repeat("1", 32), "_absorbed_message_uids": []any{strings.Repeat("b", 32)},
			"timestamp": 1_700_000_000.0},
		{"role": "assistant", "content": "", "tool_calls": []any{call}, "message_uid": strings.Repeat("2", 32)},
		{"role": "tool", "content": "r", "tool_call_id": "call_1", "message_uid": strings.Repeat("3", 32)},
		{"role": "assistant", "content": "done", "message_uid": strings.Repeat("4", 32), "_absorbed_message_uids": []any{strings.Repeat("c", 32)}},
	}
	replayed, _, err := buildGatewayAgentHistory(historyCtx(t), history, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != len(history) {
		t.Fatalf("replayed %d rows", len(replayed))
	}
	for i, m := range replayed {
		if m["role"] != history[i]["role"] || m["message_uid"] != history[i]["message_uid"] ||
			!reflect.DeepEqual(m["_absorbed_message_uids"], history[i]["_absorbed_message_uids"]) {
			t.Errorf("row %d = %v", i, m)
		}
	}
}

func TestAPlainReplayRowDoesNotCarryAToolCallUidMapWithoutItsCalls(t *testing.T) {
	history := []map[string]any{
		{"role": "user", "content": "q", "message_uid": strings.Repeat("1", 32)},
		{"role": "assistant", "content": "done", "message_uid": strings.Repeat("2", 32), "_tool_call_uids": map[string]any{"call_1": strings.Repeat("5", 32)}},
	}
	replayed, _, err := buildGatewayAgentHistory(historyCtx(t), history, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if replayed[1]["message_uid"] != strings.Repeat("2", 32) {
		t.Fatalf("row %v", replayed[1])
	}
	_, a := replayed[1]["_tool_call_uids"]
	_, b := replayed[1]["_tool_call_uid"]
	if a || b {
		t.Fatalf("tool call uid map leaked: %v", replayed[1])
	}
}

func TestSelectCachedHistoryKeepsExpectedReplayCleanup(t *testing.T) {
	live := []map[string]any{
		{"role": "user", "content": "check status", "_db_persisted": true},
		{"role": "assistant", "content": "", "_db_persisted": true, "tool_calls": []any{map[string]any{"id": "call-1", "type": "function",
			"function": map[string]any{"name": "web_search", "arguments": "{}"}}}},
		{"role": "tool", "tool_call_id": "call-1", "content": "[Command interrupted]", "_db_persisted": true},
	}
	persisted, observed, err := buildGatewayAgentHistory(historyCtx(t), live, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if observed != nil || len(persisted) >= len(live) {
		t.Fatalf("persisted %v observed %v", persisted, observed)
	}
	if !reflect.DeepEqual(persisted, []map[string]any{{"role": "user", "content": "check status", "_db_persisted": true}}) {
		t.Fatalf("persisted %v", persisted)
	}
	if _, chosen := selectCachedAgentHistory(persisted, live); chosen {
		t.Fatal("live history chosen over the cleaned persisted copy")
	}
}

func TestSelectCachedHistoryPreservesLiveWhenARealRowIsUnpersisted(t *testing.T) {
	persisted := []map[string]any{{"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi"}}
	live := []map[string]any{
		{"role": "user", "content": "hello", "_db_persisted": true},
		{"role": "assistant", "content": "hi", "_db_persisted": true},
		{"role": "user", "content": "not written"},
	}
	selected, chosen := selectCachedAgentHistory(persisted, live)
	if !chosen || !reflect.DeepEqual(selected, live) {
		t.Fatalf("selected %v chosen %v", selected, chosen)
	}
}

func TestSelectCachedHistoryIgnoresUnmarkedEphemeralScaffolding(t *testing.T) {
	persisted := []map[string]any{{"role": "user", "content": "ship the fix"}}
	live := []map[string]any{
		{"role": "user", "content": "ship the fix", "_db_persisted": true},
		{"role": "user", "content": "Verify your work before stopping.", "_pre_verify_synthetic": true},
	}
	if _, chosen := selectCachedAgentHistory(persisted, live); chosen {
		t.Fatal("ephemeral scaffolding counted as an unpersisted row")
	}
}

// History media paths through the gateway collector
func historyMediaPaths(t *testing.T, history []map[string]any) map[string]struct{} {
	asAny := make([]any, len(history))
	for i, m := range history {
		asAny[i] = m
	}
	paths, err := gateway.CollectHistoryMediaPaths(historyCtx(t), asAny)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestAutoAppendLaneStillDedupsStaleMedia(t *testing.T) {
	history := []map[string]any{
		{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c", "function": map[string]any{"name": "image_generate"}}}},
		{"role": "tool", "tool_call_id": "c", "content": `{"success": true, "image": "/tmp/gen/dog.png"}`},
		{"role": "assistant", "content": "made it! MEDIA:/tmp/gen/dog.png"},
	}
	paths := historyMediaPaths(t, history)
	if _, ok := paths["/tmp/gen/dog.png"]; !ok {
		t.Fatalf("paths %v", paths)
	}
	tags, _, err := collectAutoAppendMediaTags(history, 0, paths)
	if err != nil || len(tags) != 0 {
		t.Fatalf("stale auto-append tags re-emitted: %v %v", tags, err)
	}
}

func TestGatewayAutoAppendIgnoresMediaExamplesInSkillDocs(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": "How should I format gateway media?"},
		{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_skill", "function": map[string]any{"name": "skill_view"}}}},
		{"role": "tool", "tool_call_id": "call_skill", "content": "\nRecommended pattern:\n```text\nMEDIA:/absolute/path/to/image.png\n```\nSecond message:\n```text\ncaption\n```\n"},
		{"role": "assistant", "content": "Use a standalone media message."},
	}
	tags, voice, err := collectAutoAppendMediaTags(messages, 0, nil)
	if err != nil || len(tags) != 0 || voice {
		t.Fatalf("tags %v voice %v err %v", tags, voice, err)
	}
}

func TestImageGenerateNotReemittedAfterCompression(t *testing.T) {
	history := []map[string]any{
		{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c", "function": map[string]any{"name": "image_generate"}}}},
		{"role": "tool", "tool_call_id": "c", "content": `{"success": true, "image": "/tmp/gen/dog.png"}`},
	}
	tags, _, err := collectAutoAppendMediaTags(history, 9999, historyMediaPaths(t, history))
	if err != nil || len(tags) != 0 {
		t.Fatalf("generated image re-emitted after compression: %v %v", tags, err)
	}
}
