package openaicompletions

import (
	"encoding/json"
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
				protocol.Thinking{Text: "dropped", Signature: "sig"},
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
	if _, has := asst["content"]; has || asst["role"] != "assistant" {
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

func TestEncodeErrors(t *testing.T) {
	text := protocol.Text{Text: "x"}
	result := protocol.ToolResult{CallID: "c", Content: []protocol.Part{text}}
	cases := map[string]protocol.Request{
		"image in tool result": {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "c", Content: []protocol.Part{protocol.Image{MediaType: "image/png"}}}}}}},
		"tool result in user":  {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{result}}}},
		"text in tool message": {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{text}}}},
		"image in assistant":   {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Image{}}}}},
		"thinking only":        {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Thinking{Text: "t"}}}}},
		"empty message":        {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleUser}}},
		"unknown role":         {Model: "m", Messages: []protocol.Message{{Role: "system", Content: []protocol.Part{text}}}},
		"tool without schema":  {Model: "m", Tools: []protocol.Tool{{Name: "f"}}},
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
	cached := `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":2},"completion_tokens_details":{"reasoning_tokens":4}}}`
	resp, err = (Protocol{}).Decode(strings.NewReader(cached))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != protocol.StopLength || resp.Usage != (protocol.Usage{Input: 5, Output: 1, CacheRead: 3, CacheWrite: 2, Reasoning: 4}) {
		t.Fatalf("%+v", resp)
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
	if final.StopReason != protocol.StopToolUse || final.Usage != (protocol.Usage{Input: 40, Output: 20, CacheRead: 8, Reasoning: 2}) {
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
		"bad json":         "data: {nope}\n\n",
		"second choice":    "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":1,\"delta\":{\"content\":\"Hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		"custom tool call": "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"x\",\"type\":\"custom\"}]},\"finish_reason\":null}]}\n\n",
		"bad arguments":    "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"x\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n",
	}
	for name, in := range cases {
		var failed bool
		for _, err := range (Protocol{}).Stream(strings.NewReader(in)) {
			if err != nil {
				failed = true
				if name == "error frame" {
					pe, ok := err.(*protocol.Error)
					if !ok || pe.Status != 200 || pe.Type != "server_error" || pe.Message != "boom" {
						t.Fatalf("%v", err)
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
	if e.Status != 429 || e.Type != "rate_limit_error" || !strings.HasPrefix(e.Message, "Your request rate") || string(e.Body) != string(body) {
		t.Fatalf("%+v", e)
	}
	e = (Protocol{}).DecodeError(403, []byte("  forbidden\n"))
	if e.Status != 403 || e.Type != "" || e.Message != "forbidden" {
		t.Fatalf("%+v", e)
	}
	e = (Protocol{}).DecodeError(400, []byte(`{"error":{"message":"m","type":"","param":null,"code":"only_code"}}`))
	if e.Type != "only_code" || e.Error() != "protocol: http 400 only_code: m" {
		t.Fatalf("%+v", e)
	}
}

func TestRegistered(t *testing.T) {
	p, ok := protocol.Lookup(protocol.OpenAICompletions)
	if !ok || p.ID() != "openai-completions" {
		t.Fatal("not registered")
	}
}
