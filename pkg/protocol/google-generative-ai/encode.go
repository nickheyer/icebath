package googlegenerativeai

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
)

const (
	roleUser  = "user"
	roleModel = "model"
)

func (Protocol) Encode(req protocol.Request) (protocol.Call, error) {
	if req.Model == "" {
		return protocol.Call{}, fmt.Errorf("google-generative-ai: model is required")
	}
	body := request{}
	if req.System != "" {
		body.SystemInstruction = &content{Parts: []part{{Text: ptr(req.System)}}}
	}
	for i, msg := range req.Messages {
		c, err := encodeMessage(msg)
		if err != nil {
			return protocol.Call{}, fmt.Errorf("google-generative-ai: message %d: %w", i, err)
		}
		body.Contents = append(body.Contents, c)
	}
	if len(req.Tools) > 0 {
		decls := make([]functionDeclaration, 0, len(req.Tools))
		for _, t := range req.Tools {
			if len(t.Parameters) == 0 {
				return protocol.Call{}, fmt.Errorf("google-generative-ai: tool %q has no parameters schema", t.Name)
			}
			decls = append(decls, functionDeclaration{Name: t.Name, Description: t.Description, ParametersJSONSchema: t.Parameters})
		}
		body.Tools = []tool{{FunctionDeclarations: decls}}
	}
	cfg, err := encodeToolChoice(req.ToolChoice)
	if err != nil {
		return protocol.Call{}, err
	}
	body.ToolConfig = cfg
	if req.MaxTokens > 0 || req.Temperature != nil || len(req.StopSequences) > 0 {
		body.GenerationConfig = &generationConfig{MaxOutputTokens: req.MaxTokens, Temperature: req.Temperature, StopSequences: req.StopSequences}
	}
	raw, err := protocol.MarshalBody(body, req.Extra)
	if err != nil {
		return protocol.Call{}, err
	}
	call := protocol.Call{Method: http.MethodPost, Path: path(req.Model, req.Stream), Header: http.Header{}, Body: raw}
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set("Accept", "application/json")
	if req.Stream {
		call.Query = url.Values{"alt": {"sse"}}
		call.Header.Set("Accept", "text/event-stream")
	}
	return call, nil
}

// Builds the method path for a bare model id or an already qualified resource name
func path(model string, stream bool) string {
	resource := model
	if !strings.Contains(model, "/") {
		resource = "models/" + url.PathEscape(model)
	}
	if stream {
		return "/v1beta/" + resource + ":streamGenerateContent"
	}
	return "/v1beta/" + resource + ":generateContent"
}

func encodeToolChoice(tc protocol.ToolChoice) (*toolConfig, error) {
	cfg := functionCallingConfig{}
	switch tc.Mode {
	case "":
		return nil, nil
	case protocol.ToolChoiceAuto:
		cfg.Mode = "AUTO"
	case protocol.ToolChoiceNone:
		cfg.Mode = "NONE"
	case protocol.ToolChoiceRequired:
		cfg.Mode = "ANY"
	case protocol.ToolChoiceTool:
		if tc.Name == "" {
			return nil, fmt.Errorf("google-generative-ai: tool choice names no tool")
		}
		cfg.Mode = "ANY"
		cfg.AllowedFunctionNames = []string{tc.Name}
	default:
		return nil, fmt.Errorf("google-generative-ai: unknown tool choice mode %q", tc.Mode)
	}
	return &toolConfig{FunctionCallingConfig: cfg}, nil
}

func encodeMessage(msg protocol.Message) (content, error) {
	if len(msg.Content) == 0 {
		return content{}, fmt.Errorf("empty message")
	}
	switch msg.Role {
	case protocol.RoleUser:
		parts, err := encodeUser(msg.Content)
		return content{Role: roleUser, Parts: parts}, err
	case protocol.RoleAssistant:
		parts, err := encodeAssistant(msg.Content)
		return content{Role: roleModel, Parts: parts}, err
	case protocol.RoleTool:
		parts, err := encodeToolResults(msg.Content)
		return content{Role: roleUser, Parts: parts}, err
	}
	return content{}, fmt.Errorf("unknown role %q", msg.Role)
}

func encodeUser(parts []protocol.Part) ([]part, error) {
	out := make([]part, 0, len(parts))
	for _, p := range parts {
		switch p := p.(type) {
		case protocol.Text:
			out = append(out, part{Text: ptr(p.Text)})
		case protocol.Image:
			ip, err := encodeImage(p)
			if err != nil {
				return nil, err
			}
			out = append(out, ip)
		default:
			return nil, fmt.Errorf("user message cannot carry %T", p)
		}
	}
	return out, nil
}

// Emits model parts, folding a signature only Thinking back onto the Text before it
func encodeAssistant(parts []protocol.Part) ([]part, error) {
	out := make([]part, 0, len(parts))
	for i := 0; i < len(parts); i++ {
		switch p := parts[i].(type) {
		case protocol.Text:
			wp := part{Text: ptr(p.Text)}
			if i+1 < len(parts) {
				if th, ok := parts[i+1].(protocol.Thinking); ok && signatureOnly(th) {
					wp.ThoughtSignature = th.Signature
					i++
				}
			}
			out = append(out, wp)
		case protocol.Thinking:
			if p.Redacted {
				return nil, fmt.Errorf("assistant part %d: redacted thinking has no wire form", i)
			}
			if signatureOnly(p) {
				out = append(out, part{Text: ptr(""), ThoughtSignature: p.Signature})
				continue
			}
			out = append(out, part{Thought: true, Text: ptr(p.Text), ThoughtSignature: p.Signature})
		case protocol.ToolCall:
			fc := &functionCall{ID: p.ID, Name: p.Name}
			if len(p.Args) > 0 {
				args, err := argsJSON(p.Args)
				if err != nil {
					return nil, fmt.Errorf("assistant part %d: %w", i, err)
				}
				fc.Args = args
			}
			out = append(out, part{FunctionCall: fc, ThoughtSignature: p.Signature})
		case protocol.Image:
			ip, err := encodeImage(p)
			if err != nil {
				return nil, err
			}
			out = append(out, ip)
		default:
			return nil, fmt.Errorf("assistant message cannot carry %T", p)
		}
	}
	return out, nil
}

func signatureOnly(th protocol.Thinking) bool {
	return th.Text == "" && th.Signature != "" && !th.Redacted
}

func encodeToolResults(parts []protocol.Part) ([]part, error) {
	out := make([]part, 0, len(parts))
	for i, p := range parts {
		tr, ok := p.(protocol.ToolResult)
		if !ok {
			return nil, fmt.Errorf("tool message cannot carry %T", p)
		}
		if tr.Name == "" {
			return nil, fmt.Errorf("tool result %d: name is required", i)
		}
		fr := &functionResponse{ID: tr.CallID, Name: tr.Name}
		var texts []string
		for _, c := range tr.Content {
			switch c := c.(type) {
			case protocol.Text:
				texts = append(texts, c.Text)
			case protocol.Image:
				ip, err := encodeImage(c)
				if err != nil {
					return nil, err
				}
				fr.Parts = append(fr.Parts, functionResponsePart{InlineData: *ip.InlineData})
			default:
				return nil, fmt.Errorf("tool result %d cannot carry %T", i, c)
			}
		}
		key := "result"
		if tr.IsError {
			key = "error"
		}
		fr.Response = map[string]string{key: strings.Join(texts, "\n")}
		out = append(out, part{FunctionResponse: fr})
	}
	return out, nil
}

func encodeImage(img protocol.Image) (part, error) {
	if img.MediaType == "" {
		return part{}, fmt.Errorf("image has no media type")
	}
	return part{InlineData: &blob{MimeType: img.MediaType, Data: base64.StdEncoding.EncodeToString(img.Data)}}, nil
}

func ptr(s string) *string { return &s }

// Validates function call arguments as an object and compacts them for byte stable replay
func argsJSON(raw json.RawMessage) (json.RawMessage, error) {
	args, err := protocol.ParseArgs(string(raw))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, args); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
