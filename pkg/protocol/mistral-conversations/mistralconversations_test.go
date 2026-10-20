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
	    {"object":"entry","type":"function.result","tool_call_id":"example-tool-call-id","result":"example-tool-result-text"}
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
		"image in tool result":     {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "c", Content: []protocol.Part{protocol.Image{MediaType: "image/png", Data: []byte{1}}}}}}}},
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

func TestDecodeRejectsServerToolEntries(t *testing.T) {
	body := `{"conversation_id":"c","outputs":[{"object":"entry","type":"tool.execution","id":"t","name":"web_search","arguments":""}],"usage":{}}`
	if _, err := p.Decode(strings.NewReader(body)); err == nil || !strings.Contains(err.Error(), "tool.execution") {
		t.Fatalf("%v", err)
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
		protocol.Thinking{Text: "hmm", Signature: "sig"},
		protocol.Text{Text: "Hello <Reference title=T>"},
		protocol.Text{Text: "plain"},
		protocol.ToolCall{ID: "c1", Name: "g", Args: json.RawMessage(`{"a":1}`)},
	}
	if !reflect.DeepEqual(resp.Content, want) || resp.Model != "mm" {
		t.Fatalf("%+v", resp.Content)
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
		protocol.Thinking{Text: "example-reasoning", Signature: "example-signature"},
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
	if !errors.As(err, &perr) || perr.Status != 200 || perr.Type != "conversation.response.error" || !strings.Contains(perr.Message, "example-message") {
		t.Fatalf("error event: %v", err)
	}
	for _, bad := range []string{
		"event: mystery\ndata: {\"type\":\"mystery\"}\n\n",
		"event: tool.execution.started\ndata: {\"type\":\"tool.execution.started\",\"output_index\":1,\"id\":\"tool_exec_1\",\"name\":\"web_search\"}\n\n",
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
	if e.Status != 422 || e.Type != "missing" || e.Message != "body.inputs: Field required" || string(e.Body) != string(body) {
		t.Fatalf("%+v", e)
	}
	if e.Error() != "protocol: http 422 missing: body.inputs: Field required" {
		t.Fatal(e.Error())
	}
	raw := p.DecodeError(502, []byte(" bad gateway \n"))
	if raw.Type != "" || raw.Message != "bad gateway" || raw.Status != 502 {
		t.Fatalf("%+v", raw)
	}
}

func TestRegistered(t *testing.T) {
	p, ok := protocol.Lookup(protocol.MistralConversations)
	if !ok || p.ID() != "mistral-conversations" {
		t.Fatal("not registered")
	}
}
