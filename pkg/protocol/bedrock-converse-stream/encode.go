package bedrockconversestream

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
)

var imageFormats = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpeg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

// Builds the Converse or ConverseStream call with the model id escaped into the path
func (Protocol) Encode(req protocol.Request) (protocol.Call, error) {
	if req.Model == "" {
		return protocol.Call{}, fmt.Errorf("bedrock-converse-stream: model is required")
	}
	wire := request{}
	if req.System != "" {
		wire.System = []systemBlock{{Text: req.System}}
	}
	for i, m := range req.Messages {
		msg, err := encodeMessage(m)
		if err != nil {
			return protocol.Call{}, fmt.Errorf("bedrock-converse-stream: message %d: %w", i, err)
		}
		wire.Messages = append(wire.Messages, msg)
	}
	if req.MaxTokens > 0 || req.Temperature != nil || len(req.StopSequences) > 0 {
		wire.InferenceConfig = &inferenceConfig{MaxTokens: req.MaxTokens, Temperature: req.Temperature, StopSequences: req.StopSequences}
	}
	cfg, err := encodeToolConfig(req.Tools, req.ToolChoice)
	if err != nil {
		return protocol.Call{}, err
	}
	wire.ToolConfig = cfg
	body, err := protocol.MarshalBody(wire, req.Extra)
	if err != nil {
		return protocol.Call{}, err
	}
	op := "converse"
	if req.Stream {
		op = "converse-stream"
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return protocol.Call{
		Method: http.MethodPost,
		Path:   "/model/" + escapePath(req.Model) + "/" + op,
		Header: header,
		Body:   body,
	}, nil
}

func encodeMessage(m protocol.Message) (message, error) {
	if len(m.Content) == 0 {
		return message{}, fmt.Errorf("empty content")
	}
	switch m.Role {
	case protocol.RoleUser:
		return encodeParts("user", m.Content, encodeUserPart)
	case protocol.RoleAssistant:
		return encodeParts("assistant", m.Content, encodeAssistantPart)
	case protocol.RoleTool:
		return encodeParts("user", m.Content, encodeToolResultPart)
	}
	return message{}, fmt.Errorf("unknown role %q", m.Role)
}

func encodeParts(role string, parts []protocol.Part, encode func(protocol.Part) (contentBlock, error)) (message, error) {
	msg := message{Role: role}
	for _, p := range parts {
		block, err := encode(p)
		if err != nil {
			return message{}, err
		}
		msg.Content = append(msg.Content, block)
	}
	return msg, nil
}

func encodeUserPart(p protocol.Part) (contentBlock, error) {
	switch v := p.(type) {
	case protocol.Text:
		return contentBlock{Text: &v.Text}, nil
	case protocol.Image:
		img, err := encodeImage(v)
		if err != nil {
			return contentBlock{}, err
		}
		return contentBlock{Image: img}, nil
	}
	return contentBlock{}, fmt.Errorf("%T is not allowed in a user message", p)
}

func encodeAssistantPart(p protocol.Part) (contentBlock, error) {
	switch v := p.(type) {
	case protocol.Text:
		return contentBlock{Text: &v.Text}, nil
	case protocol.Thinking:
		if v.Redacted {
			if v.Signature == "" {
				return contentBlock{}, fmt.Errorf("redacted thinking has no redactedContent payload")
			}
			return contentBlock{ReasoningContent: &reasoningContent{RedactedContent: v.Signature}}, nil
		}
		return contentBlock{ReasoningContent: &reasoningContent{ReasoningText: &reasoningText{Text: v.Text, Signature: v.Signature}}}, nil
	case protocol.ToolCall:
		args, err := protocol.ParseArgs(string(v.Args))
		if err != nil {
			return contentBlock{}, fmt.Errorf("tool call %s: %w", v.Name, err)
		}
		return contentBlock{ToolUse: &toolUseBlock{ToolUseID: v.ID, Name: v.Name, Input: args}}, nil
	}
	return contentBlock{}, fmt.Errorf("%T is not allowed in an assistant message", p)
}

func encodeToolResultPart(p protocol.Part) (contentBlock, error) {
	r, ok := p.(protocol.ToolResult)
	if !ok {
		return contentBlock{}, fmt.Errorf("%T is not allowed in a tool message", p)
	}
	block := &toolResultBlock{ToolUseID: r.CallID, Content: []toolResultContent{}, Status: "success"}
	if r.IsError {
		block.Status = "error"
	}
	for _, c := range r.Content {
		switch v := c.(type) {
		case protocol.Text:
			block.Content = append(block.Content, toolResultContent{Text: &v.Text})
		case protocol.Image:
			img, err := encodeImage(v)
			if err != nil {
				return contentBlock{}, err
			}
			block.Content = append(block.Content, toolResultContent{Image: img})
		default:
			return contentBlock{}, fmt.Errorf("%T is not allowed in a tool result", c)
		}
	}
	return contentBlock{ToolResult: block}, nil
}

func encodeImage(img protocol.Image) (*imageBlock, error) {
	format, ok := imageFormats[img.MediaType]
	if !ok {
		return nil, fmt.Errorf("image media type %q has no ImageFormat value", img.MediaType)
	}
	if len(img.Data) == 0 {
		return nil, fmt.Errorf("image has no bytes")
	}
	return &imageBlock{Format: format, Source: imageSource{Bytes: img.Data}}, nil
}

func encodeToolConfig(tools []protocol.Tool, choice protocol.ToolChoice) (*toolConfig, error) {
	if len(tools) == 0 {
		if choice.Mode != "" {
			return nil, fmt.Errorf("bedrock-converse-stream: tool choice %q without tools", choice.Mode)
		}
		return nil, nil
	}
	cfg := &toolConfig{}
	for _, t := range tools {
		if len(t.Parameters) == 0 {
			return nil, fmt.Errorf("bedrock-converse-stream: tool %q has no parameters schema", t.Name)
		}
		cfg.Tools = append(cfg.Tools, tool{ToolSpec: toolSpec{Name: t.Name, Description: t.Description, InputSchema: toolInputSchema{JSON: t.Parameters}}})
	}
	switch choice.Mode {
	case "":
	case protocol.ToolChoiceAuto:
		cfg.ToolChoice = &toolChoice{Auto: &struct{}{}}
	case protocol.ToolChoiceRequired:
		cfg.ToolChoice = &toolChoice{Any: &struct{}{}}
	case protocol.ToolChoiceTool:
		if choice.Name == "" {
			return nil, fmt.Errorf("bedrock-converse-stream: tool choice names no tool")
		}
		cfg.ToolChoice = &toolChoice{Tool: &specificTool{Name: choice.Name}}
	case protocol.ToolChoiceNone:
		return nil, fmt.Errorf("bedrock-converse-stream: ToolChoice has no none member")
	default:
		return nil, fmt.Errorf("bedrock-converse-stream: unknown tool choice mode %q", choice.Mode)
	}
	return cfg, nil
}

// Percent encodes every byte outside the unreserved set, matching smithy-go EscapePath for a non greedy label
func escapePath(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
