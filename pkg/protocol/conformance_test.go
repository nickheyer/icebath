package protocol_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"

	_ "github.com/nickheyer/icebath/pkg/protocol/anthropic-messages"
	_ "github.com/nickheyer/icebath/pkg/protocol/bedrock-converse-stream"
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
