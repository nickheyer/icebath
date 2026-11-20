package provider

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

const (
	bc2Claude = "us.anthropic.claude-opus-4-8-v1:0"
	bc2GPT    = "global.openai.gpt-6.1-sol"
	bc2Opus   = "global.anthropic.claude-opus-5-5"
)

// Converse kwargs the Bedrock transport builds, max tokens and reasoning optional
func bc2Kwargs(t *testing.T, model string, messages []map[string]any, maxTokens *int, reasoning bool) map[string]any {
	t.Helper()
	in := bedrockConverseInput{Model: model, Messages: messages, MaxTokens: maxTokens}
	if reasoning {
		yes := true
		in.Reasoning = &anthReasoningConfig{enabled: &yes, effort: "high"}
	}
	return buildBedrockConverseKwargs(in)
}

func bc2Assistants(t *testing.T, kwargs map[string]any) [][]any {
	t.Helper()
	var out [][]any
	for _, m := range kwargs["messages"].([]any) {
		msg := bcBlock(t, m)
		if msg["role"] == "assistant" {
			out = append(out, msg["content"].([]any))
		}
	}
	return out
}

func bc2ReasoningTexts(t *testing.T, blocks []any) []any {
	t.Helper()
	out := []any{}
	for _, b := range blocks {
		if rc, has := bcBlock(t, b)["reasoningContent"]; has {
			out = append(out, bcBlock(t, rc)["reasoningText"])
		}
	}
	return out
}

func bc2AnthropicTurn(t *testing.T, question, sig, toolID string) []map[string]any {
	signed := fmt.Sprintf(`{"type": "thinking", "thinking": "plan %s", "signature": %q}`, sig, sig)
	return bcMaps(t, fmt.Sprintf(`[{"role": "user", "content": %q},
		{"role": "assistant", "content": "", "tool_calls": [{"id": %q, "type": "function", "function": {"name": "read_file", "arguments": "{\"path\": \"a\"}"}}],
		 "reasoning_details": [%s], "anthropic_content_blocks": [%s, {"type": "tool_use", "id": %q, "name": "read_file", "input": {"path": "RAW"}}]},
		{"role": "tool", "tool_call_id": %q, "content": "ok"}]`, question, toolID, signed, signed, toolID, toolID))
}

func bc2ConverseTurn(t *testing.T, question, sig, toolID string) []map[string]any {
	return bcMaps(t, fmt.Sprintf(`[{"role": "user", "content": %q},
		{"role": "assistant", "content": "", "tool_calls": [{"id": %q, "type": "function", "function": {"name": "read_file", "arguments": "{\"path\": \"a\"}"}}],
		 "reasoning_content": "own %s", "bedrock_content_blocks": [{"reasoningContent": {"text": "own %s"}},
		 {"toolUse": {"toolUseId": %q, "name": "read_file", "input": {"path": "a"}}}]},
		{"role": "tool", "tool_call_id": %q, "content": "ok"}]`, question, toolID, sig, sig, toolID, toolID))
}

func bc2AnthropicHistory(t *testing.T) []map[string]any {
	h := bc2AnthropicTurn(t, "Q1", "sig_prior", "t1")
	h = append(h, map[string]any{"role": "assistant", "content": "A1"})
	return append(h, bc2AnthropicTurn(t, "Q2", "sig_loop", "t2")...)
}

func bc2ConverseHistory(t *testing.T) []map[string]any {
	h := bc2ConverseTurn(t, "Q1", "sig_prior", "t1")
	blocks := h[1]["bedrock_content_blocks"].([]any)
	bcBlock(t, bcBlock(t, blocks[0])["reasoningContent"])["signature"] = "foreign"
	h = append(h, map[string]any{"role": "assistant", "content": "A1"})
	return append(h, bc2ConverseTurn(t, "Q2", "sig_loop", "t2")...)
}

func TestBedrockConverseReplaysEachModelsReasoningContract(t *testing.T) {
	cases := []struct{ model, prior, loop string }{
		{bc2Claude, `[{"text": "plan sig_prior", "signature": "sig_prior"}]`, `[{"text": "plan sig_loop", "signature": "sig_loop"}]`},
		{"us.deepseek.r1-v1:0", `[]`, `[{"text": "own sig_loop"}]`},
		{"global.moonshotai.kimi-k3", `[]`, `[{"text": "own sig_loop"}]`},
		{"openai.gpt-oss-120b-1:0", `[{"text": "own sig_prior"}]`, `[{"text": "own sig_loop"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			bcResetCachePoints(t)
			history := bc2ConverseHistory(t)
			if strings.Contains(tc.model, "claude") {
				history = bc2AnthropicHistory(t)
			}
			before := bcJSON(t, history)
			assistants := bc2Assistants(t, bc2Kwargs(t, tc.model, history, nil, true))
			bcSameJSON(t, bc2ReasoningTexts(t, assistants[0]), tc.prior)
			last := assistants[len(assistants)-1]
			bcSameJSON(t, bc2ReasoningTexts(t, last), tc.loop)
			var kinds []string
			for _, b := range last {
				for k := range bcBlock(t, b) {
					if k != "cachePoint" {
						kinds = append(kinds, k)
					}
				}
			}
			if !reflect.DeepEqual(kinds, []string{"reasoningContent", "toolUse"}) {
				t.Fatalf("loop block order %v", kinds)
			}
			bcSameJSON(t, bcBlock(t, bcBlock(t, last[1])["toolUse"])["input"], `{"path": "a"}`)
			if bcJSON(t, history) != before {
				t.Fatal("history mutated")
			}
			if !strings.Contains(tc.model, "claude") {
				switched := bc2Kwargs(t, tc.model, bc2AnthropicHistory(t), nil, false)
				if strings.Contains(bcJSON(t, switched["messages"]), "signature") {
					t.Fatalf("a Claude signature reached %s: %s", tc.model, bcJSON(t, switched["messages"]))
				}
			}
		})
	}
}

func TestBedrockClaudeOnConverseRequestsThinking(t *testing.T) {
	bcResetCachePoints(t)
	msgs := func() []map[string]any { return bcMaps(t, `[{"role": "user", "content": "hi"}]`) }
	adaptive := bc2Kwargs(t, bc2Claude, msgs(), nil, true)
	bcSameJSON(t, adaptive["additionalModelRequestFields"], `{"thinking": {"type": "adaptive", "display": "summarized"}, "output_config": {"effort": "high"}}`)
	manual := bc2Kwargs(t, "anthropic.claude-sonnet-4-5-20250929-v1:0", msgs(), bcInt(4096), true)
	bcSameJSON(t, manual["additionalModelRequestFields"].(map[string]any)["thinking"], `{"type": "enabled", "budget_tokens": 16000}`)
	inference := manual["inferenceConfig"].(map[string]any)
	if mt := bedrockUsageInt(inference["maxTokens"]); mt <= 16000 || bcJSON(t, inference["temperature"]) != "1" {
		t.Fatalf("manual inferenceConfig %s", bcJSON(t, inference))
	}
	nova := bc2Kwargs(t, "us.amazon.nova-2-lite-v1:0", msgs(), bcInt(4096), true)
	bcSameJSON(t, nova["additionalModelRequestFields"], `{"reasoningConfig": {"type": "enabled", "maxReasoningEffort": "high"}}`)
	if ic, _ := nova["inferenceConfig"].(map[string]any); ic != nil {
		if _, has := ic["maxTokens"]; has {
			t.Fatalf("nova high effort kept maxTokens %s", bcJSON(t, ic))
		}
	}
	if _, has := bc2Kwargs(t, "meta.llama3-70b-instruct-v1:0", msgs(), nil, true)["additionalModelRequestFields"]; has {
		t.Fatal("llama got a reasoning switch")
	}
	if _, has := bc2Kwargs(t, bc2Claude, msgs(), nil, false)["additionalModelRequestFields"]; has {
		t.Fatal("Claude without a reasoning config got a reasoning switch")
	}
}

func TestBedrockConverseReasoningSurvivesAReloadFromStateDB(t *testing.T) {
	for model, expect := range map[string]string{bc2Claude: `[{"text": "plan", "signature": "SIG"}]`, "openai.gpt-oss-120b-1:0": `[{"text": "plan"}]`} {
		t.Run(model, func(t *testing.T) {
			live := normalizeConverseResponse(bcMap(t, `{"output": {"message": {"content": [
				{"reasoningContent": {"reasoningText": {"text": "plan", "signature": "SIG"}}},
				{"toolUse": {"toolUseId": "t1", "name": "read_file", "input": {"path": "a"}}}]}}, "stopReason": "tool_use", "usage": {}}`))
			reloaded := map[string]any{"role": "assistant", "content": "", "reasoning_content": live.ReasoningContent,
				"reasoning_details": live.ReasoningDetails, "tool_calls": bcMaps(t, `[{"id": "t1", "type": "function", "function": {"name": "read_file", "arguments": "{}"}}]`)}
			history := []map[string]any{{"role": "user", "content": "Q1"}, reloaded, {"role": "tool", "tool_call_id": "t1", "content": "ok"},
				{"role": "assistant", "content": "A1"}, {"role": "user", "content": "Q2"}}
			_, msgs := convertMessagesToConverse(history, nil, model)
			bcSameJSON(t, bc2ReasoningTexts(t, bcBlock(t, msgs[1])["content"].([]any)), expect)
		})
	}
}

// Provider data of a normalized answer as the transport hands it to history, the live sidecar dropped on reload
func bc2ProviderData(t *testing.T, resp *Response, reloaded bool) map[string]any {
	t.Helper()
	d := bedrockMessageDicts([]*state.Message{resp.Message})[0]
	out := map[string]any{}
	for _, k := range []string{"reasoning_details", "bedrock_content_blocks"} {
		if v, has := d[k]; has && truthy(v) {
			out[k] = v
		}
	}
	if reloaded {
		if _, has := out["bedrock_content_blocks"]; !has {
			t.Fatalf("live answer carries no sidecar %s", bcJSON(t, d))
		}
		delete(out, "bedrock_content_blocks")
	}
	return out
}

func bc2BedrockTurn(t *testing.T, model, tag, toolID string, reloaded bool) map[string]any {
	raw := map[string]any{"output": map[string]any{"message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"reasoningContent": map[string]any{"reasoningText": map[string]any{"text": "think " + tag}}},
		map[string]any{"reasoningContent": map[string]any{"redactedContent": []byte("rsn_" + tag)}},
		map[string]any{"toolUse": map[string]any{"toolUseId": toolID, "name": "read_file", "input": map[string]any{"path": "a"}}},
	}}}, "stopReason": "tool_use", "usage": map[string]any{}}
	resp, err := bedrockTransportNormalize(raw, model)
	if err != nil {
		t.Fatal(err)
	}
	turn := map[string]any{"role": "assistant", "content": "", "reasoning_content": "think " + tag,
		"tool_calls": bcMaps(t, fmt.Sprintf(`[{"id": %q, "type": "function", "function": {"name": "read_file", "arguments": "{\"path\": \"a\"}"}}]`, toolID))}
	maps.Copy(turn, bc2ProviderData(t, resp, reloaded))
	return turn
}

func bc2Session(t *testing.T, issuer string, reloaded bool) []map[string]any {
	return []map[string]any{
		{"role": "user", "content": "Q1"}, bc2BedrockTurn(t, issuer, "prior", "call_1", reloaded),
		{"role": "tool", "tool_call_id": "call_1", "content": "ok"}, {"role": "assistant", "content": "A1"},
		{"role": "user", "content": "Q2"}, bc2BedrockTurn(t, issuer, "loop", "call_2", reloaded),
		{"role": "tool", "tool_call_id": "call_2", "content": "ok"},
	}
}

func bc2ReplayedReasoning(t *testing.T, kwargs map[string]any) [][]string {
	out := [][]string{}
	for _, content := range bc2Assistants(t, kwargs) {
		kinds, tooled := []string{}, false
		for _, b := range content {
			block := bcBlock(t, b)
			if _, has := block["toolUse"]; has {
				tooled = true
			}
			if rc, has := block["reasoningContent"]; has {
				for k := range bcBlock(t, rc) {
					kinds = append(kinds, k)
				}
			}
		}
		if tooled {
			out = append(out, kinds)
		}
	}
	return out
}

func TestBedrockCrossModelReplayDropsReasoningTheTargetCannotTake(t *testing.T) {
	both := [][]string{{"reasoningText", "redactedContent"}, {"reasoningText", "redactedContent"}}
	none := [][]string{{}, {}}
	cases := []struct {
		issuer, target string
		expect         [][]string
	}{{bc2GPT, bc2GPT, both}, {bc2GPT, bc2Opus, none}, {bc2GPT, "global.openai.gpt-6-sol", none}, {bc2Opus, bc2GPT, none}}
	for _, reloaded := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s>%s/reloaded=%v", tc.issuer, tc.target, reloaded), func(t *testing.T) {
				bcResetCachePoints(t)
				history := bc2Session(t, tc.issuer, reloaded)
				before := bcJSON(t, history)
				kwargs := bc2Kwargs(t, tc.target, history, nil, false)
				if got := bc2ReplayedReasoning(t, kwargs); !reflect.DeepEqual(got, tc.expect) {
					t.Fatalf("replayed %v want %v\n%s", got, tc.expect, bcJSON(t, kwargs["messages"]))
				}
				var ids []any
				for _, content := range bc2Assistants(t, kwargs) {
					for _, b := range content {
						if tu, has := bcBlock(t, b)["toolUse"]; has {
							ids = append(ids, bcBlock(t, tu)["toolUseId"])
						}
					}
				}
				bcSameJSON(t, ids, `["call_1", "call_2"]`)
				if bcJSON(t, history) != before {
					t.Fatal("history mutated")
				}
			})
		}
	}
}

func TestBedrockReasoningGPTOwnReasoningReplaysExactlyAcrossUserTurns(t *testing.T) {
	reasoning := func() []any {
		return []any{map[string]any{"reasoningContent": map[string]any{"reasoningText": map[string]any{"text": "exact thought", "signature": "exact-sig"}}},
			map[string]any{"reasoningContent": map[string]any{"redactedContent": []byte("\x00sealed\xff")}}}
	}
	for _, reloaded := range []bool{false, true} {
		t.Run(fmt.Sprint("reloaded=", reloaded), func(t *testing.T) {
			bcResetCachePoints(t)
			bc2Kwargs(t, bc2GPT, bcMaps(t, `[{"role": "user", "content": "Q"}]`), nil, false)
			raw := map[string]any{"output": map[string]any{"message": map[string]any{"role": "assistant",
				"content": append(reasoning(), map[string]any{"text": "A"})}}, "stopReason": "end_turn", "usage": map[string]any{}}
			resp, err := bedrockTransportNormalize(raw, bc2GPT)
			if err != nil {
				t.Fatal(err)
			}
			assistant := map[string]any{"role": "assistant", "content": "A"}
			maps.Copy(assistant, bc2ProviderData(t, resp, reloaded))
			history := []map[string]any{{"role": "user", "content": "Q"}, assistant}
			inLoop := bcBlock(t, bc2Kwargs(t, bc2GPT, history, nil, false)["messages"].([]any)[1])["content"].([]any)
			later := bc2Kwargs(t, bc2GPT, append(append([]map[string]any{}, history...), map[string]any{"role": "user", "content": "next"}), nil, false)
			bcSameJSON(t, bcBlock(t, later["messages"].([]any)[1])["content"], bcJSON(t, inLoop))
			var got []any
			for _, b := range inLoop {
				if _, has := bcBlock(t, b)["reasoningContent"]; has {
					got = append(got, b)
				}
			}
			bcSameJSON(t, got, bcJSON(t, reasoning()))
			if !bytes.Equal(bcRedacted(t, got[1]), []byte("\x00sealed\xff")) {
				t.Fatalf("sealed bytes changed %q", bcRedacted(t, got[1]))
			}
		})
	}
}

func TestBedrockClaudeMintedReasoningStillReplaysToClaude(t *testing.T) {
	bcResetCachePoints(t)
	kwargs := bc2Kwargs(t, bc2Opus, bc2Session(t, "global.anthropic.claude-sonnet-5-5", false), nil, false)
	want := [][]string{{"reasoningText", "redactedContent"}, {"reasoningText", "redactedContent"}}
	if got := bc2ReplayedReasoning(t, kwargs); !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed %v", got)
	}
}

func TestBedrockReasoningGPTReplaysNoUnstampedReasoningMidLoop(t *testing.T) {
	bcResetCachePoints(t)
	kwargs := bc2Kwargs(t, bc2GPT, bc2AnthropicHistory(t), nil, false)
	if strings.Contains(bcJSON(t, kwargs["messages"]), "reasoningContent") {
		t.Fatal(bcJSON(t, kwargs["messages"]))
	}
}

// Every text string a converted conversation sends, toolResult content included
func bc2TextBlocks(t *testing.T, msgs []any) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range bcBlock(t, m)["content"].([]any) {
			block := bcBlock(t, b)
			if text, has := block["text"]; has {
				out = append(out, text.(string))
			}
			if tr, has := block["toolResult"]; has {
				for _, tb := range bcBlock(t, tr)["content"].([]any) {
					if text, has := bcBlock(t, tb)["text"]; has {
						out = append(out, text.(string))
					}
				}
			}
		}
	}
	return out
}

func TestBedrockSafeTextPreservesRealContent(t *testing.T) {
	if bedrockSafeText("hello") != "hello" || bedrockSafeText("  padded  ") != "  padded  " {
		t.Fatal(bedrockSafeText("  padded  "))
	}
}

func TestBedrockRealContentIsPreservedAlongsideBlankSiblings(t *testing.T) {
	_, msgs := convertMessagesToConverse(bcMaps(t, `[{"role": "user", "content": [{"type": "text", "text": "  "}, {"type": "text", "text": "real question"}]}]`), nil)
	texts := bc2TextBlocks(t, msgs)
	found := false
	for _, text := range texts {
		found = found || text == "real question"
		if strings.TrimSpace(text) == "" {
			t.Fatalf("blank text block in %q", texts)
		}
	}
	if !found {
		t.Fatalf("real content lost %q", texts)
	}
}

func TestBedrockBlankSystemBlocksDroppedNotBlanked(t *testing.T) {
	system, _ := convertMessagesToConverse(bcMaps(t, `[{"role": "system", "content": [{"type": "text", "text": "keep me"}, {"type": "text", "text": "   "}]},
		{"role": "user", "content": "hi"}]`), nil)
	if system == nil {
		t.Fatal("system dropped")
	}
	kept := false
	for _, b := range system {
		text, _ := bcBlock(t, b)["text"].(string)
		if strings.TrimSpace(text) == "" {
			t.Fatalf("blank system block %s", bcJSON(t, system))
		}
		kept = kept || text == "keep me"
	}
	if !kept {
		t.Fatal(bcJSON(t, system))
	}
}

// One event stream frame with string headers, written the way the Python test encodes it
func bc2Frame(t *testing.T, kind string, payload any) []byte {
	var headers bytes.Buffer
	for _, kv := range [][2]string{{":message-type", "event"}, {":event-type", kind}, {":content-type", "application/json"}} {
		headers.WriteByte(byte(len(kv[0])))
		headers.WriteString(kv[0])
		headers.WriteByte(7)
		_ = binary.Write(&headers, binary.BigEndian, uint16(len(kv[1])))
		headers.WriteString(kv[1])
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	prelude := binary.BigEndian.AppendUint32(nil, uint32(16+headers.Len()+len(body)))
	prelude = binary.BigEndian.AppendUint32(prelude, uint32(headers.Len()))
	msg := binary.BigEndian.AppendUint32(prelude, crc32.ChecksumIEEE(prelude))
	msg = append(append(msg, headers.Bytes()...), body...)
	return binary.BigEndian.AppendUint32(msg, crc32.ChecksumIEEE(msg))
}

func TestBedrockWireRequiresMessageStop(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint("terminal=", terminal), func(t *testing.T) {
			bedrockIsolatedEnv(t)
			var body []byte
			body = append(body, bc2Frame(t, "messageStart", map[string]any{"role": "assistant"})...)
			body = append(body, bc2Frame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "partial answer"}})...)
			body = append(body, bc2Frame(t, "contentBlockStop", map[string]any{"contentBlockIndex": 0})...)
			if terminal {
				body = append(body, bc2Frame(t, "messageStop", map[string]any{"stopReason": "end_turn"})...)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				_, _ = w.Write(body)
			}))
			t.Cleanup(srv.Close)
			t.Setenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME", srv.URL)
			t.Setenv("AWS_ACCESS_KEY_ID", bedrockFakeAccessKey)
			t.Setenv("AWS_SECRET_ACCESS_KEY", bedrockFakeSecretKey)
			t.Setenv("AWS_MAX_ATTEMPTS", "1")
			kwargs := buildBedrockConverseKwargs(newBedrockConverseInput("amazon.nova-pro-v1:0", bcMaps(t, `[{"role": "user", "content": "hello"}]`)))
			resp, err := bedrockConverseSend(t.Context(), "us-east-1", kwargs, true)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			result, err := normalizeConverseStreamEvents(bedrockConverseEvents(resp.Body, resp.Header))
			if terminal {
				if err != nil || result.Content != "partial answer" || result.FinishReason != "stop" {
					t.Fatalf("%+v %v", result, err)
				}
				return
			}
			var empty *EmptyStreamError
			if !errors.As(err, &empty) || !regexp.MustCompile(`(?i)(incomplete|messageStop|truncat)`).MatchString(err.Error()) {
				t.Fatalf("want EmptyStreamError, got %+v %v", result, err)
			}
		})
	}
}

func TestBedrockExplicitInterruptKeepsPartialResponseContract(t *testing.T) {
	events := func() bedrockEventSource {
		return bcEvents(t, `[{"contentBlockDelta": {"delta": {"text": "partial"}}}, {"contentBlockStop": {"contentBlockIndex": 0}}]`)
	}
	interrupted := false
	result, flagged, err := streamConverseWithCallbacks(events(), bedrockStreamCallbacks{
		OnTextDelta:      func(string) error { interrupted = true; return nil },
		OnInterruptCheck: func() bool { return interrupted },
	})
	if err != nil || !flagged || result.Content != "partial" {
		t.Fatalf("%+v %v %v", result, flagged, err)
	}
	var empty *EmptyStreamError
	if result, _, err := streamConverseWithCallbacks(events(), bedrockStreamCallbacks{}); !errors.As(err, &empty) || result != nil {
		t.Fatalf("relay finalizer input recorded a response %+v %v", result, err)
	}
}

func TestBedrockOnEventFiresPerBedrockEvent(t *testing.T) {
	text := `[{"contentBlockDelta": {"delta": {"text": "a"}}},
		{"contentBlockStart": {"start": {"toolUse": {"toolUseId": "t1", "name": "x"}}}},
		{"contentBlockDelta": {"delta": {"toolUse": {"input": "{}"}}}},
		{"contentBlockStop": {}}, {"messageStop": {"stopReason": "end_turn"}},
		{"metadata": {"usage": {"inputTokens": 1, "outputTokens": 1}}}]`
	calls := 0
	if _, _, err := streamConverseWithCallbacks(bcEvents(t, text), bedrockStreamCallbacks{OnEvent: func() { calls++ }}); err != nil {
		t.Fatal(err)
	}
	if calls != len(bcMaps(t, text)) {
		t.Fatalf("on_event fired %d times", calls)
	}
}

func TestBedrockOnEventExceptionIsSwallowed(t *testing.T) {
	result, _, err := streamConverseWithCallbacks(bcEvents(t, `[{"messageStop": {"stopReason": "end_turn"}}]`),
		bedrockStreamCallbacks{OnEvent: func() { panic(errors.New("liveness hook blew up")) }})
	if err != nil || result == nil || result.FinishReason != "stop" {
		t.Fatalf("%+v %v", result, err)
	}
}

func bc2Event(kind string, payload map[string]any) map[string]any {
	return map[string]any{kind: payload}
}

func bc2StreamReplay() []map[string]any {
	var events []map[string]any
	events = append(events, bc2Event("messageStart", map[string]any{"role": "assistant"}))
	for i, tool := range []struct{ id, name, input, sealed string }{{"t1", "one", `{"n":1}`, "r1"}, {"t2", "two", `{"n":2}`, "r2"}} {
		events = append(events,
			bc2Event("contentBlockStart", map[string]any{"contentBlockIndex": 2 * i, "start": map[string]any{}}),
			bc2Event("contentBlockDelta", map[string]any{"contentBlockIndex": 2 * i, "delta": map[string]any{"reasoningContent": map[string]any{"redactedContent": []byte(tool.sealed)}}}),
			bc2Event("contentBlockStop", map[string]any{"contentBlockIndex": 2 * i}),
			bc2Event("contentBlockStart", map[string]any{"contentBlockIndex": 2*i + 1, "start": map[string]any{"toolUse": map[string]any{"toolUseId": tool.id, "name": tool.name}}}),
			bc2Event("contentBlockDelta", map[string]any{"contentBlockIndex": 2*i + 1, "delta": map[string]any{"toolUse": map[string]any{"input": tool.input}}}),
			bc2Event("contentBlockStop", map[string]any{"contentBlockIndex": 2*i + 1}))
	}
	return append(events, bc2Event("messageStop", map[string]any{"stopReason": "tool_use"}))
}

func TestBedrockTransportPreservesReasoningAndOrder(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint("streaming=", streaming), func(t *testing.T) {
			var adapter *bedrockConverseResult
			var err error
			if streaming {
				adapter, err = normalizeConverseStreamEvents(bedrockEventList(bc2StreamReplay()))
			} else {
				adapter = normalizeConverseResponse(map[string]any{"output": map[string]any{"message": map[string]any{"role": "assistant", "content": []any{
					map[string]any{"reasoningContent": map[string]any{"redactedContent": []byte("r1")}},
					map[string]any{"toolUse": map[string]any{"toolUseId": "t1", "name": "one", "input": map[string]any{"n": 1}}},
					map[string]any{"reasoningContent": map[string]any{"redactedContent": []byte("r2")}},
					map[string]any{"toolUse": map[string]any{"toolUseId": "t2", "name": "two", "input": map[string]any{"n": 2}}},
				}}}, "stopReason": "tool_use"})
			}
			if err != nil {
				t.Fatal(err)
			}
			normalized, err := bedrockTransportNormalize(adapter, "")
			if err != nil {
				t.Fatal(err)
			}
			data := bc2ProviderData(t, normalized, false)
			bcSameJSON(t, data["reasoning_details"], `[{"type": "redacted_thinking", "data": "cjE="}, {"type": "redacted_thinking", "data": "cjI="}]`)
			dicts := bedrockMessageDicts([]*state.Message{{Role: "user", Content: "go"}, normalized.Message})
			bcSameJSON(t, dicts[1]["reasoning_details"], bcJSON(t, data["reasoning_details"]))
			bcSameJSON(t, dicts[1]["bedrock_content_blocks"], bcJSON(t, data["bedrock_content_blocks"]))
			_, messages := convertMessagesToConverse(dicts, nil)
			var kinds []string
			var sealed []string
			for _, b := range bcBlock(t, messages[1])["content"].([]any) {
				for k := range bcBlock(t, b) {
					kinds = append(kinds, k)
				}
				if _, has := bcBlock(t, b)["reasoningContent"]; has {
					sealed = append(sealed, string(bcRedacted(t, b)))
				}
			}
			if !reflect.DeepEqual(kinds, []string{"reasoningContent", "toolUse", "reasoningContent", "toolUse"}) || !reflect.DeepEqual(sealed, []string{"r1", "r2"}) {
				t.Fatalf("%v %v", kinds, sealed)
			}
		})
	}
}

func TestBedrockTransportBuildKwargs(t *testing.T) {
	bedrockIsolatedEnv(t)
	bcResetCachePoints(t)
	model := "anthropic.claude-3-5-sonnet-20241022-v2:0"
	prepare := func(route Route, maxTokens int) *bedrockConversePrepared {
		client, err := NewClient(route)
		if err != nil {
			t.Fatal(err)
		}
		converse, ok := client.(*bedrockConverseClient)
		if !ok {
			t.Fatalf("bedrock_converse mode dispatched to %T", client)
		}
		p, err := converse.prepare(t.Context(), &Request{Messages: []*state.Message{{Role: "user", Content: "Hello"}}, MaxTokens: maxTokens})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	basic := prepare(Route{Provider: "bedrock", Model: model, Mode: modeBedrockConverse, APIKey: "aws-sdk"}, 0)
	if basic.kwargs["modelId"] != model || basic.region != "us-east-1" || basic.kwargs["messages"] == nil {
		t.Fatalf("%s %s", basic.region, bcJSON(t, basic.kwargs))
	}
	if p := prepare(Route{Provider: "bedrock", Model: model, Mode: modeBedrockConverse, Region: "eu-west-1", APIKey: "aws-sdk"}, 0); p.region != "eu-west-1" {
		t.Fatal(p.region)
	}
	p := prepare(Route{Provider: "bedrock", Model: model, Mode: modeBedrockConverse, APIKey: "aws-sdk"}, 8192)
	if bedrockUsageInt(p.kwargs["inferenceConfig"].(map[string]any)["maxTokens"]) != 8192 {
		t.Fatal(bcJSON(t, p.kwargs["inferenceConfig"]))
	}
}

func TestBedrockTransportConvertValidateFinishAndNormalize(t *testing.T) {
	tools := convertToolsToConverse(bcMaps(t, `[{"type": "function", "function": {"name": "terminal", "description": "Run commands",
		"parameters": {"type": "object", "properties": {"command": {"type": "string"}}}}}]`))
	if len(tools) != 1 || bcBlock(t, bcBlock(t, tools[0])["toolSpec"])["name"] != "terminal" {
		t.Fatal(bcJSON(t, tools))
	}
	if validateBedrockConverseResponse(nil) || !validateBedrockConverseResponse(&bedrockConverseResult{Content: "hi"}) ||
		!validateBedrockConverseResponse(map[string]any{"output": map[string]any{}}) || validateBedrockConverseResponse(map[string]any{}) {
		t.Fatal("validate_response parity")
	}
	if bedrockTransportFinishReason("end_turn") != "stop" {
		t.Fatal(bedrockTransportFinishReason("end_turn"))
	}
	nr, err := bedrockTransportNormalize(bcMap(t, `{"output": {"message": {"role": "assistant", "content": [{"toolUse": {"toolUseId": "tool_1", "name": "terminal",
		"input": {"command": "ls"}}}]}}, "stopReason": "tool_use", "usage": {"inputTokens": 10, "outputTokens": 5, "totalTokens": 15}}`), "")
	if err != nil || nr.FinishReason != "tool_calls" || len(nr.Message.ToolCalls) != 1 || nr.Message.ToolCalls[0].Function.Name != "terminal" {
		t.Fatalf("%+v %v", nr, err)
	}
	pre := &bedrockConverseResult{Content: "Hello from Bedrock", FinishReason: "stop", Usage: bedrockConverseUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}
	nr, err = bedrockTransportNormalize(pre, "")
	if err != nil || nr.Message.Text() != "Hello from Bedrock" || nr.FinishReason != "stop" || nr.Usage.InputTokens != 10 {
		t.Fatalf("%+v %v", nr, err)
	}
}
