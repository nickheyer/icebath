package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const bcCrossRegionRejection = "The model returned the following errors: {\"error\":{\"code\":\"validation_error\",\"message\":\"Encrypted content cannot be used in a " +
	"different region from the one that created it.\",\"param\":null,\"type\":\"invalid_request_error\"}}"

func TestStreamConverseWithCallbacks(t *testing.T) {
	t.Run("cache_tokens_folded_into_prompt_tokens", func(t *testing.T) {
		r, _, err := streamConverseWithCallbacks(bcEvents(t, `[{"messageStart":{"role":"assistant"}},{"contentBlockStart":{"contentBlockIndex":0,"start":{}}},
			{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"hi"}}},{"contentBlockStop":{"contentBlockIndex":0}},{"messageStop":{"stopReason":"end_turn"}},
			{"metadata":{"usage":{"inputTokens":50,"outputTokens":20,"cacheReadInputTokens":900,"cacheWriteInputTokens":300}}}]`), bedrockStreamCallbacks{})
		if err != nil || r.Usage.PromptTokens != 1250 || r.Usage.TotalTokens != 1270 || r.Usage.CacheReadInputTokens != 900 || r.Usage.CacheCreationInputTokens != 300 {
			t.Fatalf("%+v %v", r, err)
		}
	})
	t.Run("text_deltas_suppressed_when_tool_use_present", func(t *testing.T) {
		var deltas []string
		r, _, err := streamConverseWithCallbacks(bcEvents(t, `[{"messageStart":{"role":"assistant"}},{"contentBlockStart":{"contentBlockIndex":0,"start":{}}},
			{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"Let me check."}}},{"contentBlockStop":{"contentBlockIndex":0}},
			{"contentBlockStart":{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"c1","name":"search"}}}},
			{"contentBlockDelta":{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"q\":\"test\"}"}}}},{"contentBlockStop":{"contentBlockIndex":1}},
			{"messageStop":{"stopReason":"tool_use"}},{"metadata":{"usage":{"inputTokens":0,"outputTokens":0}}}]`),
			bedrockStreamCallbacks{OnTextDelta: func(s string) error { deltas = append(deltas, s); return nil }})
		if err != nil || !reflect.DeepEqual(deltas, []string{"Let me check."}) || r.Content != "Let me check." || len(r.ToolCalls) != 1 {
			t.Fatalf("%+v %v %v", r, deltas, err)
		}
	})
}

func TestGuardrailConfigInConverseKwargs(t *testing.T) {
	guardrail := map[string]any{"guardrailIdentifier": "gr-abc123", "guardrailVersion": "1", "streamProcessingMode": "async", "trace": "enabled"}
	in := newBedrockConverseInput("test-model", bcMaps(t, `[{"role":"user","content":"Hi"}]`))
	in.GuardrailConfig = guardrail
	if kw := buildBedrockConverseKwargs(in); !reflect.DeepEqual(kw["guardrailConfig"], guardrail) {
		t.Fatal(kw)
	}
	in.GuardrailConfig = map[string]any{}
	if kw := buildBedrockConverseKwargs(in); kw["guardrailConfig"] != nil {
		t.Fatal(kw)
	}
}

func TestBuildConverseKwargsToolStripping(t *testing.T) {
	in := newBedrockConverseInput("us.deepseek.r1-v1:0", bcMaps(t, `[{"role":"user","content":"Hi"}]`))
	in.Tools = bcMaps(t, `[{"type":"function","function":{"name":"test","description":"t","parameters":{}}}]`)
	if kw := buildBedrockConverseKwargs(in); kw["toolConfig"] != nil {
		t.Fatal(kw)
	}
}

func TestEmptyTextBlockFix(t *testing.T) {
	blocks := convertContentToConverse(nil)
	if text := bcBlock(t, blocks[0])["text"]; text != bedrockEmptyTextPlaceholder || strings.TrimSpace(text.(string)) == "" {
		t.Fatal(blocks)
	}
}

func TestCallConverseDoesNotEvictOnNonStaleError(t *testing.T) {
	bcCallConverseFake(t, &bedrockHTTPError{Code: "ValidationException", Message: "bad"})
	model := "anthropic.claude-3-sonnet-20240229-v1:0"
	live, err := bedrockRuntimeClient(t.Context(), "us-east-1", model)
	if err != nil {
		t.Fatal(err)
	}
	_, err = callBedrockConverse(t.Context(), "us-east-1", newBedrockConverseInput(model, bcMaps(t, `[{"role":"user","content":"hi"}]`)))
	var svc *AWSServiceError
	if !errors.As(err, &svc) || svc.Code != "ValidationException" {
		t.Fatal(err)
	}
	if again, _ := bedrockRuntimeClient(t.Context(), "us-east-1", model); again != live {
		t.Fatal("validation errors do not indicate a dead connection, the client must be kept")
	}
}

func TestImageBase64Decoding(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8/5+hHgAHggJ/PchI7wAAAABJRU5ErkJggg=="
	raw, _ := base64.StdEncoding.DecodeString(png)
	blocks := convertContentToConverse([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + png}}})
	img := bcBlock(t, bcBlock(t, blocks[0])["image"])
	if len(blocks) != 1 || img["format"] != "png" || !bytes.Equal(bcBlock(t, img["source"])["bytes"].([]byte), raw) {
		t.Fatal(blocks)
	}
	blocks = convertContentToConverse([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64,NOT_VALID_BASE64!!!"}}})
	if _, ok := bcBlock(t, bcBlock(t, bcBlock(t, blocks[0])["image"])["source"])["bytes"].([]byte); len(blocks) != 1 || !ok {
		t.Fatal(blocks)
	}
}

// Turn one as the ConverseStream path captures it, signed thinking then a sealed blob then a tool call
func bcKimiTurnHistory(t *testing.T) []map[string]any {
	events := []map[string]any{{"messageStart": map[string]any{"role": "assistant"}},
		{"contentBlockDelta": map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"reasoningContent": map[string]any{"text": "let me think"}}}},
		{"contentBlockDelta": map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"reasoningContent": map[string]any{"signature": "sig-1"}}}},
		{"contentBlockStop": map[string]any{"contentBlockIndex": 0}},
		{"contentBlockDelta": map[string]any{"contentBlockIndex": 1, "delta": map[string]any{"reasoningContent": map[string]any{"redactedContent": []byte("sealed")}}}},
		{"contentBlockStop": map[string]any{"contentBlockIndex": 1}},
		{"contentBlockStart": map[string]any{"contentBlockIndex": 2, "start": map[string]any{"toolUse": map[string]any{"toolUseId": "t1", "name": "read_file"}}}},
		{"contentBlockDelta": map[string]any{"contentBlockIndex": 2, "delta": map[string]any{"toolUse": map[string]any{"input": "{}"}}}},
		{"contentBlockStop": map[string]any{"contentBlockIndex": 2}},
		{"messageStop": map[string]any{"stopReason": "tool_use"}},
		{"metadata": map[string]any{"usage": map[string]any{"inputTokens": 1, "outputTokens": 1}}}}
	r, err := normalizeConverseStreamEvents(bedrockEventList(events))
	if err != nil {
		t.Fatal(err)
	}
	return []map[string]any{{"role": "user", "content": "go"},
		{"role": "assistant", "content": nil, "reasoning_content": r.ReasoningContent, "reasoning_details": r.ReasoningDetails, "bedrock_content_blocks": r.ContentBlocks,
			"tool_calls": []any{map[string]any{"id": "t1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}}}},
		{"role": "tool", "tool_call_id": "t1", "content": "ok"}}
}

// Wire body of recorded kwargs validated against the botocore Converse input shape
func bcValidateConverseBody(t *testing.T, kwargs map[string]any) {
	body := map[string]any{}
	for k, v := range kwargs {
		if k != "modelId" {
			body[k] = v
		}
	}
	dec := json.NewDecoder(strings.NewReader(bcJSON(t, body)))
	dec.UseNumber()
	var wire map[string]any
	if err := dec.Decode(&wire); err != nil {
		t.Fatal(err)
	}
	if rej := bedrockValidateShape(bedrockServiceShapes(t), "Converse", bedrockKwargsModel(kwargs), wire); rej != nil {
		t.Fatal(rej)
	}
}

func TestReasoningReplaySchema(t *testing.T) {
	t.Run("call_converse_replays_thinking_botocore_accepts", func(t *testing.T) {
		f := bcCallConverseFake(t, newBedrockTurn(bedrockTextBlock("done")))
		f.mu.Lock()
		f.signatures["let me think"] = "sig-1"
		f.mu.Unlock()
		in := newBedrockConverseInput("global.moonshotai.kimi-k3", bcKimiTurnHistory(t))
		bcValidateConverseBody(t, buildBedrockConverseKwargs(in))
		r, err := callBedrockConverse(t.Context(), "us-east-1", in)
		if err != nil || r.Content != "done" {
			t.Fatalf("%+v %v", r, err)
		}
		replayed := f.snapshot()[0].Body["messages"].([]any)[1].(map[string]any)["content"].([]any)
		bcSameJSON(t, replayed[0], `{"reasoningContent":{"reasoningText":{"text":"let me think","signature":"sig-1"}}}`)
		bcSameJSON(t, replayed[1], `{"reasoningContent":{"redactedContent":"`+base64.StdEncoding.EncodeToString([]byte("sealed"))+`"}}`)
		if _, ok := replayed[2].(map[string]any)["toolUse"]; !ok {
			t.Fatal(replayed)
		}
	})
	t.Run("sync_response_reads_nested_reasoning_text", func(t *testing.T) {
		r := normalizeConverseResponse(bcMap(t, `{"output":{"message":{"role":"assistant","content":[{"reasoningContent":{"reasoningText":{"text":"hmm","signature":"s"}}},{"text":"hi"}]}},
			"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1}}`))
		if r.ReasoningContent != "hmm" {
			t.Fatal(r.ReasoningContent)
		}
		bcSameJSON(t, r.ContentBlocks[0], `{"reasoningContent":{"text":"hmm","signature":"s"}}`)
	})
}

func TestSealedReasoningResendOnce(t *testing.T) {
	t.Run("call_converse_strips_sealed_blocks_and_keeps_other_turns_intact", func(t *testing.T) {
		f := bcCallConverseFake(t, &bedrockHTTPError{Code: "ValidationException", Message: bcCrossRegionRejection}, newBedrockTurn(bedrockTextBlock("done")))
		f.mu.Lock()
		f.signatures["let me think"] = "sig-1"
		f.mu.Unlock()
		r, err := callBedrockConverse(t.Context(), "us-east-1", newBedrockConverseInput("global.moonshotai.kimi-k3", bcKimiTurnHistory(t)))
		if err != nil || r.Content != "done" {
			t.Fatalf("%+v %v", r, err)
		}
		recs := f.snapshot()
		if len(recs) != 2 {
			t.Fatal(len(recs))
		}
		first, resent := recs[0].Body["messages"].([]any), recs[1].Body["messages"].([]any)
		var kept []any
		for _, b := range first[1].(map[string]any)["content"].([]any) {
			if rc, ok := b.(map[string]any)["reasoningContent"].(map[string]any); ok && rc["redactedContent"] != nil {
				continue
			}
			kept = append(kept, b)
		}
		if bcJSON(t, resent[1].(map[string]any)["content"]) != bcJSON(t, kept) || bcJSON(t, resent[0]) != bcJSON(t, first[0]) || bcJSON(t, resent[2]) != bcJSON(t, first[2]) {
			t.Fatal(bcJSON(t, resent))
		}
	})
	t.Run("call_converse_reraises_when_nothing_sealed_remains", func(t *testing.T) {
		f := bcCallConverseFake(t, &bedrockHTTPError{Code: "ValidationException", Message: bcCrossRegionRejection})
		_, err := callBedrockConverse(t.Context(), "us-east-1", newBedrockConverseInput("m", bcMaps(t, `[{"role":"user","content":"hi"}]`)))
		if err == nil || !strings.Contains(err.Error(), "ValidationException") || len(f.snapshot()) != 1 {
			t.Fatal(err)
		}
	})
}
