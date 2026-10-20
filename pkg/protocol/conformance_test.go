package protocol_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
	bedrockconversestream "github.com/nickheyer/icebath/pkg/protocol/bedrock-converse-stream"

	_ "github.com/nickheyer/icebath/pkg/protocol/anthropic-messages"
	_ "github.com/nickheyer/icebath/pkg/protocol/google-generative-ai"
	_ "github.com/nickheyer/icebath/pkg/protocol/mistral-conversations"
	_ "github.com/nickheyer/icebath/pkg/protocol/ollama-chat"
	_ "github.com/nickheyer/icebath/pkg/protocol/openai-completions"
	_ "github.com/nickheyer/icebath/pkg/protocol/openai-responses"
)

var known = []protocol.ID{
	protocol.AnthropicMessages,
	protocol.BedrockConverseStream,
	protocol.GoogleGenerativeAI,
	protocol.MistralConversations,
	protocol.OllamaChat,
	protocol.OpenAICompletions,
	protocol.OpenAIResponses,
}

// Conversation every protocol must carry, one full tool round trip with text and an image
func conversation(stream bool) protocol.Request {
	temp := 0.2
	return protocol.Request{
		Model:  "model-x",
		System: "You are terse.",
		Messages: []protocol.Message{
			{Role: protocol.RoleUser, Content: []protocol.Part{
				protocol.Text{Text: "What is in this image?"},
				protocol.Image{MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
			}},
			{Role: protocol.RoleAssistant, Content: []protocol.Part{
				protocol.Text{Text: "Let me look that up."},
				protocol.ToolCall{ID: "call_1", Name: "lookup", Args: json.RawMessage(`{"q":"png"}`)},
			}},
			{Role: protocol.RoleTool, Content: []protocol.Part{
				protocol.ToolResult{CallID: "call_1", Name: "lookup", Content: []protocol.Part{protocol.Text{Text: "a logo"}}},
			}},
			{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "Thanks"}}},
		},
		Tools: []protocol.Tool{{
			Name:        "lookup",
			Description: "Looks something up",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
		}},
		ToolChoice:  protocol.ToolChoice{Mode: protocol.ToolChoiceAuto},
		MaxTokens:   256,
		Temperature: &temp,
		Stream:      stream,
		Extra:       map[string]json.RawMessage{"icebath_marker": json.RawMessage(`true`)},
	}
}

func TestEveryProtocolIsRegistered(t *testing.T) {
	ids := protocol.IDs()
	if len(ids) != len(known) {
		t.Fatalf("registered %v", ids)
	}
	for i, id := range known {
		if ids[i] != id {
			t.Fatalf("registered %v", ids)
		}
		if _, ok := protocol.Lookup(id); !ok {
			t.Fatalf("%s not registered", id)
		}
	}
	if _, ok := protocol.Lookup("nope"); ok {
		t.Fatal("unknown id resolved")
	}
}

func TestEveryProtocolEncodesTheConversation(t *testing.T) {
	for _, id := range known {
		p, _ := protocol.Lookup(id)
		for _, stream := range []bool{false, true} {
			call, err := p.Encode(conversation(stream))
			if err != nil {
				t.Fatalf("%s stream=%v: %v", id, stream, err)
			}
			if call.Method != "POST" || call.Path == "" || !strings.HasPrefix(call.Path, "/") {
				t.Fatalf("%s call %+v", id, call)
			}
			if call.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("%s content type %q", id, call.Header.Get("Content-Type"))
			}
			var body map[string]any
			if err := json.Unmarshal(call.Body, &body); err != nil {
				t.Fatalf("%s body: %v", id, err)
			}
			if body["icebath_marker"] != true {
				t.Fatalf("%s dropped Extra", id)
			}
			if !bytes.Contains(call.Body, []byte("lookup")) || !bytes.Contains(call.Body, []byte("call_1")) {
				t.Fatalf("%s lost the tool round trip", id)
			}
		}
	}
}

func TestEveryProtocolRejectsMalformedMessages(t *testing.T) {
	bad := map[string]protocol.Request{
		"unknown role":        {Model: "m", MaxTokens: 8, Messages: []protocol.Message{{Role: "ghost", Content: []protocol.Part{protocol.Text{Text: "x"}}}}},
		"result in user":      {Model: "m", MaxTokens: 8, Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.ToolResult{CallID: "c"}}}}},
		"text in tool":        {Model: "m", MaxTokens: 8, Messages: []protocol.Message{{Role: protocol.RoleTool, Content: []protocol.Part{protocol.Text{Text: "x"}}}}},
		"tool without schema": {Model: "m", MaxTokens: 8, Tools: []protocol.Tool{{Name: "t"}}, Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "x"}}}}},
		"bad tool choice":     {Model: "m", MaxTokens: 8, ToolChoice: protocol.ToolChoice{Mode: "sometimes"}, Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "x"}}}}},
	}
	for _, id := range known {
		p, _ := protocol.Lookup(id)
		for name, req := range bad {
			if _, err := p.Encode(req); err == nil {
				t.Fatalf("%s accepted %s", id, name)
			}
		}
	}
}

func TestEveryProtocolFailsOnEmptyStream(t *testing.T) {
	for _, id := range known {
		p, _ := protocol.Lookup(id)
		if _, err := protocol.Collect(p.Stream(strings.NewReader(""))); err == nil {
			t.Fatalf("%s produced a response from nothing", id)
		}
		e := p.DecodeError(503, []byte("upstream unavailable"))
		if e == nil || e.Status != 503 || e.Message != "upstream unavailable" || string(e.Body) != "upstream unavailable" {
			t.Fatalf("%s error %+v", id, e)
		}
	}
}

func probe(parts ...protocol.Part) protocol.Request {
	return protocol.Request{Model: "model-x", MaxTokens: 64, Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hello"}}}, {Role: protocol.RoleAssistant, Content: parts}}}
}

func userProbe(parts ...protocol.Part) protocol.Request {
	return protocol.Request{Model: "model-x", MaxTokens: 64, Messages: []protocol.Message{{Role: protocol.RoleUser, Content: parts}}}
}

var lookupTool = protocol.Tool{Name: "lookup", Description: "Looks something up", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}

// Capabilities that only decoding or vendor features reveal, asserted by name
var fixedCapabilities = map[protocol.ID]struct{ serverTools, citations, caching bool }{
	protocol.AnthropicMessages:     {true, true, false},
	protocol.BedrockConverseStream: {false, true, true},
	protocol.GoogleGenerativeAI:    {true, true, false},
	protocol.MistralConversations:  {true, true, false},
	protocol.OllamaChat:            {false, false, false},
	protocol.OpenAICompletions:     {false, false, false},
	protocol.OpenAIResponses:       {true, true, false},
}

func TestEveryProtocolCapabilitiesMatchBehavior(t *testing.T) {
	for _, id := range known {
		p, _ := protocol.Lookup(id)
		caps := p.Capabilities()
		fixed := fixedCapabilities[id]
		if caps.ServerTools != fixed.serverTools || caps.Citations != fixed.citations || caps.Caching != fixed.caching {
			t.Fatalf("%s: server tools %v citations %v caching %v", id, caps.ServerTools, caps.Citations, caps.Caching)
		}
		expect := func(name string, want bool, req protocol.Request) {
			t.Helper()
			_, err := p.Encode(req)
			if want && err != nil {
				t.Fatalf("%s: %s declared but rejected: %v", id, name, err)
			}
			if !want && err == nil {
				t.Fatalf("%s: %s not declared but accepted", id, name)
			}
		}
		expect("thinking", caps.Thinking, probe(protocol.Thinking{Text: "hmm", Signature: "sig"}, protocol.Text{Text: "ok"}))
		expect("images", caps.Images, userProbe(protocol.Text{Text: "look"}, protocol.Image{MediaType: "image/png", Data: []byte{1, 2}}))
		expect("documents", caps.Documents, userProbe(protocol.Text{Text: "read"}, protocol.Document{MediaType: "application/pdf", Data: []byte{'%', 'P', 'D', 'F'}, Name: "spec.pdf"}))
		expect("stop sequences", caps.StopSequences, protocol.Request{Model: "model-x", MaxTokens: 64, StopSequences: []string{"END"}, Messages: userProbe(protocol.Text{Text: "hi"}).Messages})
		expect("tool choice none", caps.ToolChoiceNone, protocol.Request{Model: "model-x", MaxTokens: 64, Tools: []protocol.Tool{lookupTool}, ToolChoice: protocol.ToolChoice{Mode: protocol.ToolChoiceNone}, Messages: userProbe(protocol.Text{Text: "hi"}).Messages})
		expect("tool choice named", caps.ToolChoiceNamed, protocol.Request{Model: "model-x", MaxTokens: 64, Tools: []protocol.Tool{lookupTool}, ToolChoice: protocol.ToolChoice{Mode: protocol.ToolChoiceTool, Name: "lookup"}, Messages: userProbe(protocol.Text{Text: "hi"}).Messages})
		redacted := probe(protocol.Thinking{Redacted: true, Signature: "redacted-blob"}, protocol.Text{Text: "ok"})
		call, err := p.Encode(redacted)
		if err != nil {
			t.Fatalf("%s: redacted thinking must be omitted rather than rejected: %v", id, err)
		}
		if has := bytes.Contains(call.Body, []byte("redacted-blob")); has != caps.RedactedThinking {
			t.Fatalf("%s: redacted thinking declared %v but present %v", id, caps.RedactedThinking, has)
		}
		result := protocol.Request{Model: "model-x", MaxTokens: 64, Messages: []protocol.Message{
			{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "go"}}},
			{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{ID: "call_1", Name: "lookup", Args: json.RawMessage(`{"q":"x"}`)}}},
			{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "call_1", Name: "lookup", Content: []protocol.Part{protocol.Text{Text: "found"}, protocol.Image{MediaType: "image/png", Data: []byte{1, 2, 3}}}}}},
		}, Tools: []protocol.Tool{lookupTool}}
		call, err = p.Encode(result)
		if err != nil {
			t.Fatalf("%s: image in result must never fail the request: %v", id, err)
		}
		if omitted := bytes.Contains(call.Body, []byte("omitted")); omitted == caps.ImagesInResults {
			t.Fatalf("%s: images in results declared %v but omitted marker present %v", id, caps.ImagesInResults, omitted)
		}
	}
}

func TestEveryProtocolOmitsForeignSignedParts(t *testing.T) {
	for _, id := range known {
		p, _ := protocol.Lookup(id)
		other := protocol.AnthropicMessages
		if id == other {
			other = protocol.OpenAIResponses
		}
		req := probe(protocol.Thinking{Text: "secret-thought", Signature: "secret-sig", Origin: other}, protocol.Text{Text: "visible"})
		call, err := p.Encode(req)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if bytes.Contains(call.Body, []byte("secret-thought")) || bytes.Contains(call.Body, []byte("secret-sig")) {
			t.Fatalf("%s leaked a foreign thinking part: %s", id, call.Body)
		}
		if !bytes.Contains(call.Body, []byte("visible")) {
			t.Fatalf("%s dropped the visible text: %s", id, call.Body)
		}
		sig := "own-sig"
		if id == protocol.OpenAIResponses {
			sig = `{"id":"rs_1","encrypted_content":"own-sig"}`
		}
		own := probe(protocol.Thinking{Text: "own-thought", Signature: sig, Origin: id}, protocol.Text{Text: "visible"})
		call, err = p.Encode(own)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if p.Capabilities().Thinking && !bytes.Contains(call.Body, []byte("own-thought")) {
			t.Fatalf("%s dropped its own thinking: %s", id, call.Body)
		}
	}
}

func bedrockFrames(t *testing.T, frames ...bedrockconversestream.Frame) string {
	t.Helper()
	var buf bytes.Buffer
	for _, f := range frames {
		b, err := f.Encode()
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
	}
	return buf.String()
}

func bedrockEvent(kind, payload string) bedrockconversestream.Frame {
	return bedrockconversestream.Frame{Headers: map[string]any{":message-type": "event", ":event-type": kind, ":content-type": "application/json"}, Payload: []byte(payload)}
}

// Smallest valid stream for every protocol, each carrying one text block
func minimalStreams(t *testing.T) map[protocol.ID]string {
	t.Helper()
	return map[protocol.ID]string{
		protocol.AnthropicMessages: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		protocol.OpenAICompletions: "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hi\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n" +
			"data: [DONE]\n\n",
		protocol.OpenAIResponses: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"status\":\"in_progress\",\"model\":\"m\",\"output\":[]},\"sequence_number\":0}\n\n" +
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]},\"sequence_number\":1}\n\n" +
			"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[]},\"sequence_number\":2}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"Hi\",\"sequence_number\":3}\n\n" +
			"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"text\":\"Hi\",\"sequence_number\":4}\n\n" +
			"event: response.content_part.done\ndata: {\"type\":\"response.content_part.done\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"Hi\",\"annotations\":[]},\"sequence_number\":5}\n\n" +
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hi\",\"annotations\":[]}]},\"sequence_number\":6}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"model\":\"m\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hi\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}},\"sequence_number\":7}\n\n",
		protocol.GoogleGenerativeAI: "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hi\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1},\"responseId\":\"r\",\"modelVersion\":\"m\"}\n\n",
		protocol.BedrockConverseStream: bedrockFrames(t,
			bedrockEvent("messageStart", `{"role":"assistant"}`),
			bedrockEvent("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hi"}}`),
			bedrockEvent("contentBlockStop", `{"contentBlockIndex":0}`),
			bedrockEvent("messageStop", `{"stopReason":"end_turn"}`),
			bedrockEvent("metadata", `{"usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2},"metrics":{"latencyMs":5}}`),
		),
		protocol.MistralConversations: "event: conversation.response.started\ndata: {\"type\":\"conversation.response.started\",\"conversation_id\":\"c\"}\n\n" +
			"event: message.output.delta\ndata: {\"type\":\"message.output.delta\",\"id\":\"m\",\"content_index\":0,\"model\":\"m\",\"content\":\"Hi\"}\n\n" +
			"event: conversation.response.done\ndata: {\"type\":\"conversation.response.done\",\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n",
		protocol.OllamaChat: "{\"model\":\"m\",\"message\":{\"role\":\"assistant\",\"content\":\"Hi\"},\"done\":true,\"done_reason\":\"stop\",\"prompt_eval_count\":1,\"eval_count\":1}\n",
	}
}

// Stream for every protocol that carries a vendor error after the stream began
func errorStreams(t *testing.T) map[protocol.ID]string {
	t.Helper()
	return map[protocol.ID]string{
		protocol.AnthropicMessages:     "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n",
		protocol.OpenAICompletions:     "data: {\"error\":{\"message\":\"The server had an error\",\"type\":\"server_error\",\"code\":null}}\n\n",
		protocol.OpenAIResponses:       "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"Something went wrong\",\"sequence_number\":1}\n\n",
		protocol.GoogleGenerativeAI:    "data: {\"error\":{\"code\":503,\"message\":\"busy\",\"status\":\"UNAVAILABLE\"}}\n\n",
		protocol.BedrockConverseStream: bedrockFrames(t, bedrockEvent("messageStart", `{"role":"assistant"}`), bedrockconversestream.Frame{Headers: map[string]any{":message-type": "exception", ":exception-type": "throttlingException", ":content-type": "application/json"}, Payload: []byte(`{"message":"Too many requests"}`)}),
		protocol.MistralConversations:  "event: conversation.response.error\ndata: {\"type\":\"conversation.response.error\",\"message\":\"boom\",\"code\":500}\n\n",
		protocol.OllamaChat:            "{\"error\":\"server overloaded\"}\n",
	}
}

func TestEveryProtocolStartsWithStartAndEndsWithDone(t *testing.T) {
	streams := minimalStreams(t)
	for _, id := range known {
		p, _ := protocol.Lookup(id)
		var types []protocol.EventType
		var final protocol.Response
		for ev, err := range p.Stream(strings.NewReader(streams[id])) {
			if err != nil {
				t.Fatalf("%s: %v", id, err)
			}
			types = append(types, ev.Type)
			final = ev.Response
		}
		if len(types) < 2 || types[0] != protocol.EventStart || types[len(types)-1] != protocol.EventDone {
			t.Fatalf("%s: %v", id, types)
		}
		if final.Text() != "Hi" || final.StopReason != protocol.StopEnd || final.Usage.Input != 1 || final.Usage.Output != 1 {
			t.Fatalf("%s: %+v", id, final)
		}
	}
}

func TestEveryProtocolMarksInStreamErrors(t *testing.T) {
	streams := errorStreams(t)
	for _, id := range known {
		p, _ := protocol.Lookup(id)
		_, err := protocol.Collect(p.Stream(strings.NewReader(streams[id])))
		var pe *protocol.Error
		if !errors.As(err, &pe) {
			t.Fatalf("%s: %v", id, err)
		}
		if !pe.InStream || !pe.Transient || pe.Message == "" {
			t.Fatalf("%s: %+v", id, pe)
		}
	}
}
