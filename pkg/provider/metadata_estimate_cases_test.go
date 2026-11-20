package provider

import (
	"crypto/rand"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
)

func estimateRows(rows ...map[string]any) int {
	return EstimateRowsTokens(rows, DefaultImageTokenCost)
}

func TestEstimateTokensRoughEmptyString(t *testing.T) {
	if EstimateTokens("") != 0 {
		t.Fatal("an empty string has no tokens")
	}
}

func TestEstimateMessagesTokensRoughToolCallMessage(t *testing.T) {
	msg := map[string]any{"role": "assistant", "content": nil,
		"tool_calls": []any{map[string]any{"id": "1", "function": map[string]any{"name": "terminal", "arguments": "{}"}}}}
	if estimateRows(msg) <= 0 {
		t.Fatal("a tool call message without content still contributes tokens")
	}
}

func TestEstimateMessagesTokensRoughPersistenceTimestampDoesNotChangeEstimate(t *testing.T) {
	msg := map[string]any{"role": "assistant", "content": "done",
		"tool_calls": []any{map[string]any{"id": "call-1", "function": map[string]any{"name": "terminal", "arguments": "{}"}}}}
	stamped := map[string]any{"timestamp": 1_781_976_577.123456}
	for k, v := range msg {
		stamped[k] = v
	}
	if estimateRows(stamped) != estimateRows(msg) {
		t.Fatal("durability metadata must not create context pressure")
	}
}

func TestEstimateMessagesTokensRoughDisplayOnlyFieldsDoNotChangeEstimate(t *testing.T) {
	wire := map[string]any{"role": "tool", "tool_call_id": "call-1", "name": "write_file", "content": `{"bytes_written": 18000}`}
	rich := map[string]any{"_row_id": 42, "display_kind": "tool_result",
		"display_metadata": map[string]any{"tool_result_metadata": map[string]any{"inline_diff": strings.Repeat("\x1b[32m+ line\x1b[0m\n", 600)}}}
	for k, v := range wire {
		rich[k] = v
	}
	if estimateRows(rich) != estimateRows(wire) {
		t.Fatal("display metadata the request builder strips must not be priced")
	}
}

func TestEstimateMessagesTokensRoughMessageWithListContent(t *testing.T) {
	msg := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "describe"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
	}}
	if got := estimateRows(msg); got < 1500 || got >= 2000 {
		t.Fatalf("estimate %d", got)
	}
}

func TestEstimateMessagesTokensRoughApiContentSubstitutesForContentNotAddedToIt(t *testing.T) {
	body := strings.Repeat("cached prompt bytes ", 2000)
	wire := map[string]any{"role": "user", "content": body}
	persisted := map[string]any{"role": "user", "content": body, "api_content": body}
	if estimateRows(persisted) != estimateRows(wire) {
		t.Fatal("the sidecar replaces content on the wire, so only one is counted")
	}
}

func TestEstimateMessagesTokensRoughApiContentIsCountedWhenItDiffersFromContent(t *testing.T) {
	bigSidecar := strings.Repeat("cached prompt bytes ", 2000)
	msg := map[string]any{"role": "user", "content": "short", "api_content": bigSidecar}
	if got := estimateRows(msg); float64(got) < float64(len(bigSidecar)/4)*0.9 {
		t.Fatalf("estimate %d dropped the sidecar", got)
	}
}

func TestEstimateMessagesTokensRoughNonStringApiContentDoesNotDisplaceContent(t *testing.T) {
	body := strings.Repeat("clean stored content ", 2000)
	baseline := estimateRows(map[string]any{"role": "user", "content": body})
	for _, bad := range []any{nil, "", 42, []any{"not", "a", "string"}} {
		if got := estimateRows(map[string]any{"role": "user", "content": body, "api_content": bad}); got < baseline {
			t.Fatalf("sidecar %v displaced the content: %d < %d", bad, got, baseline)
		}
	}
	if got := estimateRows(map[string]any{"role": "tool", "content": body, "api_content": "ignored"}); got < baseline {
		t.Fatalf("a tool row sidecar displaced the content: %d < %d", got, baseline)
	}
}

// Data URL carrying n random bytes as base64, the raw payload a text count would price enormously
func randomImageDataURL(t *testing.T, n int) string {
	t.Helper()
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
}

func TestEstimateMessagesTokensRoughImageStrippingSurvivesShadowExtraction(t *testing.T) {
	msg := map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": randomImageDataURL(t, 300_000)}}}}
	if got := estimateRows(msg); got >= 5_000 {
		t.Fatalf("estimate %d priced the base64 as text", got)
	}
}

func TestResponsesItemImageAccountingFunctionCallOutputImageMatchesChatEstimate(t *testing.T) {
	payload := randomImageDataURL(t, 100_000)
	chat := map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": payload}}}}
	item := map[string]any{"type": "function_call_output", "call_id": "call_1", "output": []any{map[string]any{"type": "input_image", "image_url": payload}}}
	chatEst, responsesEst := estimateRows(chat), estimateRows(item)
	if diff := chatEst - responsesEst; diff >= 200 || diff <= -200 {
		t.Fatalf("chat %d responses %d", chatEst, responsesEst)
	}
}

func TestResponsesItemImageAccountingFunctionCallOutputTextOutputStillCounted(t *testing.T) {
	output := strings.Repeat("plain tool result ", 100)
	item := map[string]any{"type": "function_call_output", "call_id": "call_1", "output": output}
	if got := estimateRows(item); float64(got) < float64(len(output)/4)*0.9 {
		t.Fatalf("estimate %d", got)
	}
}

func TestEstimateRequestTokensRoughToolsCacheIsBounded(t *testing.T) {
	toolsTokensMu.Lock()
	toolsTokensCache, toolsTokensOrder = map[uintptr]toolsTokensEntry{}, nil
	toolsTokensMu.Unlock()
	t.Cleanup(func() {
		toolsTokensMu.Lock()
		toolsTokensCache, toolsTokensOrder = map[uintptr]toolsTokensEntry{}, nil
		toolsTokensMu.Unlock()
	})
	var held [][]map[string]any
	for i := range toolsTokensCacheMax + 50 {
		tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "tool_" + strconv.Itoa(i), "description": "d", "parameters": map[string]any{"type": "object"}}}}
		held = append(held, tools)
		EstimateToolsTokens(tools)
		toolsTokensMu.Lock()
		size := len(toolsTokensCache)
		toolsTokensMu.Unlock()
		if size > toolsTokensCacheMax {
			t.Fatalf("cache grew to %d", size)
		}
	}
	toolsTokensMu.Lock()
	defer toolsTokensMu.Unlock()
	if len(toolsTokensCache) != toolsTokensCacheMax || len(held) == 0 {
		t.Fatalf("cache holds %d entries", len(toolsTokensCache))
	}
}
