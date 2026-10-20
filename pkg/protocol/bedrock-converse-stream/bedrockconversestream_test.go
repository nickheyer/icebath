package bedrockconversestream

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
)

var p Protocol

// Tool definition from spec section 7.2
const inflationSchema = `{"type":"object","properties":{"year":{"type":"integer"}},"required":["year"]}`

func fixtureRequest() protocol.Request {
	temp := 0.5
	return protocol.Request{
		Model:  "anthropic.claude-3-sonnet-20240229-v1:0",
		System: "You are an economist with access to lots of data",
		Messages: []protocol.Message{
			{Role: protocol.RoleUser, Content: []protocol.Part{
				protocol.Text{Text: "What was the inflation rate in 2023?"},
				protocol.Image{MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
			}},
			{Role: protocol.RoleAssistant, Content: []protocol.Part{
				protocol.Thinking{Text: "I should look it up", Signature: "sig1"},
				protocol.Text{Text: "Let me check."},
				protocol.ToolCall{ID: "tooluse_placeholder_01", Name: "get_inflation", Args: json.RawMessage(`{"year":2023}`)},
			}},
			{Role: protocol.RoleTool, Content: []protocol.Part{
				protocol.ToolResult{CallID: "tooluse_placeholder_01", Name: "get_inflation", Content: []protocol.Part{protocol.Text{Text: "4.1"}}},
				protocol.ToolResult{CallID: "tooluse_placeholder_02", Name: "get_inflation", IsError: true, Content: []protocol.Part{
					protocol.Text{Text: "no data"},
					protocol.Image{MediaType: "image/jpeg", Data: []byte{0xff, 0xd8}},
				}},
			}},
		},
		Tools:         []protocol.Tool{{Name: "get_inflation", Description: "Returns the annual inflation rate for a year", Parameters: json.RawMessage(inflationSchema)}},
		ToolChoice:    protocol.ToolChoice{Mode: protocol.ToolChoiceAuto},
		MaxTokens:     1000,
		Temperature:   &temp,
		StopSequences: []string{"SUCCESS"},
		Stream:        true,
	}
}

func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	return m
}

func encodeMessages(t *testing.T, msgs ...protocol.Message) []any {
	t.Helper()
	call, err := p.Encode(protocol.Request{Model: "m", Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	return decodeBody(t, call.Body)["messages"].([]any)
}

func TestCapabilities(t *testing.T) {
	want := protocol.Capabilities{Thinking: true, RedactedThinking: true, Images: true, ImagesInResults: true, Documents: true, ToolChoiceNamed: true, StopSequences: true, Citations: true, Caching: true}
	if got := p.Capabilities(); got != want {
		t.Fatalf("%+v", got)
	}
}

func TestEncodeMatchesConverseStreamShape(t *testing.T) {
	call, err := p.Encode(fixtureRequest())
	if err != nil {
		t.Fatal(err)
	}
	if call.Method != "POST" || call.Path != "/model/anthropic.claude-3-sonnet-20240229-v1%3A0/converse-stream" {
		t.Fatalf("%s %s", call.Method, call.Path)
	}
	if call.Header.Get("Content-Type") != "application/json" || call.Header.Get("Accept") != "" {
		t.Fatalf("headers %v", call.Header)
	}
	m := decodeBody(t, call.Body)
	if m["system"].([]any)[0].(map[string]any)["text"] != "You are an economist with access to lots of data" {
		t.Fatal("system")
	}
	inf := m["inferenceConfig"].(map[string]any)
	if inf["maxTokens"] != 1000.0 || inf["temperature"] != 0.5 || inf["stopSequences"].([]any)[0] != "SUCCESS" {
		t.Fatalf("inferenceConfig %v", inf)
	}
	tc := m["toolConfig"].(map[string]any)
	spec := tc["tools"].([]any)[0].(map[string]any)["toolSpec"].(map[string]any)
	if spec["name"] != "get_inflation" || spec["description"] != "Returns the annual inflation rate for a year" {
		t.Fatalf("toolSpec %v", spec)
	}
	if schema := spec["inputSchema"].(map[string]any)["json"].(map[string]any); schema["type"] != "object" {
		t.Fatalf("inputSchema %v", schema)
	}
	if _, ok := tc["toolChoice"].(map[string]any)["auto"].(map[string]any); !ok {
		t.Fatalf("toolChoice %v", tc["toolChoice"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages %d", len(msgs))
	}
	user := msgs[0].(map[string]any)
	userContent := user["content"].([]any)
	if user["role"] != "user" || userContent[0].(map[string]any)["text"] != "What was the inflation rate in 2023?" {
		t.Fatalf("user %v", user)
	}
	img := userContent[1].(map[string]any)["image"].(map[string]any)
	if img["format"] != "png" || img["source"].(map[string]any)["bytes"] != base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G'}) {
		t.Fatalf("image %v", img)
	}
	asst := msgs[1].(map[string]any)
	asstContent := asst["content"].([]any)
	reasoning := asstContent[0].(map[string]any)["reasoningContent"].(map[string]any)["reasoningText"].(map[string]any)
	if asst["role"] != "assistant" || reasoning["text"] != "I should look it up" || reasoning["signature"] != "sig1" {
		t.Fatalf("reasoning %v", asst)
	}
	if asstContent[1].(map[string]any)["text"] != "Let me check." {
		t.Fatalf("assistant text %v", asstContent[1])
	}
	toolUse := asstContent[2].(map[string]any)["toolUse"].(map[string]any)
	if toolUse["toolUseId"] != "tooluse_placeholder_01" || toolUse["name"] != "get_inflation" || toolUse["input"].(map[string]any)["year"] != 2023.0 {
		t.Fatalf("toolUse %v", toolUse)
	}
	results := msgs[2].(map[string]any)
	resultContent := results["content"].([]any)
	if results["role"] != "user" || len(resultContent) != 2 {
		t.Fatalf("tool results %v", results)
	}
	first := resultContent[0].(map[string]any)["toolResult"].(map[string]any)
	if first["toolUseId"] != "tooluse_placeholder_01" || first["status"] != "success" || first["content"].([]any)[0].(map[string]any)["text"] != "4.1" {
		t.Fatalf("first result %v", first)
	}
	second := resultContent[1].(map[string]any)["toolResult"].(map[string]any)
	secondImage := second["content"].([]any)[1].(map[string]any)["image"].(map[string]any)
	if second["status"] != "error" || secondImage["format"] != "jpeg" {
		t.Fatalf("second result %v", second)
	}
	if strings.Contains(string(call.Body), "cachePoint") {
		t.Fatal("cache points must be off by default")
	}
}

func TestEncodeNonStreamingPathEscapesArn(t *testing.T) {
	req := fixtureRequest()
	req.Stream = false
	req.Model = "arn:aws:bedrock:us-west-2:123456789012:prompt/PROMPT12345:1"
	call, err := p.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	if call.Path != "/model/arn%3Aaws%3Abedrock%3Aus-west-2%3A123456789012%3Aprompt%2FPROMPT12345%3A1/converse" {
		t.Fatal(call.Path)
	}
}

func TestEncodeOmitsAbsentSections(t *testing.T) {
	call, err := p.Encode(protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(call.Body) != `{"messages":[{"role":"user","content":[{"text":"hi"}]}]}` {
		t.Fatalf("%s", call.Body)
	}
}

func TestEncodeToolChoiceAndRedactedThinking(t *testing.T) {
	req := fixtureRequest()
	req.ToolChoice = protocol.ToolChoice{Mode: protocol.ToolChoiceTool, Name: "get_inflation"}
	req.Messages[1].Content[0] = protocol.Thinking{Redacted: true, Signature: "AAAA"}
	call, err := p.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeBody(t, call.Body)
	if m["toolConfig"].(map[string]any)["toolChoice"].(map[string]any)["tool"].(map[string]any)["name"] != "get_inflation" {
		t.Fatal("tool choice")
	}
	redacted := m["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)["reasoningContent"].(map[string]any)
	if redacted["redactedContent"] != "AAAA" {
		t.Fatalf("%v", redacted)
	}
	req.ToolChoice = protocol.ToolChoice{Mode: protocol.ToolChoiceRequired}
	call, err = p.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeBody(t, call.Body)["toolConfig"].(map[string]any)["toolChoice"].(map[string]any)["any"]; !ok {
		t.Fatal("any")
	}
}

// Rule 3 replay policy for signed parts
func TestEncodeOriginPolicy(t *testing.T) {
	foreign := protocol.AnthropicMessages
	user := protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "q"}}}
	msgs := encodeMessages(t, user, protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{
		protocol.Thinking{Text: "foreign thought", Signature: "foreign-sig", Origin: foreign},
		protocol.Thinking{Redacted: true, Signature: "foreign-redacted", Origin: foreign},
		protocol.Thinking{Text: "same", Signature: "same-sig", Origin: protocol.BedrockConverseStream},
		protocol.Thinking{Redacted: true, Signature: "QUJD"},
		protocol.Thinking{Text: "empty", Signature: "empty-sig"},
		protocol.ToolCall{ID: "c1", Name: "f", Args: json.RawMessage(`{}`), Signature: "tool-sig", Origin: foreign},
		protocol.ServerToolCall{ID: "s1", Name: "web_search", Args: json.RawMessage(`{"q":"x"}`), Origin: foreign},
		protocol.ServerToolResult{CallID: "s1", Name: "web_search_tool_result", Content: json.RawMessage(`[]`), Origin: foreign},
	}})
	raw, _ := json.Marshal(msgs)
	for _, leaked := range []string{"foreign thought", "foreign-sig", "foreign-redacted", "tool-sig", "web_search"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("leaked %q in %s", leaked, raw)
		}
	}
	content := msgs[1].(map[string]any)["content"].([]any)
	if len(content) != 4 {
		t.Fatalf("%v", content)
	}
	if content[0].(map[string]any)["reasoningContent"].(map[string]any)["reasoningText"].(map[string]any)["text"] != "same" || content[1].(map[string]any)["reasoningContent"].(map[string]any)["redactedContent"] != "QUJD" || content[2].(map[string]any)["reasoningContent"].(map[string]any)["reasoningText"].(map[string]any)["text"] != "empty" {
		t.Fatalf("%v", content)
	}
	if _, has := content[3].(map[string]any)["toolUse"]; !has {
		t.Fatalf("%v", content[3])
	}
	msgs = encodeMessages(t, user, protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Thinking{Text: "gone", Signature: "s", Origin: foreign}}})
	if len(msgs) != 1 {
		t.Fatalf("empty assistant turn must be omitted: %v", msgs)
	}
}

func TestEncodeDocuments(t *testing.T) {
	msgs := encodeMessages(t,
		protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{
			protocol.Document{MediaType: "application/pdf", Data: []byte("%PDF"), Name: "report"},
			protocol.Document{MediaType: "text/markdown", Data: []byte("# hi")},
			protocol.Document{MediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", Data: []byte{1}},
		}},
		protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{ID: "c1", Name: "f"}}},
		protocol.Message{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "c1", Content: []protocol.Part{protocol.Document{MediaType: "text/csv", Data: []byte("a,b"), Name: "t"}}}}},
	)
	content := msgs[0].(map[string]any)["content"].([]any)
	first := content[0].(map[string]any)["document"].(map[string]any)
	if first["format"] != "pdf" || first["name"] != "report" || first["source"].(map[string]any)["bytes"] != "JVBERg==" {
		t.Fatalf("%v", first)
	}
	if second := content[1].(map[string]any)["document"].(map[string]any); second["format"] != "md" || second["name"] != "document-1" {
		t.Fatalf("%v", second)
	}
	if third := content[2].(map[string]any)["document"].(map[string]any); third["format"] != "xlsx" || third["name"] != "document-2" {
		t.Fatalf("%v", third)
	}
	result := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)["toolResult"].(map[string]any)
	if doc := result["content"].([]any)[0].(map[string]any)["document"].(map[string]any); doc["format"] != "csv" || doc["name"] != "t" {
		t.Fatalf("%v", doc)
	}
	for name, doc := range map[string]protocol.Document{
		"media type": {MediaType: "application/zip", Data: []byte{1}},
		"no data":    {MediaType: "application/pdf", Name: "x"},
	} {
		if _, err := p.Encode(protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{doc}}}}); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// Prompt caching guide, cache points after the system text and the tool list
func TestEncodeCachePoints(t *testing.T) {
	req := fixtureRequest()
	req.Extra = map[string]json.RawMessage{"bedrock_cache": json.RawMessage(`true`), "additionalModelRequestFields": json.RawMessage(`{"top_k":5}`)}
	call, err := p.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeBody(t, call.Body)
	if _, has := m["bedrock_cache"]; has {
		t.Fatal("bedrock_cache must not reach the body")
	}
	if m["additionalModelRequestFields"].(map[string]any)["top_k"] != 5.0 {
		t.Fatal("other extra fields must still merge")
	}
	system := m["system"].([]any)
	if len(system) != 2 || system[1].(map[string]any)["cachePoint"].(map[string]any)["type"] != "default" {
		t.Fatalf("system %v", system)
	}
	tools := m["toolConfig"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 || tools[1].(map[string]any)["cachePoint"].(map[string]any)["type"] != "default" {
		t.Fatalf("tools %v", tools)
	}
	req.Extra = map[string]json.RawMessage{"bedrock_cache": json.RawMessage(`false`)}
	call, _ = p.Encode(req)
	if strings.Contains(string(call.Body), "cachePoint") || strings.Contains(string(call.Body), "bedrock_cache") {
		t.Fatalf("%s", call.Body)
	}
	req.Extra = map[string]json.RawMessage{"bedrock_cache": json.RawMessage(`"yes"`)}
	if _, err := p.Encode(req); err == nil {
		t.Fatal("non boolean switch accepted")
	}
	req.Extra = map[string]json.RawMessage{"bedrock_cache": json.RawMessage(`true`)}
	req.System, req.Tools, req.ToolChoice = "", nil, protocol.ToolChoice{}
	call, _ = p.Encode(req)
	if strings.Contains(string(call.Body), "cachePoint") {
		t.Fatal("cache points need a system or tool list to follow")
	}
}

func TestEncodeExtraMerges(t *testing.T) {
	req := fixtureRequest()
	req.Extra = map[string]json.RawMessage{
		"additionalModelRequestFields": json.RawMessage(`{"top_k":200}`),
		"inferenceConfig":              json.RawMessage(`{"topP":0.9}`),
	}
	call, err := p.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeBody(t, call.Body)
	inf := m["inferenceConfig"].(map[string]any)
	if m["additionalModelRequestFields"].(map[string]any)["top_k"] != 200.0 || inf["topP"] != 0.9 || inf["maxTokens"] != 1000.0 {
		t.Fatalf("%v", m)
	}
}

func TestEncodeErrors(t *testing.T) {
	user := func(parts ...protocol.Part) protocol.Message {
		return protocol.Message{Role: protocol.RoleUser, Content: parts}
	}
	cases := map[string]func(*protocol.Request){
		"no model":                 func(r *protocol.Request) { r.Model = "" },
		"tool choice none":         func(r *protocol.Request) { r.ToolChoice = protocol.ToolChoice{Mode: protocol.ToolChoiceNone} },
		"unknown tool choice":      func(r *protocol.Request) { r.ToolChoice = protocol.ToolChoice{Mode: "weird"} },
		"tool choice without name": func(r *protocol.Request) { r.ToolChoice = protocol.ToolChoice{Mode: protocol.ToolChoiceTool} },
		"tool choice without tools": func(r *protocol.Request) {
			r.Tools = nil
		},
		"tool without schema": func(r *protocol.Request) { r.Tools[0].Parameters = nil },
		"unknown role":        func(r *protocol.Request) { r.Messages[0].Role = "system" },
		"empty message":       func(r *protocol.Request) { r.Messages[0].Content = nil },
		"image media type": func(r *protocol.Request) {
			r.Messages[0].Content[1] = protocol.Image{MediaType: "image/bmp", Data: []byte{1}}
		},
		"image without bytes": func(r *protocol.Request) { r.Messages[0].Content[1] = protocol.Image{MediaType: "image/png"} },
		"tool result in user": func(r *protocol.Request) { r.Messages[0] = user(protocol.ToolResult{CallID: "x"}) },
		"thinking in user":    func(r *protocol.Request) { r.Messages[0] = user(protocol.Thinking{Text: "t"}) },
		"image in assistant": func(r *protocol.Request) {
			r.Messages[1].Content[1] = protocol.Image{MediaType: "image/png", Data: []byte{1}}
		},
		"document in assistant": func(r *protocol.Request) {
			r.Messages[1].Content[1] = protocol.Document{MediaType: "application/pdf", Data: []byte{1}}
		},
		"text in tool message": func(r *protocol.Request) {
			r.Messages[2].Content = append(r.Messages[2].Content, protocol.Text{Text: "stray"})
		},
		"tool call in result": func(r *protocol.Request) {
			r.Messages[2].Content[0] = protocol.ToolResult{CallID: "x", Content: []protocol.Part{protocol.ToolCall{ID: "y"}}}
		},
		"result image no bytes": func(r *protocol.Request) {
			r.Messages[2].Content[0] = protocol.ToolResult{CallID: "x", Content: []protocol.Part{protocol.Image{MediaType: "image/png"}}}
		},
		"result document type": func(r *protocol.Request) {
			r.Messages[2].Content[0] = protocol.ToolResult{CallID: "x", Content: []protocol.Part{protocol.Document{MediaType: "x/y", Data: []byte{1}}}}
		},
		"bad tool call args": func(r *protocol.Request) {
			r.Messages[1].Content[2] = protocol.ToolCall{ID: "x", Name: "f", Args: json.RawMessage(`[1]`)}
		},
		"null tool call args": func(r *protocol.Request) {
			r.Messages[1].Content[2] = protocol.ToolCall{ID: "x", Name: "f", Args: json.RawMessage(`null`)}
		},
		"redacted without payload": func(r *protocol.Request) { r.Messages[1].Content[0] = protocol.Thinking{Redacted: true} },
		"bad extra":                func(r *protocol.Request) { r.Extra = map[string]json.RawMessage{"x": json.RawMessage(`{`)} },
	}
	for name, mutate := range cases {
		req := fixtureRequest()
		mutate(&req)
		if _, err := p.Encode(req); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// Response from spec section 7.3 with the toolUse block
const toolUseResponse = `{
    "output": {
        "message": {
            "role": "assistant",
            "content": [
                { "text": "<text generated by the model>" },
                {
                    "toolUse": {
                        "toolUseId": "tooluse_placeholder_01",
                        "name": "get_inflation",
                        "input": { "year": 2023 }
                    }
                }
            ]
        }
    },
    "stopReason": "tool_use",
    "usage": {
        "inputTokens": 30,
        "outputTokens": 628,
        "totalTokens": 658
    },
    "metrics": {
        "latencyMs": 1275
    }
}`

func TestDecodeToolUseResponse(t *testing.T) {
	resp, err := p.Decode(strings.NewReader(toolUseResponse))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != protocol.StopToolUse || resp.RawStopReason != "tool_use" || resp.ID != "" || resp.Model != "" {
		t.Fatalf("%+v", resp)
	}
	if resp.Usage != (protocol.Usage{Input: 30, Output: 628}) {
		t.Fatalf("usage %+v", resp.Usage)
	}
	if resp.Metrics != (protocol.Metrics{Latency: 1275 * time.Millisecond}) {
		t.Fatalf("metrics %+v", resp.Metrics)
	}
	if resp.Content[0].(protocol.Text).Text != "<text generated by the model>" {
		t.Fatal("text")
	}
	tc := resp.Content[1].(protocol.ToolCall)
	if tc.ID != "tooluse_placeholder_01" || tc.Name != "get_inflation" || string(tc.Args) != `{"year":2023}` {
		t.Fatalf("%+v", tc)
	}
}

func TestDecodeStopSequenceAndReasoning(t *testing.T) {
	body := `{"output":{"message":{"role":"assistant","content":[
		{"reasoningContent":{"reasoningText":{"text":"think","signature":"s1"}}},
		{"reasoningContent":{"redactedContent":"QUJD"}},
		{"text":"done"}]}},
		"stopReason":"stop_sequence",
		"usage":{"inputTokens":51,"outputTokens":442,"totalTokens":493,"cacheReadInputTokens":7,"cacheWriteInputTokens":3},
		"metrics":{"latencyMs":7944}}`
	resp, err := p.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != protocol.StopEnd || resp.RawStopReason != "stop_sequence" || resp.Usage.CacheRead != 7 || resp.Usage.CacheWrite != 3 {
		t.Fatalf("%+v", resp)
	}
	if th := resp.Content[0].(protocol.Thinking); th.Text != "think" || th.Signature != "s1" || th.Redacted || th.Origin != protocol.BedrockConverseStream {
		t.Fatalf("%+v", th)
	}
	if th := resp.Content[1].(protocol.Thinking); !th.Redacted || th.Signature != "QUJD" || th.Origin != protocol.BedrockConverseStream {
		t.Fatalf("%+v", th)
	}
	for raw, want := range map[string]protocol.StopReason{
		"end_turn": protocol.StopEnd, "max_tokens": protocol.StopLength, "model_context_window_exceeded": protocol.StopLength,
		"guardrail_intervened": protocol.StopContentFilter, "content_filtered": protocol.StopContentFilter,
		"malformed_model_output": protocol.StopOther, "malformed_tool_use": protocol.StopOther, "future_value": protocol.StopOther,
	} {
		if mapStop(raw) != want {
			t.Errorf("%s -> %s", raw, mapStop(raw))
		}
	}
}

// Citations guide response shape, with cited text following a plain text block
func TestDecodeCitationsDocumentsAndMedia(t *testing.T) {
	body := `{"output":{"message":{"role":"assistant","content":[
		{"text":"Per the report, "},
		{"citationsContent":{"content":[{"text":"inflation was 4.1%."}],"citations":[
		  {"title":"CPI 2023","sourceContent":[{"text":"4.1 percent"}],"location":{"documentChar":{"documentIndex":0,"start":10,"end":21}}},
		  {"title":"Pages","sourceContent":[],"location":{"documentPage":{"documentIndex":1,"start":2,"end":3}}},
		  {"location":{"documentChunk":{"documentIndex":2,"start":5,"end":6}}}
		]}},
		{"document":{"format":"pdf","name":"summary","source":{"bytes":"JVBERg=="}}},
		{"video":{"format":"mp4","source":{"bytes":"AQID"}}},
		{"video":{"format":"three_gp","source":{"bytes":"AQID"}}},
		{"image":{"format":"png","source":{"bytes":"AQID"}}},
		{"citationsContent":{"content":[{"text":"Standalone."}],"citations":[]}}
	]}},"stopReason":"end_turn","usage":{}}`
	resp, err := p.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.Part{
		protocol.Text{Text: "Per the report, inflation was 4.1%.", Citations: []protocol.Citation{
			{Title: "CPI 2023", Document: "0", Start: 10, End: 21, Cited: "4.1 percent"},
			{Title: "Pages", Document: "1", Start: 2, End: 3},
			{Document: "2", Start: 5, End: 6},
		}},
		protocol.Document{MediaType: "application/pdf", Data: []byte("%PDF"), Name: "summary"},
		protocol.Document{MediaType: "video/mp4", Data: []byte{1, 2, 3}},
		protocol.Document{MediaType: "video/3gpp", Data: []byte{1, 2, 3}},
		protocol.Image{MediaType: "image/png", Data: []byte{1, 2, 3}},
		protocol.Text{Text: "Standalone.", Citations: []protocol.Citation{}},
	}
	if !reflect.DeepEqual(resp.Content, want) {
		t.Fatalf("%+v", resp.Content)
	}
}

func TestDecodeRejectsUnknownBlocks(t *testing.T) {
	cases := []string{
		`{"output":{"message":{"content":[{"guardContent":{}}]}},"stopReason":"end_turn","usage":{}}`,
		`{"output":{"message":{"content":[{"toolUse":{"toolUseId":"a","name":"f","input":[1]}}]}},"stopReason":"end_turn","usage":{}}`,
		`{"output":{"message":{"content":[{"toolUse":{"toolUseId":"a","name":"f","input":{]}}]}},"stopReason":"end_turn","usage":{}}`,
		`{"output":{"message":{"content":[{"reasoningContent":{}}]}},"stopReason":"end_turn","usage":{}}`,
		`{"output":{"message":{"content":[{"reasoningContent":{"redactedContent":"!!!"}}]}},"stopReason":"end_turn","usage":{}}`,
		`{"output":{"message":{"content":[{"document":{"format":"exe","name":"x","source":{"bytes":"AQ=="}}}]}},"stopReason":"end_turn","usage":{}}`,
		`{"output":{"message":{"content":[{"document":{"format":"pdf","name":"x","source":{"s3Location":{"uri":"s3://b/k"}}}}]}},"stopReason":"end_turn","usage":{}}`,
		`{"output":{"message":{"content":[{"video":{"format":"mp4","source":{}}}]}},"stopReason":"end_turn","usage":{}}`,
		`{"output":{"message":{"content":[{"image":{"format":"bmp","source":{"bytes":"AQ=="}}}]}},"stopReason":"end_turn","usage":{}}`,
		`{"output":{"message":{"content":[{"image":{"format":"png","source":{}}}]}},"stopReason":"end_turn","usage":{}}`,
		`not json`,
	}
	for _, c := range cases {
		if _, err := p.Decode(strings.NewReader(c)); err == nil {
			t.Errorf("accepted %s", c)
		}
	}
}

func eventFrame(t *testing.T, eventType, payload string) []byte {
	t.Helper()
	b, err := Frame{Headers: map[string]any{":message-type": "event", ":event-type": eventType, ":content-type": "application/json"}, Payload: []byte(payload)}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Event sequence from spec section 7.4 with the reasoning variant prepended
func toolUseStream(t *testing.T) [][]byte {
	t.Helper()
	return [][]byte{
		eventFrame(t, "messageStart", `{"role":"assistant"}`),
		eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"text":"<reasoning text>"}}}`),
		eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"signature":"<opaque signature>"}}}`),
		eventFrame(t, "contentBlockStop", `{"contentBlockIndex":0}`),
		eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":1,"delta":{"text":""}}`),
		eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":1,"delta":{"text":"<text generated by the model>"}}`),
		eventFrame(t, "contentBlockStop", `{"contentBlockIndex":1}`),
		eventFrame(t, "contentBlockStart", `{"contentBlockIndex":2,"start":{"toolUse":{"toolUseId":"tooluse_placeholder_01","name":"get_inflation"}}}`),
		eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":2,"delta":{"toolUse":{"input":"{\"year\""}}}`),
		eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":2,"delta":{"toolUse":{"input":": 2023}"}}}`),
		eventFrame(t, "contentBlockStop", `{"contentBlockIndex":2}`),
		eventFrame(t, "messageStop", `{"stopReason":"tool_use"}`),
		eventFrame(t, "metadata", `{"usage":{"inputTokens":30,"outputTokens":628,"totalTokens":658},"metrics":{"latencyMs":1275}}`),
	}
}

func TestStreamToolUse(t *testing.T) {
	var types []protocol.EventType
	var final protocol.Response
	for ev, err := range p.Stream(bytes.NewReader(bytes.Join(toolUseStream(t), nil))) {
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, ev.Type)
		if ev.Type == protocol.EventDone {
			final = ev.Response
		}
	}
	want := []protocol.EventType{
		protocol.EventStart,
		protocol.EventThinkingStart, protocol.EventThinkingDelta, protocol.EventThinkingEnd,
		protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextDelta, protocol.EventTextEnd,
		protocol.EventToolCallStart, protocol.EventToolCallDelta, protocol.EventToolCallDelta, protocol.EventToolCallEnd,
		protocol.EventDone,
	}
	if len(types) != len(want) {
		t.Fatalf("%v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("event %d = %s want %s", i, types[i], want[i])
		}
	}
	if final.StopReason != protocol.StopToolUse || final.RawStopReason != "tool_use" || final.Usage != (protocol.Usage{Input: 30, Output: 628}) {
		t.Fatalf("%+v", final)
	}
	if final.Metrics != (protocol.Metrics{Latency: 1275 * time.Millisecond}) {
		t.Fatalf("metrics %+v", final.Metrics)
	}
	if th := final.Content[0].(protocol.Thinking); th.Text != "<reasoning text>" || th.Signature != "<opaque signature>" || th.Origin != protocol.BedrockConverseStream {
		t.Fatalf("%+v", th)
	}
	if final.Content[1].(protocol.Text).Text != "<text generated by the model>" {
		t.Fatal("text")
	}
	tc := final.Content[2].(protocol.ToolCall)
	if tc.ID != "tooluse_placeholder_01" || tc.Name != "get_inflation" || string(tc.Args) != `{"year": 2023}` {
		t.Fatalf("%+v", tc)
	}
	collected, err := protocol.Collect(p.Stream(bytes.NewReader(bytes.Join(toolUseStream(t), nil))))
	if err != nil || len(collected.Content) != 3 {
		t.Fatalf("%+v %v", collected, err)
	}
}

func TestStreamRedactedReasoning(t *testing.T) {
	frames := [][]byte{
		eventFrame(t, "messageStart", `{"role":"assistant"}`),
		eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"redactedContent":"QUJ"}}}`),
		eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"redactedContent":"DREVG"}}}`),
		eventFrame(t, "contentBlockStop", `{"contentBlockIndex":0}`),
		eventFrame(t, "messageStop", `{"stopReason":"end_turn"}`),
		eventFrame(t, "metadata", `{"usage":{"inputTokens":1,"outputTokens":2,"totalTokens":3},"metrics":{"latencyMs":5}}`),
	}
	resp, err := protocol.Collect(p.Stream(bytes.NewReader(bytes.Join(frames, nil))))
	if err != nil {
		t.Fatal(err)
	}
	if th := resp.Content[0].(protocol.Thinking); !th.Redacted || th.Signature != "QUJDREVG" || th.Text != "" || th.Origin != protocol.BedrockConverseStream {
		t.Fatalf("%+v", th)
	}
	unstopped := append(append([][]byte{}, frames[:3]...), frames[4], frames[5])
	resp, err = protocol.Collect(p.Stream(bytes.NewReader(bytes.Join(unstopped, nil))))
	if err != nil || resp.Content[0].(protocol.Thinking).Signature != "QUJDREVG" {
		t.Fatalf("%+v %v", resp, err)
	}
	invalid := append(append([][]byte{}, frames[:2]...), eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"redactedContent":"!"}}}`), frames[3])
	if _, err := protocol.Collect(p.Stream(bytes.NewReader(bytes.Join(invalid, nil)))); err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("invalid base64 accepted: %v", err)
	}
}

// Citation deltas from the ConverseStream CitationsDelta shape, alongside the cited text
func TestStreamCitations(t *testing.T) {
	frames := [][]byte{
		eventFrame(t, "messageStart", `{"role":"assistant"}`),
		eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"citation":{"title":"CPI","sourceContent":[{"text":"4.1"}],"location":{"documentChar":{"documentIndex":0,"start":1,"end":4}}}}}`),
		eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"4.1%"}}`),
		eventFrame(t, "contentBlockStop", `{"contentBlockIndex":0}`),
		eventFrame(t, "messageStop", `{"stopReason":"end_turn"}`),
		eventFrame(t, "metadata", `{"usage":{"inputTokens":1,"outputTokens":2},"metrics":{"latencyMs":5}}`),
	}
	var types []protocol.EventType
	var final protocol.Response
	for ev, err := range p.Stream(bytes.NewReader(bytes.Join(frames, nil))) {
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, ev.Type)
		final = ev.Response
	}
	want := []protocol.EventType{protocol.EventStart, protocol.EventTextStart, protocol.EventCitation, protocol.EventTextDelta, protocol.EventTextEnd, protocol.EventDone}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("%v", types)
	}
	cite := protocol.Citation{Title: "CPI", Document: "0", Start: 1, End: 4, Cited: "4.1"}
	if text := final.Content[0].(protocol.Text); text.Text != "4.1%" || len(text.Citations) != 1 || text.Citations[0] != cite {
		t.Fatalf("%+v", text)
	}
	onTool := [][]byte{frames[0], eventFrame(t, "contentBlockStart", `{"contentBlockIndex":0,"start":{"toolUse":{"toolUseId":"a","name":"f"}}}`), frames[1]}
	if _, err := protocol.Collect(p.Stream(bytes.NewReader(bytes.Join(onTool, nil)))); err == nil {
		t.Fatal("citation on a tool block accepted")
	}
}

func TestStreamExceptionAndErrorFrames(t *testing.T) {
	exception, err := Frame{Headers: map[string]any{":message-type": "exception", ":exception-type": "throttlingException", ":content-type": "application/json"}, Payload: []byte(`{"message":"Too many requests"}`)}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	validation, err := Frame{Headers: map[string]any{":message-type": "exception", ":exception-type": "validationException", ":content-type": "application/json"}, Payload: []byte(`{"message":"bad"}`)}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	errFrame, err := Frame{Headers: map[string]any{":message-type": "error", ":error-code": "InternalError", ":error-message": "boom"}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	start := eventFrame(t, "messageStart", `{"role":"assistant"}`)
	for _, c := range []struct {
		data      []byte
		typ, msg  string
		transient bool
	}{
		{append(append([]byte{}, start...), exception...), "throttlingException", "Too many requests", true},
		{validation, "validationException", "bad", false},
		{errFrame, "InternalError", "boom", false},
	} {
		var got *protocol.Error
		for _, err := range p.Stream(bytes.NewReader(c.data)) {
			if err != nil && !errors.As(err, &got) {
				t.Fatalf("unexpected %v", err)
			}
		}
		if got == nil || got.Status != 200 || got.Type != c.typ || got.Message != c.msg || !got.InStream || got.Transient != c.transient {
			t.Fatalf("%+v", got)
		}
	}
}

func TestStreamRejectsBrokenSequences(t *testing.T) {
	full := toolUseStream(t)
	cases := map[string][][]byte{
		"cut before metadata":   full[:len(full)-1],
		"metadata before stop":  append(append([][]byte{}, full[:len(full)-2]...), full[len(full)-1]),
		"toolUse without start": {full[0], eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"toolUse":{"input":"{}"}}}`)},
		"kind change":           {full[0], full[4], eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":1,"delta":{"reasoningContent":{"text":"x"}}}`)},
		"unknown event":         {full[0], eventFrame(t, "somethingElse", `{}`)},
		"missing message type":  {mustEncode(t, Frame{Payload: []byte(`{}`)})},
		"non toolUse start":     {full[0], eventFrame(t, "contentBlockStart", `{"contentBlockIndex":0,"start":{"image":{"format":"png"}}}`)},
		"repeated start":        {full[0], full[7], full[7]},
		"empty delta":           {full[0], eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{}}`)},
		"redacted after text":   {full[0], full[1], eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"redactedContent":"QUJD"}}}`)},
		"bad start json":        {full[0], eventFrame(t, "contentBlockStart", `{`)},
		"bad delta json":        {full[0], eventFrame(t, "contentBlockDelta", `{`)},
		"bad stop json":         {full[0], eventFrame(t, "contentBlockStop", `{`)},
		"bad message stop":      {full[0], eventFrame(t, "messageStop", `{`)},
		"bad metadata":          {full[0], full[11], eventFrame(t, "metadata", `{`)},
		"bad exception":         {mustEncode(t, Frame{Headers: map[string]any{":message-type": "exception"}, Payload: []byte(`{`)})},
		"bad tool args":         {full[0], full[7], eventFrame(t, "contentBlockDelta", `{"contentBlockIndex":2,"delta":{"toolUse":{"input":"["}}}`), full[10]},
		"truncated frame":       {full[0][:10]},
	}
	for name, frames := range cases {
		failed := false
		for _, err := range p.Stream(bytes.NewReader(bytes.Join(frames, nil))) {
			if err != nil {
				failed = true
			}
		}
		if !failed {
			t.Errorf("%s: no error", name)
		}
	}
}

func mustEncode(t *testing.T, f Frame) []byte {
	t.Helper()
	b, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeError(t *testing.T) {
	e := p.DecodeError(400, []byte(`{"__type":"ValidationException","message":"__Message__"}`))
	if e.Status != 400 || e.Type != "ValidationException" || e.Message != "__Message__" || e.Transient || e.InStream {
		t.Fatalf("%+v", e)
	}
	e = p.DecodeError(400, []byte(`{"__type":"com.amazon.coral.validate#ValidationException:http://internal/","message":"bad"}`))
	if e.Type != "ValidationException" || e.Message != "bad" {
		t.Fatalf("%+v", e)
	}
	e = p.DecodeError(400, []byte(`{"__type":"ThrottlingException","message":"slow down"}`))
	if !e.Transient {
		t.Fatal("ThrottlingException must be transient")
	}
	e = p.DecodeError(503, []byte("  Service Unavailable\n"))
	if e.Type != "" || e.Message != "Service Unavailable" || string(e.Body) != "  Service Unavailable\n" || !e.Transient {
		t.Fatalf("%+v", e)
	}
	if !strings.Contains(e.Error(), "503") {
		t.Fatal(e.Error())
	}
	for status, want := range map[int]bool{408: true, 409: true, 429: true, 500: true, 404: false} {
		if got := p.DecodeError(status, []byte("x")).Transient; got != want {
			t.Fatalf("%d transient %v", status, got)
		}
	}
}

func TestRegistered(t *testing.T) {
	p, ok := protocol.Lookup(protocol.BedrockConverseStream)
	if !ok || p.ID() != "bedrock-converse-stream" {
		t.Fatal("not registered")
	}
}
