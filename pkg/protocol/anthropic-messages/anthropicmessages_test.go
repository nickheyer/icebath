package anthropicmessages

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// Spec 6.1 tool definition reused by the encode tests
const stockSchema = `{"type":"object","properties":{"ticker":{"type":"string","description":"The stock ticker symbol, e.g. AAPL for Apple Inc."}},"required":["ticker"]}`

func temp(v float64) *float64 { return &v }

func multiTurn() protocol.Request {
	return protocol.Request{
		Model:       "claude-opus-5-5",
		System:      "Be terse.",
		MaxTokens:   1024,
		Temperature: temp(1),
		Stream:      true,
		Tools:       []protocol.Tool{{Name: "get_stock_price", Description: "Get the current stock price for a given ticker symbol.", Parameters: json.RawMessage(stockSchema)}},
		ToolChoice:  protocol.ToolChoice{Mode: protocol.ToolChoiceAuto},
		Messages: []protocol.Message{
			{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "What's the S&P 500 at today?"}}},
			{Role: protocol.RoleAssistant, Content: []protocol.Part{
				protocol.Thinking{Text: "Look it up.", Signature: "sig1"},
				protocol.Text{Text: "Checking."},
				protocol.ToolCall{ID: "toolu_01D7FLrfh4GYq7yT1ULFeyMV", Name: "get_stock_price", Args: json.RawMessage(`{"ticker": "^GSPC"}`), Signature: "ignored"},
			}},
			{Role: protocol.RoleTool, Content: []protocol.Part{
				protocol.ToolResult{CallID: "toolu_01D7FLrfh4GYq7yT1ULFeyMV", Name: "get_stock_price", Content: []protocol.Part{protocol.Text{Text: "259.75 USD"}}},
				protocol.ToolResult{CallID: "toolu_02", Name: "get_stock_price", IsError: true, Content: []protocol.Part{protocol.Image{MediaType: "image/png", Data: []byte{1, 2, 3}}}},
			}},
		},
	}
}

func encodeMap(t *testing.T, req protocol.Request) (protocol.Call, map[string]any) {
	t.Helper()
	call, err := Protocol{}.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatal(err)
	}
	return call, body
}

func at(t *testing.T, v any, keys ...any) any {
	t.Helper()
	for _, k := range keys {
		switch key := k.(type) {
		case string:
			v = v.(map[string]any)[key]
		case int:
			v = v.([]any)[key]
		}
	}
	return v
}

func assistant(parts ...protocol.Part) protocol.Request {
	return protocol.Request{Model: "m", MaxTokens: 8, Messages: []protocol.Message{
		{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "q"}}},
		{Role: protocol.RoleAssistant, Content: parts},
	}}
}

func TestCapabilities(t *testing.T) {
	want := protocol.Capabilities{Thinking: true, RedactedThinking: true, Images: true, ImagesInResults: true, Documents: true, ToolChoiceNone: true, ToolChoiceNamed: true, StopSequences: true, ServerTools: true, Citations: true}
	if got := (Protocol{}).Capabilities(); got != want {
		t.Fatalf("%+v", got)
	}
}

// Spec 6.1 and 2.3 block shapes
func TestEncodeMultiTurn(t *testing.T) {
	call, body := encodeMap(t, multiTurn())
	if call.Method != "POST" || call.Path != "/v1/messages" {
		t.Fatalf("%s %s", call.Method, call.Path)
	}
	if call.Header.Get("anthropic-version") != "2023-06-01" || call.Header.Get("Content-Type") != "application/json" || call.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("headers %v", call.Header)
	}
	if body["system"] != "Be terse." || body["max_tokens"] != 1024.0 || body["stream"] != true || body["temperature"] != 1.0 || body["model"] != "claude-opus-5-5" {
		t.Fatalf("top level %v", body)
	}
	if at(t, body, "tools", 0, "input_schema", "required", 0) != "ticker" || at(t, body, "tools", 0, "description") == "" {
		t.Fatal("tool definition")
	}
	if at(t, body, "tool_choice", "type") != "auto" {
		t.Fatal("tool choice")
	}
	if at(t, body, "messages", 0, "content", 0, "text") != "What's the S&P 500 at today?" {
		t.Fatal("user text")
	}
	assistant := at(t, body, "messages", 1, "content").([]any)
	if at(t, assistant[0], "type") != "thinking" || at(t, assistant[0], "signature") != "sig1" || at(t, assistant[0], "thinking") != "Look it up." {
		t.Fatalf("thinking %v", assistant[0])
	}
	if at(t, assistant[2], "type") != "tool_use" || at(t, assistant[2], "input", "ticker") != "^GSPC" || at(t, assistant[2], "id") != "toolu_01D7FLrfh4GYq7yT1ULFeyMV" {
		t.Fatalf("tool_use %v", assistant[2])
	}
	if _, has := assistant[2].(map[string]any)["signature"]; has {
		t.Fatal("tool_use must not carry a signature")
	}
	if at(t, body, "messages", 2, "role") != "user" {
		t.Fatal("tool results must ride in a user message")
	}
	results := at(t, body, "messages", 2, "content").([]any)
	if len(results) != 2 || at(t, results[0], "type") != "tool_result" || at(t, results[0], "tool_use_id") != "toolu_01D7FLrfh4GYq7yT1ULFeyMV" || at(t, results[0], "content", 0, "text") != "259.75 USD" {
		t.Fatalf("results %v", results)
	}
	if _, has := results[0].(map[string]any)["is_error"]; has {
		t.Fatal("is_error must be omitted when false")
	}
	if at(t, results[1], "is_error") != true || at(t, results[1], "content", 0, "source", "media_type") != "image/png" || at(t, results[1], "content", 0, "source", "data") != "AQID" || at(t, results[1], "content", 0, "source", "type") != "base64" {
		t.Fatalf("image result %v", results[1])
	}
	if strings.Contains(string(call.Body), `\u0026`) || !strings.Contains(string(call.Body), "S&P") {
		t.Fatal("body must not escape HTML")
	}
}

func TestEncodeRedactedThinkingAndChoices(t *testing.T) {
	req := multiTurn()
	req.Stream = false
	req.Messages = []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Thinking{Redacted: true, Signature: "opaque"}}}}
	for mode, want := range map[protocol.ToolChoice]map[string]any{
		{Mode: protocol.ToolChoiceNone}:                          {"type": "none"},
		{Mode: protocol.ToolChoiceRequired}:                      {"type": "any"},
		{Mode: protocol.ToolChoiceTool, Name: "get_stock_price"}: {"type": "tool", "name": "get_stock_price"},
	} {
		req.ToolChoice = mode
		call, body := encodeMap(t, req)
		if call.Header.Get("Accept") != "application/json" {
			t.Fatal("accept")
		}
		if _, has := body["stream"]; has {
			t.Fatal("stream must be omitted")
		}
		for k, v := range want {
			if at(t, body, "tool_choice", k) != v {
				t.Fatalf("tool_choice %v", body["tool_choice"])
			}
		}
		if at(t, body, "messages", 0, "content", 0, "type") != "redacted_thinking" || at(t, body, "messages", 0, "content", 0, "data") != "opaque" {
			t.Fatal("redacted thinking")
		}
	}
	req.ToolChoice = protocol.ToolChoice{}
	_, body := encodeMap(t, req)
	if _, has := body["tool_choice"]; has {
		t.Fatal("zero tool choice must be omitted")
	}
}

// Rule 3 replay policy for signed parts
func TestEncodeOriginPolicy(t *testing.T) {
	foreign := protocol.OpenAIResponses
	_, body := encodeMap(t, assistant(
		protocol.Thinking{Text: "foreign thought", Signature: "foreign-sig", Origin: foreign},
		protocol.Thinking{Redacted: true, Signature: "foreign-redacted", Origin: foreign},
		protocol.Thinking{Text: "same", Signature: "same-sig", Origin: protocol.AnthropicMessages},
		protocol.Thinking{Text: "empty", Signature: "empty-sig"},
		protocol.ToolCall{ID: "c1", Name: "f", Args: json.RawMessage(`{}`), Signature: "tool-sig", Origin: foreign},
		protocol.ServerToolCall{ID: "srv1", Name: "web_search", Args: json.RawMessage(`{"query":"x"}`), Origin: foreign},
		protocol.ServerToolResult{CallID: "srv1", Name: "web_search_tool_result", Content: json.RawMessage(`[]`), Origin: foreign},
		protocol.ServerToolCall{ID: "srv2", Name: "web_search", Args: json.RawMessage(`{"query":"y"}`), Origin: protocol.AnthropicMessages},
		protocol.ServerToolResult{CallID: "srv2", Name: "web_search_tool_result", Content: json.RawMessage(`[{"type":"web_search_result","url":"https://x"}]`), Origin: protocol.AnthropicMessages},
	))
	raw, _ := json.Marshal(body)
	for _, leaked := range []string{"foreign thought", "foreign-sig", "foreign-redacted", "tool-sig", `"query":"x"`, "srv1"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("leaked %q in %s", leaked, raw)
		}
	}
	content := at(t, body, "messages", 1, "content").([]any)
	if len(content) != 5 {
		t.Fatalf("%v", content)
	}
	if at(t, content[0], "thinking") != "same" || at(t, content[1], "thinking") != "empty" || at(t, content[2], "type") != "tool_use" {
		t.Fatalf("%v", content)
	}
	if at(t, content[3], "type") != "server_tool_use" || at(t, content[3], "id") != "srv2" || at(t, content[3], "input", "query") != "y" {
		t.Fatalf("server tool use %v", content[3])
	}
	if at(t, content[4], "type") != "web_search_tool_result" || at(t, content[4], "tool_use_id") != "srv2" || at(t, content[4], "content", 0, "url") != "https://x" {
		t.Fatalf("server tool result %v", content[4])
	}
	_, body = encodeMap(t, assistant(protocol.Thinking{Text: "gone", Signature: "s", Origin: foreign}))
	if msgs := body["messages"].([]any); len(msgs) != 1 || at(t, msgs[0], "role") != "user" {
		t.Fatalf("empty assistant turn must be omitted: %v", msgs)
	}
}

func TestEncodeDocuments(t *testing.T) {
	req := protocol.Request{Model: "m", MaxTokens: 8, Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{
		protocol.Document{MediaType: "application/pdf", Data: []byte("%PDF"), Name: "report.pdf"},
		protocol.Document{MediaType: "text/markdown", Data: []byte("# hi"), Name: "notes"},
		protocol.Document{MediaType: "text/plain", Data: []byte("plain")},
	}}}}
	_, body := encodeMap(t, req)
	content := at(t, body, "messages", 0, "content").([]any)
	if at(t, content[0], "type") != "document" || at(t, content[0], "source", "type") != "base64" || at(t, content[0], "source", "media_type") != "application/pdf" || at(t, content[0], "source", "data") != "JVBERg==" || at(t, content[0], "title") != "report.pdf" {
		t.Fatalf("pdf %v", content[0])
	}
	if at(t, content[1], "source", "type") != "text" || at(t, content[1], "source", "media_type") != "text/plain" || at(t, content[1], "source", "data") != "# hi" || at(t, content[1], "title") != "notes" {
		t.Fatalf("markdown %v", content[1])
	}
	if _, has := content[2].(map[string]any)["title"]; has {
		t.Fatal("empty title must be omitted")
	}
	for name, doc := range map[string]protocol.Document{
		"media type": {MediaType: "application/zip", Data: []byte{1}},
		"no data":    {MediaType: "application/pdf"},
	} {
		req.Messages[0].Content = []protocol.Part{doc}
		if _, err := (Protocol{}).Encode(req); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	req.Messages = []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Document{MediaType: "application/pdf", Data: []byte{1}}}}}
	if _, err := (Protocol{}).Encode(req); err == nil {
		t.Fatal("document in assistant accepted")
	}
}

func TestEncodeWhitespaceText(t *testing.T) {
	_, body := encodeMap(t, protocol.Request{Model: "m", MaxTokens: 8, Messages: []protocol.Message{
		{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "   "}, protocol.Text{Text: "\n"}}},
		{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Text{Text: " "}, protocol.Text{Text: "kept"}}},
		{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: ""}}},
	}})
	msgs := body["messages"].([]any)
	if content := at(t, msgs[0], "content").([]any); len(content) != 1 || at(t, content[0], "text") != " " {
		t.Fatalf("whitespace user %v", content)
	}
	if content := at(t, msgs[1], "content").([]any); len(content) != 1 || at(t, content[0], "text") != "kept" {
		t.Fatalf("assistant %v", content)
	}
	if content := at(t, msgs[2], "content").([]any); len(content) != 1 || at(t, content[0], "text") != " " {
		t.Fatalf("empty user %v", content)
	}
}

func TestEncodeExtraMerges(t *testing.T) {
	req := multiTurn()
	req.Extra = map[string]json.RawMessage{"thinking": json.RawMessage(`{"type":"adaptive"}`), "max_tokens": json.RawMessage(`4096`)}
	_, body := encodeMap(t, req)
	if at(t, body, "thinking", "type") != "adaptive" || body["max_tokens"] != 4096.0 {
		t.Fatalf("%v", body)
	}
}

func TestEncodeErrors(t *testing.T) {
	user := func(parts ...protocol.Part) protocol.Message {
		return protocol.Message{Role: protocol.RoleUser, Content: parts}
	}
	cases := map[string]func(*protocol.Request){
		"max_tokens":  func(r *protocol.Request) { r.MaxTokens = 0 },
		"tool schema": func(r *protocol.Request) { r.Tools[0].Parameters = nil },
		"unknown role": func(r *protocol.Request) {
			r.Messages = []protocol.Message{{Role: "system", Content: []protocol.Part{protocol.Text{Text: "x"}}}}
		},
		"empty message":  func(r *protocol.Request) { r.Messages = []protocol.Message{{Role: protocol.RoleUser}} },
		"result in user": func(r *protocol.Request) { r.Messages = []protocol.Message{user(protocol.ToolResult{CallID: "a"})} },
		"text in tool": func(r *protocol.Request) {
			r.Messages = []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.Text{Text: "x"}}}}
		},
		"media type": func(r *protocol.Request) {
			r.Messages = []protocol.Message{user(protocol.Image{MediaType: "image/bmp", Data: []byte{1}})}
		},
		"image without data": func(r *protocol.Request) {
			r.Messages = []protocol.Message{user(protocol.Image{MediaType: "image/png"})}
		},
		"image in result": func(r *protocol.Request) {
			r.Messages = []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "a", Content: []protocol.Part{protocol.Image{MediaType: "image/tiff"}}}}}}
		},
		"thinking in user": func(r *protocol.Request) { r.Messages = []protocol.Message{user(protocol.Thinking{Text: "x"})} },
		"call in user": func(r *protocol.Request) {
			r.Messages = []protocol.Message{user(protocol.ToolCall{ID: "a", Name: "b"})}
		},
		"server call in user": func(r *protocol.Request) {
			r.Messages = []protocol.Message{user(protocol.ServerToolCall{ID: "a", Name: "b"})}
		},
		"server result in user": func(r *protocol.Request) {
			r.Messages = []protocol.Message{user(protocol.ServerToolResult{CallID: "a", Name: "b"})}
		},
		"server result without type": func(r *protocol.Request) {
			r.Messages = []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ServerToolResult{CallID: "a"}}}}
		},
		"bad server args": func(r *protocol.Request) {
			r.Messages = []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ServerToolCall{ID: "a", Name: "b", Args: json.RawMessage(`null`)}}}}
		},
		"image in assistant": func(r *protocol.Request) {
			r.Messages = []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Image{MediaType: "image/png"}}}}
		},
		"bad args": func(r *protocol.Request) {
			r.Messages = []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{ID: "a", Name: "b", Args: json.RawMessage(`[`)}}}}
		},
		"nameless tool choice": func(r *protocol.Request) { r.ToolChoice = protocol.ToolChoice{Mode: protocol.ToolChoiceTool} },
		"unknown tool choice":  func(r *protocol.Request) { r.ToolChoice = protocol.ToolChoice{Mode: "maybe"} },
		"bad extra":            func(r *protocol.Request) { r.Extra = map[string]json.RawMessage{"x": json.RawMessage(`{`)} },
	}
	for name, mutate := range cases {
		req := multiTurn()
		mutate(&req)
		if _, err := (Protocol{}).Encode(req); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// Spec 6.2 assembled non streaming tool_use response
const toolUseResponse = `{
  "id": "msg_013Zva2CMHLNnXjNJJKqJ2EF",
  "type": "message",
  "role": "assistant",
  "model": "claude-opus-5-5",
  "content": [
    {
      "type": "text",
      "text": "I'll help you check the current weather and time in San Francisco."
    },
    {
      "type": "tool_use",
      "id": "toolu_01A09q90qw90lq917835lq9",
      "name": "get_weather",
      "input": { "location": "San Francisco, CA" }
    }
  ],
  "stop_reason": "tool_use",
  "stop_sequence": null,
  "usage": {
    "input_tokens": 2095,
    "output_tokens": 503,
    "cache_creation_input_tokens": 2051,
    "cache_read_input_tokens": 2051
  }
}`

func TestDecodeToolUse(t *testing.T) {
	resp, err := Protocol{}.Decode(strings.NewReader(toolUseResponse))
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "msg_013Zva2CMHLNnXjNJJKqJ2EF" || resp.Model != "claude-opus-5-5" || resp.StopReason != protocol.StopToolUse || resp.RawStopReason != "tool_use" {
		t.Fatalf("%+v", resp)
	}
	if resp.Usage != (protocol.Usage{Input: 2095, Output: 503, CacheRead: 2051, CacheWrite: 2051}) {
		t.Fatalf("usage %+v", resp.Usage)
	}
	if len(resp.Content) != 2 || resp.Content[0].(protocol.Text).Text != "I'll help you check the current weather and time in San Francisco." {
		t.Fatalf("%+v", resp.Content)
	}
	tc := resp.Content[1].(protocol.ToolCall)
	if tc.ID != "toolu_01A09q90qw90lq917835lq9" || tc.Name != "get_weather" || string(tc.Args) != `{"location":"San Francisco, CA"}` || tc.Origin != "" {
		t.Fatalf("%+v", tc)
	}
}

// Spec 3.3 verbatim usage block with thinking tokens and spec 3.1 redacted thinking
func TestDecodeThinkingAndUsageDetails(t *testing.T) {
	body := `{"id":"m","type":"message","role":"assistant","model":"x","content":[{"type":"thinking","thinking":"t","signature":"s"},{"type":"redacted_thinking","data":"d"},{"type":"tool_use","id":"i","name":"n"}],"stop_reason":"refusal","usage":{"input_tokens":2095,"output_tokens":503,"output_tokens_details":{"thinking_tokens":7}}}`
	resp, err := Protocol{}.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != protocol.StopContentFilter || resp.Usage.Reasoning != 7 {
		t.Fatalf("%+v", resp)
	}
	if th := resp.Content[0].(protocol.Thinking); th.Text != "t" || th.Signature != "s" || th.Redacted || th.Origin != protocol.AnthropicMessages {
		t.Fatalf("%+v", th)
	}
	if th := resp.Content[1].(protocol.Thinking); !th.Redacted || th.Signature != "d" || th.Origin != protocol.AnthropicMessages {
		t.Fatalf("%+v", th)
	}
	if string(resp.Content[2].(protocol.ToolCall).Args) != "{}" {
		t.Fatal("missing input must decode as empty arguments")
	}
	for raw, want := range map[string]protocol.StopReason{"end_turn": protocol.StopEnd, "stop_sequence": protocol.StopEnd, "max_tokens": protocol.StopLength, "model_context_window_exceeded": protocol.StopLength, "pause_turn": protocol.StopOther} {
		resp, err := Protocol{}.Decode(strings.NewReader(`{"type":"message","content":[],"stop_reason":"` + raw + `","usage":{}}`))
		if err != nil || resp.StopReason != want || resp.RawStopReason != raw {
			t.Fatalf("%s -> %v %v", raw, resp.StopReason, err)
		}
	}
}

func TestDecodeStopSequence(t *testing.T) {
	resp, err := Protocol{}.Decode(strings.NewReader(`{"type":"message","content":[{"type":"text","text":"a"}],"stop_reason":"stop_sequence","stop_sequence":"END","usage":{}}`))
	if err != nil || resp.StopReason != protocol.StopEnd || resp.RawStopReason != "stop_sequence:END" {
		t.Fatalf("%+v %v", resp, err)
	}
}

// Spec web search tool blocks and the document citations shape, assembled
func TestDecodeServerToolsAndCitations(t *testing.T) {
	body := `{"id":"m","type":"message","model":"x","content":[
	  {"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query": "weather"}},
	  {"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://w.example","title":"Weather"}]},
	  {"type":"text","text":"Sunny.","citations":[
	    {"type":"web_search_result_location","url":"https://w.example","title":"Weather","cited_text":"It is sunny","encrypted_index":"abc"},
	    {"type":"char_location","document_index":2,"document_title":"Doc","start_char_index":5,"end_char_index":9,"cited_text":"four"},
	    {"type":"page_location","document_index":0,"start_page_number":1,"end_page_number":2},
	    {"type":"content_block_location","document_index":1,"start_block_index":3,"end_block_index":4}
	  ]},
	  {"type":"code_execution_tool_result","tool_use_id":"srvtoolu_2","content":{"type":"code_execution_result","stdout":"hi"}}
	],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`
	resp, err := Protocol{}.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	call := resp.Content[0].(protocol.ServerToolCall)
	if call.ID != "srvtoolu_1" || call.Name != "web_search" || string(call.Args) != `{"query":"weather"}` || call.Origin != protocol.AnthropicMessages {
		t.Fatalf("%+v", call)
	}
	result := resp.Content[1].(protocol.ServerToolResult)
	if result.CallID != "srvtoolu_1" || result.Name != "web_search_tool_result" || !strings.Contains(string(result.Content), "w.example") || result.Origin != protocol.AnthropicMessages {
		t.Fatalf("%+v", result)
	}
	text := resp.Content[2].(protocol.Text)
	want := []protocol.Citation{
		{Title: "Weather", URL: "https://w.example", Cited: "It is sunny"},
		{Title: "Doc", Document: "2", Start: 5, End: 9, Cited: "four"},
		{Document: "0", Start: 1, End: 2},
		{Document: "1", Start: 3, End: 4},
	}
	if text.Text != "Sunny." || !reflect.DeepEqual(text.Citations, want) {
		t.Fatalf("%+v", text)
	}
	if other := resp.Content[3].(protocol.ServerToolResult); other.Name != "code_execution_tool_result" || other.CallID != "srvtoolu_2" {
		t.Fatalf("%+v", other)
	}
	_, replay := encodeMap(t, assistant(resp.Content...))
	content := at(t, replay, "messages", 1, "content").([]any)
	if len(content) != 4 || at(t, content[0], "type") != "server_tool_use" || at(t, content[1], "type") != "web_search_tool_result" || at(t, content[3], "content", "stdout") != "hi" {
		t.Fatalf("replay %v", content)
	}
}

func TestDecodeRejects(t *testing.T) {
	for name, body := range map[string]string{
		"unknown block":   `{"type":"message","content":[{"type":"audio","id":"a"}],"stop_reason":"end_turn","usage":{}}`,
		"error body":      `{"type":"error","error":{"type":"api_error","message":"x"}}`,
		"missing stop":    `{"type":"message","content":[],"usage":{}}`,
		"not json":        `<html>`,
		"array input":     `{"type":"message","content":[{"type":"tool_use","id":"i","name":"n","input":[1]}],"stop_reason":"tool_use","usage":{}}`,
		"server null":     `{"type":"message","content":[{"type":"server_tool_use","id":"i","name":"n","input":null}],"stop_reason":"end_turn","usage":{}}`,
		"server bad json": `{"type":"message","content":[{"type":"server_tool_use","id":"i","name":"n","input":[1]}],"stop_reason":"end_turn","usage":{}}`,
	} {
		if _, err := (Protocol{}).Decode(strings.NewReader(body)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// Spec 6.3 verbatim streaming transcript with tool use
const toolUseStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_014p7gG3wDgGV9EUtLvnow3U","type":"message","role":"assistant","model":"claude-opus-5","stop_sequence":null,"usage":{"input_tokens":472,"output_tokens":2},"content":[],"stop_reason":null}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type": "ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Okay"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":","}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" let"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"'s"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" check"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" the"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" weather"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" for"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" San"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" Francisco"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":","}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" CA"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":":"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01T1x1fJ34qAmk2tNTrN7Up6","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"location\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":" \"San"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":" Francisc"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"o,"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":" CA\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":89}}

event: message_stop
data: {"type":"message_stop"}

`

// Spec 6.4 verbatim streaming transcript with thinking
const thinkingStream = `event: message_start
data: {"type": "message_start", "message": {"id": "msg_01...", "type": "message", "role": "assistant", "content": [], "model": "claude-opus-5-5", "stop_reason": null, "stop_sequence": null}}

event: content_block_start
data: {"type": "content_block_start", "index": 0, "content_block": {"type": "thinking", "thinking": "", "signature": ""}}

event: content_block_delta
data: {"type": "content_block_delta", "index": 0, "delta": {"type": "thinking_delta", "thinking": "I need to find the GCD of 1071 and 462 using the Euclidean algorithm.\n\n1071 = 2 × 462 + 147"}}

event: content_block_delta
data: {"type": "content_block_delta", "index": 0, "delta": {"type": "thinking_delta", "thinking": "\n462 = 3 × 147 + 21"}}

event: content_block_delta
data: {"type": "content_block_delta", "index": 0, "delta": {"type": "thinking_delta", "thinking": "\n147 = 7 × 21 + 0"}}

event: content_block_delta
data: {"type": "content_block_delta", "index": 0, "delta": {"type": "thinking_delta", "thinking": "\nThe remainder is 0, so GCD(1071, 462) = 21."}}

event: content_block_delta
data: {"type": "content_block_delta", "index": 0, "delta": {"type": "signature_delta", "signature": "EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds..."}}

event: content_block_stop
data: {"type": "content_block_stop", "index": 0}

event: content_block_start
data: {"type": "content_block_start", "index": 1, "content_block": {"type": "text", "text": ""}}

event: content_block_delta
data: {"type": "content_block_delta", "index": 1, "delta": {"type": "text_delta", "text": "The greatest common divisor of 1071 and 462 is **21**."}}

event: content_block_stop
data: {"type": "content_block_stop", "index": 1}

event: message_delta
data: {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": null}}

event: message_stop
data: {"type": "message_stop"}

`

// Spec 6.5 verbatim basic transcript with a ping
const basicStream = `event: message_start
data: {"type": "message_start", "message": {"id": "msg_1nZdL29xx5MUA1yADyHTEsnR8uuvGzszyY", "type": "message", "role": "assistant", "content": [], "model": "claude-opus-5-5", "stop_reason": null, "stop_sequence": null, "usage": {"input_tokens": 25, "output_tokens": 1}}}

event: content_block_start
data: {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}

event: ping
data: {"type": "ping"}

event: content_block_delta
data: {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "Hello"}}

event: content_block_delta
data: {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "!"}}

event: content_block_stop
data: {"type": "content_block_stop", "index": 0}

event: message_delta
data: {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence":null}, "usage": {"output_tokens": 15}}

event: message_stop
data: {"type": "message_stop"}

`

func stream(t *testing.T, in string) ([]protocol.Event, error) {
	t.Helper()
	var events []protocol.Event
	for ev, err := range (Protocol{}).Stream(strings.NewReader(in)) {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}

func assertTypes(t *testing.T, events []protocol.Event, want ...protocol.EventType) {
	t.Helper()
	if len(events) != len(want) {
		t.Fatalf("got %d events want %d", len(events), len(want))
	}
	for i := range want {
		if events[i].Type != want[i] {
			t.Fatalf("event %d = %s want %s", i, events[i].Type, want[i])
		}
	}
}

func TestStreamToolUse(t *testing.T) {
	events, err := stream(t, toolUseStream)
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.EventType{protocol.EventStart, protocol.EventUsage, protocol.EventTextStart}
	for range 13 {
		want = append(want, protocol.EventTextDelta)
	}
	want = append(want, protocol.EventTextEnd, protocol.EventToolCallStart)
	for range 6 {
		want = append(want, protocol.EventToolCallDelta)
	}
	want = append(want, protocol.EventToolCallEnd, protocol.EventDone)
	assertTypes(t, events, want...)
	if events[1].Response.Usage != (protocol.Usage{Input: 472, Output: 2}) {
		t.Fatalf("early usage %+v", events[1].Response.Usage)
	}
	resp, err := protocol.Collect(Protocol{}.Stream(strings.NewReader(toolUseStream)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "msg_014p7gG3wDgGV9EUtLvnow3U" || resp.Model != "claude-opus-5" || resp.StopReason != protocol.StopToolUse || resp.RawStopReason != "tool_use" {
		t.Fatalf("%+v", resp)
	}
	if resp.Usage != (protocol.Usage{Input: 472, Output: 89}) {
		t.Fatalf("usage %+v", resp.Usage)
	}
	if resp.Content[0].(protocol.Text).Text != "Okay, let's check the weather for San Francisco, CA:" {
		t.Fatalf("%+v", resp.Content[0])
	}
	tc := resp.Content[1].(protocol.ToolCall)
	if tc.ID != "toolu_01T1x1fJ34qAmk2tNTrN7Up6" || tc.Name != "get_weather" || string(tc.Args) != `{"location": "San Francisco, CA"}` {
		t.Fatalf("%+v", tc)
	}
	if start := events[17]; start.Part.(protocol.ToolCall).Name != "get_weather" || start.Index != 1 {
		t.Fatalf("%+v", start)
	}
	if last := events[23]; last.Partial != `{"location": "San Francisco, CA"}` {
		t.Fatalf("partial %q", last.Partial)
	}
}

func TestStreamThinking(t *testing.T) {
	events, err := stream(t, thinkingStream)
	if err != nil {
		t.Fatal(err)
	}
	assertTypes(t, events, protocol.EventStart, protocol.EventUsage, protocol.EventThinkingStart, protocol.EventThinkingDelta, protocol.EventThinkingDelta, protocol.EventThinkingDelta, protocol.EventThinkingDelta, protocol.EventThinkingEnd, protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextEnd, protocol.EventDone)
	resp := events[len(events)-1].Response
	th := resp.Content[0].(protocol.Thinking)
	if !strings.HasPrefix(th.Text, "I need to find the GCD") || !strings.HasSuffix(th.Text, "GCD(1071, 462) = 21.") || th.Signature != "EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds..." || th.Redacted || th.Origin != protocol.AnthropicMessages {
		t.Fatalf("%+v", th)
	}
	if resp.Content[1].(protocol.Text).Text != "The greatest common divisor of 1071 and 462 is **21**." || resp.StopReason != protocol.StopEnd || resp.Usage != (protocol.Usage{}) {
		t.Fatalf("%+v", resp)
	}
}

func TestStreamBasicWithPing(t *testing.T) {
	events, err := stream(t, basicStream)
	if err != nil {
		t.Fatal(err)
	}
	assertTypes(t, events, protocol.EventStart, protocol.EventUsage, protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextDelta, protocol.EventTextEnd, protocol.EventDone)
	resp := events[6].Response
	if resp.ID != "msg_1nZdL29xx5MUA1yADyHTEsnR8uuvGzszyY" || resp.Content[0].(protocol.Text).Text != "Hello!" || resp.Usage != (protocol.Usage{Input: 25, Output: 15}) {
		t.Fatalf("%+v", resp)
	}
	if events[3].Delta != "Hello" || events[3].Index != 0 {
		t.Fatalf("%+v", events[3])
	}
}

// Spec 6.6 verbatim cumulative usage lines around a redacted thinking block with a citation
func TestStreamCumulativeUsageRedactedAndCitation(t *testing.T) {
	in := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01G...","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2679,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":3}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"EmwKAhgBEgy"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"citations_delta","citation":{"type":"char_location","cited_text":"The grass is green","document_index":0,"document_title":"My Document","start_char_index":0,"end_char_index":18}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Done"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"stop_sequence","stop_sequence":"END"},"usage":{"input_tokens":10682,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":510,"server_tool_use":{"web_search_requests":1},"output_tokens_details":{"thinking_tokens":12}}}

event: message_stop
data: {"type":"message_stop"}

`
	events, err := stream(t, in)
	if err != nil {
		t.Fatal(err)
	}
	assertTypes(t, events, protocol.EventStart, protocol.EventUsage, protocol.EventThinkingStart, protocol.EventThinkingEnd, protocol.EventTextStart, protocol.EventCitation, protocol.EventTextDelta, protocol.EventTextEnd, protocol.EventDone)
	resp := events[len(events)-1].Response
	if resp.Usage != (protocol.Usage{Input: 10682, Output: 510, Reasoning: 12}) {
		t.Fatalf("usage %+v", resp.Usage)
	}
	if resp.StopReason != protocol.StopEnd || resp.RawStopReason != "stop_sequence:END" {
		t.Fatalf("%+v", resp)
	}
	if th := resp.Content[0].(protocol.Thinking); !th.Redacted || th.Signature != "EmwKAhgBEgy" || th.Origin != protocol.AnthropicMessages {
		t.Fatalf("%+v", th)
	}
	want := protocol.Citation{Title: "My Document", Document: "0", End: 18, Cited: "The grass is green"}
	if text := resp.Content[1].(protocol.Text); text.Text != "Done" || len(text.Citations) != 1 || text.Citations[0] != want {
		t.Fatalf("%+v", text)
	}
	if events[5].Citation != want || events[5].Index != 1 {
		t.Fatalf("%+v", events[5])
	}
}

// Web search streaming blocks from the server tools reference, assembled
func TestStreamServerTools(t *testing.T) {
	in := `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","model":"x","content":[],"usage":{"input_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"w\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://x"}]}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}

event: message_stop
data: {"type":"message_stop"}

`
	events, err := stream(t, in)
	if err != nil {
		t.Fatal(err)
	}
	assertTypes(t, events, protocol.EventStart, protocol.EventUsage, protocol.EventServerToolCallStart, protocol.EventServerToolCallDelta, protocol.EventServerToolCallDelta, protocol.EventServerToolCallEnd, protocol.EventServerToolResult, protocol.EventDone)
	resp := events[len(events)-1].Response
	call := resp.Content[0].(protocol.ServerToolCall)
	if call.ID != "srvtoolu_1" || call.Name != "web_search" || string(call.Args) != `{"query":"w"}` || call.Origin != protocol.AnthropicMessages {
		t.Fatalf("%+v", call)
	}
	result := resp.Content[1].(protocol.ServerToolResult)
	if result.CallID != "srvtoolu_1" || result.Name != "web_search_tool_result" || result.Origin != protocol.AnthropicMessages || !strings.Contains(string(result.Content), "https://x") {
		t.Fatalf("%+v", result)
	}
}

// Spec 4.6 verbatim error event
func TestStreamErrorEvent(t *testing.T) {
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"content\":[],\"model\":\"x\"}}\n\nevent: error\ndata: {\"type\": \"error\", \"error\": {\"type\": \"overloaded_error\", \"message\": \"Overloaded\"}}\n\n"
	events, err := stream(t, in)
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Type != "overloaded_error" || perr.Message != "Overloaded" || perr.Status != 200 || !perr.InStream || !perr.Transient || len(perr.Body) == 0 {
		t.Fatalf("%+v", perr)
	}
	assertTypes(t, events, protocol.EventStart, protocol.EventUsage)
	_, err = stream(t, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"bad\"}}\n\n")
	if !errors.As(err, &perr) || !perr.InStream || perr.Transient {
		t.Fatalf("%+v", perr)
	}
}

func TestStreamFailures(t *testing.T) {
	cut := strings.Index(basicStream, "event: message_stop")
	cases := map[string]string{
		"truncated":        basicStream[:cut],
		"unknown block":    strings.Replace(basicStream, `{"type": "text", "text": ""}`, `{"type": "audio", "id": "s"}`, 1),
		"unknown delta":    strings.Replace(basicStream, `"type": "text_delta", "text": "Hello"`, `"type": "audio_delta", "text": "Hello"`, 1),
		"citation missing": strings.Replace(basicStream, `"type": "text_delta", "text": "Hello"`, `"type": "citations_delta"`, 1),
		"bad json":         strings.Replace(basicStream, `{"type": "ping"}`, `{`, 1),
		"missing stop":     strings.Replace(basicStream, `"stop_reason": "end_turn"`, `"stop_reason": null`, 1),
		"stop before open": strings.Replace(basicStream, `"content_block_start", "index": 0`, `"content_block_start", "index": 7`, 1),
		"bad tool args":    strings.Replace(toolUseStream, `" CA\"}"`, `" CA\""`, 1),
		"no message":       strings.Replace(basicStream, `"message": {"id"`, `"msg": {"id"`, 1),
		"no content block": strings.Replace(basicStream, `"content_block": {"type": "text", "text": ""}`, `"cb": {}`, 1),
		"no delta":         strings.Replace(basicStream, `"delta": {"type": "text_delta", "text": "Hello"}`, `"d": {}`, 1),
	}
	for name, in := range cases {
		if _, err := stream(t, in); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestStreamStopsWhenConsumerBreaks(t *testing.T) {
	n := 0
	for _, err := range (Protocol{}).Stream(strings.NewReader(basicStream)) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		if n == 2 {
			break
		}
	}
	if n != 2 {
		t.Fatal(n)
	}
}

// Spec 5 verbatim error body
func TestDecodeError(t *testing.T) {
	body := []byte(`{
  "type": "error",
  "error": {
    "type": "not_found_error",
    "message": "The requested resource could not be found."
  },
  "request_id": "req_011CSHoEeqs5C35K2UUqR7Fy"
}`)
	e := Protocol{}.DecodeError(404, body)
	if e.Status != 404 || e.Type != "not_found_error" || e.Message != "The requested resource could not be found." || string(e.Body) != string(body) || e.Transient || e.InStream {
		t.Fatalf("%+v", e)
	}
	if e.Error() != "protocol: http 404 not_found_error: The requested resource could not be found." {
		t.Fatal(e.Error())
	}
	plain := Protocol{}.DecodeError(502, []byte(" Bad Gateway \n"))
	if plain.Type != "" || plain.Message != "Bad Gateway" || plain.Status != 502 || !plain.Transient {
		t.Fatalf("%+v", plain)
	}
	for status, want := range map[int]bool{408: true, 409: true, 429: true, 500: true, 503: true, 400: false, 401: false} {
		if got := (Protocol{}).DecodeError(status, []byte("x")).Transient; got != want {
			t.Fatalf("%d transient %v", status, got)
		}
	}
	typed := Protocol{}.DecodeError(400, []byte(`{"type":"error","error":{"type":"api_error","message":"x"}}`))
	if !typed.Transient {
		t.Fatal("api_error must be transient")
	}
}

func TestRegistered(t *testing.T) {
	p, ok := protocol.Lookup(protocol.AnthropicMessages)
	if !ok || p.ID() != "anthropic-messages" {
		t.Fatal("not registered")
	}
}
