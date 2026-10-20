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

func modelParts(t *testing.T, msgs ...protocol.Message) []any {
	t.Helper()
	_, m := encodeMap(t, protocol.Request{Model: "m", Messages: msgs})
	contents := m["contents"].([]any)
	return contents[len(contents)-1].(map[string]any)["parts"].([]any)
}

func TestCapabilities(t *testing.T) {
	want := protocol.Capabilities{Thinking: true, Images: true, ImagesInResults: true, Documents: true, ToolChoiceNone: true, ToolChoiceNamed: true, StopSequences: true, ServerTools: true, Citations: true}
	if got := (Protocol{}).Capabilities(); got != want {
		t.Fatalf("%+v", got)
	}
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

// Rule 3 replay policy for signed parts, with the fold skipped for a foreign signature
func TestEncodeOriginPolicy(t *testing.T) {
	foreign := protocol.AnthropicMessages
	user := protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "q"}}}
	parts := modelParts(t, user, protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{
		protocol.Thinking{Text: "foreign thought", Signature: "foreign-sig", Origin: foreign},
		protocol.Thinking{Redacted: true, Signature: "redacted-sig"},
		protocol.Text{Text: "folded"},
		protocol.Thinking{Signature: "foreign-fold", Origin: foreign},
		protocol.Thinking{Text: "same", Signature: "same-sig", Origin: protocol.GoogleGenerativeAI},
		protocol.Thinking{Text: "empty", Signature: "empty-sig"},
		protocol.ToolCall{ID: "c1", Name: "f", Args: json.RawMessage(`{}`), Signature: "tool-sig", Origin: foreign},
		protocol.ServerToolCall{Name: "code_execution", Args: json.RawMessage(`{"code":"x"}`), Origin: foreign},
		protocol.ServerToolResult{Name: "code_execution", Content: json.RawMessage(`{"outcome":"OUTCOME_OK"}`), Origin: foreign},
		protocol.ServerToolCall{Name: "code_execution", Args: json.RawMessage(`{"language":"PYTHON","code":"print(1)"}`), Origin: protocol.GoogleGenerativeAI},
		protocol.ServerToolResult{Name: "code_execution", Content: json.RawMessage(`{"outcome":"OUTCOME_OK","output":"1"}`), Origin: protocol.GoogleGenerativeAI},
		protocol.ServerToolCall{ID: "t1", Name: "tool_call", Args: json.RawMessage(`{"id":"t1","name":"search"}`)},
		protocol.ServerToolResult{CallID: "t1", Name: "tool_call", Content: json.RawMessage(`{"id":"t1","response":{}}`)},
	}})
	raw, _ := json.Marshal(parts)
	for _, leaked := range []string{"foreign thought", "foreign-sig", "redacted-sig", "foreign-fold", "tool-sig", `"code":"x"`, "OUTCOME_OK\"}"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("leaked %q in %s", leaked, raw)
		}
	}
	want := []any{
		map[string]any{"text": "folded"},
		map[string]any{"thought": true, "text": "same", "thoughtSignature": "same-sig"},
		map[string]any{"thought": true, "text": "empty", "thoughtSignature": "empty-sig"},
		map[string]any{"functionCall": map[string]any{"id": "c1", "name": "f", "args": map[string]any{}}},
		map[string]any{"executableCode": map[string]any{"language": "PYTHON", "code": "print(1)"}},
		map[string]any{"codeExecutionResult": map[string]any{"outcome": "OUTCOME_OK", "output": "1"}},
		map[string]any{"toolCall": map[string]any{"id": "t1", "name": "search"}},
		map[string]any{"toolResponse": map[string]any{"id": "t1", "response": map[string]any{}}},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("%s", raw)
	}
	_, m := encodeMap(t, protocol.Request{Model: "m", Messages: []protocol.Message{user, {Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Thinking{Text: "gone", Signature: "s", Origin: foreign}}}}})
	if contents := m["contents"].([]any); len(contents) != 1 {
		t.Fatalf("empty model turn must be omitted: %v", contents)
	}
}

func TestEncodeDocumentsAndResultNames(t *testing.T) {
	_, m := encodeMap(t, protocol.Request{Model: "m", Messages: []protocol.Message{
		{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Document{MediaType: "application/pdf", Data: []byte("%PDF"), Name: "r.pdf"}}},
		{Role: protocol.RoleAssistant, Content: []protocol.Part{
			protocol.ToolCall{ID: "c1", Name: "first", Args: json.RawMessage(`{}`)},
			protocol.ToolCall{Name: "second", Args: json.RawMessage(`{}`)},
		}},
		{Role: protocol.RoleTool, Content: []protocol.Part{
			protocol.ToolResult{CallID: "c1", Content: []protocol.Part{protocol.Document{MediaType: "text/csv", Data: []byte("a,b"), Name: "t.csv"}}},
			protocol.ToolResult{Content: []protocol.Part{protocol.Text{Text: "by position"}}},
		}},
	}})
	contents := m["contents"].([]any)
	doc := contents[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["inlineData"].(map[string]any)
	if doc["mimeType"] != "application/pdf" || doc["data"] != "JVBERg==" {
		t.Fatalf("%v", doc)
	}
	results := contents[2].(map[string]any)["parts"].([]any)
	first := results[0].(map[string]any)["functionResponse"].(map[string]any)
	if first["name"] != "first" || first["parts"].([]any)[0].(map[string]any)["inlineData"].(map[string]any)["mimeType"] != "text/csv" {
		t.Fatalf("%v", first)
	}
	if second := results[1].(map[string]any)["functionResponse"].(map[string]any); second["name"] != "second" {
		t.Fatalf("%v", second)
	}
	for name, msgs := range map[string][]protocol.Message{
		"unknown call id": {{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{ID: "c1", Name: "f"}}}, {Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "other"}}}},
		"no calls":        {{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{}}}},
		"position past":   {{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{Name: "f"}}}, {Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{}, protocol.ToolResult{}}}},
	} {
		if _, err := (Protocol{}).Encode(protocol.Request{Model: "m", Messages: msgs}); err == nil {
			t.Fatalf("%s accepted", name)
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
	want := []protocol.Part{protocol.Text{Text: "Hi there"}, protocol.Thinking{Signature: "SIG_C", Origin: protocol.GoogleGenerativeAI}}
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
	model := func(parts ...protocol.Part) protocol.Message {
		return protocol.Message{Role: protocol.RoleAssistant, Content: parts}
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
		"image without type":       {Model: "m", Messages: []protocol.Message{user(protocol.Image{Data: []byte{1}})}},
		"image without data":       {Model: "m", Messages: []protocol.Message{user(protocol.Image{MediaType: "image/png"})}},
		"document without type":    {Model: "m", Messages: []protocol.Message{user(protocol.Document{Data: []byte{1}})}},
		"document without data":    {Model: "m", Messages: []protocol.Message{user(protocol.Document{MediaType: "application/pdf"})}},
		"result image no type":     {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{Name: "f", Content: []protocol.Part{protocol.Image{Data: []byte{1}}}}}}}},
		"result document no data":  {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{Name: "f", Content: []protocol.Part{protocol.Document{MediaType: "text/plain"}}}}}}},
		"tool choice without name": {Model: "m", Messages: []protocol.Message{hi}, ToolChoice: protocol.ToolChoice{Mode: protocol.ToolChoiceTool}},
		"unknown tool choice":      {Model: "m", Messages: []protocol.Message{hi}, ToolChoice: protocol.ToolChoice{Mode: "maybe"}},
		"bad call args":            {Model: "m", Messages: []protocol.Message{model(protocol.ToolCall{Name: "f", Args: json.RawMessage(`[1]`)})}},
		"null call args":           {Model: "m", Messages: []protocol.Message{model(protocol.ToolCall{Name: "f", Args: json.RawMessage(`null`)})}},
		"server unknown name":      {Model: "m", Messages: []protocol.Message{model(protocol.ServerToolCall{Name: "web_search", Args: json.RawMessage(`{}`)})}},
		"server bad args":          {Model: "m", Messages: []protocol.Message{model(protocol.ServerToolCall{Name: "code_execution", Args: json.RawMessage(`[1]`)})}},
		"server result name":       {Model: "m", Messages: []protocol.Message{model(protocol.ServerToolResult{Name: "web_search", Content: json.RawMessage(`{}`)})}},
		"server result empty":      {Model: "m", Messages: []protocol.Message{model(protocol.ServerToolResult{Name: "code_execution"})}},
		"image in assistant bad":   {Model: "m", Messages: []protocol.Message{model(protocol.Image{Data: []byte{1}})}},
		"result in assistant":      {Model: "m", Messages: []protocol.Message{model(protocol.ToolResult{Name: "f"})}},
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
	body := `{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"Need Paris."},{"text":"Looking it up."},{"functionCall":{"id":"fc1","name":"get_weather","args":{"city": "Paris"}},"thoughtSignature":"SIG_A"},{"functionCall":{"name":"get_weather","args":{"city":"London"}}}]},"finishReason":"STOP","index":0,"safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"NEGLIGIBLE"}],"finishMessage":"ok","avgLogprobs":-0.25}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":9,"cachedContentTokenCount":4,"thoughtsTokenCount":3,"totalTokenCount":36},"modelVersion":"gemini-3-pro","responseId":"abc"}`
	resp, err := Protocol{}.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	want := protocol.Response{ID: "abc", Model: "gemini-3-pro", StopReason: protocol.StopToolUse, RawStopReason: "STOP",
		Usage: protocol.Usage{Input: 16, Output: 12, CacheRead: 4, Reasoning: 3},
		Content: []protocol.Part{
			protocol.Thinking{Text: "Need Paris.", Origin: protocol.GoogleGenerativeAI},
			protocol.Text{Text: "Looking it up."},
			protocol.ToolCall{ID: "fc1", Name: "get_weather", Args: json.RawMessage(`{"city":"Paris"}`), Signature: "SIG_A", Origin: protocol.GoogleGenerativeAI},
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

// Code execution parts from the Gemini code execution guide plus the toolCall and toolResponse kinds
func TestDecodeServerToolsAndImages(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[
	  {"executableCode":{"language":"PYTHON","code":"print(1)"}},
	  {"codeExecutionResult":{"outcome":"OUTCOME_OK","output":"1\n"}},
	  {"toolCall":{"id":"t1","name":"search","args":{"q":"x"}}},
	  {"toolResponse":{"id":"t1","response":{"ok":true}}},
	  {"inlineData":{"mimeType":"image/png","data":"AQID"}},
	  {"text":"Done"}
	]},"finishReason":"STOP"}]}`
	resp, err := Protocol{}.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.Part{
		protocol.ServerToolCall{Name: "code_execution", Args: json.RawMessage(`{"language":"PYTHON","code":"print(1)"}`), Origin: protocol.GoogleGenerativeAI},
		protocol.ServerToolResult{Name: "code_execution", Content: json.RawMessage(`{"outcome":"OUTCOME_OK","output":"1\n"}`), Origin: protocol.GoogleGenerativeAI},
		protocol.ServerToolCall{ID: "t1", Name: "tool_call", Args: json.RawMessage(`{"id":"t1","name":"search","args":{"q":"x"}}`), Origin: protocol.GoogleGenerativeAI},
		protocol.ServerToolResult{CallID: "t1", Name: "tool_call", Content: json.RawMessage(`{"id":"t1","response":{"ok":true}}`), Origin: protocol.GoogleGenerativeAI},
		protocol.Image{MediaType: "image/png", Data: []byte{1, 2, 3}},
		protocol.Text{Text: "Done"},
	}
	if !reflect.DeepEqual(resp.Content, want) || resp.StopReason != protocol.StopEnd {
		t.Fatalf("%+v", resp.Content)
	}
	parts := modelParts(t, protocol.Message{Role: protocol.RoleAssistant, Content: resp.Content})
	if len(parts) != 6 || parts[0].(map[string]any)["executableCode"].(map[string]any)["code"] != "print(1)" || parts[3].(map[string]any)["toolResponse"].(map[string]any)["id"] != "t1" || parts[4].(map[string]any)["inlineData"].(map[string]any)["data"] != "AQID" {
		t.Fatalf("%v", parts)
	}
}

// Citation sources and grounding chunks from the grounding with Google Search guide, assembled
func TestDecodeCitations(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[{"text":"Spain won in 2024."}]},"finishReason":"STOP",
	  "citationMetadata":{"citationSources":[{"startIndex":0,"endIndex":5,"uri":"https://c.example","license":"CC"}],"citations":[{"startIndex":6,"endIndex":9,"uri":"https://v.example","title":"Vertex"}]},
	  "groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://g.example","title":"Euro 2024"}},{"retrievedContext":{"uri":"gs://doc","title":"Doc","text":"body"}}],
	    "groundingSupports":[{"segment":{"startIndex":0,"endIndex":18,"text":"Spain won in 2024."},"groundingChunkIndices":[0,1],"confidenceScores":[0.9,0.8]}]}}]}`
	resp, err := Protocol{}.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.Citation{
		{URL: "https://c.example", Start: 0, End: 5},
		{Title: "Vertex", URL: "https://v.example", Start: 6, End: 9},
		{Title: "Euro 2024", URL: "https://g.example", Start: 0, End: 18, Cited: "Spain won in 2024."},
		{Title: "Doc", URL: "gs://doc", Start: 0, End: 18, Cited: "Spain won in 2024."},
	}
	if text := resp.Content[0].(protocol.Text); text.Text != "Spain won in 2024." || !reflect.DeepEqual(text.Citations, want) {
		t.Fatalf("%+v", text)
	}
	afterCall := `{"candidates":[{"content":{"parts":[{"text":"Hi"},{"functionCall":{"name":"f"}}]},"finishReason":"STOP","citationMetadata":{"citationSources":[{"uri":"https://x"}]}}]}`
	resp, err = Protocol{}.Decode(strings.NewReader(afterCall))
	if err != nil || len(resp.Content[0].(protocol.Text).Citations) != 1 {
		t.Fatalf("%+v %v", resp, err)
	}
	onlyCites := `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f"}}]},"finishReason":"STOP","citationMetadata":{"citationSources":[{"uri":"https://x"}]}}]}`
	resp, err = Protocol{}.Decode(strings.NewReader(onlyCites))
	if err != nil || len(resp.Content) != 2 || resp.Content[1].(protocol.Text).Citations[0].URL != "https://x" {
		t.Fatalf("%+v %v", resp, err)
	}
	bad := `{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP","groundingMetadata":{"groundingChunks":[],"groundingSupports":[{"groundingChunkIndices":[3]}]}}]}`
	if _, err := (Protocol{}).Decode(strings.NewReader(bad)); err == nil {
		t.Fatal("out of range chunk accepted")
	}
}

func TestDecodePromptBlocked(t *testing.T) {
	resp, err := Protocol{}.Decode(strings.NewReader(`{"promptFeedback":{"blockReason":"SAFETY","safetyRatings":[]},"usageMetadata":{"promptTokenCount":12}}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != protocol.StopContentFilter || resp.RawStopReason != "SAFETY" || len(resp.Content) != 0 || resp.Usage.Input != 12 {
		t.Fatalf("%+v", resp)
	}
	streamed, err := protocol.Collect(Protocol{}.Stream(strings.NewReader(`data: {"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}` + "\n\n")))
	if err != nil || streamed.StopReason != protocol.StopContentFilter || streamed.RawStopReason != "PROHIBITED_CONTENT" || len(streamed.Content) != 0 {
		t.Fatalf("%+v %v", streamed, err)
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
		"no candidates":    `{"candidates":[]}`,
		"file data":        `{"candidates":[{"content":{"parts":[{"fileData":{"fileUri":"gs://x"}}]},"finishReason":"STOP"}]}`,
		"bad image base64": `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"!!"}}]},"finishReason":"STOP"}]}`,
		"bad args":         `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":[1]}}]},"finishReason":"STOP"}]}`,
		"bad exec":         `{"candidates":[{"content":{"parts":[{"executableCode":[1]}]},"finishReason":"STOP"}]}`,
		"bad exec result":  `{"candidates":[{"content":{"parts":[{"codeExecutionResult":"x"}]},"finishReason":"STOP"}]}`,
		"not json":         `nope`,
	}
	for name, body := range failures {
		if _, err := (Protocol{}).Decode(strings.NewReader(body)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	_, err := Protocol{}.Decode(strings.NewReader(`{"error":{"code":400,"message":"bad","status":"INVALID_ARGUMENT"}}`))
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Status != 400 || perr.Type != "INVALID_ARGUMENT" || perr.InStream || perr.Transient {
		t.Fatalf("%v", err)
	}
}

func chunk(parts, tail string) string {
	return `data: {"candidates":[{"content":{"role":"model","parts":[` + parts + `]}` + tail + `}],"responseId":"r9","modelVersion":"gemini-3-pro"}` + "\n\n"
}

func eventTypes(t *testing.T, in string) ([]protocol.Event, protocol.Response) {
	t.Helper()
	var got []protocol.Event
	var final protocol.Response
	for ev, err := range (Protocol{}).Stream(strings.NewReader(in)) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ev)
		final = ev.Response
	}
	return got, final
}

func types(events []protocol.Event) []protocol.EventType {
	out := make([]protocol.EventType, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

// Spec section 4 with framing from the official SDK stream loops, ASSEMBLED
func TestStreamAssemblesBlocks(t *testing.T) {
	in := chunk(`{"thought":true,"text":"pond"}`, "") +
		chunk(`{"thought":true,"text":"ering"},{"text":"Hel"}`, "") +
		chunk(`{"text":"lo"}`, "") +
		chunk(`{"functionCall":{"name":"get_weather","args":{"city":"Paris"}},"thoughtSignature":"SIG_A"}`, `,"finishReason":"STOP"`) +
		`data: {"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":5,"thoughtsTokenCount":2},"responseId":"r9"}` + "\n\n"
	events, final := eventTypes(t, in)
	want := []protocol.EventType{protocol.EventStart, protocol.EventThinkingStart, protocol.EventThinkingDelta, protocol.EventThinkingDelta, protocol.EventThinkingEnd, protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextDelta, protocol.EventTextEnd, protocol.EventToolCallStart, protocol.EventToolCallDelta, protocol.EventToolCallEnd, protocol.EventUsage, protocol.EventDone}
	if got := types(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v", got)
	}
	wantResp := protocol.Response{ID: "r9", Model: "gemini-3-pro", StopReason: protocol.StopToolUse, RawStopReason: "STOP", Usage: protocol.Usage{Input: 7, Output: 7, Reasoning: 2},
		Content: []protocol.Part{protocol.Thinking{Text: "pondering", Origin: protocol.GoogleGenerativeAI}, protocol.Text{Text: "Hello"}, protocol.ToolCall{Name: "get_weather", Args: json.RawMessage(`{"city":"Paris"}`), Signature: "SIG_A", Origin: protocol.GoogleGenerativeAI}}}
	if !reflect.DeepEqual(final, wantResp) {
		t.Fatalf("response %+v", final)
	}
	collected, err := protocol.Collect(Protocol{}.Stream(strings.NewReader(in)))
	if err != nil || !reflect.DeepEqual(collected, wantResp) {
		t.Fatalf("%+v %v", collected, err)
	}
}

// Usage repeated on every chunk raises one usage event per change, before the text finishes
func TestStreamEarlyUsageAndCitations(t *testing.T) {
	in := `data: {"candidates":[{"content":{"parts":[{"text":"Hi"}]}}],"usageMetadata":{"promptTokenCount":7}}` + "\n\n" +
		`data: {"candidates":[{"content":{"parts":[{"text":" there"}]}}],"usageMetadata":{"promptTokenCount":7}}` + "\n\n" +
		`data: {"candidates":[{"content":{"parts":[]},"finishReason":"STOP","groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://g","title":"G"}}],"groundingSupports":[{"segment":{"startIndex":0,"endIndex":8,"text":"Hi there"},"groundingChunkIndices":[0]}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2}}` + "\n\n"
	events, final := eventTypes(t, in)
	want := []protocol.EventType{protocol.EventStart, protocol.EventUsage, protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextDelta, protocol.EventUsage, protocol.EventCitation, protocol.EventTextEnd, protocol.EventDone}
	if got := types(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v", got)
	}
	if events[1].Response.Usage.Input != 7 {
		t.Fatalf("early usage %+v", events[1].Response.Usage)
	}
	cite := protocol.Citation{Title: "G", URL: "https://g", End: 8, Cited: "Hi there"}
	if text := final.Content[0].(protocol.Text); text.Text != "Hi there" || len(text.Citations) != 1 || text.Citations[0] != cite || events[6].Citation != cite {
		t.Fatalf("%+v", text)
	}
	if final.Usage != (protocol.Usage{Input: 7, Output: 2}) {
		t.Fatalf("%+v", final.Usage)
	}
}

// Vertex thought signatures doc, trailing empty text part carrying the signature when streaming
func TestStreamTrailingSignature(t *testing.T) {
	in := chunk(`{"text":"Hi"}`, "") + chunk(`{"text":"","thoughtSignature":"SIG_C"}`, `,"finishReason":"STOP"`)
	events, final := eventTypes(t, in)
	want := []protocol.EventType{protocol.EventStart, protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextEnd, protocol.EventThinkingStart, protocol.EventThinkingEnd, protocol.EventDone}
	if got := types(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v", got)
	}
	if !reflect.DeepEqual(final.Content, []protocol.Part{protocol.Text{Text: "Hi"}, protocol.Thinking{Signature: "SIG_C", Origin: protocol.GoogleGenerativeAI}}) || final.StopReason != protocol.StopEnd {
		t.Fatalf("%+v", final)
	}
}

// Event stream fields the WHATWG reader handles, with comment, event and id lines around the data
func TestStreamSSEFraming(t *testing.T) {
	in := ": keep alive\r\nevent: chunk\r\nid: 1\r\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"a\"}]},\r\ndata: \"finishReason\":\"STOP\"}]}\r\n\r\n"
	resp, err := protocol.Collect(Protocol{}.Stream(strings.NewReader(in)))
	if err != nil || resp.Content[0].(protocol.Text).Text != "a" || resp.StopReason != protocol.StopEnd {
		t.Fatalf("%+v %v", resp, err)
	}
}

func TestStreamFailures(t *testing.T) {
	cases := map[string]string{
		"no finish reason": chunk(`{"text":"Hi"}`, ""),
		"empty":            "",
		"bad json":         "data: {\n\n",
		"raw garbage":      "garbage\n\n",
		"raw object":       "{\"not\":\"an error\"}",
		"bad part":         chunk(`{"fileData":{"fileUri":"gs://x"}}`, `,"finishReason":"STOP"`),
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
		if !errors.As(err, &perr) || perr.Status == 0 || perr.Type == "" || perr.Message == "" || !perr.InStream || !perr.Transient || len(perr.Body) == 0 {
			t.Fatalf("%q -> %+v", in, perr)
		}
	}
	_, err := protocol.Collect(Protocol{}.Stream(strings.NewReader(`data: {"error":{"code":400,"message":"bad","status":"INVALID_ARGUMENT"}}` + "\n\n")))
	var perr *protocol.Error
	if !errors.As(err, &perr) || !perr.InStream || perr.Transient {
		t.Fatalf("%+v", perr)
	}
	n := 0
	for range (Protocol{}).Stream(strings.NewReader(chunk(`{"text":"Hi"}`, `,"finishReason":"STOP"`))) {
		n++
		break
	}
	if n != 1 {
		t.Fatal("stream must stop when the consumer breaks")
	}
}

// AIP-193 HTTP JSON mapping of google.rpc.Status
func TestDecodeError(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"}]}}`)
	e := Protocol{}.DecodeError(429, body)
	if e.Status != 429 || e.Type != "RESOURCE_EXHAUSTED" || e.Message != "Quota exceeded" || string(e.Body) != string(body) || !e.Transient || e.InStream {
		t.Fatalf("%+v", e)
	}
	e = Protocol{}.DecodeError(502, []byte(" bad gateway \n"))
	if e.Status != 502 || e.Type != "" || e.Message != "bad gateway" || !e.Transient {
		t.Fatalf("%+v", e)
	}
	e = Protocol{}.DecodeError(400, []byte(`{"error":{"code":400,"message":"x","status":"INTERNAL"}}`))
	if !e.Transient {
		t.Fatal("INTERNAL must be transient")
	}
	for status, want := range map[int]bool{408: true, 409: true, 500: true, 400: false} {
		if got := (Protocol{}).DecodeError(status, []byte("x")).Transient; got != want {
			t.Fatalf("%d transient %v", status, got)
		}
	}
	if _, ok := protocol.Lookup(protocol.GoogleGenerativeAI); !ok {
		t.Fatal("not registered")
	}
}
