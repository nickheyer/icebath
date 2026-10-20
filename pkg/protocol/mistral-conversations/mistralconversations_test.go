package mistralconversations

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
)

var p Protocol

var schema = json.RawMessage(`{"type":"object","properties":{"example_arg":{"type":"string"}},"required":["example_arg"]}`)

func fullRequest(stream bool) protocol.Request {
	temp := 0.5
	return protocol.Request{
		Model:  "example-model",
		System: "example-system-prompt",
		Messages: []protocol.Message{
			{Role: protocol.RoleUser, Content: []protocol.Part{
				protocol.Text{Text: "Example input."},
				protocol.Image{MediaType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47, 0x0d}},
			}},
			{Role: protocol.RoleAssistant, Content: []protocol.Part{
				protocol.Thinking{Text: "example-reasoning", Signature: "example-signature"},
				protocol.Text{Text: "Example content."},
				protocol.ToolCall{ID: "example-tool-call-id", Name: "example_function", Args: json.RawMessage(`{"example_arg":"example-value"}`)},
			}},
			{Role: protocol.RoleTool, Content: []protocol.Part{
				protocol.ToolResult{CallID: "example-tool-call-id", Name: "example_function", Content: []protocol.Part{protocol.Text{Text: "example-tool-result-text"}}, IsError: true},
			}},
		},
		Tools:         []protocol.Tool{{Name: "example_function", Description: "example description", Parameters: schema}},
		ToolChoice:    protocol.ToolChoice{Mode: protocol.ToolChoiceAuto},
		MaxTokens:     1024,
		Temperature:   &temp,
		StopSequences: []string{"example-stop"},
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

// Mirrors spec section 6.2 with the same entry order and constant fields
func TestEncodeFullConversation(t *testing.T) {
	call, err := p.Encode(fullRequest(false))
	if err != nil {
		t.Fatal(err)
	}
	if call.Method != "POST" || call.Path != "/v1/conversations" || call.Header.Get("Content-Type") != "application/json" || call.Header.Get("Accept") != "application/json" {
		t.Fatalf("%+v", call)
	}
	got := decodeBody(t, call.Body)
	want := decodeBody(t, []byte(`{
	  "model": "example-model",
	  "instructions": "example-system-prompt",
	  "inputs": [
	    {"object":"entry","type":"message.input","role":"user","content":[
	      {"type":"text","text":"Example input."},
	      {"type":"image_url","image_url":"data:image/png;base64,iVBORw0="}]},
	    {"object":"entry","type":"message.output","role":"assistant","content":[
	      {"type":"thinking","thinking":[{"type":"text","text":"example-reasoning"}],"signature":"example-signature"},
	      {"type":"text","text":"Example content."}]},
	    {"object":"entry","type":"function.call","tool_call_id":"example-tool-call-id","name":"example_function","arguments":"{\"example_arg\":\"example-value\"}"},
	    {"object":"entry","type":"function.result","tool_call_id":"example-tool-call-id","result":"[error] example-tool-result-text"}
	  ],
	  "tools": [{"type":"function","function":{"name":"example_function","description":"example description","parameters":{"type":"object","properties":{"example_arg":{"type":"string"}},"required":["example_arg"]}}}],
	  "completion_args": {"temperature":0.5,"max_tokens":1024,"stop":["example-stop"],"tool_choice":"auto"},
	  "stream": false,
	  "store": false
	}`))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("body mismatch\n got %s", call.Body)
	}
}

func TestEncodeStreamingAndExtra(t *testing.T) {
	req := protocol.Request{Model: "m", Stream: true, Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}}}}
	req.Extra = map[string]json.RawMessage{"completion_args": json.RawMessage(`{"reasoning_effort":"high"}`), "agent_id": json.RawMessage(`"ag_1"`)}
	call, err := p.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	if call.Header.Get("Accept") != "text/event-stream" {
		t.Fatal(call.Header)
	}
	got := decodeBody(t, call.Body)
	if got["stream"] != true || got["store"] != false || got["agent_id"] != "ag_1" {
		t.Fatalf("%s", call.Body)
	}
	if got["completion_args"].(map[string]any)["reasoning_effort"] != "high" {
		t.Fatalf("%s", call.Body)
	}
	if _, has := got["instructions"]; has {
		t.Fatal("empty system must be omitted")
	}
	if _, has := got["tools"]; has {
		t.Fatal("no tools must be omitted")
	}
	plain, _ := p.Encode(protocol.Request{Model: "m"})
	if _, has := decodeBody(t, plain.Body)["completion_args"]; has {
		t.Fatal("empty completion_args must be omitted")
	}
}

func TestEncodeAssistantOrderAndToolChoice(t *testing.T) {
	req := protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{
		protocol.Text{Text: "a"},
		protocol.ToolCall{ID: "c1", Name: "f", Args: json.RawMessage(`{}`)},
		protocol.Text{Text: "b"},
	}}}}
	choices := map[protocol.ToolChoiceMode]string{protocol.ToolChoiceNone: "none", protocol.ToolChoiceRequired: "any"}
	for mode, want := range choices {
		req.ToolChoice = protocol.ToolChoice{Mode: mode}
		call, err := p.Encode(req)
		if err != nil {
			t.Fatal(err)
		}
		got := decodeBody(t, call.Body)
		if got["completion_args"].(map[string]any)["tool_choice"] != want {
			t.Fatalf("%s", call.Body)
		}
		inputs := got["inputs"].([]any)
		types := []string{}
		for _, e := range inputs {
			types = append(types, e.(map[string]any)["type"].(string))
		}
		if strings.Join(types, ",") != "message.output,function.call,message.output" {
			t.Fatalf("%v", types)
		}
	}
}

func TestEncodeErrors(t *testing.T) {
	high := 1.5
	user := func(p ...protocol.Part) protocol.Message {
		return protocol.Message{Role: protocol.RoleUser, Content: p}
	}
	cases := map[string]protocol.Request{
		"missing model":            {Messages: []protocol.Message{user(protocol.Text{Text: "x"})}},
		"named tool choice":        {Model: "m", ToolChoice: protocol.ToolChoice{Mode: protocol.ToolChoiceTool, Name: "f"}},
		"unknown tool choice":      {Model: "m", ToolChoice: protocol.ToolChoice{Mode: "weird"}},
		"temperature range":        {Model: "m", Temperature: &high},
		"tool without schema":      {Model: "m", Tools: []protocol.Tool{{Name: "f"}}},
		"image without type":       {Model: "m", Messages: []protocol.Message{user(protocol.Image{Data: []byte{1}})}},
		"tool result in user":      {Model: "m", Messages: []protocol.Message{user(protocol.ToolResult{CallID: "c"})}},
		"thinking in user":         {Model: "m", Messages: []protocol.Message{user(protocol.Thinking{Text: "t"})}},
		"text in tool message":     {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.Text{Text: "t"}}}}},
		"image without data":       {Model: "m", Messages: []protocol.Message{user(protocol.Image{MediaType: "image/png"})}},
		"document in user":         {Model: "m", Messages: []protocol.Message{user(protocol.Document{MediaType: "application/pdf", Data: []byte{1}, Name: "d.pdf"})}},
		"bad tool call args":       {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{ID: "c", Name: "f", Args: json.RawMessage(`[1]`)}}}}},
		"thinking in tool result":  {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "c", Content: []protocol.Part{protocol.Thinking{}}}}}}},
		"tool result in assistant": {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolResult{CallID: "c"}}}}},
		"unknown role":             {Model: "m", Messages: []protocol.Message{{Role: "system"}}},
		"bad extra":                {Model: "m", Extra: map[string]json.RawMessage{"x": json.RawMessage(`{`)}},
	}
	for name, req := range cases {
		if _, err := p.Encode(req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Spec section 6.3 with the arguments shown in string form
const functionCallResponse = `{
  "object": "conversation.response",
  "conversation_id": "019b2bd7-96e7-7219-8c0b-45a73da50088",
  "outputs": [
    {"object":"entry","type":"message.output","id":"example-entry-id-1","agent_id":null,"model":"example-model","role":"assistant",
     "content":[{"type":"text","text":"Example content."}],"created_at":"2026-10-19T00:00:00Z","completed_at":"2026-10-19T00:00:01Z"},
    {"object":"entry","type":"function.call","id":"example-entry-id-2","agent_id":null,"model":"example-model","tool_call_id":"example-tool-call-id",
     "name":"example_function","arguments":"{\"example_arg\":\"example-value\"}","confirmation_status":null,"created_at":"2026-10-19T00:00:01Z","completed_at":"2026-10-19T00:00:01Z"}
  ],
  "usage": {"prompt_tokens": 91, "completion_tokens": 29, "total_tokens": 120, "connector_tokens": null, "connectors": null},
  "guardrails": null
}`

func TestDecodeFunctionCallResponse(t *testing.T) {
	resp, err := p.Decode(strings.NewReader(functionCallResponse))
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "019b2bd7-96e7-7219-8c0b-45a73da50088" || resp.Model != "example-model" || resp.StopReason != protocol.StopToolUse || resp.RawStopReason != "" {
		t.Fatalf("%+v", resp)
	}
	if resp.Usage != (protocol.Usage{Input: 91, Output: 29}) {
		t.Fatalf("%+v", resp.Usage)
	}
	want := []protocol.Part{
		protocol.Text{Text: "Example content."},
		protocol.ToolCall{ID: "example-tool-call-id", Name: "example_function", Args: json.RawMessage(`{"example_arg":"example-value"}`)},
	}
	if !reflect.DeepEqual(resp.Content, want) {
		t.Fatalf("%+v", resp.Content)
	}
}

func TestDecodeServerToolEntries(t *testing.T) {
	body := `{"conversation_id":"c","outputs":[
	  {"object":"entry","type":"tool.execution","id":"t","name":"web_search","arguments":"{\"q\":\"go\"}","confirmation_status":"confirmed","info":{"results":[{"url":"https://go.dev"}]}},
	  {"object":"entry","type":"agent.handoff","id":"h","previous_agent_id":"a1","previous_agent_name":"planner","next_agent_id":"a2","next_agent_name":"coder"},
	  {"object":"entry","type":"message.output","id":"m","model":"mm","role":"assistant","content":"done"}
	],"usage":{}}`
	resp, err := p.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 4 {
		t.Fatalf("%+v", resp.Content)
	}
	call := resp.Content[0].(protocol.ServerToolCall)
	if call.ID != "t" || call.Name != "web_search" || call.Origin != protocol.MistralConversations || !strings.Contains(string(call.Args), `"confirmation_status":"confirmed"`) || !strings.Contains(string(call.Args), `"arguments":"{\"q\":\"go\"}"`) {
		t.Fatalf("%+v", call)
	}
	result := resp.Content[1].(protocol.ServerToolResult)
	if result.CallID != "t" || result.Name != "web_search" || string(result.Content) != `{"results":[{"url":"https://go.dev"}]}` || result.Origin != protocol.MistralConversations {
		t.Fatalf("%+v", result)
	}
	handoff := resp.Content[2].(protocol.ServerToolCall)
	if handoff.Name != "handoff" || !strings.Contains(string(handoff.Args), `"next_agent_name":"coder"`) {
		t.Fatalf("%+v", handoff)
	}
	if _, err := p.Decode(strings.NewReader(`{"conversation_id":"c","outputs":[{"object":"entry","type":"tool.execution","id":"t"}],"usage":{}}`)); err == nil {
		t.Fatal("execution without a name accepted")
	}
	req := protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: resp.Content}}}
	call2, err := p.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	inputs := decodeBody(t, call2.Body)["inputs"].([]any)
	if len(inputs) != 3 {
		t.Fatalf("%s", call2.Body)
	}
	exec := inputs[0].(map[string]any)
	if exec["type"] != "tool.execution" || exec["name"] != "web_search" || exec["id"] != "t" || exec["confirmation_status"] != "confirmed" || exec["info"].(map[string]any)["results"] == nil {
		t.Fatalf("replayed execution %v", exec)
	}
	if h := inputs[1].(map[string]any); h["type"] != "agent.handoff" || h["next_agent_name"] != "coder" {
		t.Fatalf("replayed handoff %v", h)
	}
	foreign := protocol.ServerToolCall{ID: "x", Name: "web_search", Args: json.RawMessage(`{}`), Origin: protocol.AnthropicMessages}
	call3, err := p.Encode(protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{foreign, protocol.Text{Text: "t"}}}}})
	if err != nil || strings.Contains(string(call3.Body), "web_search") {
		t.Fatalf("foreign server tool must be omitted: %v %s", err, call3.Body)
	}
}

func TestDecodeTextThinkingAndObjectArguments(t *testing.T) {
	body := `{"conversation_id":"c","outputs":[
	  {"object":"entry","type":"message.output","id":"m","model":"mm","role":"assistant","content":[
	    {"type":"thinking","thinking":[{"type":"text","text":"hm"},{"type":"text","text":"m"}],"signature":"sig"},
	    {"type":"text","text":"Hel"},{"type":"text","text":"lo "},
	    {"type":"tool_reference","tool":"web_search","title":"T","url":"https://x"}]},
	  {"object":"entry","type":"message.output","id":"m2","model":"mm","role":"assistant","content":"plain"},
	  {"object":"entry","type":"function.call","id":"f","tool_call_id":"c1","name":"g","arguments":{"a": 1}}
	],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
	resp, err := p.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.Part{
		protocol.Thinking{Text: "hmm", Signature: "sig", Origin: protocol.MistralConversations},
		protocol.Text{Text: "Hello ", Citations: []protocol.Citation{{Title: "T", URL: "https://x", Start: 6, End: 6}}},
		protocol.Text{Text: "plain"},
		protocol.ToolCall{ID: "c1", Name: "g", Args: json.RawMessage(`{"a":1}`)},
	}
	if !reflect.DeepEqual(resp.Content, want) || resp.Model != "mm" {
		t.Fatalf("%+v", resp.Content)
	}
	leading := `{"conversation_id":"c","outputs":[{"object":"entry","type":"message.output","id":"m","model":"mm","role":"assistant","content":[{"type":"tool_file","tool":"document_library","file_id":"f1","file_name":"spec.pdf"},{"type":"text","text":"see"}]}],"usage":{}}`
	resp, err = p.Decode(strings.NewReader(leading))
	if err != nil {
		t.Fatal(err)
	}
	if tx := resp.Content[0].(protocol.Text); tx.Text != "see" || len(tx.Citations) != 1 || tx.Citations[0].Document != "f1" || tx.Citations[0].Title != "spec.pdf" {
		t.Fatalf("leading citation %+v", tx)
	}
	bad := []string{
		`{"conversation_id":"c","outputs":[{"type":"mystery"}],"usage":{}}`,
		`{"conversation_id":"c","outputs":[{"type":"message.output","content":[{"type":"image_url","image_url":"https://x"}]}],"usage":{}}`,
		`{"conversation_id":"c","outputs":[{"type":"function.call","tool_call_id":"c","name":"g","arguments":"[1]"}],"usage":{}}`,
		`not json`,
	}
	for _, b := range bad {
		if _, err := p.Decode(strings.NewReader(b)); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
}

// Spec section 6.4, assembled from the event schemas and the docs streaming example
const transcript = `event: conversation.response.started
data: {"type":"conversation.response.started","created_at":"2026-10-19T00:00:00Z","conversation_id":"019b2bd7-96e7-7219-8c0b-45a73da50088"}

event: message.output.delta
data: {"type":"message.output.delta","created_at":"2026-10-19T00:00:00Z","output_index":0,"id":"example-entry-id-1","content_index":0,"model":"example-model","agent_id":null,"role":"assistant","content":{"type":"thinking","thinking":[{"type":"text","text":"example-"}],"closed":false}}

event: message.output.delta
data: {"type":"message.output.delta","created_at":"2026-10-19T00:00:00Z","output_index":0,"id":"example-entry-id-1","content_index":0,"model":"example-model","agent_id":null,"role":"assistant","content":{"type":"thinking","thinking":[{"type":"text","text":"reasoning"}],"signature":"example-signature","closed":true}}

event: message.output.delta
data: {"type":"message.output.delta","created_at":"2026-10-19T00:00:00Z","output_index":0,"id":"example-entry-id-1","content_index":1,"model":"example-model","agent_id":null,"role":"assistant","content":"Example "}

event: message.output.delta
data: {"type":"message.output.delta","created_at":"2026-10-19T00:00:00Z","output_index":0,"id":"example-entry-id-1","content_index":1,"model":"example-model","agent_id":null,"role":"assistant","content":"content."}

event: function.call.delta
data: {"type":"function.call.delta","created_at":"2026-10-19T00:00:01Z","output_index":2,"id":"example-entry-id-2","model":"example-model","agent_id":null,"name":"example_function","tool_call_id":"example-tool-call-id","arguments":"{\"example_arg\":","confirmation_status":null}

event: function.call.delta
data: {"type":"function.call.delta","created_at":"2026-10-19T00:00:01Z","output_index":2,"id":"example-entry-id-2","model":"example-model","agent_id":null,"name":"example_function","tool_call_id":"example-tool-call-id","arguments":"\"example-value\"}","confirmation_status":null}

event: conversation.response.done
data: {"type":"conversation.response.done","created_at":"2026-10-19T00:00:01Z","usage":{"prompt_tokens":18709,"total_tokens":18892,"completion_tokens":183}}

`

func TestStreamTranscript(t *testing.T) {
	var types []protocol.EventType
	var final protocol.Response
	for ev, err := range p.Stream(strings.NewReader(transcript)) {
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
		protocol.EventThinkingStart, protocol.EventThinkingDelta, protocol.EventThinkingDelta,
		protocol.EventThinkingEnd, protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextDelta,
		protocol.EventTextEnd, protocol.EventToolCallStart, protocol.EventToolCallDelta, protocol.EventToolCallDelta,
		protocol.EventToolCallEnd, protocol.EventDone,
	}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("%v", types)
	}
	if final.ID != "019b2bd7-96e7-7219-8c0b-45a73da50088" || final.Model != "example-model" || final.StopReason != protocol.StopToolUse {
		t.Fatalf("%+v", final)
	}
	if final.Usage != (protocol.Usage{Input: 18709, Output: 183}) {
		t.Fatalf("%+v", final.Usage)
	}
	wantParts := []protocol.Part{
		protocol.Thinking{Text: "example-reasoning", Signature: "example-signature", Origin: protocol.MistralConversations},
		protocol.Text{Text: "Example content."},
		protocol.ToolCall{ID: "example-tool-call-id", Name: "example_function", Args: json.RawMessage(`{"example_arg":"example-value"}`)},
	}
	if !reflect.DeepEqual(final.Content, wantParts) {
		t.Fatalf("%+v", final.Content)
	}
	collected, err := protocol.Collect(p.Stream(strings.NewReader(transcript)))
	if err != nil || !reflect.DeepEqual(collected, final) {
		t.Fatalf("%v %+v", err, collected)
	}
}

func TestStreamFailures(t *testing.T) {
	truncated := transcript[:strings.LastIndex(transcript, "event: conversation.response.done")]
	if _, err := protocol.Collect(p.Stream(strings.NewReader(truncated))); err == nil || !strings.Contains(err.Error(), "before conversation.response.done") {
		t.Fatalf("truncated: %v", err)
	}
	errStream := "event: conversation.response.error\ndata: {\"type\":\"conversation.response.error\",\"created_at\":\"2026-10-19T00:00:01Z\",\"message\":\"example-message\",\"code\":0}\n\n"
	_, err := protocol.Collect(p.Stream(strings.NewReader(errStream)))
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Status != 200 || perr.Type != "conversation.response.error" || !strings.Contains(perr.Message, "example-message") || !perr.InStream || perr.Transient || len(perr.Body) == 0 {
		t.Fatalf("error event: %v %+v", err, perr)
	}
	throttled := "event: conversation.response.error\ndata: {\"type\":\"conversation.response.error\",\"message\":\"slow down\",\"code\":429}\n\n"
	_, err = protocol.Collect(p.Stream(strings.NewReader(throttled)))
	if !errors.As(err, &perr) || !perr.Transient {
		t.Fatalf("throttle must be transient: %v", err)
	}
	for _, bad := range []string{
		"event: mystery\ndata: {\"type\":\"mystery\"}\n\n",
		"event: tool.execution.delta\ndata: {\"type\":\"tool.execution.delta\",\"id\":\"unknown\",\"arguments\":\"x\"}\n\n",
		"event: tool.execution.done\ndata: {\"type\":\"tool.execution.done\",\"id\":\"unknown\"}\n\n",
		"event: agent.handoff.done\ndata: {\"type\":\"agent.handoff.done\",\"id\":\"unknown\"}\n\n",
		"event: message.output.delta\ndata: not json\n\n",
		"event: message.output.delta\ndata: {\"type\":\"message.output.delta\",\"id\":\"m\",\"content\":{\"type\":\"image_url\",\"image_url\":\"https://x\"}}\n\n",
		"event: function.call.delta\ndata: {\"type\":\"function.call.delta\",\"id\":\"f\",\"tool_call_id\":\"c\",\"name\":\"g\",\"arguments\":\"[\"}\n\nevent: conversation.response.done\ndata: {\"type\":\"conversation.response.done\",\"usage\":{}}\n\n",
	} {
		if _, err := protocol.Collect(p.Stream(strings.NewReader(bad))); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	n := 0
	for range p.Stream(strings.NewReader(transcript)) {
		n++
		break
	}
	if n != 1 {
		t.Fatal("stream must stop when the consumer breaks")
	}
}

func TestDecodeError(t *testing.T) {
	body := []byte(`{"detail":[{"loc":["body","inputs"],"msg":"Field required","type":"missing","input":null}]}`)
	e := p.DecodeError(422, body)
	if e.Status != 422 || e.Type != "missing" || e.Message != "body.inputs: Field required" || string(e.Body) != string(body) || e.Transient {
		t.Fatalf("%+v", e)
	}
	if e.Error() != "protocol: http 422 missing: body.inputs: Field required" {
		t.Fatal(e.Error())
	}
	raw := p.DecodeError(502, []byte(" bad gateway \n"))
	if raw.Type != "" || raw.Message != "bad gateway" || raw.Status != 502 || !raw.Transient {
		t.Fatalf("%+v", raw)
	}
	obj := p.DecodeError(401, []byte(`{"object":"error","message":"Unauthorized","type":"invalid_request_error","code":"1000"}`))
	if obj.Type != "invalid_request_error" || obj.Message != "Unauthorized" || obj.Transient {
		t.Fatalf("%+v", obj)
	}
	coded := p.DecodeError(429, []byte(`{"object":"error","message":"Too many","code":429}`))
	if coded.Type != "429" || !coded.Transient {
		t.Fatalf("%+v", coded)
	}
	plain := p.DecodeError(404, []byte(`{"detail":"Not Found"}`))
	if plain.Message != "Not Found" || plain.Type != "" {
		t.Fatalf("%+v", plain)
	}
	empty := p.DecodeError(400, []byte(`{"detail":[]}`))
	if empty.Message != `{"detail":[]}` {
		t.Fatalf("%+v", empty)
	}
}

func TestReplayPolicyAndResultShapes(t *testing.T) {
	same := protocol.Thinking{Text: "mine", Signature: "s", Origin: protocol.MistralConversations}
	blank := protocol.Thinking{Text: "blank", Signature: "s"}
	foreign := protocol.Thinking{Text: "theirs", Signature: "s", Origin: protocol.AnthropicMessages}
	redacted := protocol.Thinking{Redacted: true, Signature: "blob"}
	call, err := p.Encode(protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{same, blank, foreign, redacted, protocol.Text{Text: "t"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	body := string(call.Body)
	if !strings.Contains(body, "mine") || !strings.Contains(body, "blank") || strings.Contains(body, "theirs") || strings.Contains(body, "blob") {
		t.Fatalf("%s", body)
	}
	only, err := p.Encode(protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{foreign}}}})
	if err != nil {
		t.Fatal(err)
	}
	if inputs := decodeBody(t, only.Body)["inputs"].([]any); len(inputs) != 0 {
		t.Fatalf("a turn left empty by policy must encode as no entries: %s", only.Body)
	}
	withImage, err := p.Encode(protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "c", Content: []protocol.Part{protocol.Image{MediaType: "image/png", Data: []byte{1, 2}}, protocol.Text{Text: "ok"}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if entry := decodeBody(t, withImage.Body)["inputs"].([]any)[0].(map[string]any); entry["result"] != "[image image/png, 2 bytes omitted]\nok" {
		t.Fatalf("%v", entry)
	}
	nilArgs, err := p.Encode(protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{ID: "c", Name: "f"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if entry := decodeBody(t, nilArgs.Body)["inputs"].([]any)[0].(map[string]any); entry["arguments"] != "{}" {
		t.Fatalf("%v", entry)
	}
	caps := p.Capabilities()
	if !caps.Thinking || caps.RedactedThinking || !caps.Images || caps.ImagesInResults || caps.Documents || !caps.ToolChoiceNone || caps.ToolChoiceNamed || !caps.StopSequences || !caps.ServerTools || !caps.Citations || caps.Caching {
		t.Fatalf("%+v", caps)
	}
}

// Vendor tool run and handoff events assembled from the streaming schemas
const executionStream = `event: conversation.response.started
data: {"type":"conversation.response.started","conversation_id":"c1"}

event: tool.execution.started
data: {"type":"tool.execution.started","id":"ex1","name":"web_search","arguments":"{\"q\":","confirmation_status":"confirmed"}

event: tool.execution.delta
data: {"type":"tool.execution.delta","id":"ex1","arguments":"\"go\"}"}

event: tool.execution.done
data: {"type":"tool.execution.done","id":"ex1","info":{"results":[{"url":"https://go.dev"}]}}

event: agent.handoff.started
data: {"type":"agent.handoff.started","id":"h1","previous_agent_id":"a1","previous_agent_name":"planner"}

event: agent.handoff.done
data: {"type":"agent.handoff.done","id":"h1","next_agent_id":"a2","next_agent_name":"coder"}

event: message.output.delta
data: {"type":"message.output.delta","id":"m1","content_index":0,"model":"mm","content":{"type":"tool_reference","tool":"web_search","title":"Go","url":"https://go.dev"}}

event: message.output.delta
data: {"type":"message.output.delta","id":"m1","content_index":0,"model":"mm","content":"Go is fast"}

event: message.output.delta
data: {"type":"message.output.delta","id":"m1","content_index":0,"model":"mm","content":{"type":"tool_file","tool":"document_library","file_id":"f9","file_name":"notes.md"}}

event: conversation.response.done
data: {"type":"conversation.response.done","usage":{"prompt_tokens":5,"completion_tokens":6}}

`

func TestStreamServerToolsAndCitations(t *testing.T) {
	var types []protocol.EventType
	var citations []protocol.Citation
	var final protocol.Response
	for ev, err := range p.Stream(strings.NewReader(executionStream)) {
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, ev.Type)
		if ev.Type == protocol.EventCitation {
			citations = append(citations, ev.Citation)
		}
		if ev.Type == protocol.EventDone {
			final = ev.Response
		}
	}
	want := []protocol.EventType{
		protocol.EventStart,
		protocol.EventServerToolCallStart, protocol.EventServerToolCallDelta, protocol.EventServerToolCallEnd, protocol.EventServerToolResult,
		protocol.EventServerToolCallStart, protocol.EventServerToolCallDelta, protocol.EventServerToolCallEnd,
		protocol.EventTextStart, protocol.EventCitation, protocol.EventTextDelta, protocol.EventCitation, protocol.EventTextEnd,
		protocol.EventDone,
	}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("%v", types)
	}
	exec := final.Content[0].(protocol.ServerToolCall)
	if exec.ID != "ex1" || exec.Name != "web_search" || exec.Origin != protocol.MistralConversations || !strings.Contains(string(exec.Args), `"arguments":"{\"q\":\"go\"}"`) || !strings.Contains(string(exec.Args), `"confirmation_status":"confirmed"`) {
		t.Fatalf("%+v", exec)
	}
	if res := final.Content[1].(protocol.ServerToolResult); res.CallID != "ex1" || string(res.Content) != `{"results":[{"url":"https://go.dev"}]}` {
		t.Fatalf("%+v", res)
	}
	if h := final.Content[2].(protocol.ServerToolCall); h.Name != "handoff" || !strings.Contains(string(h.Args), `"previous_agent_name":"planner"`) || !strings.Contains(string(h.Args), `"next_agent_name":"coder"`) {
		t.Fatalf("%+v", h)
	}
	text := final.Content[3].(protocol.Text)
	if text.Text != "Go is fast" || len(text.Citations) != 2 || text.Citations[0].URL != "https://go.dev" || text.Citations[0].Start != 0 || text.Citations[1].Document != "f9" || text.Citations[1].Start != 10 {
		t.Fatalf("%+v", text)
	}
	if len(citations) != 2 || citations[1].Title != "notes.md" {
		t.Fatalf("%+v", citations)
	}
	if final.Usage != (protocol.Usage{Input: 5, Output: 6}) || final.StopReason != protocol.StopEnd {
		t.Fatalf("%+v", final)
	}
}

func TestRegistered(t *testing.T) {
	p, ok := protocol.Lookup(protocol.MistralConversations)
	if !ok || p.ID() != "mistral-conversations" {
		t.Fatal("not registered")
	}
}
