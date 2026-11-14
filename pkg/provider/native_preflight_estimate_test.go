package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

func nativePreflightClient(t *testing.T, nativeCompaction bool) (context.Context, NativePreflightEstimator) {
	t.Helper()
	h := t.TempDir()
	writeConfigYAML(t, h, "codex_responses:\n  native_compaction: "+map[bool]string{true: "true", false: "false"}[nativeCompaction]+"\ncompression:\n  enabled: true\n")
	client, err := newCodexResponsesClient(Route{Provider: "openai-codex", Model: "gpt-5.6", BaseURL: "https://chatgpt.com/backend-api/codex", APIKey: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	estimator, ok := client.(NativePreflightEstimator)
	if !ok {
		t.Fatal("the codex responses client estimates the native preflight")
	}
	return home.WithOverride(context.Background(), h), estimator
}

func historyWithCheckpoint() []*state.Message {
	var msgs []*state.Message
	for i := range 30 {
		id := "call-" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)
		msgs = append(msgs,
			&state.Message{Role: "user", Content: "ask " + id},
			&state.Message{Role: "assistant", Content: "working " + strings.Repeat("tool output ", 400)},
			&state.Message{Role: "tool", Content: "result " + strings.Repeat("payload ", 400), ToolCallID: id},
		)
	}
	msgs = append(msgs, &state.Message{Role: "assistant", Content: "checkpointed turn", CodexReasoningItems: json.RawMessage(`[{"type": "compaction", "encrypted_content": "blob", "_issuer_kind": "codex_backend"}]`)})
	return append(msgs, &state.Message{Role: "user", Content: "follow-up after checkpoint"})
}

func TestReturnsNoneWhenApiModeIsNotCodexResponses(t *testing.T) {
	client, err := newChatCompletionsClient(Route{Provider: "openrouter", Model: "gpt-5.6", BaseURL: "https://openrouter.ai/api/v1", APIKey: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if estimator, ok := client.(NativePreflightEstimator); ok {
		if _, eligible := estimator.EstimateNativePreflightTokens(context.Background(), &Request{Messages: historyWithCheckpoint()}, "", nil); eligible {
			t.Fatal("a chat completions route never estimates the native wire")
		}
	}
}

func TestReturnsNoneWhenNativeCompactionIsDisabled(t *testing.T) {
	ctx, estimator := nativePreflightClient(t, false)
	if _, eligible := estimator.EstimateNativePreflightTokens(ctx, &Request{Messages: historyWithCheckpoint()}, "", nil); eligible {
		t.Fatal("disabled native compaction leaves the generic estimate in charge")
	}
}

func TestPrunedEstimateIsFarBelowDurableTranscript(t *testing.T) {
	ctx, estimator := nativePreflightClient(t, true)
	messages := historyWithCheckpoint()
	generic := EstimateRequestTokens(ctx, messages, "", nil, true)
	native, eligible := estimator.EstimateNativePreflightTokens(ctx, &Request{Messages: messages}, "", nil)
	if !eligible || generic <= native*2 || native >= 8_000 {
		t.Fatalf("the pruned estimate is far below the transcript, generic %d native %d eligible %v", generic, native, eligible)
	}
}
