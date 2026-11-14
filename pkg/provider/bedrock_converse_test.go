package provider

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// JSON text as a map with float numbers, the shape a decoded fixture takes
func bcMap(t testing.TB, text string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("bad fixture %s: %v", text, err)
	}
	return out
}

func bcMaps(t testing.TB, text string) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("bad fixture %s: %v", text, err)
	}
	return out
}

// JSON form of a value so bytes and maps compare the way the wire sees them
func bcJSON(t testing.TB, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func bcSameJSON(t testing.TB, got any, want string) {
	t.Helper()
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	var g any
	_ = json.Unmarshal([]byte(bcJSON(t, got)), &g)
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("got %s\nwant %s", bcJSON(t, got), want)
	}
}

func bcEvents(t testing.TB, text string) bedrockEventSource {
	return bedrockEventList(bcMaps(t, text))
}

func bcBlock(t testing.TB, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not a block %#v", v)
	}
	return m
}

func bcRedacted(t testing.TB, block any) []byte {
	t.Helper()
	rc := bcBlock(t, bcBlock(t, block)["reasoningContent"])
	b, ok := rc["redactedContent"].([]byte)
	if !ok {
		t.Fatalf("no redacted bytes in %#v", block)
	}
	return b
}

func TestConvertToolsToConverse(t *testing.T) {
	result := convertToolsToConverse([]map[string]any{bcMap(t, `{"type":"function","function":{"name":"read_file","description":"Read a file from disk",
		"parameters":{"type":"object","properties":{"path":{"type":"string","description":"File path"}},"required":["path"]}}}`)})
	if len(result) != 1 {
		t.Fatal(result)
	}
	spec := bcBlock(t, bcBlock(t, result[0])["toolSpec"])
	schema := bcBlock(t, bcBlock(t, spec["inputSchema"])["json"])
	if spec["name"] != "read_file" || spec["description"] != "Read a file from disk" || schema["type"] != "object" {
		t.Fatal(spec)
	}
	if _, ok := bcBlock(t, schema["properties"])["path"]; !ok {
		t.Fatal(schema)
	}
	if got := convertToolsToConverse(nil); len(got) != 0 {
		t.Fatal(got)
	}
	if got := convertToolsToConverse([]map[string]any{}); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestConvertMessagesToConverse(t *testing.T) {
	t.Run("extracts_system_prompt", func(t *testing.T) {
		system, msgs := convertMessagesToConverse(bcMaps(t, `[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":"Hello"}]`), nil)
		bcSameJSON(t, system, `[{"text":"You are a helpful assistant."}]`)
		if len(msgs) != 1 || bcBlock(t, msgs[0])["role"] != "user" {
			t.Fatal(msgs)
		}
	})
	t.Run("assistant_with_tool_calls", func(t *testing.T) {
		_, msgs := convertMessagesToConverse(bcMaps(t, `[{"role":"user","content":"Read the file"},{"role":"assistant","content":"I'll read that file.",
			"tool_calls":[{"id":"call_123","type":"function","function":{"name":"read_file","arguments":"{\"path\": \"/tmp/test.txt\"}"}}]}]`), nil)
		if len(msgs) != 3 {
			t.Fatal(len(msgs))
		}
		bcSameJSON(t, bcBlock(t, msgs[1])["content"], `[{"text":"I'll read that file."},{"toolUse":{"toolUseId":"call_123","name":"read_file","input":{"path":"/tmp/test.txt"}}}]`)
	})
	t.Run("tool_result_becomes_user_message", func(t *testing.T) {
		_, msgs := convertMessagesToConverse(bcMaps(t, `[{"role":"user","content":"Read it"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function",
			"function":{"name":"read_file","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"file contents here"}]`), nil)
		bcSameJSON(t, msgs[2], `{"role":"user","content":[{"toolResult":{"toolUseId":"call_1","content":[{"text":"file contents here"}]}}]}`)
	})
}

func TestNormalizeConverseResponse(t *testing.T) {
	t.Run("text_response", func(t *testing.T) {
		r := normalizeConverseResponse(bcMap(t, `{"output":{"message":{"role":"assistant","content":[{"text":"Hello, world!"}]}},"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":5}}`))
		if r.Content != "Hello, world!" || r.ToolCalls != nil || r.FinishReason != "stop" || r.Usage.PromptTokens != 10 || r.Usage.CompletionTokens != 5 || r.Usage.TotalTokens != 15 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("cache_tokens_folded_into_prompt_tokens", func(t *testing.T) {
		r := normalizeConverseResponse(bcMap(t, `{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},"stopReason":"end_turn",
			"usage":{"inputTokens":50,"outputTokens":20,"cacheReadInputTokens":900,"cacheWriteInputTokens":300}}`))
		want := bedrockConverseUsage{PromptTokens: 1250, CompletionTokens: 20, TotalTokens: 1270, CacheReadInputTokens: 900, CacheCreationInputTokens: 300}
		if r.Usage != want {
			t.Fatalf("%+v", r.Usage)
		}
	})
	t.Run("tool_use_response", func(t *testing.T) {
		r := normalizeConverseResponse(bcMap(t, `{"output":{"message":{"role":"assistant","content":[{"text":"I'll read that file."},
			{"toolUse":{"toolUseId":"call_abc","name":"read_file","input":{"path":"/tmp/test.txt"}}}]}},"stopReason":"tool_use","usage":{"inputTokens":20,"outputTokens":15}}`))
		if r.Content != "I'll read that file." || r.FinishReason != "tool_calls" || len(r.ToolCalls) != 1 || r.ToolCalls[0].ID != "call_abc" || r.ToolCalls[0].Function.Name != "read_file" {
			t.Fatalf("%+v", r)
		}
		bcSameJSON(t, json.RawMessage(r.ToolCalls[0].Function.Arguments), `{"path":"/tmp/test.txt"}`)
	})
	t.Run("redacted_reasoning_is_preserved_for_replay", func(t *testing.T) {
		r := normalizeConverseResponse(map[string]any{"output": map[string]any{"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"reasoningContent": map[string]any{"redactedContent": []byte("opaque-bedrock-bytes")}},
			map[string]any{"toolUse": map[string]any{"toolUseId": "call_1", "name": "read_file", "input": map[string]any{}}}}}},
			"stopReason": "tool_use", "usage": map[string]any{"inputTokens": 1, "outputTokens": 2}})
		bcSameJSON(t, r.ReasoningDetails, `[{"type":"redacted_thinking","data":"b3BhcXVlLWJlZHJvY2stYnl0ZXM="}]`)
	})
	t.Run("redacted_reasoning_replays_as_bedrock_content_block", func(t *testing.T) {
		_, msgs := convertMessagesToConverse(bcMaps(t, `[{"role":"assistant","content":null,"reasoning_details":[{"type":"redacted_thinking","data":"b3BhcXVlLWJlZHJvY2stYnl0ZXM="}],
			"tool_calls":[{"id":"call_1","function":{"name":"read_file","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"ok"}]`), nil)
		assistant := bcBlock(t, msgs[1])
		if assistant["role"] != "assistant" || !bytes.Equal(bcRedacted(t, assistant["content"].([]any)[0]), []byte("opaque-bedrock-bytes")) {
			t.Fatal(msgs)
		}
	})
	t.Run("interleaved_reasoning_and_tools_keep_exact_order", func(t *testing.T) {
		r := normalizeConverseResponse(map[string]any{"output": map[string]any{"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"reasoningContent": map[string]any{"redactedContent": []byte("r1")}},
			map[string]any{"toolUse": map[string]any{"toolUseId": "t1", "name": "one", "input": map[string]any{"n": 1}}},
			map[string]any{"reasoningContent": map[string]any{"redactedContent": []byte("r2")}},
			map[string]any{"toolUse": map[string]any{"toolUseId": "t2", "name": "two", "input": map[string]any{"n": 2}}}}}},
			"stopReason": "tool_use"})
		var calls []any
		for _, tc := range r.ToolCalls {
			calls = append(calls, map[string]any{"id": tc.ID, "type": "function", "function": map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments}})
		}
		_, replay := convertMessagesToConverse([]map[string]any{{"role": "user", "content": "go"},
			{"role": "assistant", "content": r.Content, "tool_calls": calls, "reasoning_details": r.ReasoningDetails, "bedrock_content_blocks": r.ContentBlocks}}, nil)
		blocks := bcBlock(t, replay[1])["content"].([]any)
		var kinds []string
		for _, b := range blocks {
			for k := range bcBlock(t, b) {
				kinds = append(kinds, k)
			}
		}
		if !reflect.DeepEqual(kinds, []string{"reasoningContent", "toolUse", "reasoningContent", "toolUse"}) {
			t.Fatal(kinds)
		}
		if string(bcRedacted(t, blocks[0])) != "r1" || string(bcRedacted(t, blocks[2])) != "r2" {
			t.Fatal(blocks)
		}
	})
}

const bcLiveTextThenTool = `[{"messageStart":{"role":"assistant"}},
	{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"I"}}},
	{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"'ll echo "}}},
	{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"banana"}}},
	{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":" now."}}},
	{"contentBlockStop":{"contentBlockIndex":0}},
	{"contentBlockStart":{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"tooluse_1","name":"echo"}}}},
	{"contentBlockDelta":{"contentBlockIndex":1,"delta":{"toolUse":{"input":""}}}},
	{"contentBlockDelta":{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"s\": \"banana\"}"}}}},
	{"contentBlockStop":{"contentBlockIndex":1}},
	{"messageStop":{"stopReason":"tool_use"}},
	{"metadata":{"usage":{"inputTokens":10,"outputTokens":8}}}]`

func TestNormalizeConverseStreamEvents(t *testing.T) {
	t.Run("text_stream", func(t *testing.T) {
		r, err := normalizeConverseStreamEvents(bcEvents(t, `[{"messageStart":{"role":"assistant"}},{"contentBlockStart":{"contentBlockIndex":0,"start":{}}},
			{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"Hello"}}},{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":", world!"}}},
			{"contentBlockStop":{"contentBlockIndex":0}},{"messageStop":{"stopReason":"end_turn"}},{"metadata":{"usage":{"inputTokens":5,"outputTokens":3}}}]`))
		if err != nil || r.Content != "Hello, world!" || r.FinishReason != "stop" || r.Usage.PromptTokens != 5 || r.Usage.CompletionTokens != 3 {
			t.Fatalf("%+v %v", r, err)
		}
	})
	t.Run("redacted_reasoning_stream_is_preserved", func(t *testing.T) {
		events := []map[string]any{{"messageStart": map[string]any{"role": "assistant"}}, {"contentBlockStart": map[string]any{"contentBlockIndex": 0, "start": map[string]any{}}},
			{"contentBlockDelta": map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"reasoningContent": map[string]any{"redactedContent": []byte("stream-secret")}}}},
			{"contentBlockStop": map[string]any{"contentBlockIndex": 0}}, {"messageStop": map[string]any{"stopReason": "end_turn"}},
			{"metadata": map[string]any{"usage": map[string]any{"inputTokens": 2, "outputTokens": 3}}}}
		r, err := normalizeConverseStreamEvents(bedrockEventList(events))
		if err != nil {
			t.Fatal(err)
		}
		bcSameJSON(t, r.ReasoningDetails, `[{"type":"redacted_thinking","data":"c3RyZWFtLXNlY3JldA=="}]`)
	})
	t.Run("tool_use_stream", func(t *testing.T) {
		r, err := normalizeConverseStreamEvents(bcEvents(t, `[{"messageStart":{"role":"assistant"}},
			{"contentBlockStart":{"contentBlockIndex":0,"start":{"toolUse":{"toolUseId":"call_1","name":"read_file"}}}},
			{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"toolUse":{"input":"{\"path\":"}}}},
			{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"toolUse":{"input":"\"/tmp/f\"}"}}}},
			{"contentBlockStop":{"contentBlockIndex":0}},{"messageStop":{"stopReason":"tool_use"}},{"metadata":{"usage":{"inputTokens":10,"outputTokens":8}}}]`))
		if err != nil || r.FinishReason != "tool_calls" || len(r.ToolCalls) != 1 || r.ToolCalls[0].ID != "call_1" || r.ToolCalls[0].Function.Name != "read_file" {
			t.Fatalf("%+v %v", r, err)
		}
		bcSameJSON(t, json.RawMessage(r.ToolCalls[0].Function.Arguments), `{"path":"/tmp/f"}`)
	})
	t.Run("text_deltas_without_content_block_start_stay_one_block_ahead_of_tool_use", func(t *testing.T) {
		r, err := normalizeConverseStreamEvents(bcEvents(t, bcLiveTextThenTool))
		if err != nil || r.Content != "I'll echo banana now." || len(r.ToolCalls) != 1 || r.ToolCalls[0].Function.Name != "echo" {
			t.Fatalf("%+v %v", r, err)
		}
		bcSameJSON(t, r.ContentBlocks, `[{"text":"I'll echo banana now."},{"toolUse":{"toolUseId":"tooluse_1","name":"echo","input":{"s":"banana"}}}]`)
	})
	t.Run("events_without_content_block_index_fall_back_to_arrival_order", func(t *testing.T) {
		live := bcMaps(t, bcLiveTextThenTool)
		var events []map[string]any
		for _, e := range live[:len(live)-2] {
			out := map[string]any{}
			for k, v := range e {
				inner := map[string]any{}
				for kk, vv := range v.(map[string]any) {
					if kk != "contentBlockIndex" {
						inner[kk] = vv
					}
				}
				out[k] = inner
			}
			events = append(events, out)
		}
		events = append(events, bcMaps(t, `[{"contentBlockDelta":{"delta":{"text":"done"}}},{"contentBlockStop":{}},{"messageStop":{"stopReason":"tool_use"}}]`)...)
		r, err := normalizeConverseStreamEvents(bedrockEventList(events))
		if err != nil || r.Content != "I'll echo banana now.\ndone" {
			t.Fatalf("%+v %v", r, err)
		}
		bcSameJSON(t, r.ContentBlocks, `[{"text":"I'll echo banana now."},{"toolUse":{"toolUseId":"tooluse_1","name":"echo","input":{"s":"banana"}}},{"text":"done"}]`)
	})
}
