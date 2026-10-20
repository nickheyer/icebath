package openairesponses

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
)

const weatherSchema = `{"type":"object","properties":{"location":{"type":"string","description":"The city and state, e.g. San Francisco, CA"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["location","unit"]}`

// Spec section 6.2, verbatim function_call Response
const functionCallResponse = `{
  "id": "resp_67ca09c5efe0819096d0511c92b8c890096610f474011cc0",
  "object": "response",
  "access_programs": null,
  "created_at": 1741294021,
  "status": "completed",
  "completed_at": 1741294022,
  "error": null,
  "incomplete_details": null,
  "instructions": null,
  "max_output_tokens": null,
  "model": "gpt-6-astra",
  "output": [
    {
      "type": "function_call",
      "id": "fc_67ca09c6bedc8190a7abfec07b1a1332096610f474011cc0",
      "call_id": "call_unLAR8MvFNptuiZK6K6HCy5k",
      "name": "get_current_weather",
      "arguments": "{\"location\":\"Boston, MA\",\"unit\":\"celsius\"}",
      "status": "completed"
    }
  ],
  "parallel_tool_calls": true,
  "previous_response_id": null,
  "reasoning": {"effort": null, "summary": null},
  "store": true,
  "temperature": 1.0,
  "text": {"format": {"type": "text"}},
  "tool_choice": "auto",
  "tools": [{"type": "function", "description": "Get the current weather in a given location", "name": "get_current_weather", "parameters": ` + weatherSchema + `, "strict": true}],
  "top_p": 1.0,
  "truncation": "disabled",
  "usage": {"input_tokens": 291, "output_tokens": 23, "output_tokens_details": {"reasoning_tokens": 0}, "total_tokens": 314, "input_tokens_details": {"cached_tokens": 0, "cache_write_tokens": 0}},
  "user": null,
  "metadata": {}
}`

// Spec section 6.3, verbatim reasoning usage Response
const reasoningUsageResponse = `{
  "id": "resp_67ccd7eca01881908ff0b5146584e408072912b2993db808",
  "object": "response",
  "access_programs": null,
  "created_at": 1741477868,
  "status": "completed",
  "completed_at": 1741477869,
  "error": null,
  "incomplete_details": null,
  "instructions": null,
  "max_output_tokens": null,
  "model": "gpt-6-astra",
  "output": [
    {
      "type": "message",
      "id": "msg_67ccd7f7b5848190a6f3e95d809f6b44072912b2993db808",
      "status": "completed",
      "role": "assistant",
      "content": [{"type": "output_text", "text": "The classic tongue twister...", "annotations": [], "logprobs": []}]
    }
  ],
  "parallel_tool_calls": true,
  "previous_response_id": null,
  "reasoning": {"effort": "high", "summary": null},
  "store": true,
  "temperature": 1.0,
  "text": {"format": {"type": "text"}},
  "tool_choice": "auto",
  "tools": [],
  "top_p": 1.0,
  "truncation": "disabled",
  "usage": {"input_tokens": 81, "input_tokens_details": {"cached_tokens": 0, "cache_write_tokens": 0}, "output_tokens": 1035, "output_tokens_details": {"reasoning_tokens": 832}, "total_tokens": 1116},
  "user": null,
  "metadata": {}
}`

// Spec section 6.4, verbatim text streaming transcript including the omitted events comment
const textTranscript = `event: response.created
data: {"type":"response.created","response":{"id":"resp_67c9fdcecf488190bdd9a0409de3a1ec07b8b0ad4e5eb654","object":"response","access_programs":null,"created_at":1741290958,"status":"in_progress","error":null,"incomplete_details":null,"instructions":"You are a helpful assistant.","max_output_tokens":null,"model":"gpt-6-astra","output":[],"parallel_tool_calls":true,"previous_response_id":null,"reasoning":{"effort":null,"summary":null},"store":true,"temperature":1.0,"text":{"format":{"type":"text"}},"tool_choice":"auto","tools":[],"top_p":1.0,"truncation":"disabled","usage":null,"user":null,"metadata":{}},"sequence_number":0}

event: response.in_progress
data: {"type":"response.in_progress","response":{"id":"resp_67c9fdcecf488190bdd9a0409de3a1ec07b8b0ad4e5eb654","object":"response","access_programs":null,"created_at":1741290958,"status":"in_progress","error":null,"incomplete_details":null,"instructions":"You are a helpful assistant.","max_output_tokens":null,"model":"gpt-6-astra","output":[],"parallel_tool_calls":true,"previous_response_id":null,"reasoning":{"effort":null,"summary":null},"store":true,"temperature":1.0,"text":{"format":{"type":"text"}},"tool_choice":"auto","tools":[],"top_p":1.0,"truncation":"disabled","usage":null,"user":null,"metadata":{}},"sequence_number":1}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_67c9fdcf37fc8190ba82116e33fb28c507b8b0ad4e5eb654","type":"message","status":"in_progress","role":"assistant","content":[]},"sequence_number":2}

event: response.content_part.added
data: {"type":"response.content_part.added","item_id":"msg_67c9fdcf37fc8190ba82116e33fb28c507b8b0ad4e5eb654","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[],"logprobs":[]},"sequence_number":3}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_67c9fdcf37fc8190ba82116e33fb28c507b8b0ad4e5eb654","output_index":0,"content_index":0,"delta":"Hi","sequence_number":4,"logprobs":[]}

: Intermediate response events omitted.

event: response.output_text.done
data: {"type":"response.output_text.done","item_id":"msg_67c9fdcf37fc8190ba82116e33fb28c507b8b0ad4e5eb654","output_index":0,"content_index":0,"text":"Hi there! How can I assist you today?","sequence_number":10,"logprobs":[]}

event: response.content_part.done
data: {"type":"response.content_part.done","item_id":"msg_67c9fdcf37fc8190ba82116e33fb28c507b8b0ad4e5eb654","output_index":0,"content_index":0,"part":{"type":"output_text","text":"Hi there! How can I assist you today?","annotations":[],"logprobs":[]},"sequence_number":11}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_67c9fdcf37fc8190ba82116e33fb28c507b8b0ad4e5eb654","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hi there! How can I assist you today?","annotations":[],"logprobs":[]}]},"sequence_number":12}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_67c9fdcecf488190bdd9a0409de3a1ec07b8b0ad4e5eb654","object":"response","access_programs":null,"created_at":1741290958,"status":"completed","error":null,"incomplete_details":null,"instructions":"You are a helpful assistant.","max_output_tokens":null,"model":"gpt-6-astra","output":[{"id":"msg_67c9fdcf37fc8190ba82116e33fb28c507b8b0ad4e5eb654","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hi there! How can I assist you today?","annotations":[],"logprobs":[]}]}],"parallel_tool_calls":true,"previous_response_id":null,"reasoning":{"effort":null,"summary":null},"store":true,"temperature":1.0,"text":{"format":{"type":"text"}},"tool_choice":"auto","tools":[],"top_p":1.0,"truncation":"disabled","usage":{"input_tokens":37,"output_tokens":11,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":48,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}},"user":null,"metadata":{}},"sequence_number":13}

`

// Spec sections 6.5 and 6.6, ASSEMBLED FROM SOURCE EXAMPLES with one consistent item set
const toolTranscript = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"gpt-6-astra","output":[],"usage":null},"sequence_number":0}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[],"status":"in_progress"},"sequence_number":1}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""},"sequence_number":2}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"**Check","sequence_number":3}

event: response.reasoning_summary_text.done
data: {"type":"response.reasoning_summary_text.done","item_id":"rs_1","output_index":0,"summary_index":0,"text":"**Checking weather**","sequence_number":4}

event: response.reasoning_summary_part.done
data: {"type":"response.reasoning_summary_part.done","item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"**Checking weather**"},"sequence_number":5}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","item_id":"rs_1","output_index":0,"summary_index":1,"part":{"type":"summary_text","text":""},"sequence_number":6}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":1,"delta":"Boston it is.","sequence_number":7}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"**Checking weather**"},{"type":"summary_text","text":"Boston it is."}],"encrypted_content":"enc_1","status":"completed"},"sequence_number":8}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_unLAR8MvFNptuiZK6K6HCy5k","name":"get_current_weather","arguments":"","status":"in_progress"},"sequence_number":9}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"location\":","sequence_number":10}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"\"Boston, MA\",\"unit\":\"celsius\"}","sequence_number":11}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":1,"arguments":"{\"location\":\"Boston, MA\",\"unit\":\"celsius\"}","sequence_number":12}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_unLAR8MvFNptuiZK6K6HCy5k","name":"get_current_weather","arguments":"{\"location\":\"Boston, MA\",\"unit\":\"celsius\"}","status":"completed"},"sequence_number":13}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-6-astra","output":[{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"**Checking weather**"},{"type":"summary_text","text":"Boston it is."}],"encrypted_content":"enc_1"},{"type":"function_call","id":"fc_1","call_id":"call_unLAR8MvFNptuiZK6K6HCy5k","name":"get_current_weather","arguments":"{\"location\":\"Boston, MA\",\"unit\":\"celsius\"}","status":"completed"}],"usage":{"input_tokens":291,"output_tokens":23,"output_tokens_details":{"reasoning_tokens":12},"total_tokens":314,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":7}}},"sequence_number":14}

`

// Spec section 5, verbatim 429 slow_down body
const rateLimitBody = `{"error": {"message": "Your request rate increased too quickly. Please reduce the request rate and gradually increase it again.", "type": "rate_limit_error", "param": null, "code": "slow_down"}}`

func body(t *testing.T, call protocol.Call) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(call.Body, &m); err != nil {
		t.Fatalf("body %s: %v", call.Body, err)
	}
	return m
}

func items(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw := m["input"].([]any)
	out := make([]map[string]any, len(raw))
	for i, v := range raw {
		out[i] = v.(map[string]any)
	}
	return out
}

func encodeItems(t *testing.T, msgs ...protocol.Message) []map[string]any {
	t.Helper()
	call, err := Protocol{}.Encode(protocol.Request{Model: "m", Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	return items(t, body(t, call))
}

func fullRequest(stream bool) protocol.Request {
	temp := 0.5
	return protocol.Request{
		Model:  "gpt-6-astra",
		System: "You are a helpful assistant.",
		Tools:  []protocol.Tool{{Name: "get_current_weather", Description: "Get the current weather in a given location", Parameters: json.RawMessage(weatherSchema)}},
		Messages: []protocol.Message{
			{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "What is the weather like in Boston today?"}, protocol.Image{MediaType: "image/png", Data: []byte{1, 2, 3}}}},
			{Role: protocol.RoleAssistant, Content: []protocol.Part{
				protocol.Thinking{Text: "**Checking weather**", Signature: `{"id":"rs_1","encrypted_content":"enc_1"}`},
				protocol.Text{Text: "Checking."},
				protocol.ToolCall{ID: "call_unLAR8MvFNptuiZK6K6HCy5k", Name: "get_current_weather", Args: json.RawMessage(`{"location":"Boston, MA","unit":"celsius"}`)},
				protocol.Text{Text: "Now."},
			}},
			{Role: protocol.RoleTool, Content: []protocol.Part{
				protocol.ToolResult{CallID: "call_unLAR8MvFNptuiZK6K6HCy5k", Name: "get_current_weather", Content: []protocol.Part{protocol.Text{Text: "72F"}, protocol.Text{Text: "clear"}}},
				protocol.ToolResult{CallID: "call_2", Content: []protocol.Part{protocol.Text{Text: "chart"}, protocol.Image{MediaType: "image/jpeg", Data: []byte{9}}}, IsError: true},
			}},
		},
		ToolChoice:  protocol.ToolChoice{Mode: protocol.ToolChoiceAuto},
		MaxTokens:   100,
		Temperature: &temp,
		Stream:      stream,
	}
}

func TestCapabilities(t *testing.T) {
	want := protocol.Capabilities{Thinking: true, Images: true, ImagesInResults: true, Documents: true, ToolChoiceNone: true, ToolChoiceNamed: true, ServerTools: true, Citations: true}
	if got := (Protocol{}).Capabilities(); got != want {
		t.Fatalf("%+v", got)
	}
}

func TestEncodeRequest(t *testing.T) {
	call, err := Protocol{}.Encode(fullRequest(true))
	if err != nil {
		t.Fatal(err)
	}
	if call.Method != "POST" || call.Path != "/responses" || call.Header.Get("Content-Type") != "application/json" || call.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("call %+v", call)
	}
	m := body(t, call)
	if m["model"] != "gpt-6-astra" || m["instructions"] != "You are a helpful assistant." || m["store"] != false || m["stream"] != true || m["max_output_tokens"] != 100.0 || m["temperature"] != 0.5 || m["tool_choice"] != "auto" {
		t.Fatalf("top level %v", m)
	}
	if inc := m["include"].([]any); len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Fatalf("include %v", m["include"])
	}
	tool := m["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "get_current_weather" || tool["strict"] != false || tool["parameters"].(map[string]any)["type"] != "object" {
		t.Fatalf("tool %v", tool)
	}
	in := items(t, m)
	if len(in) != 6 {
		t.Fatalf("input %v", in)
	}
	user := in[0]["content"].([]any)
	if in[0]["type"] != "message" || in[0]["role"] != "user" || user[0].(map[string]any)["type"] != "input_text" {
		t.Fatalf("user %v", in[0])
	}
	if img := user[1].(map[string]any); img["type"] != "input_image" || img["image_url"] != "data:image/png;base64,AQID" || img["detail"] != "auto" {
		t.Fatalf("image %v", img)
	}
	if in[1]["type"] != "reasoning" || in[1]["id"] != "rs_1" || in[1]["encrypted_content"] != "enc_1" || in[1]["summary"].([]any)[0].(map[string]any)["text"] != "**Checking weather**" {
		t.Fatalf("reasoning %v", in[1])
	}
	texts := in[2]["content"].([]any)
	if in[2]["type"] != "message" || in[2]["role"] != "assistant" || in[2]["status"] != "completed" || len(texts) != 2 {
		t.Fatalf("assistant %v", in[2])
	}
	if _, has := in[2]["id"]; has {
		t.Fatal("assistant message must not fabricate an id")
	}
	if texts[0].(map[string]any)["type"] != "output_text" || texts[0].(map[string]any)["text"] != "Checking." || texts[1].(map[string]any)["text"] != "Now." {
		t.Fatalf("assistant text %v", texts)
	}
	if in[3]["type"] != "function_call" || in[3]["call_id"] != "call_unLAR8MvFNptuiZK6K6HCy5k" || in[3]["name"] != "get_current_weather" || in[3]["arguments"] != `{"location":"Boston, MA","unit":"celsius"}` {
		t.Fatalf("function_call %v", in[3])
	}
	if in[4]["type"] != "function_call_output" || in[4]["call_id"] != "call_unLAR8MvFNptuiZK6K6HCy5k" || in[4]["output"] != "72F\nclear" {
		t.Fatalf("output %v", in[4])
	}
	mixed := in[5]["output"].([]any)
	if in[5]["call_id"] != "call_2" || mixed[0].(map[string]any)["text"] != "[error] chart" || mixed[1].(map[string]any)["image_url"] != "data:image/jpeg;base64,CQ==" {
		t.Fatalf("mixed output %v", in[5])
	}
	if strings.Contains(string(call.Body), "stop") {
		t.Fatal("stop field leaked into body")
	}
}

func TestEncodeNonStreamAndToolChoice(t *testing.T) {
	req := fullRequest(false)
	req.ToolChoice = protocol.ToolChoice{Mode: protocol.ToolChoiceTool, Name: "get_current_weather"}
	call, err := Protocol{}.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	m := body(t, call)
	if call.Header.Get("Accept") != "application/json" || m["stream"] != nil {
		t.Fatalf("non stream %v", m)
	}
	if tc := m["tool_choice"].(map[string]any); tc["type"] != "function" || tc["name"] != "get_current_weather" {
		t.Fatalf("tool choice %v", tc)
	}
	for _, mode := range []protocol.ToolChoiceMode{protocol.ToolChoiceRequired, protocol.ToolChoiceNone} {
		req.ToolChoice = protocol.ToolChoice{Mode: mode}
		call, _ = Protocol{}.Encode(req)
		if body(t, call)["tool_choice"] != string(mode) {
			t.Fatal(mode)
		}
	}
	req.ToolChoice = protocol.ToolChoice{}
	call, _ = Protocol{}.Encode(req)
	if _, ok := body(t, call)["tool_choice"]; ok {
		t.Fatal("empty tool choice must be omitted")
	}
}

func TestEncodeExtraMerges(t *testing.T) {
	req := fullRequest(false)
	req.Extra = map[string]json.RawMessage{"reasoning": json.RawMessage(`{"effort":"high"}`), "store": json.RawMessage(`true`)}
	call, err := Protocol{}.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	m := body(t, call)
	if m["store"] != true || m["reasoning"].(map[string]any)["effort"] != "high" {
		t.Fatalf("extra %v", m)
	}
}

func TestEncodeDocuments(t *testing.T) {
	in := encodeItems(t,
		protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{
			protocol.Document{MediaType: "application/pdf", Data: []byte("%PDF"), Name: "report.pdf"},
			protocol.Document{MediaType: "text/plain", Data: []byte("hi")},
		}},
		protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{ID: "c1", Name: "f"}}},
		protocol.Message{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "c1", IsError: true, Content: []protocol.Part{protocol.Document{MediaType: "application/pdf", Data: []byte{1}, Name: "x.pdf"}}}}},
	)
	content := in[0]["content"].([]any)
	first := content[0].(map[string]any)
	if first["type"] != "input_file" || first["filename"] != "report.pdf" || first["file_data"] != "data:application/pdf;base64,JVBERg==" {
		t.Fatalf("%v", first)
	}
	if second := content[1].(map[string]any); second["filename"] != "document.txt" {
		t.Fatalf("%v", second)
	}
	output := in[2]["output"].([]any)
	if output[0].(map[string]any)["text"] != "[error] " || output[1].(map[string]any)["type"] != "input_file" {
		t.Fatalf("%v", output)
	}
	if _, err := (Protocol{}).Encode(protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Document{MediaType: "application/pdf"}}}}}); err == nil {
		t.Fatal("document without data accepted")
	}
}

// Rule 3 replay policy for signed parts and vendor tool items
func TestEncodeOriginPolicy(t *testing.T) {
	foreign := protocol.AnthropicMessages
	in := encodeItems(t, protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{
		protocol.Thinking{Text: "foreign thought", Signature: `{"id":"rs_f","encrypted_content":"foreign-enc"}`, Origin: foreign},
		protocol.Thinking{Redacted: true, Signature: "redacted-sig"},
		protocol.Thinking{Text: "same", Signature: `{"id":"rs_s"}`, Origin: protocol.OpenAIResponses},
		protocol.Thinking{Text: "empty", Signature: `{"id":"rs_e"}`},
		protocol.ToolCall{ID: "c1", Name: "f", Args: json.RawMessage(`{"a":1}`), Signature: "tool-sig", Origin: foreign},
		protocol.ServerToolCall{ID: "ws_f", Name: "web_search_call", Args: json.RawMessage(`{"status":"completed"}`), Origin: foreign},
		protocol.ServerToolResult{CallID: "ws_f", Name: "web_search_call", Content: json.RawMessage(`[]`), Origin: foreign},
		protocol.ServerToolCall{ID: "ws_1", Name: "web_search_call", Args: json.RawMessage(`{"status":"completed","action":{"type":"search","query":"q"}}`), Origin: protocol.OpenAIResponses},
		protocol.ServerToolResult{CallID: "ws_1", Name: "web_search_call", Content: json.RawMessage(`[]`), Origin: protocol.OpenAIResponses},
		protocol.Text{Text: "answer"},
	}})
	raw, _ := json.Marshal(in)
	for _, leaked := range []string{"foreign thought", "foreign-enc", "rs_f", "redacted-sig", "tool-sig", "ws_f"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("leaked %q in %s", leaked, raw)
		}
	}
	if len(in) != 5 || in[0]["id"] != "rs_s" || in[1]["id"] != "rs_e" || in[2]["type"] != "function_call" {
		t.Fatalf("%v", in)
	}
	if in[3]["type"] != "web_search_call" || in[3]["id"] != "ws_1" || in[3]["status"] != "completed" || in[3]["action"].(map[string]any)["query"] != "q" {
		t.Fatalf("server tool item %v", in[3])
	}
	if in[4]["type"] != "message" || in[4]["content"].([]any)[0].(map[string]any)["text"] != "answer" {
		t.Fatalf("%v", in[4])
	}
	in = encodeItems(t, protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Thinking{Text: "gone", Signature: `{"id":"rs_g"}`, Origin: foreign}}})
	if len(in) != 0 {
		t.Fatalf("empty assistant turn must omit items: %v", in)
	}
}

func TestEncodeErrors(t *testing.T) {
	user := func(parts ...protocol.Part) []protocol.Message {
		return []protocol.Message{{Role: protocol.RoleUser, Content: parts}}
	}
	cases := map[string]protocol.Request{
		"no model":           {Messages: user(protocol.Text{Text: "x"})},
		"stop sequences":     {Model: "m", StopSequences: []string{"END"}, Messages: user(protocol.Text{Text: "x"})},
		"tool no schema":     {Model: "m", Tools: []protocol.Tool{{Name: "f"}}, Messages: user(protocol.Text{Text: "x"})},
		"tool no name":       {Model: "m", Tools: []protocol.Tool{{Parameters: json.RawMessage(`{}`)}}, Messages: user(protocol.Text{Text: "x"})},
		"tool choice noname": {Model: "m", ToolChoice: protocol.ToolChoice{Mode: protocol.ToolChoiceTool}, Messages: user(protocol.Text{Text: "x"})},
		"tool choice mode":   {Model: "m", ToolChoice: protocol.ToolChoice{Mode: "sometimes"}, Messages: user(protocol.Text{Text: "x"})},
		"empty message":      {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleUser}}},
		"unknown role":       {Model: "m", Messages: []protocol.Message{{Role: "system", Content: []protocol.Part{protocol.Text{Text: "x"}}}}},
		"image no type":      {Model: "m", Messages: user(protocol.Image{Data: []byte{1}})},
		"image no data":      {Model: "m", Messages: user(protocol.Image{MediaType: "image/png"})},
		"user tool result":   {Model: "m", Messages: user(protocol.ToolResult{CallID: "c"})},
		"user thinking":      {Model: "m", Messages: user(protocol.Thinking{Text: "t"})},
		"assistant result":   {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolResult{CallID: "c"}}}}},
		"bad call args":      {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{ID: "c", Name: "f", Args: json.RawMessage(`[1]`)}}}}},
		"null call args":     {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{ID: "c", Name: "f", Args: json.RawMessage(`null`)}}}}},
		"server no type":     {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ServerToolCall{ID: "s"}}}}},
		"server bad args":    {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ServerToolCall{ID: "s", Name: "web_search_call", Args: json.RawMessage(`[1]`)}}}}},
		"tool turn text":     {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.Text{Text: "x"}}}}},
		"result thinking":    {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "c", Content: []protocol.Part{protocol.Thinking{}}}}}}},
		"result bad image":   {Model: "m", Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "c", Content: []protocol.Part{protocol.Image{MediaType: "image/png"}}}}}}},
		"bad extra":          {Model: "m", Extra: map[string]json.RawMessage{"x": json.RawMessage(`{`)}, Messages: user(protocol.Text{Text: "x"})},
	}
	for name, req := range cases {
		if _, err := (Protocol{}).Encode(req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Thinking that another protocol signed has no reasoning item to rebuild and is left out
func TestEncodeOmitsForeignThinking(t *testing.T) {
	for _, sig := range []string{"", "sig_from_elsewhere", `{"encrypted_content":"x"}`} {
		req := protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{
			protocol.Thinking{Text: "t", Signature: sig},
			protocol.Text{Text: "answer"},
		}}}}
		call, err := (Protocol{}).Encode(req)
		if err != nil {
			t.Fatalf("%q: %v", sig, err)
		}
		input := body(t, call)["input"].([]any)
		if len(input) != 1 || input[0].(map[string]any)["type"] != "message" {
			t.Fatalf("%q: %v", sig, input)
		}
	}
}

func TestEncodeEmptyToolCallArgsAndResult(t *testing.T) {
	req := protocol.Request{Model: "m", Messages: []protocol.Message{
		{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{ID: "c1", Name: "f"}}},
		{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "c1"}, protocol.ToolResult{CallID: "c2", IsError: true}}},
	}}
	call, err := Protocol{}.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	in := items(t, body(t, call))
	if in[0]["arguments"] != "{}" || in[1]["output"] != "" || in[2]["output"] != "[error] " {
		t.Fatalf("%v", in)
	}
}

func TestDecodeFunctionCall(t *testing.T) {
	resp, err := Protocol{}.Decode(strings.NewReader(functionCallResponse))
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "resp_67ca09c5efe0819096d0511c92b8c890096610f474011cc0" || resp.Model != "gpt-6-astra" {
		t.Fatalf("%+v", resp)
	}
	if resp.StopReason != protocol.StopToolUse || resp.RawStopReason != "completed" {
		t.Fatalf("stop %s %s", resp.StopReason, resp.RawStopReason)
	}
	tc := resp.Content[0].(protocol.ToolCall)
	if tc.ID != "call_unLAR8MvFNptuiZK6K6HCy5k" || tc.Name != "get_current_weather" || string(tc.Args) != `{"location":"Boston, MA","unit":"celsius"}` {
		t.Fatalf("%+v", tc)
	}
	if resp.Usage != (protocol.Usage{Input: 291, Output: 23}) {
		t.Fatalf("usage %+v", resp.Usage)
	}
}

func TestDecodeReasoningUsage(t *testing.T) {
	resp, err := Protocol{}.Decode(strings.NewReader(reasoningUsageResponse))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != protocol.StopEnd || resp.Content[0].(protocol.Text).Text != "The classic tongue twister..." {
		t.Fatalf("%+v", resp)
	}
	if resp.Usage != (protocol.Usage{Input: 81, Output: 1035, Reasoning: 832}) {
		t.Fatalf("usage %+v", resp.Usage)
	}
	cached := `{"id":"r","status":"completed","model":"m","output":[],"usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`
	resp, err = Protocol{}.Decode(strings.NewReader(cached))
	if err != nil || resp.Usage != (protocol.Usage{Input: 60, Output: 5, CacheRead: 40, CacheWrite: 3, Reasoning: 2}) {
		t.Fatalf("%+v %v", resp.Usage, err)
	}
}

// Reasoning item shaped by the ReasoningItem schema in spec section 2.2.7
func TestSignatureRoundTrip(t *testing.T) {
	in := `{"id":"resp_2","status":"completed","model":"m","output":[{"type":"reasoning","id":"rs_9","summary":[{"type":"summary_text","text":"one"},{"type":"summary_text","text":"two"}],"content":[{"type":"reasoning_text","text":"raw"}],"encrypted_content":"ENC"},{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]}`
	resp, err := Protocol{}.Decode(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	th := resp.Content[0].(protocol.Thinking)
	if th.Text != "one\n\ntwo\n\nraw" || th.Signature != `{"id":"rs_9","encrypted_content":"ENC"}` || th.Origin != protocol.OpenAIResponses {
		t.Fatalf("%+v", th)
	}
	if resp.Content[1].(protocol.Text).Text != "no" {
		t.Fatalf("%+v", resp.Content[1])
	}
	call, err := Protocol{}.Encode(protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{th}}}})
	if err != nil {
		t.Fatal(err)
	}
	item := items(t, body(t, call))[0]
	if item["type"] != "reasoning" || item["id"] != "rs_9" || item["encrypted_content"] != "ENC" || item["summary"].([]any)[0].(map[string]any)["type"] != "summary_text" {
		t.Fatalf("%v", item)
	}
	empty := protocol.Thinking{Signature: `{"id":"rs_0"}`}
	call, _ = Protocol{}.Encode(protocol.Request{Model: "m", Messages: []protocol.Message{{Role: protocol.RoleAssistant, Content: []protocol.Part{empty}}}})
	if !strings.Contains(string(call.Body), `"summary":[]`) {
		t.Fatalf("%s", call.Body)
	}
}

// Built in tool items from the web search, file search, code interpreter and MCP guides, assembled
func TestDecodeServerToolsAndCitations(t *testing.T) {
	in := `{"id":"r","status":"completed","model":"m","output":[
	  {"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"weather"}},
	  {"type":"file_search_call","id":"fs_1","status":"completed","queries":["q"],"results":[{"file_id":"file_1","filename":"a.pdf","text":"hello"}]},
	  {"type":"code_interpreter_call","id":"ci_1","status":"completed","code":"print(1)","container_id":"c","outputs":[{"type":"logs","logs":"1"}]},
	  {"type":"mcp_call","id":"mcp_1","name":"roll","server_label":"dice","arguments":"{}","output":"6","error":null},
	  {"type":"mcp_list_tools","id":"mcpl_1","server_label":"dice","tools":[{"name":"roll"}]},
	  {"type":"mcp_approval_request","id":"mcpr_1","name":"roll","server_label":"dice","arguments":"{}"},
	  {"type":"image_generation_call","id":"ig_1","status":"completed","result":"QUJD"},
	  {"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Sunny [1].","annotations":[
	    {"type":"url_citation","start_index":6,"end_index":9,"url":"https://w.example","title":"Weather"},
	    {"type":"file_citation","file_id":"file_1","filename":"a.pdf","index":9},
	    {"type":"container_file_citation","container_id":"c","file_id":"f","start_index":0,"end_index":1}
	  ]}]}
	]}`
	resp, err := Protocol{}.Decode(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != protocol.StopEnd {
		t.Fatalf("%+v", resp)
	}
	ws := resp.Content[0].(protocol.ServerToolCall)
	if ws.ID != "ws_1" || ws.Name != "web_search_call" || ws.Origin != protocol.OpenAIResponses || string(ws.Args) != `{"action":{"type":"search","query":"weather"},"status":"completed"}` {
		t.Fatalf("%+v", ws)
	}
	fs := resp.Content[1].(protocol.ServerToolCall)
	fsr := resp.Content[2].(protocol.ServerToolResult)
	if fs.Name != "file_search_call" || fsr.CallID != "fs_1" || fsr.Name != "file_search_call" || fsr.Origin != protocol.OpenAIResponses || !strings.Contains(string(fsr.Content), "hello") {
		t.Fatalf("%+v %+v", fs, fsr)
	}
	if r := resp.Content[4].(protocol.ServerToolResult); r.Name != "code_interpreter_call" || !strings.Contains(string(r.Content), "logs") {
		t.Fatalf("%+v", r)
	}
	if r := resp.Content[6].(protocol.ServerToolResult); r.Name != "mcp_call" || string(r.Content) != `"6"` {
		t.Fatalf("%+v", r)
	}
	if r := resp.Content[8].(protocol.ServerToolResult); r.Name != "mcp_list_tools" || !strings.Contains(string(r.Content), "roll") {
		t.Fatalf("%+v", r)
	}
	if c := resp.Content[9].(protocol.ServerToolCall); c.Name != "mcp_approval_request" {
		t.Fatalf("%+v", c)
	}
	if r := resp.Content[11].(protocol.ServerToolResult); r.Name != "image_generation_call" || string(r.Content) != `"QUJD"` {
		t.Fatalf("%+v", r)
	}
	text := resp.Content[12].(protocol.Text)
	want := []protocol.Citation{
		{Title: "Weather", URL: "https://w.example", Start: 6, End: 9},
		{Title: "a.pdf", Document: "a.pdf", Start: 9, End: 9},
	}
	if text.Text != "Sunny [1]." || !reflect.DeepEqual(text.Citations, want) {
		t.Fatalf("%+v", text)
	}
	if len(resp.Content) != 13 {
		t.Fatalf("%d parts", len(resp.Content))
	}
	replay := encodeItems(t, protocol.Message{Role: protocol.RoleAssistant, Content: resp.Content})
	if len(replay) != 8 {
		t.Fatalf("replay %v", replay)
	}
	if replay[0]["type"] != "web_search_call" || replay[0]["id"] != "ws_1" || replay[0]["action"].(map[string]any)["query"] != "weather" {
		t.Fatalf("%v", replay[0])
	}
	if replay[1]["type"] != "file_search_call" || replay[1]["results"].([]any)[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("%v", replay[1])
	}
	if replay[7]["type"] != "message" {
		t.Fatalf("%v", replay[7])
	}
}

func TestDecodeStatuses(t *testing.T) {
	resp, err := Protocol{}.Decode(strings.NewReader(`{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"model":"m","output":[{"type":"message","id":"msg","role":"assistant","content":[{"type":"output_text","text":"partial"}]}],"usage":null}`))
	if err != nil || resp.StopReason != protocol.StopLength || resp.RawStopReason != "incomplete/max_output_tokens" {
		t.Fatalf("%+v %v", resp, err)
	}
	resp, _ = Protocol{}.Decode(strings.NewReader(`{"id":"r","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[]}`))
	if resp.StopReason != protocol.StopContentFilter {
		t.Fatal("content filter")
	}
	resp, _ = Protocol{}.Decode(strings.NewReader(`{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_messages"},"output":[]}`))
	if resp.StopReason != protocol.StopOther || resp.RawStopReason != "incomplete/max_messages" {
		t.Fatal("other")
	}
	failed := `{"id":"r","status":"failed","error":{"code":"server_error","message":"The model failed to generate a response."},"output":[]}`
	_, err = Protocol{}.Decode(strings.NewReader(failed))
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Type != "server_error" || pe.Message != "The model failed to generate a response." || string(pe.Body) != failed || !pe.Transient || pe.InStream {
		t.Fatalf("%+v", pe)
	}
	_, err = Protocol{}.Decode(strings.NewReader(`{"id":"r","status":"failed","output":[]}`))
	if !errors.As(err, &pe) || pe.Message != "response failed" || pe.Transient {
		t.Fatalf("%+v", pe)
	}
	for name, in := range map[string]string{
		"non terminal":   `{"id":"r","status":"in_progress","output":[]}`,
		"output item":    `{"id":"r","status":"completed","output":[{"type":"function_call_output","call_id":"c","output":"x"}]}`,
		"untyped item":   `{"id":"r","status":"completed","output":[{"id":"x"}]}`,
		"bad arguments":  `{"id":"r","status":"completed","output":[{"type":"function_call","call_id":"c","name":"f","arguments":"[1]"}]}`,
		"bad part":       `{"id":"r","status":"completed","output":[{"type":"message","id":"m","content":[{"type":"output_audio"}]}]}`,
		"item not json":  `{"id":"r","status":"completed","output":[1]}`,
		"item null":      `{"id":"r","status":"completed","output":[null]}`,
		"garbage":        `not json`,
		"server as list": `{"id":"r","status":"completed","output":[{"type":"web_search_call","id":"ws","action":}]}`,
	} {
		if _, err := (Protocol{}).Decode(strings.NewReader(in)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func eventTypes(t *testing.T, transcript string) ([]protocol.Event, error) {
	t.Helper()
	var events []protocol.Event
	for ev, err := range (Protocol{}).Stream(strings.NewReader(transcript)) {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}

func assertTypes(t *testing.T, events []protocol.Event, want ...protocol.EventType) {
	t.Helper()
	got := make([]protocol.EventType, len(events))
	for i, e := range events {
		got[i] = e.Type
	}
	if len(got) != len(want) {
		t.Fatalf("events %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %s want %s (all %v)", i, got[i], want[i], got)
		}
	}
}

func TestStreamText(t *testing.T) {
	events, err := eventTypes(t, textTranscript)
	if err != nil {
		t.Fatal(err)
	}
	assertTypes(t, events, protocol.EventStart, protocol.EventTextStart, protocol.EventTextDelta, protocol.EventTextDelta, protocol.EventTextEnd, protocol.EventDone)
	if events[2].Delta != "Hi" || events[3].Delta != " there! How can I assist you today?" {
		t.Fatalf("deltas %q %q", events[2].Delta, events[3].Delta)
	}
	resp, err := protocol.Collect(Protocol{}.Stream(strings.NewReader(textTranscript)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "resp_67c9fdcecf488190bdd9a0409de3a1ec07b8b0ad4e5eb654" || resp.Model != "gpt-6-astra" || resp.StopReason != protocol.StopEnd {
		t.Fatalf("%+v", resp)
	}
	if resp.Content[0].(protocol.Text).Text != "Hi there! How can I assist you today?" || resp.Usage != (protocol.Usage{Input: 37, Output: 11}) {
		t.Fatalf("%+v", resp)
	}
}

func TestStreamReasoningAndFunctionCall(t *testing.T) {
	events, err := eventTypes(t, toolTranscript)
	if err != nil {
		t.Fatal(err)
	}
	assertTypes(t, events,
		protocol.EventStart,
		protocol.EventThinkingStart, protocol.EventThinkingDelta, protocol.EventThinkingDelta, protocol.EventThinkingDelta, protocol.EventThinkingDelta, protocol.EventThinkingEnd,
		protocol.EventToolCallStart, protocol.EventToolCallDelta, protocol.EventToolCallDelta, protocol.EventToolCallEnd,
		protocol.EventDone)
	if events[3].Delta != "ing weather**" || events[4].Delta != "\n\n" || events[5].Delta != "Boston it is." {
		t.Fatalf("thinking deltas %q %q %q", events[3].Delta, events[4].Delta, events[5].Delta)
	}
	final := events[len(events)-1].Response
	th := final.Content[0].(protocol.Thinking)
	if th.Text != "**Checking weather**\n\nBoston it is." || th.Signature != `{"id":"rs_1","encrypted_content":"enc_1"}` || th.Origin != protocol.OpenAIResponses {
		t.Fatalf("%+v", th)
	}
	tc := final.Content[1].(protocol.ToolCall)
	if tc.ID != "call_unLAR8MvFNptuiZK6K6HCy5k" || tc.Name != "get_current_weather" || string(tc.Args) != `{"location":"Boston, MA","unit":"celsius"}` {
		t.Fatalf("%+v", tc)
	}
	if final.StopReason != protocol.StopToolUse || final.Usage != (protocol.Usage{Input: 286, Output: 23, CacheRead: 5, CacheWrite: 7, Reasoning: 12}) {
		t.Fatalf("%+v", final)
	}
}

// Web search streaming events from the built in tools guide with a url citation annotation, assembled
func TestStreamServerToolAndCitation(t *testing.T) {
	transcript := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"in_progress"}}

event: response.web_search_call.searching
data: {"type":"response.web_search_call.searching","item_id":"ws_1","output_index":0}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"q"}}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"file_search_call","id":"fs_1","status":"in_progress"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"type":"file_search_call","id":"fs_1","status":"completed","queries":["q"],"results":[{"file_id":"f1","text":"t"}]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":2,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","item_id":"msg_1","output_index":2,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":2,"content_index":0,"delta":"Sunny"}

event: response.output_text.annotation.added
data: {"type":"response.output_text.annotation.added","item_id":"msg_1","output_index":2,"content_index":0,"annotation_index":0,"annotation":{"type":"url_citation","start_index":0,"end_index":5,"url":"https://w.example","title":"Weather"}}

event: response.output_text.annotation.added
data: {"type":"response.output_text.annotation.added","item_id":"msg_1","output_index":2,"content_index":0,"annotation_index":1,"annotation":{"type":"file_path","file_id":"f"}}

event: response.content_part.done
data: {"type":"response.content_part.done","item_id":"msg_1","output_index":2,"content_index":0,"part":{"type":"output_text","text":"Sunny","annotations":[]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"r","status":"completed","model":"m","output":[]}}

`
	events, err := eventTypes(t, transcript)
	if err != nil {
		t.Fatal(err)
	}
	assertTypes(t, events,
		protocol.EventStart,
		protocol.EventServerToolCallStart, protocol.EventServerToolCallDelta, protocol.EventServerToolCallEnd,
		protocol.EventServerToolCallStart, protocol.EventServerToolCallDelta, protocol.EventServerToolCallEnd, protocol.EventServerToolResult,
		protocol.EventTextStart, protocol.EventTextDelta, protocol.EventCitation, protocol.EventTextEnd,
		protocol.EventDone)
	final := events[len(events)-1].Response
	ws := final.Content[0].(protocol.ServerToolCall)
	if ws.ID != "ws_1" || ws.Name != "web_search_call" || ws.Origin != protocol.OpenAIResponses || string(ws.Args) != `{"action":{"type":"search","query":"q"},"status":"completed"}` {
		t.Fatalf("%+v", ws)
	}
	if r := final.Content[2].(protocol.ServerToolResult); r.CallID != "fs_1" || r.Name != "file_search_call" || r.Origin != protocol.OpenAIResponses || !strings.Contains(string(r.Content), "f1") {
		t.Fatalf("%+v", r)
	}
	text := final.Content[3].(protocol.Text)
	want := protocol.Citation{Title: "Weather", URL: "https://w.example", End: 5}
	if text.Text != "Sunny" || len(text.Citations) != 1 || text.Citations[0] != want || events[10].Citation != want {
		t.Fatalf("%+v", text)
	}
	if final.StopReason != protocol.StopEnd {
		t.Fatalf("%+v", final)
	}
}

func TestStreamRecoversArgumentsFromDoneOnly(t *testing.T) {
	transcript := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"c1","name":"f","arguments":""}}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"a\":1}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"c1","name":"f","arguments":"{\"a\":1}"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"r","status":"completed","model":"m","output":[]}}

`
	resp, err := protocol.Collect(Protocol{}.Stream(strings.NewReader(transcript)))
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Content[0].(protocol.ToolCall).Args) != `{"a":1}` || resp.StopReason != protocol.StopToolUse {
		t.Fatalf("%+v", resp)
	}
}

func TestStreamFailures(t *testing.T) {
	head := `event: response.created
data: {"type":"response.created","response":{"id":"r","status":"in_progress","model":"m","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"c1","name":"f","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"a\":"}

`
	cases := map[string]struct {
		tail      string
		errType   string
		transient bool
	}{
		"diverging arguments": {tail: "event: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"output_index\":0,\"arguments\":\"{\\\"b\\\":2}\"}\n\n"},
		"truncated":           {tail: ""},
		"response.failed":     {tail: "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"r\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"The model failed to generate a response.\"}},\"sequence_number\":1}\n\n", errType: "server_error", transient: true},
		"failed no response":  {tail: "event: response.failed\ndata: {\"type\":\"response.failed\",\"sequence_number\":1}\n\n"},
		"error event":         {tail: "event: error\ndata: {\"type\":\"error\",\"code\":\"ERR_SOMETHING\",\"message\":\"Something went wrong\",\"param\":null,\"sequence_number\":1}\n\n", errType: "ERR_SOMETHING"},
		"transient error":     {tail: "event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow\",\"sequence_number\":1}\n\n", errType: "rate_limit_exceeded", transient: true},
		"output item":         {tail: "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call_output\",\"call_id\":\"c\"}}\n\n"},
		"item null":           {tail: "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1}\n\n"},
		"done item null":      {tail: "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1}\n\n"},
		"done output item":    {tail: "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"function_call_output\"}}\n\n"},
		"bad json":            {tail: "event: response.completed\ndata: {nope}\n\n"},
		"orphan delta":        {tail: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_x\",\"content_index\":0,\"delta\":\"hi\"}\n\n"},
		"orphan citation":     {tail: "event: response.output_text.annotation.added\ndata: {\"type\":\"response.output_text.annotation.added\",\"item_id\":\"msg_x\",\"content_index\":0,\"annotation\":{\"type\":\"url_citation\",\"url\":\"u\"}}\n\n"},
		"part missing":        {tail: "event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"item_id\":\"m\",\"content_index\":0}\n\n"},
		"part done missing":   {tail: "event: response.content_part.done\ndata: {\"type\":\"response.content_part.done\",\"item_id\":\"m\",\"content_index\":0}\n\n"},
		"summary missing":     {tail: "event: response.reasoning_summary_part.done\ndata: {\"type\":\"response.reasoning_summary_part.done\",\"item_id\":\"rs\",\"summary_index\":0}\n\n"},
		"terminal no resp":    {tail: "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"},
		"terminal bad item":   {tail: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[1]}}\n\n"},
	}
	for name, c := range cases {
		events, err := eventTypes(t, head+c.tail)
		if err == nil {
			t.Errorf("%s: no error, events %d", name, len(events))
			continue
		}
		if len(events) != 3 {
			t.Errorf("%s: %d events before the error", name, len(events))
		}
		if c.errType != "" {
			var pe *protocol.Error
			if !errors.As(err, &pe) || pe.Type != c.errType || pe.Status != 200 || !pe.InStream || pe.Transient != c.transient || len(pe.Body) == 0 {
				t.Errorf("%s: %+v", name, pe)
			}
		}
	}
}

func TestStreamIgnoresDoneSentinelAndProgressEvents(t *testing.T) {
	transcript := `event: response.created
data: {"type":"response.created","response":{"id":"r","status":"in_progress","model":"m","output":[]}}

event: response.output_text.annotation.added
data: {"type":"response.output_text.annotation.added","item_id":"msg_1","output_index":0,"content_index":0,"annotation_index":0,"annotation":null}

event: response.incomplete
data: {"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"model":"m","output":[],"usage":null}}

data: [DONE]

`
	resp, err := protocol.Collect(Protocol{}.Stream(strings.NewReader(transcript)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != protocol.StopLength || resp.RawStopReason != "incomplete/max_output_tokens" {
		t.Fatalf("%+v", resp)
	}
}

func TestDecodeError(t *testing.T) {
	e := Protocol{}.DecodeError(429, []byte(rateLimitBody))
	if e.Status != 429 || e.Type != "slow_down" || !strings.HasPrefix(e.Message, "Your request rate increased too quickly.") || len(e.Body) == 0 || !e.Transient || e.InStream {
		t.Fatalf("%+v", e)
	}
	if e.Error() != "protocol: http 429 slow_down: "+e.Message {
		t.Fatal(e.Error())
	}
	e = Protocol{}.DecodeError(401, []byte(`{"error":{"message":"bad key","type":"invalid_request_error","param":null,"code":null}}`))
	if e.Type != "invalid_request_error" || e.Message != "bad key" || e.Transient {
		t.Fatalf("%+v", e)
	}
	e = Protocol{}.DecodeError(400, []byte(`{"error":{"message":"busy","type":"server_error"}}`))
	if !e.Transient {
		t.Fatal("server_error must be transient")
	}
	e = Protocol{}.DecodeError(502, []byte("  <html>gateway</html>\n"))
	if e.Status != 502 || e.Type != "" || e.Message != "<html>gateway</html>" || !e.Transient {
		t.Fatalf("%+v", e)
	}
	for status, want := range map[int]bool{408: true, 409: true, 500: true, 404: false} {
		if got := (Protocol{}).DecodeError(status, []byte("x")).Transient; got != want {
			t.Fatalf("%d transient %v", status, got)
		}
	}
}

func TestRegistered(t *testing.T) {
	p, ok := protocol.Lookup(protocol.OpenAIResponses)
	if !ok || p.ID() != "openai-responses" {
		t.Fatal("not registered")
	}
}
