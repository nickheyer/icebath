package googlegenerativeai

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
)

var schema = json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)

func fixtureRequest(stream bool) protocol.Request {
	temp := 0.5
	return protocol.Request{
		Model:  "gemini-2.0-flash",
		System: "You are terse.",
		Messages: []protocol.Message{
			{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "Weather?"}, protocol.Image{MediaType: "image/png", Data: []byte{1, 2, 3}}}},
			{Role: protocol.RoleAssistant, Content: []protocol.Part{
				protocol.Thinking{Text: "Need two cities.", Signature: "SIG_T"},
				protocol.Text{Text: "Checking."},
				protocol.Thinking{Signature: "SIG_TEXT"},
				protocol.ToolCall{ID: "c1", Name: "get_weather", Args: json.RawMessage(`{"city": "Paris"}`), Signature: "SIG_A"},
				protocol.ToolCall{Name: "get_weather", Args: json.RawMessage(`{"city":"London"}`)},
			}},
			{Role: protocol.RoleTool, Content: []protocol.Part{
				protocol.ToolResult{CallID: "c1", Name: "get_weather", Content: []protocol.Part{protocol.Text{Text: "15C"}, protocol.Image{MediaType: "image/jpeg", Data: []byte{9}}}},
				protocol.ToolResult{Name: "get_weather", Content: []protocol.Part{protocol.Text{Text: "down"}, protocol.Text{Text: "retry"}}, IsError: true},
			}},
		},
		Tools:         []protocol.Tool{{Name: "get_weather", Description: "Current weather", Parameters: schema}},
		ToolChoice:    protocol.ToolChoice{Mode: protocol.ToolChoiceTool, Name: "get_weather"},
		MaxTokens:     256,
		Temperature:   &temp,
		StopSequences: []string{"END"},
		Stream:        stream,
	}
}

func encodeMap(t *testing.T, req protocol.Request) (protocol.Call, map[string]any) {
	t.Helper()
	call, err := Protocol{}.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(call.Body, &m); err != nil {
		t.Fatal(err)
	}
	return call, m
}

// Spec sections 1.2, 2.1 to 2.13 and the Vertex thought signature REST examples
func TestEncodeRequest(t *testing.T) {
	call, m := encodeMap(t, fixtureRequest(true))
	if call.Method != "POST" || call.Path != "/v1beta/models/gemini-2.0-flash:streamGenerateContent" || call.Query.Get("alt") != "sse" {
		t.Fatalf("call %+v", call)
	}
	if call.Header.Get("Content-Type") != "application/json" || call.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("headers %v", call.Header)
	}
	want := map[string]any{
		"systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "You are terse."}}},
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{
				map[string]any{"text": "Weather?"},
				map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "AQID"}},
			}},
			map[string]any{"role": "model", "parts": []any{
				map[string]any{"thought": true, "text": "Need two cities.", "thoughtSignature": "SIG_T"},
				map[string]any{"text": "Checking.", "thoughtSignature": "SIG_TEXT"},
				map[string]any{"functionCall": map[string]any{"id": "c1", "name": "get_weather", "args": map[string]any{"city": "Paris"}}, "thoughtSignature": "SIG_A"},
				map[string]any{"functionCall": map[string]any{"name": "get_weather", "args": map[string]any{"city": "London"}}},
			}},
			map[string]any{"role": "user", "parts": []any{
				map[string]any{"functionResponse": map[string]any{"id": "c1", "name": "get_weather", "response": map[string]any{"result": "15C"}, "parts": []any{map[string]any{"inlineData": map[string]any{"mimeType": "image/jpeg", "data": "CQ=="}}}}},
				map[string]any{"functionResponse": map[string]any{"name": "get_weather", "response": map[string]any{"error": "down\nretry"}}},
			}},
		},
		"tools": []any{map[string]any{"functionDeclarations": []any{map[string]any{
			"name": "get_weather", "description": "Current weather",
			"parametersJsonSchema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []any{"city"}},
		}}}},
		"toolConfig":       map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY", "allowedFunctionNames": []any{"get_weather"}}},
		"generationConfig": map[string]any{"maxOutputTokens": 256.0, "temperature": 0.5, "stopSequences": []any{"END"}},
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("body mismatch\n got %s", call.Body)
	}
	if strings.Contains(string(call.Body), `"args":{"city": "Paris"}`) {
		t.Fatal("arguments must be compacted")
	}
}

func TestEncodePathsAndModes(t *testing.T) {
	call, m := encodeMap(t, protocol.Request{Model: "models/gemini-2.0-flash", Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}}}})
	if call.Path != "/v1beta/models/gemini-2.0-flash:generateContent" || len(call.Query) != 0 || call.Header.Get("Accept") != "application/json" {
		t.Fatalf("call %+v", call)
	}
	for _, k := range []string{"systemInstruction", "tools", "toolConfig", "generationConfig"} {
		if _, ok := m[k]; ok {
			t.Fatalf("%s must be omitted", k)
		}
	}
	modes := map[protocol.ToolChoiceMode]string{protocol.ToolChoiceAuto: "AUTO", protocol.ToolChoiceNone: "NONE", protocol.ToolChoiceRequired: "ANY"}
	for mode, want := range modes {
		req := protocol.Request{Model: "m", ToolChoice: protocol.ToolChoice{Mode: mode}, Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}}}}
		_, m := encodeMap(t, req)
		got := m["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)
		if got["mode"] != want || got["allowedFunctionNames"] != nil {
			t.Fatalf("%s -> %v", mode, got)
		}
	}
}

// Vertex thought signatures doc, rule that a signature stays inside its original Part
func TestSignatureRoundTrip(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"Hi there","thoughtSignature":"SIG_C"}]},"finishReason":"STOP"}],"responseId":"r1","modelVersion":"gemini-3-pro"}`
	resp, err := Protocol{}.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.Part{protocol.Text{Text: "Hi there"}, protocol.Thinking{Signature: "SIG_C"}}
	if !reflect.DeepEqual(resp.Content, want) || resp.StopReason != protocol.StopEnd {
		t.Fatalf("%+v", resp)
	}
	_, m := encodeMap(t, protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: resp.Content}}})
	parts := m["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	if len(parts) != 1 || !reflect.DeepEqual(parts[0], map[string]any{"text": "Hi there", "thoughtSignature": "SIG_C"}) {
		t.Fatalf("parts %v", parts)
	}
	_, m = encodeMap(t, protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Thinking{Signature: "SIG_D"}}}}})
	parts = m["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	if !reflect.DeepEqual(parts[0], map[string]any{"text": "", "thoughtSignature": "SIG_D"}) {
		t.Fatalf("parts %v", parts)
	}
}

func TestEncodeExtraMerge(t *testing.T) {
	req := protocol.Request{Model: "m", MaxTokens: 10, Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}}}}
	req.Extra = map[string]json.RawMessage{"generationConfig": json.RawMessage(`{"thinkingConfig":{"includeThoughts":true}}`)}
	_, m := encodeMap(t, req)
	gc := m["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != 10.0 || !reflect.DeepEqual(gc["thinkingConfig"], map[string]any{"includeThoughts": true}) {
		t.Fatalf("%v", gc)
	}
}

func TestEncodeErrors(t *testing.T) {
	user := func(parts ...protocol.Part) protocol.Message {
		return protocol.Message{Role: protocol.RoleUser, Content: parts}
	}
	hi := user(protocol.Text{Text: "hi"})
	cases := map[string]protocol.Request{
		"no model":                 {Messages: []protocol.Message{hi}},
		"empty message":            {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleUser}}},
		"unknown role":             {Model: "m", Messages: []protocol.Message{{Role: "system", Content: []protocol.Part{protocol.Text{Text: "x"}}}}},
		"tool result in user":      {Model: "m", Messages: []protocol.Message{user(protocol.ToolResult{Name: "f"})}},
		"thinking in user":         {Model: "m", Messages: []protocol.Message{user(protocol.Thinking{Text: "x"})}},
		"text in tool":             {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.Text{Text: "x"}}}}},
		"tool result no name":      {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "c"}}}}},
		"thinking in result":       {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{Name: "f", Content: []protocol.Part{protocol.Thinking{Text: "x"}}}}}}},
		"tool without schema":      {Model: "m", Messages: []protocol.Message{hi}, Tools: []protocol.Tool{{Name: "f"}}},
		"redacted thinking":        {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Thinking{Redacted: true, Signature: "s"}}}}},
		"image without type":       {Model: "m", Messages: []protocol.Message{user(protocol.Image{Data: []byte{1}})}},
		"tool choice without name": {Model: "m", Messages: []protocol.Message{hi}, ToolChoice: protocol.ToolChoice{Mode: protocol.ToolChoiceTool}},
		"unknown tool choice":      {Model: "m", Messages: []protocol.Message{hi}, ToolChoice: protocol.ToolChoice{Mode: "maybe"}},
		"bad call args":            {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{Name: "f", Args: json.RawMessage(`[1]`)}}}}},
		"result in assistant":      {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolResult{Name: "f"}}}}},
		"bad extra":                {Model: "m", Messages: []protocol.Message{hi}, Extra: map[string]json.RawMessage{"x": json.RawMessage(`{`)}},
	}
	for name, req := range cases {
		if _, err := (Protocol{}).Encode(req); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// Spec sections 2.5, 3.1 to 3.5, assembled from the schema
func TestDecodeFunctionCall(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"Need Paris."},{"text":"Looking it up."},{"functionCall":{"id":"fc1","name":"get_weather","args":{"city": "Paris"}},"thoughtSignature":"SIG_A"},{"functionCall":{"name":"get_weather","args":{"city":"London"}}}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":9,"cachedContentTokenCount":4,"thoughtsTokenCount":3,"totalTokenCount":36},"modelVersion":"gemini-3-pro","responseId":"abc"}`
	resp, err := Protocol{}.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	want := protocol.Response{ID: "abc", Model: "gemini-3-pro", StopReason: protocol.StopToolUse, RawStopReason: "STOP",
		Usage: protocol.Usage{Input: 20, Output: 9, CacheRead: 4, Reasoning: 3},
		Content: []protocol.Part{
			protocol.Thinking{Text: "Need Paris."},
			protocol.Text{Text: "Looking it up."},
			protocol.ToolCall{ID: "fc1", Name: "get_weather", Args: json.RawMessage(`{"city":"Paris"}`), Signature: "SIG_A"},
			protocol.ToolCall{Name: "get_weather", Args: json.RawMessage(`{"city":"London"}`)},
		}}
	if !reflect.DeepEqual(resp, want) {
		t.Fatalf("got %+v", resp)
	}
	noArgs, err := Protocol{}.Decode(strings.NewReader(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"ping"}}]},"finishReason":"STOP"}]}`))
	if err != nil || string(noArgs.Content[0].(protocol.ToolCall).Args) != "{}" {
		t.Fatalf("%+v %v", noArgs, err)
	}
}

func TestDecodeStopReasonsAndFailures(t *testing.T) {
	stops := map[string]protocol.StopReason{"STOP": protocol.StopEnd, "MAX_TOKENS": protocol.StopLength, "CONTINUATION": protocol.StopLength, "SAFETY": protocol.StopContentFilter, "ESCALATION": protocol.StopContentFilter, "MALFORMED_FUNCTION_CALL": protocol.StopOther}
	for raw, want := range stops {
		resp, err := Protocol{}.Decode(strings.NewReader(`{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"` + raw + `"}]}`))
		if err != nil || resp.StopReason != want || resp.RawStopReason != raw {
			t.Fatalf("%s -> %+v %v", raw, resp, err)
		}
	}
	failures := map[string]string{
		"blocked":          `{"promptFeedback":{"blockReason":"SAFETY"}}`,
		"no candidates":    `{"candidates":[]}`,
		"unsupported kind": `{"candidates":[{"content":{"parts":[{"executableCode":{"language":"PYTHON","code":"1"}}]},"finishReason":"STOP"}]}`,
		"inline output":    `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]},"finishReason":"STOP"}]}`,
		"bad args":         `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":[1]}}]},"finishReason":"STOP"}]}`,
		"not json":         `nope`,
	}
	for name, body := range failures {
		if _, err := (Protocol{}).Decode(strings.NewReader(body)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	_, err := Protocol{}.Decode(strings.NewReader(`{"error":{"code":400,"message":"bad","status":"INVALID_ARGUMENT"}}`))
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Status != 400 || perr.Type != "INVALID_ARGUMENT" {
		t.Fatalf("%v", err)
	}
}

func chunk(parts, tail string) string {
	return `data: {"candidates":[{"content":{"role":"model","parts":[` + parts + `]}` + tail + `}],"responseId":"r9","modelVersion":"gemini-3-pro"}` + "\n\n"
}

func eventTypes(t *testing.T, in string) ([]protocol.EventType, protocol.Response) {
	t.Helper()
	var got []protocol.EventType
	var final protocol.Response
	for ev, err := range (Protocol{}).Stream(strings.NewReader(in)) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ev.Type)
		final = ev.Response
	}
	return got, final
}

// Spec section 4 with framing from the official SDK stream loops, ASSEMBLED
func TestStreamAssemblesBlocks(t *testing.T) {
	in := chunk(`{"thought":true,"text":"pond"}`, "") +
		chunk(`{"thought":true,"text":"ering"},{"text":"Hel"}`, "") +
		chunk(`{"text":"lo"}`, "") +
		chunk(`{"functionCall":{"name":"get_weather","args":{"city":"Paris"}},"thoughtSignature":"SIG_A"}`, `,"finishReason":"STOP"`) +
		`data: {"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":5,"thoughtsTokenCount":2},"responseId":"r9"}` + "\n\n"
	got, final := eventTypes(t, in)
	want := []protocol.EventType{protocol.EventStart, protocol.EventThinkingStart, protocol.EventThinkingDelta, protocol.EventThinkingDelta, protocol.EventThinkingEnd, protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextDelta, protocol.EventTextEnd, protocol.EventToolCallStart, protocol.EventToolCallDelta, protocol.EventToolCallEnd, protocol.EventDone}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v", got)
	}
	wantResp := protocol.Response{ID: "r9", Model: "gemini-3-pro", StopReason: protocol.StopToolUse, RawStopReason: "STOP", Usage: protocol.Usage{Input: 7, Output: 5, Reasoning: 2},
		Content: []protocol.Part{protocol.Thinking{Text: "pondering"}, protocol.Text{Text: "Hello"}, protocol.ToolCall{Name: "get_weather", Args: json.RawMessage(`{"city":"Paris"}`), Signature: "SIG_A"}}}
	if !reflect.DeepEqual(final, wantResp) {
		t.Fatalf("response %+v", final)
	}
	collected, err := protocol.Collect(Protocol{}.Stream(strings.NewReader(in)))
	if err != nil || !reflect.DeepEqual(collected, wantResp) {
		t.Fatalf("%+v %v", collected, err)
	}
}

// Vertex thought signatures doc, trailing empty text part carrying the signature when streaming
func TestStreamTrailingSignature(t *testing.T) {
	in := chunk(`{"text":"Hi"}`, "") + chunk(`{"text":"","thoughtSignature":"SIG_C"}`, `,"finishReason":"STOP"`)
	got, final := eventTypes(t, in)
	want := []protocol.EventType{protocol.EventStart, protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextEnd, protocol.EventThinkingStart, protocol.EventThinkingEnd, protocol.EventDone}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v", got)
	}
	if !reflect.DeepEqual(final.Content, []protocol.Part{protocol.Text{Text: "Hi"}, protocol.Thinking{Signature: "SIG_C"}}) || final.StopReason != protocol.StopEnd {
		t.Fatalf("%+v", final)
	}
}

func TestStreamFailures(t *testing.T) {
	cases := map[string]string{
		"no finish reason": chunk(`{"text":"Hi"}`, ""),
		"empty":            "",
		"bad json":         "data: {\n\n",
		"raw garbage":      "garbage\n\n",
		"blocked":          `data: {"promptFeedback":{"blockReason":"SAFETY"}}` + "\n\n",
	}
	for name, in := range cases {
		if _, err := protocol.Collect(Protocol{}.Stream(strings.NewReader(in))); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	raw := "{\n  \"error\": {\n    \"code\": 429,\n    \"message\": \"quota\",\n    \"status\": \"RESOURCE_EXHAUSTED\"\n  }\n}\n"
	inData := `data: {"error":{"code":503,"message":"busy","status":"UNAVAILABLE"}}` + "\n\n"
	for _, in := range []string{raw, inData} {
		_, err := protocol.Collect(Protocol{}.Stream(strings.NewReader(in)))
		var perr *protocol.Error
		if !errors.As(err, &perr) || perr.Status == 0 || perr.Type == "" || perr.Message == "" {
			t.Fatalf("%q -> %v", in, err)
		}
	}
}

func TestRecordsFraming(t *testing.T) {
	in := "data: {\"a\":\r\ndata: 1}\r\n\r\ndata: {\"b\":2}"
	var got []string
	for rec, err := range records(strings.NewReader(in)) {
		if err != nil {
			t.Fatal(err)
		}
		d, ok := rec.data()
		if !ok {
			t.Fatal("not data")
		}
		got = append(got, d)
	}
	if !reflect.DeepEqual(got, []string{"{\"a\":\n1}", `{"b":2}`}) {
		t.Fatalf("%q", got)
	}
}

// AIP-193 HTTP JSON mapping of google.rpc.Status
func TestDecodeError(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"}]}}`)
	e := Protocol{}.DecodeError(429, body)
	if e.Status != 429 || e.Type != "RESOURCE_EXHAUSTED" || e.Message != "Quota exceeded" || string(e.Body) != string(body) {
		t.Fatalf("%+v", e)
	}
	e = Protocol{}.DecodeError(502, []byte(" bad gateway \n"))
	if e.Status != 502 || e.Type != "" || e.Message != "bad gateway" {
		t.Fatalf("%+v", e)
	}
	if _, ok := protocol.Lookup(protocol.GoogleGenerativeAI); !ok {
		t.Fatal("not registered")
	}
}
