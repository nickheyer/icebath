package provider

import (
	"context"
	"reflect"
	"testing"
)

// Relays one completion through the sync or async surface with a capturing create callback
func auxRelayCapture(t *testing.T, asyncMode bool, client AuxClient, kwargs map[string]any) map[string]any {
	t.Helper()
	var captured map[string]any
	send := func(_ context.Context, request map[string]any) (any, error) {
		captured = request
		return nil, nil
	}
	var err error
	if asyncMode {
		_, err = auxRelayAsyncCompletion(context.Background(), client, kwargs, "", "", send)
	} else {
		_, err = auxRelaySyncCompletion(context.Background(), client, kwargs, "", "", send)
	}
	if err != nil || captured == nil {
		t.Fatalf("async=%v got (%v, %v)", asyncMode, captured, err)
	}
	return captured
}

func TestAuxiliaryChatWireSanitizesWithoutMutatingHistory(t *testing.T) {
	for _, asyncMode := range []bool{false, true} {
		history := []map[string]any{{"role": "assistant", "content": "answer", "_db_persisted": true,
			"timestamp": 12, "tool_calls": []any{}, "reasoning": "private"}}
		original := []map[string]any{auxCloneMap(history[0])}
		kwargs := map[string]any{"model": "fixture-model", "messages": history}
		client := newAuxOpenAIClient("fixture", "http://127.0.0.1:1/v1", auxOpenAIOptions{})
		captured := auxRelayCapture(t, asyncMode, client, kwargs)
		want := []map[string]any{{"role": "assistant", "content": "answer", "reasoning": "private"}}
		if got := auxMessageList(captured["messages"]); !reflect.DeepEqual(got, want) {
			t.Fatalf("async=%v wire messages %v", asyncMode, got)
		}
		if !reflect.DeepEqual(kwargs["messages"], original) {
			t.Fatalf("async=%v history mutated %v", asyncMode, kwargs["messages"])
		}
	}
}

func TestAuxiliaryNativeAdaptersKeepReplayAndToolFields(t *testing.T) {
	for _, asyncMode := range []bool{false, true} {
		history := []map[string]any{{"role": "assistant", "content": "", "_db_persisted": true,
			"codex_reasoning_items": []any{map[string]any{"type": "reasoning", "id": "rs_fixture"}},
			"thinking_blocks":       []any{map[string]any{"type": "thinking", "thinking": "native", "signature": "sig"}},
			"tool_calls": []any{map[string]any{"id": "call_fixture", "type": "function",
				"function": map[string]any{"name": "fixture", "arguments": "{}"}}}}}
		kwargs := map[string]any{"model": "native", "messages": history}
		captured := auxRelayCapture(t, asyncMode, auxBareClient{}, kwargs)
		if reflect.ValueOf(captured).Pointer() != reflect.ValueOf(kwargs).Pointer() {
			t.Fatalf("async=%v native adapter kwargs were copied", asyncMode)
		}
		got, ok := captured["messages"].([]map[string]any)
		if !ok || reflect.ValueOf(got).Pointer() != reflect.ValueOf(history).Pointer() {
			t.Fatalf("async=%v native adapter history was replaced by %T", asyncMode, captured["messages"])
		}
	}
}
