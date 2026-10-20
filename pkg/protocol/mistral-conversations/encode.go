package mistralconversations

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
)

func (Protocol) Encode(req protocol.Request) (protocol.Call, error) {
	if req.Model == "" {
		return protocol.Call{}, fmt.Errorf("mistral-conversations: model is required")
	}
	inputs, err := encodeInputs(req.Messages)
	if err != nil {
		return protocol.Call{}, err
	}
	tools, err := encodeTools(req.Tools)
	if err != nil {
		return protocol.Call{}, err
	}
	args, err := encodeCompletionArgs(req)
	if err != nil {
		return protocol.Call{}, err
	}
	body, err := protocol.MarshalBody(request{
		Model:          req.Model,
		Instructions:   req.System,
		Inputs:         inputs,
		Tools:          tools,
		CompletionArgs: args,
		Stream:         req.Stream,
	}, req.Extra)
	if err != nil {
		return protocol.Call{}, err
	}
	header := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}
	if req.Stream {
		header.Set("Accept", "text/event-stream")
	}
	return protocol.Call{Method: http.MethodPost, Path: path, Header: header, Body: body}, nil
}

// Flattens every turn into the ordered entry list the conversation history is made of
func encodeInputs(messages []protocol.Message) ([]any, error) {
	inputs := []any{}
	for i, m := range messages {
		entries, err := encodeMessage(m)
		if err != nil {
			return nil, fmt.Errorf("mistral-conversations: message %d: %w", i, err)
		}
		inputs = append(inputs, entries...)
	}
	return inputs, nil
}

func encodeMessage(m protocol.Message) ([]any, error) {
	if len(m.Content) == 0 {
		return nil, fmt.Errorf("empty %s message", m.Role)
	}
	switch m.Role {
	case protocol.RoleUser:
		content, err := encodeUserContent(m.Content)
		if err != nil {
			return nil, err
		}
		return []any{messageEntry{Object: objectName, Type: "message.input", Role: "user", Content: content}}, nil
	case protocol.RoleAssistant:
		return encodeAssistant(m.Content)
	case protocol.RoleTool:
		return encodeToolResults(m.Content)
	}
	return nil, fmt.Errorf("unknown role %q", m.Role)
}

func encodeUserContent(parts []protocol.Part) ([]any, error) {
	content := []any{}
	for _, p := range parts {
		switch p := p.(type) {
		case protocol.Text:
			content = append(content, textChunk{Type: "text", Text: p.Text})
		case protocol.Image:
			c, err := encodeImage(p)
			if err != nil {
				return nil, err
			}
			content = append(content, c)
		default:
			return nil, fmt.Errorf("%T is not user content", p)
		}
	}
	return content, nil
}

// Emits a message.output entry per run of text and thinking parts and a function.call entry per tool call
func encodeAssistant(parts []protocol.Part) ([]any, error) {
	var entries []any
	var pending []any
	flush := func() {
		if len(pending) > 0 {
			entries = append(entries, messageEntry{Object: objectName, Type: "message.output", Role: "assistant", Content: pending})
			pending = nil
		}
	}
	for _, p := range parts {
		switch p := p.(type) {
		case protocol.Text:
			pending = append(pending, textChunk{Type: "text", Text: p.Text})
		case protocol.Image:
			c, err := encodeImage(p)
			if err != nil {
				return nil, err
			}
			pending = append(pending, c)
		case protocol.Thinking:
			pending = append(pending, thinkChunk{Type: "thinking", Thinking: []textChunk{{Type: "text", Text: p.Text}}, Signature: p.Signature})
		case protocol.ToolCall:
			flush()
			entries = append(entries, functionCallEntry{Object: objectName, Type: "function.call", ToolCallID: p.ID, Name: p.Name, Arguments: string(p.Args)})
		default:
			return nil, fmt.Errorf("%T is not assistant content", p)
		}
	}
	flush()
	return entries, nil
}

// Joins text parts with newlines because function.result carries a single string
func encodeToolResults(parts []protocol.Part) ([]any, error) {
	var entries []any
	for _, p := range parts {
		r, ok := p.(protocol.ToolResult)
		if !ok {
			return nil, fmt.Errorf("%T is not a tool result", p)
		}
		var texts []string
		for _, c := range r.Content {
			t, ok := c.(protocol.Text)
			if !ok {
				return nil, fmt.Errorf("tool result %s: function.result carries only text, not %T", r.CallID, c)
			}
			texts = append(texts, t.Text)
		}
		entries = append(entries, functionResultEntry{Object: objectName, Type: "function.result", ToolCallID: r.CallID, Result: strings.Join(texts, "\n")})
	}
	return entries, nil
}

func encodeImage(img protocol.Image) (imageChunk, error) {
	if img.MediaType == "" {
		return imageChunk{}, fmt.Errorf("image without media type")
	}
	url := "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
	return imageChunk{Type: "image_url", ImageURL: url}, nil
}

func encodeTools(tools []protocol.Tool) ([]functionTool, error) {
	var out []functionTool
	for _, t := range tools {
		if len(t.Parameters) == 0 {
			return nil, fmt.Errorf("mistral-conversations: tool %s has no parameters schema", t.Name)
		}
		out = append(out, functionTool{Type: "function", Function: function{Name: t.Name, Description: t.Description, Parameters: t.Parameters}})
	}
	return out, nil
}

func encodeCompletionArgs(req protocol.Request) (*completionArgs, error) {
	args := &completionArgs{MaxTokens: req.MaxTokens, Stop: req.StopSequences, Temperature: req.Temperature}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 1) {
		return nil, fmt.Errorf("mistral-conversations: temperature %v outside [0, 1]", *req.Temperature)
	}
	switch req.ToolChoice.Mode {
	case "":
	case protocol.ToolChoiceAuto:
		args.ToolChoice = "auto"
	case protocol.ToolChoiceNone:
		args.ToolChoice = "none"
	case protocol.ToolChoiceRequired:
		args.ToolChoice = "any"
	case protocol.ToolChoiceTool:
		return nil, fmt.Errorf("mistral-conversations: completion_args.tool_choice cannot name a function")
	default:
		return nil, fmt.Errorf("mistral-conversations: unknown tool choice %q", req.ToolChoice.Mode)
	}
	if args.Temperature == nil && args.MaxTokens == 0 && len(args.Stop) == 0 && args.ToolChoice == "" {
		return nil, nil
	}
	return args, nil
}
