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
	// Neutral names for the executableCode and toolCall part kinds the model runs itself
	nameCodeExecution = "code_execution"
	nameToolCall      = "tool_call"
)

func (Protocol) Encode(req protocol.Request) (protocol.Call, error) {
	if req.Model == "" {
		return protocol.Call{}, fmt.Errorf("google-generative-ai: model is required")
	}
	body := request{}
	if req.System != "" {
		body.SystemInstruction = &content{Parts: []part{{Text: ptr(req.System)}}}
	}
	var calls []protocol.ToolCall
	for i, msg := range req.Messages {
		c, keep, err := encodeMessage(msg, calls)
		if err != nil {
			return protocol.Call{}, fmt.Errorf("google-generative-ai: message %d: %w", i, err)
		}
		if msg.Role == protocol.RoleAssistant {
			calls = toolCalls(msg)
		}
		if keep {
			body.Contents = append(body.Contents, c)
		}
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

func toolCalls(msg protocol.Message) []protocol.ToolCall {
	var out []protocol.ToolCall
	for _, p := range msg.Content {
		if c, ok := p.(protocol.ToolCall); ok {
			out = append(out, c)
		}
	}
	return out
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

// Maps one turn to a content, reporting false when the replay policy left no parts to send
func encodeMessage(msg protocol.Message, calls []protocol.ToolCall) (content, bool, error) {
	if len(msg.Content) == 0 {
		return content{}, false, fmt.Errorf("empty message")
	}
	switch msg.Role {
	case protocol.RoleUser:
		parts, err := encodeUser(msg.Content)
		return content{Role: roleUser, Parts: parts}, true, err
	case protocol.RoleAssistant:
		parts, err := encodeAssistant(msg.Content)
		return content{Role: roleModel, Parts: parts}, len(parts) > 0, err
	case protocol.RoleTool:
		parts, err := encodeToolResults(msg.Content, calls)
		return content{Role: roleUser, Parts: parts}, true, err
	}
	return content{}, false, fmt.Errorf("unknown role %q", msg.Role)
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
		case protocol.Document:
			dp, err := encodeDocument(p)
			if err != nil {
				return nil, err
			}
			out = append(out, dp)
		default:
			return nil, fmt.Errorf("user message cannot carry %T", p)
		}
	}
	return out, nil
}

// Emits model parts, folding a replayable signature only Thinking back onto the Text before it
func encodeAssistant(parts []protocol.Part) ([]part, error) {
	out := make([]part, 0, len(parts))
	for i := 0; i < len(parts); i++ {
		switch p := parts[i].(type) {
		case protocol.Text:
			wp := part{Text: ptr(p.Text)}
			if i+1 < len(parts) {
				if th, ok := parts[i+1].(protocol.Thinking); ok && signatureOnly(th) && replayable(th.Origin) {
					wp.ThoughtSignature = th.Signature
					i++
				}
			}
			out = append(out, wp)
		case protocol.Thinking:
			if p.Redacted || !replayable(p.Origin) {
				continue
			}
			if signatureOnly(p) {
				out = append(out, part{Text: ptr(""), ThoughtSignature: p.Signature})
				continue
			}
			out = append(out, part{Thought: true, Text: ptr(p.Text), ThoughtSignature: p.Signature})
		case protocol.ToolCall:
			fc := &functionCall{ID: p.ID, Name: p.Name}
			args, err := argsJSON(p.Args)
			if err != nil {
				return nil, fmt.Errorf("assistant part %d: %w", i, err)
			}
			if len(p.Args) > 0 {
				fc.Args = args
			}
			wp := part{FunctionCall: fc}
			if replayable(p.Origin) {
				wp.ThoughtSignature = p.Signature
			}
			out = append(out, wp)
		case protocol.ServerToolCall:
			if !replayable(p.Origin) {
				continue
			}
			wp, err := encodeServerToolCall(p)
			if err != nil {
				return nil, fmt.Errorf("assistant part %d: %w", i, err)
			}
			out = append(out, wp)
		case protocol.ServerToolResult:
			if !replayable(p.Origin) {
				continue
			}
			wp, err := encodeServerToolResult(p)
			if err != nil {
				return nil, fmt.Errorf("assistant part %d: %w", i, err)
			}
			out = append(out, wp)
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

func replayable(origin protocol.ID) bool {
	return protocol.Replayable(origin, protocol.GoogleGenerativeAI)
}

func signatureOnly(th protocol.Thinking) bool {
	return th.Text == "" && th.Signature != "" && !th.Redacted
}

// Restores the executableCode or toolCall part a vendor side call was decoded from
func encodeServerToolCall(c protocol.ServerToolCall) (part, error) {
	args, err := protocol.ParseArgs(string(c.Args))
	if err != nil {
		return part{}, fmt.Errorf("server tool call %s: %w", c.Name, err)
	}
	switch c.Name {
	case nameCodeExecution:
		return part{ExecutableCode: args}, nil
	case nameToolCall:
		return part{ToolCall: args}, nil
	}
	return part{}, fmt.Errorf("server tool call %q has no part kind", c.Name)
}

// Restores the codeExecutionResult or toolResponse part a vendor side result was decoded from
func encodeServerToolResult(r protocol.ServerToolResult) (part, error) {
	if len(r.Content) == 0 {
		return part{}, fmt.Errorf("server tool result %s has no content", r.Name)
	}
	switch r.Name {
	case nameCodeExecution:
		return part{CodeExecutionResult: r.Content}, nil
	case nameToolCall:
		return part{ToolResponse: r.Content}, nil
	}
	return part{}, fmt.Errorf("server tool result %q has no part kind", r.Name)
}

// Names each result from the matching call of the previous model turn when the result carries none
func encodeToolResults(parts []protocol.Part, calls []protocol.ToolCall) ([]part, error) {
	out := make([]part, 0, len(parts))
	for i, p := range parts {
		tr, ok := p.(protocol.ToolResult)
		if !ok {
			return nil, fmt.Errorf("tool message cannot carry %T", p)
		}
		name, err := resultName(tr, i, calls)
		if err != nil {
			return nil, fmt.Errorf("tool result %d: %w", i, err)
		}
		fr := &functionResponse{ID: tr.CallID, Name: name}
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
			case protocol.Document:
				dp, err := encodeDocument(c)
				if err != nil {
					return nil, err
				}
				fr.Parts = append(fr.Parts, functionResponsePart{InlineData: *dp.InlineData})
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

// Resolves the function name by id, then by position among the previous calls
func resultName(tr protocol.ToolResult, index int, calls []protocol.ToolCall) (string, error) {
	if tr.Name != "" {
		return tr.Name, nil
	}
	if tr.CallID != "" {
		for _, c := range calls {
			if c.ID == tr.CallID {
				return c.Name, nil
			}
		}
		return "", fmt.Errorf("no function call %s to name the result", tr.CallID)
	}
	if index < len(calls) {
		return calls[index].Name, nil
	}
	return "", fmt.Errorf("name is required")
}

func encodeImage(img protocol.Image) (part, error) {
	if img.MediaType == "" {
		return part{}, fmt.Errorf("image has no media type")
	}
	if len(img.Data) == 0 {
		return part{}, fmt.Errorf("image has no data")
	}
	return part{InlineData: &blob{MimeType: img.MediaType, Data: base64.StdEncoding.EncodeToString(img.Data)}}, nil
}

func encodeDocument(d protocol.Document) (part, error) {
	if d.MediaType == "" {
		return part{}, fmt.Errorf("document %q has no media type", d.Name)
	}
	if len(d.Data) == 0 {
		return part{}, fmt.Errorf("document %q has no data", d.Name)
	}
	return part{InlineData: &blob{MimeType: d.MediaType, Data: base64.StdEncoding.EncodeToString(d.Data)}}, nil
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
