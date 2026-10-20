package openaicompletions

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
)

const weatherSchema = `{"type":"object","properties":{"location":{"type":"string","description":"The city and state, e.g. San Francisco, CA"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["location"]}`

var temp = 0.5

// Spec 6.1 request with tools extended by an assistant tool call turn and its tool result
func sampleRequest(stream bool) protocol.Request {
	return protocol.Request{
		Model:  "gpt-6-astra",
		System: "You are a helpful assistant.",
		Messages: []protocol.Message{
			{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "What is the weather like in Boston today?"}}},
			{Role: protocol.RoleAssistant, Content: []protocol.Part{
				protocol.Thinking{Text: "replayed", Signature: "sig"},
				protocol.ToolCall{ID: "call_abc123", Name: "get_current_weather", Args: json.RawMessage(`{"location":"Boston, MA"}`)},
			}},
			{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "call_abc123", Name: "get_current_weather", Content: []protocol.Part{protocol.Text{Text: "72F and sunny"}}}}},
			{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "Thanks, here is a photo"}, protocol.Image{MediaType: "image/png", Data: []byte{1, 2, 3}}}},
		},
		Tools:         []protocol.Tool{{Name: "get_current_weather", Description: "Get the current weather in a given location", Parameters: json.RawMessage(weatherSchema)}},
		ToolChoice:    protocol.ToolChoice{Mode: protocol.ToolChoiceAuto},
		MaxTokens:     256,
		Temperature:   &temp,
		StopSequences: []string{"END"},
		Stream:        stream,
	}
}

func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return m
}

func encodeMessages(t *testing.T, msgs ...protocol.Message) []any {
	t.Helper()
	call, err := (Protocol{}).Encode(protocol.Request{Model: "m", Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	return decodeBody(t, call.Body)["messages"].([]any)
}

func TestCapabilities(t *testing.T) {
	want := protocol.Capabilities{Thinking: true, Images: true, ToolChoiceNone: true, ToolChoiceNamed: true, StopSequences: true}
	if got := (Protocol{}).Capabilities(); got != want {
		t.Fatalf("%+v", got)
	}
}

func TestEncodeRequest(t *testing.T) {
	call, err := (Protocol{}).Encode(sampleRequest(true))
	if err != nil {
		t.Fatal(err)
	}
	if call.Method != "POST" || call.Path != "/chat/completions" || call.Header.Get("Content-Type") != "application/json" || call.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("%+v", call)
	}
	m := decodeBody(t, call.Body)
	if m["model"] != "gpt-6-astra" || m["max_completion_tokens"] != float64(256) || m["temperature"] != 0.5 || m["stream"] != true || m["tool_choice"] != "auto" {
		t.Fatalf("%v", m)
	}
	if m["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatal("stream_options")
	}
	if m["stop"].([]any)[0] != "END" {
		t.Fatal("stop")
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages %d", len(msgs))
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "You are a helpful assistant." {
		t.Fatalf("%v", sys)
	}
	if u := msgs[1].(map[string]any); u["role"] != "user" || u["content"] != "What is the weather like in Boston today?" {
		t.Fatalf("%v", u)
	}
	asst := msgs[2].(map[string]any)
	if _, has := asst["content"]; has || asst["role"] != "assistant" || asst["reasoning_content"] != "replayed" {
		t.Fatalf("%v", asst)
	}
	tc := asst["tool_calls"].([]any)[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if tc["id"] != "call_abc123" || tc["type"] != "function" || fn["name"] != "get_current_weather" || fn["arguments"] != `{"location":"Boston, MA"}` {
		t.Fatalf("%v", tc)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_abc123" || tool["content"] != "72F and sunny" {
		t.Fatalf("%v", tool)
	}
	parts := msgs[4].(map[string]any)["content"].([]any)
	img := parts[1].(map[string]any)["image_url"].(map[string]any)
	if parts[0].(map[string]any)["text"] != "Thanks, here is a photo" || img["url"] != "data:image/png;base64,AQID" {
		t.Fatalf("%v", parts)
	}
	def := m["tools"].([]any)[0].(map[string]any)
	if def["type"] != "function" || def["function"].(map[string]any)["name"] != "get_current_weather" || def["function"].(map[string]any)["parameters"].(map[string]any)["type"] != "object" {
		t.Fatalf("%v", def)
	}
	call, err = (Protocol{}).Encode(sampleRequest(false))
	if err != nil {
		t.Fatal(err)
	}
	m = decodeBody(t, call.Body)
	if _, has := m["stream"]; has || m["stream_options"] != nil || call.Header.Get("Accept") != "application/json" {
		t.Fatalf("%v", m)
	}
}

func TestEncodeToolChoiceAndExtra(t *testing.T) {
	req := sampleRequest(false)
	req.ToolChoice = protocol.ToolChoice{Mode: protocol.ToolChoiceTool, Name: "get_current_weather"}
	req.Extra = map[string]json.RawMessage{"reasoning_effort": json.RawMessage(`"high"`), "stream_options": json.RawMessage(`{"include_obfuscation":false}`)}
	call, err := (Protocol{}).Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeBody(t, call.Body)
	tc := m["tool_choice"].(map[string]any)
	if tc["type"] != "function" || tc["function"].(map[string]any)["name"] != "get_current_weather" {
		t.Fatalf("%v", tc)
	}
	if m["reasoning_effort"] != "high" || m["stream_options"].(map[string]any)["include_obfuscation"] != false {
		t.Fatalf("%v", m)
	}
	for _, mode := range []protocol.ToolChoiceMode{protocol.ToolChoiceNone, protocol.ToolChoiceRequired} {
		req.ToolChoice = protocol.ToolChoice{Mode: mode}
		call, err := (Protocol{}).Encode(req)
		if err != nil {
			t.Fatal(err)
		}
		if decodeBody(t, call.Body)["tool_choice"] != string(mode) {
			t.Fatal(mode)
		}
	}
	req.ToolChoice = protocol.ToolChoice{}
	call, _ = (Protocol{}).Encode(req)
	if _, has := decodeBody(t, call.Body)["tool_choice"]; has {
		t.Fatal("zero tool choice must be omitted")
	}
}

// Rule 3 replay policy for signed parts
func TestEncodeOriginPolicy(t *testing.T) {
	foreign := protocol.AnthropicMessages
	msgs := encodeMessages(t, protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{
		protocol.Thinking{Text: "foreign thought", Signature: "foreign-sig", Origin: foreign},
		protocol.Thinking{Redacted: true, Signature: "redacted-sig"},
		protocol.Thinking{Text: "same", Origin: protocol.OpenAICompletions},
		protocol.Thinking{Text: "empty"},
		protocol.ToolCall{ID: "c1", Name: "f", Args: json.RawMessage(`{"a": 1}`), Signature: "tool-sig", Origin: foreign},
		protocol.ServerToolCall{ID: "s1", Name: "web_search", Args: json.RawMessage(`{"q":"x"}`), Origin: foreign},
		protocol.ServerToolResult{CallID: "s1", Name: "web_search_tool_result", Content: json.RawMessage(`[]`), Origin: foreign},
		protocol.Text{Text: "answer"},
	}})
	raw, _ := json.Marshal(msgs)
	for _, leaked := range []string{"foreign thought", "foreign-sig", "redacted-sig", "tool-sig", "web_search"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("leaked %q in %s", leaked, raw)
		}
	}
	asst := msgs[0].(map[string]any)
	if asst["reasoning_content"] != "same\nempty" || asst["content"] != "answer" {
		t.Fatalf("%v", asst)
	}
	if fn := asst["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any); fn["arguments"] != `{"a": 1}` {
		t.Fatalf("%v", fn)
	}
	msgs = encodeMessages(t, protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Thinking{Text: "gone", Origin: foreign}}})
	if asst := msgs[0].(map[string]any); asst["content"] != "" || asst["reasoning_content"] != nil {
		t.Fatalf("empty assistant turn %v", asst)
	}
}

func TestEncodeToolResultErrorAndImage(t *testing.T) {
	msgs := encodeMessages(t, protocol.Message{Role: protocol.RoleTool, Content: []protocol.Part{
		protocol.ToolResult{CallID: "c1", IsError: true, Content: []protocol.Part{protocol.Text{Text: "boom"}}},
		protocol.ToolResult{CallID: "c2", Content: []protocol.Part{protocol.Image{MediaType: "image/png", Data: []byte{1, 2, 3}}}},
		protocol.ToolResult{CallID: "c3", IsError: true},
		protocol.ToolResult{CallID: "c4", IsError: true, Content: []protocol.Part{protocol.Text{Text: "a"}, protocol.Text{Text: "b"}}},
	}})
	if msgs[0].(map[string]any)["content"] != "[error] boom" {
		t.Fatalf("%v", msgs[0])
	}
	if msgs[1].(map[string]any)["content"] != "[image image/png, 3 bytes omitted]" {
		t.Fatalf("%v", msgs[1])
	}
	if msgs[2].(map[string]any)["content"] != "[error] " {
		t.Fatalf("%v", msgs[2])
	}
	parts := msgs[3].(map[string]any)["content"].([]any)
	if parts[0].(map[string]any)["text"] != "[error] a" || parts[1].(map[string]any)["text"] != "b" {
		t.Fatalf("%v", parts)
	}
}

func TestEncodeErrors(t *testing.T) {
	text := protocol.Text{Text: "x"}
	result := protocol.ToolResult{CallID: "c", Content: []protocol.Part{text}}
	cases := map[string]protocol.Request{
		"document":             {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Document{MediaType: "application/pdf", Data: []byte{1}}}}}},
		"image without type":   {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{text, protocol.Image{Data: []byte{1}}}}}},
		"image without data":   {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{text, protocol.Image{MediaType: "image/png"}}}}},
		"thinking in result":   {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "c", Content: []protocol.Part{protocol.Thinking{}}}}}}},
		"tool result in user":  {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{result}}}},
		"text in tool message": {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{text}}}},
		"image in assistant":   {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Image{}}}}},
		"bad args":             {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{ID: "c", Name: "f", Args: json.RawMessage(`[1]`)}}}}},
		"null args":            {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{ID: "c", Name: "f", Args: json.RawMessage(`null`)}}}}},
		"empty message":        {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleUser}}},
		"unknown role":         {Model: "m", Messages: []protocol.Message{{Role: "system", Content: []protocol.Part{text}}}},
		"tool without schema":  {Model: "m", Tools: []protocol.Tool{{Name: "f"}}},
		"nameless tool choice": {Model: "m", ToolChoice: protocol.ToolChoice{Mode: protocol.ToolChoiceTool}},
		"unknown tool choice":  {Model: "m", ToolChoice: protocol.ToolChoice{Mode: "sometimes"}},
		"invalid extra":        {Model: "m", Extra: map[string]json.RawMessage{"x": json.RawMessage(`{`)}},
	}
	for name, req := range cases {
		if _, err := (Protocol{}).Encode(req); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// Spec 6.2 non streaming response containing tool_calls
const toolCallsResponse = `{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "created": 1699896916,
  "model": "gpt-6-astra",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": null,
        "tool_calls": [
          {
            "id": "call_abc123",
            "type": "function",
            "function": {
              "name": "get_current_weather",
              "arguments": "{\n\"location\": \"Boston, MA\"\n}"
            }
          }
        ]
      },
      "logprobs": null,
      "finish_reason": "tool_calls"
    }
  ],
  "usage": {
    "prompt_tokens": 82,
    "completion_tokens": 17,
    "total_tokens": 99,
    "completion_tokens_details": {
      "reasoning_tokens": 0,
      "accepted_prediction_tokens": 0,
      "rejected_prediction_tokens": 0
    }
  }
}`

// Spec 6.3 non streaming text response
const textResponse = `{
  "id": "chatcmpl-B9MBs8CjcvOU2jLn4n570S5qMJKcT",
  "object": "chat.completion",
  "created": 1741569952,
  "model": "gpt-6-astra",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Hello! How can I assist you today?",
        "refusal": null,
        "annotations": []
      },
      "logprobs": null,
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 19,
    "completion_tokens": 10,
    "total_tokens": 29,
    "prompt_tokens_details": {
      "cached_tokens": 0,
      "audio_tokens": 0
    },
    "completion_tokens_details": {
      "reasoning_tokens": 0,
      "audio_tokens": 0,
      "accepted_prediction_tokens": 0,
      "rejected_prediction_tokens": 0
    }
  },
  "service_tier": "default"
}`

func TestDecode(t *testing.T) {
	resp, err := (Protocol{}).Decode(strings.NewReader(toolCallsResponse))
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "chatcmpl-abc123" || resp.Model != "gpt-6-astra" || resp.StopReason != protocol.StopToolUse || resp.RawStopReason != "tool_calls" {
		t.Fatalf("%+v", resp)
	}
	if resp.Usage != (protocol.Usage{Input: 82, Output: 17}) {
		t.Fatalf("%+v", resp.Usage)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("%+v", resp.Content)
	}
	tc := resp.Content[0].(protocol.ToolCall)
	if tc.ID != "call_abc123" || tc.Name != "get_current_weather" || string(tc.Args) != "{\n\"location\": \"Boston, MA\"\n}" {
		t.Fatalf("%+v", tc)
	}
	resp, err = (Protocol{}).Decode(strings.NewReader(textResponse))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != protocol.StopEnd || len(resp.Content) != 1 || resp.Content[0].(protocol.Text).Text != "Hello! How can I assist you today?" {
		t.Fatalf("%+v", resp)
	}
	if resp.Usage != (protocol.Usage{Input: 19, Output: 10}) {
		t.Fatalf("%+v", resp.Usage)
	}
	for name, body := range map[string]string{
		"no choices":    `{"id":"x","model":"m","choices":[]}`,
		"custom tool":   `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c","type":"custom"}]},"finish_reason":"tool_calls"}]}`,
		"bad arguments": `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{"}}]},"finish_reason":"tool_calls"}]}`,
		"not json":      `<html>`,
	} {
		if _, err := (Protocol{}).Decode(strings.NewReader(body)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// Usage with cached and reasoning tokens, Input excluding the cached share
func TestDecodeUsageNormalization(t *testing.T) {
	cached := `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":2},"completion_tokens_details":{"reasoning_tokens":4}}}`
	resp, err := (Protocol{}).Decode(strings.NewReader(cached))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != protocol.StopLength || resp.Usage != (protocol.Usage{Input: 2, Output: 1, CacheRead: 3, CacheWrite: 2, Reasoning: 4}) {
		t.Fatalf("%+v", resp)
	}
	if resp.Usage.Context() != 7 {
		t.Fatalf("context %d", resp.Usage.Context())
	}
}

// Reasoning fields DeepSeek and OpenRouter style vendors add to the message, with a refusal kept separate
func TestDecodeReasoningAndRefusal(t *testing.T) {
	body := `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"think","content":"answer","refusal":"nope"},"finish_reason":"stop"}]}`
	resp, err := (Protocol{}).Decode(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 3 {
		t.Fatalf("%+v", resp.Content)
	}
	if th := resp.Content[0].(protocol.Thinking); th.Text != "think" || th.Origin != protocol.OpenAICompletions {
		t.Fatalf("%+v", th)
	}
	if resp.Content[1].(protocol.Text).Text != "answer" || resp.Content[2].(protocol.Text).Text != "nope" {
		t.Fatalf("%+v", resp.Content)
	}
	alt := `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","reasoning":"alt","content":"a"},"finish_reason":""}]}`
	resp, err = (Protocol{}).Decode(strings.NewReader(alt))
	if err != nil || resp.Content[0].(protocol.Thinking).Text != "alt" || resp.StopReason != protocol.StopEnd {
		t.Fatalf("%+v %v", resp, err)
	}
}

// Spec 6.4 streaming text transcript, verbatim including the elision comment line
const textStream = `data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1694268190,"model":"gpt-6-astra", "system_fingerprint": "fp_44709d6fcb", "choices":[{"index":0,"delta":{"role":"assistant","content":""},"logprobs":null,"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1694268190,"model":"gpt-6-astra", "system_fingerprint": "fp_44709d6fcb", "choices":[{"index":0,"delta":{"content":"Hello"},"logprobs":null,"finish_reason":null}]}

: Intermediate chat completion chunks omitted.

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1694268190,"model":"gpt-6-astra", "system_fingerprint": "fp_44709d6fcb", "choices":[{"index":0,"delta":{},"logprobs":null,"finish_reason":"stop"}]}

data: [DONE]

`

func collect(t *testing.T, in string) ([]protocol.EventType, protocol.Response) {
	t.Helper()
	var types []protocol.EventType
	var final protocol.Response
	for ev, err := range (Protocol{}).Stream(strings.NewReader(in)) {
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, ev.Type)
		final = ev.Response
	}
	return types, final
}

func assertTypes(t *testing.T, got, want []protocol.EventType) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestStreamText(t *testing.T) {
	types, final := collect(t, textStream)
	assertTypes(t, types, []protocol.EventType{protocol.EventStart, protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextEnd, protocol.EventDone})
	if final.ID != "chatcmpl-123" || final.Model != "gpt-6-astra" || final.StopReason != protocol.StopEnd || final.Content[0].(protocol.Text).Text != "Hello" {
		t.Fatalf("%+v", final)
	}
	resp, err := protocol.Collect((Protocol{}).Stream(strings.NewReader(textStream)))
	if err != nil || resp.Content[0].(protocol.Text).Text != "Hello" {
		t.Fatalf("%+v %v", resp, err)
	}
}

// ASSEMBLED from the spec 4.2 chunk schema, two parallel tool calls by index plus a usage chunk
const toolStream = `data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"gpt-6-astra","choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_current_weather","arguments":""}}]},"finish_reason":null}],"usage":null,"obfuscation":"r4N7vQ2m"}

data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"gpt-6-astra","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"location\":"}}]},"finish_reason":null}],"usage":null}

data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"gpt-6-astra","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" \"Boston\"}"}}]},"finish_reason":null}],"usage":null}

data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"gpt-6-astra","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"get_current_weather","arguments":""}}]},"finish_reason":null}],"usage":null}

data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"gpt-6-astra","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"location\": \"Paris\"}"}}]},"finish_reason":null}],"usage":null}

data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"gpt-6-astra","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":null}

data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"gpt-6-astra","choices":[],"usage":{"prompt_tokens":40,"completion_tokens":20,"total_tokens":60,"prompt_tokens_details":{"cached_tokens":8},"completion_tokens_details":{"reasoning_tokens":2}}}

data: [DONE]

`

func TestStreamToolCalls(t *testing.T) {
	types, final := collect(t, toolStream)
	assertTypes(t, types, []protocol.EventType{
		protocol.EventStart,
		protocol.EventToolCallStart, protocol.EventToolCallDelta, protocol.EventToolCallDelta, protocol.EventToolCallEnd,
		protocol.EventToolCallStart, protocol.EventToolCallDelta, protocol.EventToolCallEnd,
		protocol.EventDone,
	})
	if final.StopReason != protocol.StopToolUse || final.Usage != (protocol.Usage{Input: 32, Output: 20, CacheRead: 8, Reasoning: 2}) {
		t.Fatalf("%+v", final)
	}
	a := final.Content[0].(protocol.ToolCall)
	b := final.Content[1].(protocol.ToolCall)
	if a.ID != "call_1" || a.Name != "get_current_weather" || string(a.Args) != `{"location": "Boston"}` {
		t.Fatalf("%+v", a)
	}
	if b.ID != "call_2" || string(b.Args) != `{"location": "Paris"}` {
		t.Fatalf("%+v", b)
	}
}

func TestStreamTextThenToolCallClosesText(t *testing.T) {
	in := `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"Checking"},"finish_reason":null}]}

data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}

data: {"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

`
	types, final := collect(t, in)
	assertTypes(t, types, []protocol.EventType{protocol.EventStart, protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextEnd, protocol.EventToolCallStart, protocol.EventToolCallDelta, protocol.EventToolCallEnd, protocol.EventDone})
	if len(final.Content) != 2 || final.Content[0].(protocol.Text).Text != "Checking" {
		t.Fatalf("%+v", final)
	}
}

// Reasoning deltas precede content and close when content starts, with refusal in its own block
func TestStreamReasoningThenContentAndRefusal(t *testing.T) {
	in := `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"Let me"},"finish_reason":null}]}

data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"reasoning":" think"},"finish_reason":null}]}

data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"Sky","refusal":"no"},"finish_reason":null}]}

data: {"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	types, final := collect(t, in)
	assertTypes(t, types, []protocol.EventType{protocol.EventStart, protocol.EventThinkingStart, protocol.EventThinkingDelta, protocol.EventThinkingDelta, protocol.EventThinkingEnd, protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextEnd, protocol.EventTextEnd, protocol.EventDone})
	if th := final.Content[0].(protocol.Thinking); th.Text != "Let me think" || th.Origin != protocol.OpenAICompletions {
		t.Fatalf("%+v", th)
	}
	if final.Content[1].(protocol.Text).Text != "Sky" || final.Content[2].(protocol.Text).Text != "no" {
		t.Fatalf("%+v", final.Content)
	}
	withCall := `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"r"},"finish_reason":null}]}

data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}

data: {"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

`
	types, _ = collect(t, withCall)
	assertTypes(t, types, []protocol.EventType{protocol.EventStart, protocol.EventThinkingStart, protocol.EventThinkingDelta, protocol.EventThinkingEnd, protocol.EventToolCallStart, protocol.EventToolCallDelta, protocol.EventToolCallEnd, protocol.EventDone})
}

// Vendors that never send finish_reason still terminate with [DONE]
func TestStreamDoneWithoutFinishReason(t *testing.T) {
	in := `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":""}]}

data: [DONE]

`
	_, final := collect(t, in)
	if final.StopReason != protocol.StopEnd || final.RawStopReason != "" || final.Content[0].(protocol.Text).Text != "Hi" {
		t.Fatalf("%+v", final)
	}
	calls := `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}

data: [DONE]

`
	_, final = collect(t, calls)
	if final.StopReason != protocol.StopToolUse || len(final.Content) != 1 {
		t.Fatalf("%+v", final)
	}
}

func TestStreamIdentityArrivingLate(t *testing.T) {
	in := `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]},"finish_reason":null}]}

data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"name":"f","arguments":"1}"}}]},"finish_reason":null}]}

data: {"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	types, final := collect(t, in)
	assertTypes(t, types, []protocol.EventType{protocol.EventStart, protocol.EventToolCallStart, protocol.EventToolCallDelta, protocol.EventToolCallEnd, protocol.EventDone})
	tc := final.Content[0].(protocol.ToolCall)
	if tc.ID != "call_9" || tc.Name != "f" || string(tc.Args) != `{"a":1}` {
		t.Fatalf("%+v", tc)
	}
}

func TestStreamErrors(t *testing.T) {
	cases := map[string]string{
		"truncated":        "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"},\"finish_reason\":null}]}\n\n",
		"error frame":      "data: {\"error\":{\"message\":\"boom\",\"type\":\"server_error\",\"param\":null,\"code\":null}}\n\n",
		"plain error":      "data: {\"error\":{\"message\":\"bad\",\"type\":\"invalid_request_error\",\"param\":null,\"code\":null}}\n\n",
		"bad json":         "data: {nope}\n\n",
		"second choice":    "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":1,\"delta\":{\"content\":\"Hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		"custom tool call": "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"x\",\"type\":\"custom\"}]},\"finish_reason\":null}]}\n\n",
		"bad arguments":    "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"x\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n",
		"bad args at done": "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"x\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"[\"}}]},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n",
	}
	for name, in := range cases {
		var failed bool
		for _, err := range (Protocol{}).Stream(strings.NewReader(in)) {
			if err != nil {
				failed = true
				var pe *protocol.Error
				switch name {
				case "error frame":
					if !errors.As(err, &pe) || pe.Status != 200 || pe.Type != "server_error" || pe.Message != "boom" || !pe.InStream || !pe.Transient || len(pe.Body) == 0 {
						t.Fatalf("%+v", pe)
					}
				case "plain error":
					if !errors.As(err, &pe) || !pe.InStream || pe.Transient {
						t.Fatalf("%+v", pe)
					}
				}
			}
		}
		if !failed {
			t.Fatalf("%s accepted", name)
		}
	}
}

// Spec 5 verbatim rate limit error body
func TestDecodeError(t *testing.T) {
	body := []byte(`{"error": {"message": "Your request rate increased too quickly. Please reduce the request rate and gradually increase it again.", "type": "rate_limit_error", "param": null, "code": "slow_down"}}`)
	e := (Protocol{}).DecodeError(429, body)
	if e.Status != 429 || e.Type != "rate_limit_error" || !strings.HasPrefix(e.Message, "Your request rate") || string(e.Body) != string(body) || !e.Transient || e.InStream {
		t.Fatalf("%+v", e)
	}
	e = (Protocol{}).DecodeError(403, []byte("  forbidden\n"))
	if e.Status != 403 || e.Type != "" || e.Message != "forbidden" || e.Transient {
		t.Fatalf("%+v", e)
	}
	e = (Protocol{}).DecodeError(400, []byte(`{"error":{"message":"m","type":"","param":null,"code":"only_code"}}`))
	if e.Type != "only_code" || e.Error() != "protocol: http 400 only_code: m" || e.Transient {
		t.Fatalf("%+v", e)
	}
	e = (Protocol{}).DecodeError(400, []byte(`{"error":{"message":"m","type":"","code":"overloaded"}}`))
	if !e.Transient {
		t.Fatal("overloaded code must be transient")
	}
	for status, want := range map[int]bool{408: true, 409: true, 500: true, 502: true, 404: false} {
		if got := (Protocol{}).DecodeError(status, []byte("x")).Transient; got != want {
			t.Fatalf("%d transient %v", status, got)
		}
	}
}

func TestRegistered(t *testing.T) {
	p, ok := protocol.Lookup(protocol.OpenAICompletions)
	if !ok || p.ID() != "openai-completions" {
		t.Fatal("not registered")
	}
}
