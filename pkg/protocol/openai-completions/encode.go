package openaicompletions

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
)

const errorPrefix = "[error] "

// Builds POST /chat/completions with a leading system message and one tool message per result
func (Protocol) Encode(req protocol.Request) (protocol.Call, error) {
	wire := request{
		Model:               req.Model,
		MaxCompletionTokens: req.MaxTokens,
		Temperature:         req.Temperature,
		Stop:                req.StopSequences,
		Stream:              req.Stream,
	}
	if req.System != "" {
		wire.Messages = append(wire.Messages, message{Role: "system", Content: req.System})
	}
	for i, m := range req.Messages {
		msgs, err := encodeMessage(m)
		if err != nil {
			return protocol.Call{}, fmt.Errorf("openai-completions: message %d: %w", i, err)
		}
		wire.Messages = append(wire.Messages, msgs...)
	}
	for _, t := range req.Tools {
		if len(t.Parameters) == 0 {
			return protocol.Call{}, fmt.Errorf("openai-completions: tool %q has no parameters schema", t.Name)
		}
		wire.Tools = append(wire.Tools, tool{Type: "function", Function: function{Name: t.Name, Description: t.Description, Parameters: t.Parameters}})
	}
	choice, err := encodeToolChoice(req.ToolChoice)
	if err != nil {
		return protocol.Call{}, err
	}
	wire.ToolChoice = choice
	if req.Stream {
		wire.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	body, err := protocol.MarshalBody(wire, req.Extra)
	if err != nil {
		return protocol.Call{}, err
	}
	header := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}
	if req.Stream {
		header.Set("Accept", "text/event-stream")
	}
	return protocol.Call{Method: http.MethodPost, Path: "/chat/completions", Header: header, Body: body}, nil
}

func encodeToolChoice(tc protocol.ToolChoice) (any, error) {
	switch tc.Mode {
	case "":
		return nil, nil
	case protocol.ToolChoiceAuto, protocol.ToolChoiceNone, protocol.ToolChoiceRequired:
		return string(tc.Mode), nil
	case protocol.ToolChoiceTool:
		if tc.Name == "" {
			return nil, fmt.Errorf("openai-completions: tool choice names no tool")
		}
		return namedToolChoice{Type: "function", Function: namedFunction{Name: tc.Name}}, nil
	}
	return nil, fmt.Errorf("openai-completions: unknown tool choice mode %q", tc.Mode)
}

func encodeMessage(m protocol.Message) ([]message, error) {
	if len(m.Content) == 0 {
		return nil, fmt.Errorf("empty %s message", m.Role)
	}
	switch m.Role {
	case protocol.RoleUser:
		return encodeUser(m.Content)
	case protocol.RoleAssistant:
		return encodeAssistant(m.Content)
	case protocol.RoleTool:
		return encodeToolResults(m.Content)
	}
	return nil, fmt.Errorf("unknown role %q", m.Role)
}

func encodeUser(parts []protocol.Part) ([]message, error) {
	if len(parts) == 1 {
		if t, ok := parts[0].(protocol.Text); ok {
			return []message{{Role: "user", Content: t.Text}}, nil
		}
	}
	var content []contentPart
	for _, p := range parts {
		switch v := p.(type) {
		case protocol.Text:
			content = append(content, contentPart{Type: "text", Text: v.Text})
		case protocol.Image:
			url, err := dataURL(v)
			if err != nil {
				return nil, err
			}
			content = append(content, contentPart{Type: "image_url", ImageURL: &imageURL{URL: url}})
		case protocol.Document:
			return nil, fmt.Errorf("the wire has no document part for %q", v.Name)
		default:
			return nil, fmt.Errorf("user message cannot carry %T", p)
		}
	}
	return []message{{Role: "user", Content: content}}, nil
}

// Encodes text, replayable reasoning and tool calls, sending an empty turn when policy drops everything
func encodeAssistant(parts []protocol.Part) ([]message, error) {
	var texts, thoughts []string
	var calls []toolCall
	for _, p := range parts {
		switch v := p.(type) {
		case protocol.Text:
			texts = append(texts, v.Text)
		case protocol.Thinking:
			if v.Redacted || !protocol.Replayable(v.Origin, protocol.OpenAICompletions) {
				continue
			}
			thoughts = append(thoughts, v.Text)
		case protocol.ToolCall:
			args, err := protocol.ParseArgs(string(v.Args))
			if err != nil {
				return nil, fmt.Errorf("tool call %s: %w", v.ID, err)
			}
			calls = append(calls, toolCall{ID: v.ID, Type: "function", Function: toolCallFunction{Name: v.Name, Arguments: string(args)}})
		case protocol.ServerToolCall, protocol.ServerToolResult:
		default:
			return nil, fmt.Errorf("assistant message cannot carry %T", p)
		}
	}
	msg := message{Role: "assistant", ToolCalls: calls, ReasoningContent: strings.Join(thoughts, "\n")}
	switch len(texts) {
	case 0:
		if len(calls) == 0 {
			msg.Content = ""
		}
	case 1:
		msg.Content = texts[0]
	default:
		content := make([]contentPart, len(texts))
		for i, t := range texts {
			content[i] = contentPart{Type: "text", Text: t}
		}
		msg.Content = content
	}
	return []message{msg}, nil
}

// Emits one tool message per result, flagging failures and standing in for images with text
func encodeToolResults(parts []protocol.Part) ([]message, error) {
	var out []message
	for _, p := range parts {
		r, ok := p.(protocol.ToolResult)
		if !ok {
			return nil, fmt.Errorf("tool message cannot carry %T", p)
		}
		var texts []string
		for _, c := range r.Content {
			switch v := c.(type) {
			case protocol.Text:
				texts = append(texts, v.Text)
			case protocol.Image:
				texts = append(texts, omittedImage(v))
			default:
				return nil, fmt.Errorf("tool result %s cannot carry %T", r.CallID, c)
			}
		}
		if r.IsError {
			if len(texts) == 0 {
				texts = []string{""}
			}
			texts[0] = errorPrefix + texts[0]
		}
		msg := message{Role: "tool", ToolCallID: r.CallID, Content: ""}
		switch len(texts) {
		case 0:
		case 1:
			msg.Content = texts[0]
		default:
			content := make([]contentPart, len(texts))
			for i, t := range texts {
				content[i] = contentPart{Type: "text", Text: t}
			}
			msg.Content = content
		}
		out = append(out, msg)
	}
	return out, nil
}

func omittedImage(img protocol.Image) string {
	return fmt.Sprintf("[image %s, %d bytes omitted]", img.MediaType, len(img.Data))
}

func dataURL(img protocol.Image) (string, error) {
	if img.MediaType == "" || len(img.Data) == 0 {
		return "", fmt.Errorf("image needs a media type and data")
	}
	return "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data), nil
}
