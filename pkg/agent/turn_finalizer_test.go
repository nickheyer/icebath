package agent

import (
	"context"
	"testing"
)

func TestBackgroundReviewSnapshotIsolatedFromLiveNestedMessages(t *testing.T) {
	original := []map[string]any{{
		"role":    "assistant",
		"content": []any{map[string]any{"type": "text", "text": "answer"}},
		"tool_calls": []any{map[string]any{
			"id":       "call-1",
			"function": map[string]any{"name": "read_file", "arguments": `{"path":"x"}`},
		}},
	}}
	snapshot := cloneBackgroundReviewMessages(original)
	snapshot[0]["content"].([]any)[0].(map[string]any)["text"] = "review mutation"
	snapshot[0]["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"] = "{}"
	if got := original[0]["content"].([]any)[0].(map[string]any)["text"]; got != "answer" {
		t.Fatalf("content text = %v", got)
	}
	if got := original[0]["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"]; got != `{"path":"x"}` {
		t.Fatalf("arguments = %v", got)
	}
}

func TestSettleHeldFinalRewritesAnUntransformedHeldReply(t *testing.T) {
	calls := 0
	a := &AIAgent{sessionID: "s1", model: "m", holdStream: true, currentTurnID: "turn-9"}
	a.heldTransforms = func(_ context.Context, kw map[string]any) (string, bool) {
		calls++
		return "REWRITTEN", false
	}
	result := map[string]any{"final_response": "raw text", "error": "raw text", "pre_transform_response": "raw text"}
	settleHeldFinal(context.Background(), a, result)
	if result["final_response"] != "REWRITTEN" || result["error"] != "REWRITTEN" || result["response_transformed"] != true || result["pre_transform_response"] != nil {
		t.Fatalf("result = %v", result)
	}
	settleHeldFinal(context.Background(), a, map[string]any{"final_response": "second"})
	if calls != 1 {
		t.Fatalf("a turn already recorded must not transform again, calls = %d", calls)
	}
}

func TestSettleHeldFinalLeavesUnheldAndEmptyResultsAlone(t *testing.T) {
	a := &AIAgent{currentTurnID: "t"}
	a.heldTransforms = func(context.Context, map[string]any) (string, bool) { t.Fatal("transform ran"); return "", false }
	unheld := map[string]any{"final_response": "x"}
	settleHeldFinal(context.Background(), a, unheld)
	if unheld["final_response"] != "x" || len(unheld) != 1 {
		t.Fatalf("unheld = %v", unheld)
	}
	a.holdStream = true
	settleHeldFinal(context.Background(), a, nil)
	for _, r := range []map[string]any{{"final_response": ""}, {"final_response": nil}, {"final_response": 3}} {
		settleHeldFinal(context.Background(), a, r)
		if _, ok := r["response_transformed"]; ok {
			t.Fatalf("non text result touched %v", r)
		}
	}
}

func TestSettleHeldFinalWithheldNoticeWhenTheHeldTransformFails(t *testing.T) {
	a := &AIAgent{holdStream: true, currentTurnID: "t"}
	a.heldTransforms = func(context.Context, map[string]any) (string, bool) { return "", true }
	result := map[string]any{"final_response": "secret", "error": "other"}
	settleHeldFinal(context.Background(), a, result)
	if result["final_response"] != heldTransformFailedReply || result["error"] != "other" {
		t.Fatalf("result = %v", result)
	}
}
