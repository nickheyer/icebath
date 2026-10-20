package bedrockconversestream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// Extra key that turns on prompt caching, consumed rather than merged into the body
const cacheKey = "bedrock_cache"

var imageFormats = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpeg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

var documentFormats = map[string]string{
	"application/pdf":  "pdf",
	"text/plain":       "txt",
	"text/markdown":    "md",
	"text/csv":         "csv",
	"text/html":        "html",
	"application/json": "json",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "docx",
	"application/msword":       "doc",
	"application/vnd.ms-excel": "xls",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": "xlsx",
}

// Builds the Converse or ConverseStream call with the model id escaped into the path
func (Protocol) Encode(req protocol.Request) (protocol.Call, error) {
	if req.Model == "" {
		return protocol.Call{}, fmt.Errorf("bedrock-converse-stream: model is required")
	}
	cache, extra, err := consumeCache(req.Extra)
	if err != nil {
		return protocol.Call{}, err
	}
	wire := request{}
	if req.System != "" {
		wire.System = []systemBlock{{Text: &req.System}}
		if cache {
			wire.System = append(wire.System, systemBlock{CachePoint: &cachePoint{Type: "default"}})
		}
	}
	enc := &encoder{}
	for i, m := range req.Messages {
		msg, keep, err := enc.message(m)
		if err != nil {
			return protocol.Call{}, fmt.Errorf("bedrock-converse-stream: message %d: %w", i, err)
		}
		if keep {
			wire.Messages = append(wire.Messages, msg)
		}
	}
	if req.MaxTokens > 0 || req.Temperature != nil || len(req.StopSequences) > 0 {
		wire.InferenceConfig = &inferenceConfig{MaxTokens: req.MaxTokens, Temperature: req.Temperature, StopSequences: req.StopSequences}
	}
	cfg, err := encodeToolConfig(req.Tools, req.ToolChoice, cache)
	if err != nil {
		return protocol.Call{}, err
	}
	wire.ToolConfig = cfg
	body, err := protocol.MarshalBody(wire, extra)
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

// Reads and removes the caching switch so the remaining extra fields merge into the body
func consumeCache(extra map[string]json.RawMessage) (bool, map[string]json.RawMessage, error) {
	raw, ok := extra[cacheKey]
	if !ok {
		return false, extra, nil
	}
	var on bool
	if err := json.Unmarshal(raw, &on); err != nil {
		return false, nil, fmt.Errorf("bedrock-converse-stream: %s must be a boolean: %w", cacheKey, err)
	}
	rest := make(map[string]json.RawMessage, len(extra)-1)
	for k, v := range extra {
		if k != cacheKey {
			rest[k] = v
		}
	}
	return on, rest, nil
}

// Numbers generated document names across one request
type encoder struct {
	docs int
}

// Maps one turn to a wire message, reporting false when the replay policy left no content
func (e *encoder) message(m protocol.Message) (message, bool, error) {
	if len(m.Content) == 0 {
		return message{}, false, fmt.Errorf("empty content")
	}
	switch m.Role {
	case protocol.RoleUser:
		msg, err := e.parts("user", m.Content, e.userPart)
		return msg, true, err
	case protocol.RoleAssistant:
		msg, err := e.parts("assistant", m.Content, e.assistantPart)
		return msg, len(msg.Content) > 0, err
	case protocol.RoleTool:
		msg, err := e.parts("user", m.Content, e.toolResultPart)
		return msg, true, err
	}
	return message{}, false, fmt.Errorf("unknown role %q", m.Role)
}

// Collects the blocks each part maps to, skipping parts the encoder reports as omitted
func (e *encoder) parts(role string, parts []protocol.Part, encode func(protocol.Part) (*contentBlock, error)) (message, error) {
	msg := message{Role: role}
	for _, p := range parts {
		block, err := encode(p)
		if err != nil {
			return message{}, err
		}
		if block != nil {
			msg.Content = append(msg.Content, *block)
		}
	}
	return msg, nil
}

func (e *encoder) userPart(p protocol.Part) (*contentBlock, error) {
	switch v := p.(type) {
	case protocol.Text:
		return &contentBlock{Text: &v.Text}, nil
	case protocol.Image:
		img, err := encodeImage(v)
		if err != nil {
			return nil, err
		}
		return &contentBlock{Image: img}, nil
	case protocol.Document:
		doc, err := e.document(v)
		if err != nil {
			return nil, err
		}
		return &contentBlock{Document: doc}, nil
	}
	return nil, fmt.Errorf("%T is not allowed in a user message", p)
}

func (e *encoder) assistantPart(p protocol.Part) (*contentBlock, error) {
	switch v := p.(type) {
	case protocol.Text:
		return &contentBlock{Text: &v.Text}, nil
	case protocol.Thinking:
		if !protocol.Replayable(v.Origin, protocol.BedrockConverseStream) {
			return nil, nil
		}
		if v.Redacted {
			if v.Signature == "" {
				return nil, fmt.Errorf("redacted thinking has no redactedContent payload")
			}
			return &contentBlock{ReasoningContent: &reasoningContent{RedactedContent: v.Signature}}, nil
		}
		return &contentBlock{ReasoningContent: &reasoningContent{ReasoningText: &reasoningText{Text: v.Text, Signature: v.Signature}}}, nil
	case protocol.ToolCall:
		args, err := protocol.ParseArgs(string(v.Args))
		if err != nil {
			return nil, fmt.Errorf("tool call %s: %w", v.Name, err)
		}
		return &contentBlock{ToolUse: &toolUseBlock{ToolUseID: v.ID, Name: v.Name, Input: args}}, nil
	case protocol.ServerToolCall, protocol.ServerToolResult:
		return nil, nil
	}
	return nil, fmt.Errorf("%T is not allowed in an assistant message", p)
}

func (e *encoder) toolResultPart(p protocol.Part) (*contentBlock, error) {
	r, ok := p.(protocol.ToolResult)
	if !ok {
		return nil, fmt.Errorf("%T is not allowed in a tool message", p)
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
				return nil, err
			}
			block.Content = append(block.Content, toolResultContent{Image: img})
		case protocol.Document:
			doc, err := e.document(v)
			if err != nil {
				return nil, err
			}
			block.Content = append(block.Content, toolResultContent{Document: doc})
		default:
			return nil, fmt.Errorf("%T is not allowed in a tool result", c)
		}
	}
	return &contentBlock{ToolResult: block}, nil
}

func encodeImage(img protocol.Image) (*imageBlock, error) {
	format, ok := imageFormats[img.MediaType]
	if !ok {
		return nil, fmt.Errorf("image media type %q has no ImageFormat value", img.MediaType)
	}
	if len(img.Data) == 0 {
		return nil, fmt.Errorf("image has no bytes")
	}
	return &imageBlock{Format: format, Source: byteSource{Bytes: img.Data}}, nil
}

// Maps the media type onto a DocumentFormat value and names unnamed documents by number
func (e *encoder) document(d protocol.Document) (*documentBlock, error) {
	format, ok := documentFormats[d.MediaType]
	if !ok {
		return nil, fmt.Errorf("document media type %q has no DocumentFormat value", d.MediaType)
	}
	if len(d.Data) == 0 {
		return nil, fmt.Errorf("document %q has no bytes", d.Name)
	}
	name := d.Name
	if name == "" {
		e.docs++
		name = fmt.Sprintf("document-%d", e.docs)
	}
	return &documentBlock{Format: format, Name: name, Source: byteSource{Bytes: d.Data}}, nil
}

func encodeToolConfig(tools []protocol.Tool, choice protocol.ToolChoice, cache bool) (*toolConfig, error) {
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
		cfg.Tools = append(cfg.Tools, tool{ToolSpec: &toolSpec{Name: t.Name, Description: t.Description, InputSchema: toolInputSchema{JSON: t.Parameters}}})
	}
	if cache {
		cfg.Tools = append(cfg.Tools, tool{CachePoint: &cachePoint{Type: "default"}})
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
