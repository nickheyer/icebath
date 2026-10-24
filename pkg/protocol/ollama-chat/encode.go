package ollamachat

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
)

const errorPrefix = "[error] "

// Builds POST /api/chat with a leading system message and sampling options under options
func (Protocol) Encode(req protocol.Request) (protocol.Call, error) {
	if req.Model == "" {
		return protocol.Call{}, fmt.Errorf("ollama-chat: model is required")
	}
	wire := request{Model: req.Model, Messages: []message{}, Stream: req.Stream}
	if req.System != "" {
		wire.Messages = append(wire.Messages, message{Role: "system", Content: req.System})
	}
	for i, m := range req.Messages {
		msgs, err := encodeMessage(m)
		if err != nil {
			return protocol.Call{}, fmt.Errorf("ollama-chat: message %d: %w", i, err)
		}
		wire.Messages = append(wire.Messages, msgs...)
	}
	for _, t := range req.Tools {
		if len(t.Parameters) == 0 {
			return protocol.Call{}, fmt.Errorf("ollama-chat: tool %q has no parameters schema", t.Name)
		}
		wire.Tools = append(wire.Tools, tool{Type: "function", Function: toolFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters}})
	}
	switch req.ToolChoice.Mode {
	case "", protocol.ToolChoiceAuto:
	case protocol.ToolChoiceNone, protocol.ToolChoiceRequired, protocol.ToolChoiceTool:
		return protocol.Call{}, fmt.Errorf("ollama-chat: the wire has no tool choice, cannot force %q", req.ToolChoice.Mode)
	default:
		return protocol.Call{}, fmt.Errorf("ollama-chat: unknown tool choice mode %q", req.ToolChoice.Mode)
	}
	wire.Options = encodeOptions(req)
	body, err := protocol.MarshalBody(wire, req.Extra)
	if err != nil {
		return protocol.Call{}, err
	}
	header := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}
	if req.Stream {
		header.Set("Accept", "application/x-ndjson")
	}
	return protocol.Call{Method: http.MethodPost, Path: path, Header: header, Body: body}, nil
}

func encodeOptions(req protocol.Request) map[string]any {
	opts := map[string]any{}
	if req.MaxTokens > 0 {
		opts["num_predict"] = req.MaxTokens
	}
	if req.Temperature != nil {
		opts["temperature"] = *req.Temperature
	}
	if len(req.StopSequences) > 0 {
		opts["stop"] = req.StopSequences
	}
	return opts
}

func encodeMessage(m protocol.Message) ([]message, error) {
	if len(m.Content) == 0 {
		return nil, fmt.Errorf("empty %s message", m.Role)
	}
	switch m.Role {
	case protocol.RoleUser:
		msg, err := encodeUser(m.Content)
		return []message{msg}, err
	case protocol.RoleAssistant:
		msg, err := encodeAssistant(m.Content)
		return []message{msg}, err
	case protocol.RoleTool:
		return encodeToolResults(m.Content)
	}
	return nil, fmt.Errorf("unknown role %q", m.Role)
}

// Joins text parts with newlines because a message carries one content string
func encodeUser(parts []protocol.Part) (message, error) {
	msg := message{Role: "user"}
	var texts []string
	for _, p := range parts {
		switch v := p.(type) {
		case protocol.Text:
			texts = append(texts, v.Text)
		case protocol.Image:
			img, err := encodeImage(v)
			if err != nil {
				return message{}, err
			}
			msg.Images = append(msg.Images, img)
		case protocol.Document:
			return message{}, fmt.Errorf("the wire has no document part for %q", v.Name)
		default:
			return message{}, fmt.Errorf("user message cannot carry %T", p)
		}
	}
	msg.Content = strings.Join(texts, "\n")
	return msg, nil
}

// Replays text, replayable thinking and tool calls, sending an empty turn when policy drops everything
func encodeAssistant(parts []protocol.Part) (message, error) {
	msg := message{Role: "assistant"}
	var texts, thoughts []string
	for _, p := range parts {
		switch v := p.(type) {
		case protocol.Text:
			texts = append(texts, v.Text)
		case protocol.Thinking:
			if v.Redacted || !protocol.Replayable(v.Origin, protocol.OllamaChat) {
				continue
			}
			thoughts = append(thoughts, v.Text)
		case protocol.ToolCall:
			args, err := protocol.ParseArgs(string(v.Args))
			if err != nil {
				return message{}, fmt.Errorf("tool call %s: %w", v.Name, err)
			}
			msg.ToolCalls = append(msg.ToolCalls, toolCall{ID: v.ID, Function: toolCallFunction{Name: v.Name, Arguments: args}})
		case protocol.ServerToolCall, protocol.ServerToolResult:
		default:
			return message{}, fmt.Errorf("assistant message cannot carry %T", p)
		}
	}
	msg.Content = strings.Join(texts, "\n")
	msg.Thinking = strings.Join(thoughts, "\n")
	return msg, nil
}

// Emits one tool message per result, linked by tool_call_id and tool_name and flagged on failure
func encodeToolResults(parts []protocol.Part) ([]message, error) {
	var out []message
	for _, p := range parts {
		r, ok := p.(protocol.ToolResult)
		if !ok {
			return nil, fmt.Errorf("tool message cannot carry %T", p)
		}
		if r.CallID == "" && r.Name == "" {
			return nil, fmt.Errorf("tool result has neither a call id nor a tool name")
		}
		msg := message{Role: "tool", ToolName: r.Name, ToolCallID: r.CallID}
		var texts []string
		for _, c := range r.Content {
			switch v := c.(type) {
			case protocol.Text:
				texts = append(texts, v.Text)
			case protocol.Image:
				img, err := encodeImage(v)
				if err != nil {
					return nil, fmt.Errorf("tool result %s: %w", r.CallID, err)
				}
				msg.Images = append(msg.Images, img)
			default:
				return nil, fmt.Errorf("tool result %s cannot carry %T", r.CallID, c)
			}
		}
		msg.Content = strings.Join(texts, "\n")
		if r.IsError {
			msg.Content = errorPrefix + msg.Content
		}
		out = append(out, msg)
	}
	return out, nil
}

// Checks the media type names an image before sending the bare base64 the wire expects
func encodeImage(img protocol.Image) (string, error) {
	if !strings.HasPrefix(img.MediaType, "image/") {
		return "", fmt.Errorf("image media type %q is not an image type", img.MediaType)
	}
	if len(img.Data) == 0 {
		return "", fmt.Errorf("image has no data")
	}
	return base64.StdEncoding.EncodeToString(img.Data), nil
}
