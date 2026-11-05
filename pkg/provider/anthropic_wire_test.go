package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func anthSSEStart() map[string]any {
	return map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_s", "type": "message", "role": "assistant", "model": "claude-test",
		"content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": 10, "output_tokens": 0}}}
}

func anthSSEBlockStart(index int, block map[string]any) map[string]any {
	return map[string]any{"type": "content_block_start", "index": index, "content_block": block}
}

func anthSSEDelta(index int, delta map[string]any) map[string]any {
	return map[string]any{"type": "content_block_delta", "index": index, "delta": delta}
}

func anthSSETextDelta(index int, text string) map[string]any {
	return anthSSEDelta(index, map[string]any{"type": "text_delta", "text": text})
}

func anthSSEBlockStop(index int) map[string]any {
	return map[string]any{"type": "content_block_stop", "index": index}
}

func anthSSEMessageDelta(stop string) map[string]any {
	return map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 5}}
}

func anthSSEStop() map[string]any { return map[string]any{"type": "message_stop"} }

func TestAnthropicEOFBeforeMessageStopRaisesEmptyStream(t *testing.T) {
	srv := newAnthServer(t, anthReplySSE(anthSSEStart(), anthSSEBlockStart(0, anthText("")), anthSSETextDelta(0, "HALF")))
	c := srv.client(srv.route("", "sk-ant-api03-test"))
	_, err := c.Stream(context.Background(), anthUserRequest("claude-test", "hi"), func(StreamEvent) error { return nil })
	var empty *EmptyStreamError
	if !errors.As(err, &empty) || !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("unterminated stream must raise EmptyStreamError naming message_stop, got %v", err)
	}
}

func TestAnthropicMessageStopAcceptsCompletedStreamWithoutRetry(t *testing.T) {
	srv := newAnthServer(t, anthReplySSE(anthSSEStart(), anthSSEBlockStart(0, anthText("")), anthSSETextDelta(0, "done"), anthSSEBlockStop(0),
		anthSSEMessageDelta("end_turn"), anthSSEStop()))
	c := srv.client(srv.route("", "sk-ant-api03-test"))
	var delivered []string
	resp, err := c.Stream(context.Background(), anthUserRequest("claude-test", "hi"), func(ev StreamEvent) error {
		if ev.Kind == EventTextDelta {
			delivered = append(delivered, ev.Text)
		}
		return nil
	})
	if err != nil || resp.Message.Content != "done" || resp.FinishReason != "stop" {
		t.Fatalf("completed stream: %v %+v", err, resp)
	}
	if n := len(srv.requests()); n != 1 || strings.Join(delivered, "") != "done" {
		t.Fatalf("one request and the text delta expected, got %d %v", n, delivered)
	}
}

func TestAnthropicSafeText(t *testing.T) {
	for _, v := range []any{nil, "   ", "\n", "\t", " \n\t "} {
		if got := safeText(v); got != emptyTextPlaceholder {
			t.Fatalf("safeText(%q) = %q", v, got)
		}
	}
}

func TestAnthropicSanitizeReplayBlockWhitespace(t *testing.T) {
	if sanitizeReplayBlock(map[string]any{"type": "text", "text": "   \n"}) != nil || sanitizeReplayBlock(map[string]any{"type": "text", "text": nil}) != nil {
		t.Fatal("blank replay text must drop")
	}
	anthMustEqualJSON(t, sanitizeReplayBlock(map[string]any{"type": "text", "text": "hi"}), anthText("hi"))
}

func TestAnthropicConvertAssistantMessageWhitespace(t *testing.T) {
	blocks := anthConvertAssistant(t, `{"role":"assistant","anthropic_content_blocks":[{"type":"thinking","thinking":"reasoning","signature":"sig-A"},{"type":"text","text":"  "}]}`)
	var texts []map[string]any
	for _, b := range blocks {
		if b["type"] == "text" {
			texts = append(texts, b)
		}
	}
	anthMustEqualJSON(t, texts, []any{anthText(emptyTextPlaceholder)})

	blocks = anthConvertAssistant(t, `{"role":"assistant","anthropic_content_blocks":[{"type":"thinking","thinking":"  ","signature":"sig-B"},{"type":"text","text":"real"}]}`)
	var thinking []map[string]any
	for _, b := range blocks {
		if b["type"] == "thinking" {
			thinking = append(thinking, b)
		}
	}
	anthMustEqualJSON(t, thinking, []any{map[string]any{"type": "thinking", "thinking": "  ", "signature": "sig-B"}})
}

func TestAnthropicTruncatedResponseNormalization(t *testing.T) {
	c := &anthropicClient{route: Route{Provider: "anthropic", Model: "claude-sonnet-4-6"}, transport: newAnthDirectTransport(Route{})}
	resp, err := c.normalizeResponse(anthMessage("max_tokens", anthText("partial response that was cut off")), 200, nil, 0)
	if err != nil || !strings.Contains(asStr(resp.Message.Content), "partial response") || len(resp.Message.ToolCalls) != 0 || resp.FinishReason != "length" {
		t.Fatalf("truncated text response wrong: %v %+v", err, resp)
	}
	empty := anthMessage("max_tokens")
	if anthValidateMessage(empty) {
		t.Fatal("empty max_tokens content must fail the structural check so the caller retries")
	}
	resp = c.normalizeMessage(empty, nil, "msg_01", 200, nil, 0)
	if resp == nil || resp.Message == nil || len(resp.Message.ToolCalls) != 0 {
		t.Fatalf("empty truncated content must normalize: %+v", resp)
	}
}

func TestAnthropicSanitizeKwargsStripsResponsesOnlyKeys(t *testing.T) {
	payload := map[string]any{"model": "claude-sonnet-4-6", "instructions": "You are Hermes.", "input": []any{map[string]any{"role": "user", "content": "hi"}},
		"store": false, "parallel_tool_calls": true}
	anthSanitizeKwargs(payload, "")
	anthMustEqualJSON(t, payload, map[string]any{"model": "claude-sonnet-4-6"})
}

func anthWriteConfig(t *testing.T, yaml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

const anthWAFConfig = `custom_providers:
  - name: wafproxy
    base_url: https://proxy.example.com/v1
    api_mode: anthropic_messages
    extra_headers:
      User-Agent: IcebathAgent/1.0
      X-Privacy-Tier: enterprise
`

func TestAnthropicMatchingRouteMergesExtraHeadersAfterBetas(t *testing.T) {
	srv := newAnthServer(t)
	anthWriteConfig(t, anthWAFConfig)
	if _, err := srv.client(srv.route("https://proxy.example.com/v1", "sk-test")).Complete(context.Background(), anthUserRequest("claude-sonnet-4-6", "hi")); err != nil {
		t.Fatal(err)
	}
	h := srv.last().Header
	if h.Get("User-Agent") != "IcebathAgent/1.0" || h.Get("X-Privacy-Tier") != "enterprise" || h.Get("anthropic-beta") == "" {
		t.Fatalf("provider headers must layer over the betas: %v", h)
	}
}

func TestAnthropicOtherRouteDoesNotInheritExtraHeaders(t *testing.T) {
	srv := newAnthServer(t)
	anthWriteConfig(t, anthWAFConfig)
	if _, err := srv.client(srv.route("https://other.example.com/v1", "sk-test")).Complete(context.Background(), anthUserRequest("claude-sonnet-4-6", "hi")); err != nil {
		t.Fatal(err)
	}
	h := srv.last().Header
	if h.Get("User-Agent") == "IcebathAgent/1.0" || h.Get("X-Privacy-Tier") != "" {
		t.Fatalf("another route must not inherit the headers: %v", h)
	}
}

func anthAssertClean(t *testing.T, b map[string]any) {
	t.Helper()
	for _, k := range []string{"parsed_output", "caller"} {
		if _, has := b[k]; has {
			t.Fatalf("forbidden field %s survived: %v", k, b)
		}
	}
	if c, has := b["citations"]; has {
		if list, ok := c.([]any); !ok || len(list) == 0 {
			t.Fatalf("citations must be a non empty list: %v", b)
		}
	}
}

func TestAnthropicOutputFieldsDoNotLeakIntoInput(t *testing.T) {
	out := sanitizeReplayBlock(map[string]any{"type": "tool_use", "id": "toolu_1", "name": "read_file", "input": map[string]any{"path": "a"}, "caller": map[string]any{"type": "agent"}})
	anthAssertClean(t, out)
	if out["name"] != "read_file" || !anthJSONEqual(out["input"], map[string]any{"path": "a"}) {
		t.Fatalf("tool_use lost fields: %v", out)
	}
	if sanitizeReplayBlock(map[string]any{"type": "server_tool_use", "foo": 1}) != nil {
		t.Fatal("unknown block type must drop")
	}
	anthAssertClean(t, convertContentPartToAnthropic(map[string]any{"type": "text", "text": "hello", "parsed_output": nil, "citations": nil}))

	blocks := anthConvertAssistant(t, `{"role":"assistant","anthropic_content_blocks":[{"type":"thinking","thinking":"plan","signature":"s1"},
		{"type":"text","text":"doing it","parsed_output":null,"citations":null},{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a"},"caller":{"type":"agent"}}]}`)
	if len(blocks) != 3 || blocks[0]["type"] != "thinking" || blocks[1]["type"] != "text" || blocks[2]["type"] != "tool_use" {
		t.Fatalf("replay order wrong: %v", blocks)
	}
	for _, b := range blocks {
		anthAssertClean(t, b)
	}
	if blocks[0]["signature"] != "s1" || blocks[2]["name"] != "read_file" {
		t.Fatalf("replay lost fields: %v", blocks)
	}
}

func anthAssertNoBlank(t *testing.T, system any, messages []map[string]any) {
	t.Helper()
	for _, b := range anthBlocks(system) {
		if b["type"] == "text" && strings.TrimSpace(asStr(b["text"])) == "" {
			t.Fatalf("blank system block: %v", b)
		}
	}
	for _, m := range messages {
		for _, b := range anthBlocks(m["content"]) {
			if s, ok := b["text"].(string); b["type"] == "text" && !(ok && strings.TrimSpace(s) != "") {
				t.Fatalf("blank text block survived: %v", messages)
			}
		}
	}
}

func TestAnthropicRequestBlankBlockGuard(t *testing.T) {
	for _, fixture := range []string{
		`[{"role":"user","content":[{"type":"text","text":"   "},{"type":"text","text":"real question"}]}]`,
		`[{"role":"user","content":"hi"},{"role":"assistant","content":"answer"},{"role":"user","content":"\n\n"}]`,
		`[{"role":"system","content":[{"type":"text","text":"  ","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":"hello"}]`,
	} {
		system, result := convertMessagesToAnthropic(anthMsgs(t, fixture), convertOptions{})
		anthAssertNoBlank(t, system, result)
	}
	system, result := convertMessagesToAnthropic(anthMsgs(t, `[{"role":"assistant","content":"summary of earlier turns"},{"role":"user","content":"continue"}]`), convertOptions{})
	if result[0]["role"] != "user" {
		t.Fatalf("leading user turn must be prepended: %v", result)
	}
	anthAssertNoBlank(t, system, result)

	_, result = convertMessagesToAnthropic(anthMsgs(t, `[{"role":"user","content":"what is 2+2?"}]`), convertOptions{})
	var texts []string
	for _, m := range result {
		if s, ok := m["content"].(string); ok {
			texts = append(texts, s)
		}
		for _, b := range anthBlocks(m["content"]) {
			if b["type"] == "text" {
				texts = append(texts, asStr(b["text"]))
			}
		}
	}
	joined := strings.Join(texts, "\x00")
	if !strings.Contains(joined, "what is 2+2?") || strings.Contains(joined, emptyTextPlaceholder) {
		t.Fatalf("real text must stay untouched: %v", texts)
	}
}
