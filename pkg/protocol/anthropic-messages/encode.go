package anthropicmessages

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
)

var imageMediaTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

const (
	pdfMediaType  = "application/pdf"
	textMediaType = "text/plain"
)

func (Protocol) Encode(req protocol.Request) (protocol.Call, error) {
	if req.MaxTokens <= 0 {
		return protocol.Call{}, fmt.Errorf("anthropic-messages: max_tokens is required")
	}
	body := request{
		Model:         req.Model,
		MaxTokens:     req.MaxTokens,
		System:        req.System,
		Temperature:   req.Temperature,
		StopSequences: req.StopSequences,
		Stream:        req.Stream,
	}
	for _, t := range req.Tools {
		if len(t.Parameters) == 0 {
			return protocol.Call{}, fmt.Errorf("anthropic-messages: tool %q has no input schema", t.Name)
		}
		body.Tools = append(body.Tools, tool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters})
	}
	choice, err := encodeToolChoice(req.ToolChoice)
	if err != nil {
		return protocol.Call{}, err
	}
	body.ToolChoice = choice
	body.Messages = []message{}
	for i, m := range req.Messages {
		wire, keep, err := encodeMessage(m)
		if err != nil {
			return protocol.Call{}, fmt.Errorf("anthropic-messages: message %d: %w", i, err)
		}
		if keep {
			body.Messages = append(body.Messages, wire)
		}
	}
	raw, err := protocol.MarshalBody(body, req.Extra)
	if err != nil {
		return protocol.Call{}, err
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("anthropic-version", version)
	if req.Stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	return protocol.Call{Method: http.MethodPost, Path: path, Header: h, Body: raw}, nil
}

func encodeToolChoice(tc protocol.ToolChoice) (*toolChoice, error) {
	switch tc.Mode {
	case "":
		return nil, nil
	case protocol.ToolChoiceAuto:
		return &toolChoice{Type: "auto"}, nil
	case protocol.ToolChoiceNone:
		return &toolChoice{Type: "none"}, nil
	case protocol.ToolChoiceRequired:
		return &toolChoice{Type: "any"}, nil
	case protocol.ToolChoiceTool:
		if tc.Name == "" {
			return nil, fmt.Errorf("anthropic-messages: tool choice names no tool")
		}
		return &toolChoice{Type: "tool", Name: tc.Name}, nil
	}
	return nil, fmt.Errorf("anthropic-messages: unknown tool choice mode %q", tc.Mode)
}

// Maps one turn to a wire message, reporting false when the replay policy left nothing to send
func encodeMessage(m protocol.Message) (message, bool, error) {
	if len(m.Content) == 0 {
		return message{}, false, fmt.Errorf("empty %s message", m.Role)
	}
	switch m.Role {
	case protocol.RoleUser, protocol.RoleAssistant:
		blocks, err := encodeParts(m.Role, m.Content)
		if err != nil {
			return message{}, false, err
		}
		if len(blocks) == 0 {
			return message{}, false, nil
		}
		return message{Role: string(m.Role), Content: blocks}, true, nil
	case protocol.RoleTool:
		blocks := make([]any, 0, len(m.Content))
		for _, p := range m.Content {
			r, ok := p.(protocol.ToolResult)
			if !ok {
				return message{}, false, fmt.Errorf("%T in tool message", p)
			}
			content, err := encodeParts(protocol.RoleUser, r.Content)
			if err != nil {
				return message{}, false, fmt.Errorf("result for %s: %w", r.CallID, err)
			}
			blocks = append(blocks, toolResultBlock{Type: "tool_result", ToolUseID: r.CallID, Content: content, IsError: r.IsError})
		}
		return message{Role: "user", Content: blocks}, true, nil
	}
	return message{}, false, fmt.Errorf("unknown role %q", m.Role)
}

// Maps parts to content blocks the role may carry, sending a single space for whitespace only text
func encodeParts(role protocol.Role, parts []protocol.Part) ([]any, error) {
	blocks := make([]any, 0, len(parts))
	sawText := false
	for _, p := range parts {
		switch v := p.(type) {
		case protocol.Text:
			sawText = true
			if strings.TrimSpace(v.Text) == "" {
				continue
			}
			blocks = append(blocks, textBlock{Type: "text", Text: v.Text})
		case protocol.Image:
			if role != protocol.RoleUser {
				return nil, fmt.Errorf("image in %s message", role)
			}
			block, err := encodeImage(v)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		case protocol.Document:
			if role != protocol.RoleUser {
				return nil, fmt.Errorf("document in %s message", role)
			}
			block, err := encodeDocument(v)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		case protocol.Thinking:
			if role != protocol.RoleAssistant {
				return nil, fmt.Errorf("thinking in %s message", role)
			}
			if !protocol.Replayable(v.Origin, protocol.AnthropicMessages) {
				continue
			}
			if v.Redacted {
				blocks = append(blocks, redactedThinkingBlock{Type: "redacted_thinking", Data: v.Signature})
			} else {
				blocks = append(blocks, thinkingBlock{Type: "thinking", Thinking: v.Text, Signature: v.Signature})
			}
		case protocol.ToolCall:
			if role != protocol.RoleAssistant {
				return nil, fmt.Errorf("tool call in %s message", role)
			}
			args, err := protocol.ParseArgs(string(v.Args))
			if err != nil {
				return nil, fmt.Errorf("tool call %s: %w", v.ID, err)
			}
			blocks = append(blocks, toolUseBlock{Type: "tool_use", ID: v.ID, Name: v.Name, Input: args})
		case protocol.ServerToolCall:
			if role != protocol.RoleAssistant {
				return nil, fmt.Errorf("server tool call in %s message", role)
			}
			if !protocol.Replayable(v.Origin, protocol.AnthropicMessages) {
				continue
			}
			args, err := protocol.ParseArgs(string(v.Args))
			if err != nil {
				return nil, fmt.Errorf("server tool call %s: %w", v.ID, err)
			}
			blocks = append(blocks, toolUseBlock{Type: "server_tool_use", ID: v.ID, Name: v.Name, Input: args})
		case protocol.ServerToolResult:
			if role != protocol.RoleAssistant {
				return nil, fmt.Errorf("server tool result in %s message", role)
			}
			if !protocol.Replayable(v.Origin, protocol.AnthropicMessages) {
				continue
			}
			if v.Name == "" {
				return nil, fmt.Errorf("server tool result %s has no block type", v.CallID)
			}
			blocks = append(blocks, serverToolResultBlock{Type: v.Name, ToolUseID: v.CallID, Content: v.Content})
		case protocol.ToolResult:
			return nil, fmt.Errorf("tool result in %s message", role)
		default:
			return nil, fmt.Errorf("unsupported part %T", p)
		}
	}
	if len(blocks) == 0 && sawText {
		blocks = append(blocks, textBlock{Type: "text", Text: " "})
	}
	return blocks, nil
}

func encodeImage(img protocol.Image) (imageBlock, error) {
	if !imageMediaTypes[img.MediaType] {
		return imageBlock{}, fmt.Errorf("unsupported image media type %q", img.MediaType)
	}
	if len(img.Data) == 0 {
		return imageBlock{}, fmt.Errorf("image has no data")
	}
	src := imageSource{Type: "base64", MediaType: img.MediaType, Data: base64.StdEncoding.EncodeToString(img.Data)}
	return imageBlock{Type: "image", Source: src}, nil
}

// Sends PDFs as base64 and text media types as a text source titled by the document name
func encodeDocument(d protocol.Document) (documentBlock, error) {
	if len(d.Data) == 0 {
		return documentBlock{}, fmt.Errorf("document %q has no data", d.Name)
	}
	var src documentSource
	switch {
	case d.MediaType == pdfMediaType:
		src = documentSource{Type: "base64", MediaType: pdfMediaType, Data: base64.StdEncoding.EncodeToString(d.Data)}
	case strings.HasPrefix(d.MediaType, "text/"):
		src = documentSource{Type: "text", MediaType: textMediaType, Data: string(d.Data)}
	default:
		return documentBlock{}, fmt.Errorf("unsupported document media type %q", d.MediaType)
	}
	return documentBlock{Type: "document", Source: src, Title: d.Name}, nil
}
