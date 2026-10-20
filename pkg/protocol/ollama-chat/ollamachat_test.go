package ollamachat

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
)

var p = Protocol{}

func body(t *testing.T, call protocol.Call) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(call.Body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Mirrors the docs example "Chat request (With history, with tools)" plus a system prompt and an image
func TestEncodeRequest(t *testing.T) {
	temp := 0.1
	req := protocol.Request{
		Model:  "llama3.2",
		System: "Be brief",
		Messages: []protocol.Message{
			{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "what is the weather in Toronto?"}, protocol.Image{MediaType: "image/png", Data: []byte{1, 2, 3}}}},
			{Role: protocol.RoleAssistant, Content: []protocol.Part{
				protocol.Thinking{Text: "need the weather"},
				protocol.Thinking{Redacted: true, Signature: "opaque"},
				protocol.ToolCall{ID: "call_1", Name: "get_weather", Args: json.RawMessage(`{"city": "Toronto"}`)},
			}},
			{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "call_1", Name: "get_weather", Content: []protocol.Part{protocol.Text{Text: "11 degrees celsius"}}}}},
		},
		Tools:         []protocol.Tool{{Name: "get_weather", Description: "Get the weather in a given city", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}},
		ToolChoice:    protocol.ToolChoice{Mode: protocol.ToolChoiceAuto},
		MaxTokens:     64,
		Temperature:   &temp,
		StopSequences: []string{"END"},
		Stream:        true,
		Extra:         map[string]json.RawMessage{"think": json.RawMessage(`true`), "options": json.RawMessage(`{"seed":42}`)},
	}
	call, err := p.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	if call.Method != "POST" || call.Path != "/api/chat" || call.Header.Get("Accept") != "application/x-ndjson" || call.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%+v", call)
	}
	m := body(t, call)
	msgs := m["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages %v", msgs)
	}
	if sys := msgs[0].(map[string]any); sys["role"] != "system" || sys["content"] != "Be brief" {
		t.Fatalf("system %v", sys)
	}
	user := msgs[1].(map[string]any)
	if user["content"] != "what is the weather in Toronto?" || user["images"].([]any)[0] != "AQID" {
		t.Fatalf("user %v", user)
	}
	asst := msgs[2].(map[string]any)
	if asst["thinking"] != "need the weather" || asst["content"] != "" {
		t.Fatalf("assistant %v", asst)
	}
	tc := asst["tool_calls"].([]any)[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if tc["id"] != "call_1" || fn["name"] != "get_weather" || fn["arguments"].(map[string]any)["city"] != "Toronto" {
		t.Fatalf("tool call %v", tc)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["content"] != "11 degrees celsius" || tool["tool_name"] != "get_weather" || tool["tool_call_id"] != "call_1" {
		t.Fatalf("tool message %v", tool)
	}
	def := m["tools"].([]any)[0].(map[string]any)
	if def["type"] != "function" || def["function"].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("tool %v", def)
	}
	opts := m["options"].(map[string]any)
	if opts["num_predict"] != 64.0 || opts["temperature"] != 0.1 || opts["stop"].([]any)[0] != "END" || opts["seed"] != 42.0 {
		t.Fatalf("options %v", opts)
	}
	if m["stream"] != true || m["think"] != true {
		t.Fatalf("flags %v", m)
	}
	if _, ok := m["tool_choice"]; ok {
		t.Fatal("tool_choice must not be sent")
	}
}

func TestEncodeMinimal(t *testing.T) {
	call, err := p.Encode(protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(call.Body) != `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}` {
		t.Fatalf("%s", call.Body)
	}
	if call.Header.Get("Accept") != "application/json" {
		t.Fatal(call.Header)
	}
}

func TestEncodeErrors(t *testing.T) {
	user := func(parts ...protocol.Part) []protocol.Message {
		return []protocol.Message{{Role: protocol.RoleUser, Content: parts}}
	}
	cases := map[string]protocol.Request{
		"no model":         {Messages: user(protocol.Text{Text: "x"})},
		"empty message":    {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleUser}}},
		"unknown role":     {Model: "m", Messages: []protocol.Message{{Role: "ghost", Content: []protocol.Part{protocol.Text{Text: "x"}}}}},
		"result in user":   {Model: "m", Messages: user(protocol.ToolResult{CallID: "c"})},
		"thinking in user": {Model: "m", Messages: user(protocol.Thinking{Text: "t"})},
		"image assistant":  {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Image{Data: []byte{1}}}}}},
		"bad args":         {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{Name: "f", Args: json.RawMessage(`[1]`)}}}}},
		"text in tool":     {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.Text{Text: "x"}}}}},
		"unlinked result":  {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{Content: []protocol.Part{protocol.Text{Text: "x"}}}}}}},
		"thinking result":  {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "c", Content: []protocol.Part{protocol.Thinking{}}}}}}},
		"tool no schema":   {Model: "m", Tools: []protocol.Tool{{Name: "f"}}, Messages: user(protocol.Text{Text: "x"})},
		"choice none":      {Model: "m", ToolChoice: protocol.ToolChoice{Mode: protocol.ToolChoiceNone}, Messages: user(protocol.Text{Text: "x"})},
		"choice required":  {Model: "m", ToolChoice: protocol.ToolChoice{Mode: protocol.ToolChoiceRequired}, Messages: user(protocol.Text{Text: "x"})},
		"choice tool":      {Model: "m", ToolChoice: protocol.ToolChoice{Mode: protocol.ToolChoiceTool, Name: "f"}, Messages: user(protocol.Text{Text: "x"})},
		"choice unknown":   {Model: "m", ToolChoice: protocol.ToolChoice{Mode: "sometimes"}, Messages: user(protocol.Text{Text: "x"})},
		"bad extra":        {Model: "m", Extra: map[string]json.RawMessage{"x": json.RawMessage(`{`)}, Messages: user(protocol.Text{Text: "x"})},
	}
	for name, req := range cases {
		if _, err := p.Encode(req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Docs example "Chat request (With history, with tools)" response, verbatim
const historyResponse = `{
  "model": "llama3.2",
  "created_at": "2025-07-07T20:43:37.688511Z",
  "message": {
    "role": "assistant",
    "content": "The current temperature in Toronto is 11°C."
  },
  "done_reason": "stop",
  "done": true,
  "total_duration": 890771750,
  "load_duration": 707634750,
  "prompt_eval_count": 94,
  "prompt_eval_duration": 91703208,
  "eval_count": 11,
  "eval_duration": 90282125
}`

// ASSEMBLED from the streaming with tools chunk and the final line of the same docs example
const toolResponse = `{"model":"llama3.2","created_at":"2025-07-07T20:22:19.19314Z","message":{"role":"assistant","content":"","thinking":"Tokyo it is","tool_calls":[{"id":"call_abc","function":{"name":"get_weather","arguments":{"city": "Tokyo"}}}]},"done_reason":"stop","done":true,"prompt_eval_count":169,"eval_count":15,"prompt_eval_cached_count":100}`

func TestDecode(t *testing.T) {
	resp, err := p.Decode(strings.NewReader(historyResponse))
	if err != nil {
		t.Fatal(err)
	}
	want := protocol.Response{Model: "llama3.2", Content: []protocol.Part{protocol.Text{Text: "The current temperature in Toronto is 11°C."}}, StopReason: protocol.StopEnd, RawStopReason: "stop", Usage: protocol.Usage{Input: 94, Output: 11}}
	if !reflect.DeepEqual(resp, want) {
		t.Fatalf("%+v", resp)
	}
	resp, err = p.Decode(strings.NewReader(toolResponse))
	if err != nil {
		t.Fatal(err)
	}
	wantParts := []protocol.Part{
		protocol.Thinking{Text: "Tokyo it is"},
		protocol.ToolCall{ID: "call_abc", Name: "get_weather", Args: json.RawMessage(`{"city":"Tokyo"}`)},
	}
	if !reflect.DeepEqual(resp.Content, wantParts) || resp.StopReason != protocol.StopToolUse || resp.Usage != (protocol.Usage{Input: 169, Output: 15, CacheRead: 100}) {
		t.Fatalf("%+v", resp)
	}
	for name, in := range map[string]string{
		"not done":   `{"model":"m","message":{"role":"assistant","content":"x"},"done":false}`,
		"bad args":   `{"model":"m","message":{"role":"assistant","tool_calls":[{"function":{"name":"f","arguments":[1]}}]},"done":true}`,
		"not json":   `nope`,
		"wire error": `{"error":"model 'x' not found"}`,
	} {
		if _, err := p.Decode(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err = p.Decode(strings.NewReader(`{"error":"model 'x' not found"}`))
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Message != "model 'x' not found" {
		t.Fatalf("%v", err)
	}
}

func TestDecodeLengthAndLoadReasons(t *testing.T) {
	for raw, want := range map[string]protocol.StopReason{"length": protocol.StopLength, "load": protocol.StopOther, "": protocol.StopEnd} {
		resp, err := p.Decode(strings.NewReader(`{"model":"m","message":{"role":"assistant","content":"x"},"done":true,"done_reason":"` + raw + `"}`))
		if err != nil || resp.StopReason != want || resp.RawStopReason != raw {
			t.Fatalf("%q: %+v %v", raw, resp, err)
		}
	}
}

// Docs example "Chat request (Streaming)", both verbatim lines compacted onto one line each
const textStream = `{"model":"llama3.2","created_at":"2023-08-04T08:52:19.385406455-07:00","message":{"role":"assistant","content":"The","images":null},"done":false}
{"model":"llama3.2","created_at":"2023-08-04T19:22:45.499127Z","message":{"role":"assistant","content":""},"done":true,"total_duration":4883583458,"load_duration":1334875,"prompt_eval_count":26,"prompt_eval_duration":342546000,"eval_count":282,"eval_duration":4535599000}
`

// Docs example "Chat request (Streaming with tools)", verbatim, compacted onto one line each
const toolStream = `{"model":"llama3.2","created_at":"2025-07-07T20:22:19.184789Z","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Tokyo"}}}]},"done":false}
{"model":"llama3.2","created_at":"2025-07-07T20:22:19.19314Z","message":{"role":"assistant","content":""},"done_reason":"stop","done":true,"total_duration":182242375,"load_duration":41295167,"prompt_eval_count":169,"prompt_eval_duration":24573166,"eval_count":15,"eval_duration":115959084}
`

// ASSEMBLED from the Message schema, thinking fragments followed by content fragments
const thinkingStream = `{"model":"m","message":{"role":"assistant","content":"","thinking":"Let me"},"done":false}
{"model":"m","message":{"role":"assistant","content":"","thinking":" think"},"done":false}

{"model":"m","message":{"role":"assistant","content":"Sky"},"done":false}
{"model":"m","message":{"role":"assistant","content":" blue"},"done":false}
{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":3,"eval_count":4}
`

func collect(t *testing.T, in string) ([]protocol.EventType, protocol.Response) {
	t.Helper()
	var types []protocol.EventType
	var final protocol.Response
	for ev, err := range p.Stream(strings.NewReader(in)) {
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, ev.Type)
		if ev.Type == protocol.EventDone {
			final = ev.Response
		}
	}
	return types, final
}

func TestStreamText(t *testing.T) {
	types, final := collect(t, textStream)
	want := []protocol.EventType{protocol.EventStart, protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextEnd, protocol.EventDone}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("%v", types)
	}
	if final.Model != "llama3.2" || final.StopReason != protocol.StopEnd || final.RawStopReason != "" || final.Usage != (protocol.Usage{Input: 26, Output: 282}) {
		t.Fatalf("%+v", final)
	}
	if !reflect.DeepEqual(final.Content, []protocol.Part{protocol.Text{Text: "The"}}) {
		t.Fatalf("%+v", final.Content)
	}
}

func TestStreamToolCall(t *testing.T) {
	types, final := collect(t, toolStream)
	want := []protocol.EventType{protocol.EventStart, protocol.EventToolCallStart, protocol.EventToolCallDelta, protocol.EventToolCallEnd, protocol.EventDone}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("%v", types)
	}
	wantParts := []protocol.Part{protocol.ToolCall{Name: "get_weather", Args: json.RawMessage(`{"city":"Tokyo"}`)}}
	if !reflect.DeepEqual(final.Content, wantParts) || final.StopReason != protocol.StopToolUse || final.Usage != (protocol.Usage{Input: 169, Output: 15}) {
		t.Fatalf("%+v", final)
	}
}

func TestStreamThinkingThenText(t *testing.T) {
	types, final := collect(t, thinkingStream)
	want := []protocol.EventType{
		protocol.EventStart,
		protocol.EventThinkingStart, protocol.EventThinkingDelta, protocol.EventThinkingDelta, protocol.EventThinkingEnd,
		protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextDelta, protocol.EventTextEnd,
		protocol.EventDone,
	}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("%v", types)
	}
	wantParts := []protocol.Part{protocol.Thinking{Text: "Let me think"}, protocol.Text{Text: "Sky blue"}}
	if !reflect.DeepEqual(final.Content, wantParts) || final.Usage != (protocol.Usage{Input: 3, Output: 4}) {
		t.Fatalf("%+v", final)
	}
	collected, err := protocol.Collect(p.Stream(strings.NewReader(thinkingStream)))
	if err != nil || !reflect.DeepEqual(collected, final) {
		t.Fatalf("%v %+v", err, collected)
	}
}

func TestStreamFailures(t *testing.T) {
	truncated := textStream[:strings.Index(textStream, "\n")+1]
	if _, err := protocol.Collect(p.Stream(strings.NewReader(truncated))); err == nil || !strings.Contains(err.Error(), "before a done line") {
		t.Fatalf("truncated: %v", err)
	}
	_, err := protocol.Collect(p.Stream(strings.NewReader(`{"model":"m","message":{"role":"assistant","content":"a"},"done":false}` + "\n" + `{"error":"runner exited"}` + "\n")))
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Status != 200 || perr.Message != "runner exited" {
		t.Fatalf("error line: %v", err)
	}
	for name, in := range map[string]string{
		"not json": "nope\n",
		"bad args": `{"model":"m","message":{"role":"assistant","tool_calls":[{"function":{"name":"f","arguments":"x"}}]},"done":false}` + "\n",
	} {
		if _, err := protocol.Collect(p.Stream(strings.NewReader(in))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDecodeError(t *testing.T) {
	e := p.DecodeError(404, []byte(`{"error":"model 'x' not found"}`))
	if e.Status != 404 || e.Type != "" || e.Message != "model 'x' not found" {
		t.Fatalf("%+v", e)
	}
	e = p.DecodeError(502, []byte(" bad gateway \n"))
	if e.Status != 502 || e.Message != "bad gateway" || string(e.Body) != " bad gateway \n" {
		t.Fatalf("%+v", e)
	}
}

func TestRegistered(t *testing.T) {
	got, ok := protocol.Lookup(protocol.OllamaChat)
	if !ok || got.ID() != "ollama-chat" {
		t.Fatal("not registered")
	}
}
