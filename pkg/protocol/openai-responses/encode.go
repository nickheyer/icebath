package openairesponses

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
)

const (
	path             = "/responses"
	includeEncrypted = "reasoning.encrypted_content"
	imageDetail      = "auto"
	errorPrefix      = "[error] "
	defaultFilename  = "document"
)

var documentExtensions = map[string]string{
	"application/pdf":  ".pdf",
	"text/plain":       ".txt",
	"text/markdown":    ".md",
	"text/csv":         ".csv",
	"text/html":        ".html",
	"application/json": ".json",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": ".docx",
	"application/msword":       ".doc",
	"application/vnd.ms-excel": ".xls",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": ".xlsx",
}

// Builds POST /responses with store off and encrypted reasoning requested for stateless replay
func (Protocol) Encode(req protocol.Request) (protocol.Call, error) {
	if req.Model == "" {
		return protocol.Call{}, errors.New("openai-responses: model is required")
	}
	if len(req.StopSequences) > 0 {
		return protocol.Call{}, errors.New("openai-responses: the wire has no stop sequences field")
	}
	input, err := encodeInput(req.Messages)
	if err != nil {
		return protocol.Call{}, err
	}
	tools, err := encodeTools(req.Tools)
	if err != nil {
		return protocol.Call{}, err
	}
	choice, err := encodeToolChoice(req.ToolChoice)
	if err != nil {
		return protocol.Call{}, err
	}
	body := request{
		Model:           req.Model,
		Instructions:    req.System,
		Input:           input,
		Tools:           tools,
		ToolChoice:      choice,
		MaxOutputTokens: req.MaxTokens,
		Temperature:     req.Temperature,
		Stream:          req.Stream,
		Store:           false,
		Include:         []string{includeEncrypted},
	}
	raw, err := protocol.MarshalBody(body, req.Extra)
	if err != nil {
		return protocol.Call{}, err
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	if req.Stream {
		header.Set("Accept", "text/event-stream")
	} else {
		header.Set("Accept", "application/json")
	}
	return protocol.Call{Method: http.MethodPost, Path: path, Header: header, Body: raw}, nil
}

func encodeInput(messages []protocol.Message) ([]any, error) {
	items := []any{}
	for i, msg := range messages {
		if len(msg.Content) == 0 {
			return nil, fmt.Errorf("openai-responses: message %d has no content", i)
		}
		var err error
		switch msg.Role {
		case protocol.RoleUser:
			items, err = appendUser(items, i, msg)
		case protocol.RoleAssistant:
			items, err = appendAssistant(items, i, msg)
		case protocol.RoleTool:
			items, err = appendToolResults(items, i, msg)
		default:
			err = fmt.Errorf("openai-responses: message %d has unknown role %q", i, msg.Role)
		}
		if err != nil {
			return nil, err
		}
	}
	return items, nil
}

func appendUser(items []any, i int, msg protocol.Message) ([]any, error) {
	content, err := inputContent(msg.Content, fmt.Sprintf("message %d", i))
	if err != nil {
		return nil, err
	}
	return append(items, messageItem{Type: itemMessage, Role: "user", Content: content}), nil
}

// Converts user facing text, image and document parts into input_text, input_image and input_file parts
func inputContent(parts []protocol.Part, where string) ([]any, error) {
	content := make([]any, 0, len(parts))
	for _, p := range parts {
		switch v := p.(type) {
		case protocol.Text:
			content = append(content, inputText{Type: partInputText, Text: v.Text})
		case protocol.Image:
			url, err := dataURL(v.MediaType, v.Data)
			if err != nil {
				return nil, fmt.Errorf("openai-responses: %s: image %w", where, err)
			}
			content = append(content, inputImage{Type: partInputImage, ImageURL: url, Detail: imageDetail})
		case protocol.Document:
			url, err := dataURL(v.MediaType, v.Data)
			if err != nil {
				return nil, fmt.Errorf("openai-responses: %s: document %w", where, err)
			}
			content = append(content, inputFile{Type: partInputFile, Filename: filename(v), FileData: url})
		default:
			return nil, fmt.Errorf("openai-responses: %s cannot carry a %T part", where, p)
		}
	}
	return content, nil
}

// Uses the document name, otherwise derives one from the media type extension
func filename(d protocol.Document) string {
	if d.Name != "" {
		return d.Name
	}
	return defaultFilename + documentExtensions[d.MediaType]
}

// Replays an assistant turn as one message item plus reasoning, function call and server tool items
func appendAssistant(items []any, i int, msg protocol.Message) ([]any, error) {
	var text *messageItem
	for _, p := range msg.Content {
		switch v := p.(type) {
		case protocol.Text:
			part := outputText{Type: partOutputText, Text: v.Text, Annotations: []json.RawMessage{}, Logprobs: []json.RawMessage{}}
			if text == nil {
				text = &messageItem{Type: itemMessage, Role: "assistant", Status: "completed"}
				items = append(items, text)
			}
			text.Content = append(text.Content, part)
		case protocol.Thinking:
			if v.Redacted || !protocol.Replayable(v.Origin, protocol.OpenAIResponses) {
				continue
			}
			if item, ok := reasoningFromThinking(v); ok {
				items = append(items, item)
			}
		case protocol.ToolCall:
			args, err := protocol.ParseArgs(string(v.Args))
			if err != nil {
				return nil, fmt.Errorf("openai-responses: message %d tool call %s: %w", i, v.ID, err)
			}
			items = append(items, functionCallItem{Type: itemFunctionCall, CallID: v.ID, Name: v.Name, Arguments: string(args)})
		case protocol.ServerToolCall:
			if !protocol.Replayable(v.Origin, protocol.OpenAIResponses) {
				continue
			}
			item, err := serverToolItem(v)
			if err != nil {
				return nil, fmt.Errorf("openai-responses: message %d: %w", i, err)
			}
			items = append(items, item)
		case protocol.ServerToolResult:
		default:
			return nil, fmt.Errorf("openai-responses: message %d cannot carry a %T part in an assistant turn", i, p)
		}
	}
	return items, nil
}

// Rebuilds a vendor item from the raw fields kept in Args with its id and type restored
func serverToolItem(c protocol.ServerToolCall) (map[string]json.RawMessage, error) {
	if c.Name == "" {
		return nil, fmt.Errorf("server tool call %s has no item type", c.ID)
	}
	fields := map[string]json.RawMessage{}
	if len(c.Args) > 0 {
		if err := json.Unmarshal(c.Args, &fields); err != nil {
			return nil, fmt.Errorf("server tool call %s: %w", c.ID, err)
		}
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	fields["type"] = quote(c.Name)
	if c.ID != "" {
		fields["id"] = quote(c.ID)
	}
	return fields, nil
}

func quote(s string) json.RawMessage {
	raw, _ := json.Marshal(s)
	return raw
}

// Rebuilds a reasoning item from a signature this protocol issued, reporting false for any other
func reasoningFromThinking(t protocol.Thinking) (reasoningItem, bool) {
	var sig reasoningSignature
	if err := json.Unmarshal([]byte(t.Signature), &sig); err != nil || sig.ID == "" {
		return reasoningItem{}, false
	}
	item := reasoningItem{Type: itemReasoning, ID: sig.ID, Summary: []summaryPart{}, EncryptedContent: sig.EncryptedContent}
	if t.Text != "" {
		item.Summary = []summaryPart{{Type: partSummaryText, Text: t.Text}}
	}
	return item, true
}

func appendToolResults(items []any, i int, msg protocol.Message) ([]any, error) {
	for j, p := range msg.Content {
		res, ok := p.(protocol.ToolResult)
		if !ok {
			return nil, fmt.Errorf("openai-responses: message %d part %d is a %T, tool turns carry only tool results", i, j, p)
		}
		output, err := toolOutput(res, fmt.Sprintf("message %d part %d", i, j))
		if err != nil {
			return nil, err
		}
		items = append(items, functionCallOutputItem{Type: itemFunctionCallOutput, CallID: res.CallID, Output: output})
	}
	return items, nil
}

// Sends text only results as a string and mixed results as a content list, flagging failures
func toolOutput(res protocol.ToolResult, where string) (any, error) {
	texts := make([]string, 0, len(res.Content))
	textOnly := true
	for _, p := range res.Content {
		switch v := p.(type) {
		case protocol.Text:
			texts = append(texts, v.Text)
		case protocol.Image, protocol.Document:
			textOnly = false
		default:
			return nil, fmt.Errorf("openai-responses: %s: tool result cannot carry a %T part", where, p)
		}
	}
	if textOnly {
		joined := strings.Join(texts, "\n")
		if res.IsError {
			joined = errorPrefix + joined
		}
		return joined, nil
	}
	content, err := inputContent(res.Content, where)
	if err != nil {
		return nil, err
	}
	if !res.IsError {
		return content, nil
	}
	if first, ok := content[0].(inputText); ok {
		first.Text = errorPrefix + first.Text
		content[0] = first
		return content, nil
	}
	return append([]any{inputText{Type: partInputText, Text: errorPrefix}}, content...), nil
}

func dataURL(mediaType string, data []byte) (string, error) {
	if mediaType == "" || len(data) == 0 {
		return "", errors.New("needs a media type and data")
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func encodeTools(tools []protocol.Tool) ([]functionTool, error) {
	out := make([]functionTool, 0, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			return nil, errors.New("openai-responses: tool without a name")
		}
		if len(t.Parameters) == 0 {
			return nil, fmt.Errorf("openai-responses: tool %s declares no parameters schema", t.Name)
		}
		out = append(out, functionTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Parameters, Strict: false})
	}
	return out, nil
}

func encodeToolChoice(tc protocol.ToolChoice) (any, error) {
	switch tc.Mode {
	case "":
		return nil, nil
	case protocol.ToolChoiceAuto, protocol.ToolChoiceNone, protocol.ToolChoiceRequired:
		return string(tc.Mode), nil
	case protocol.ToolChoiceTool:
		if tc.Name == "" {
			return nil, errors.New("openai-responses: tool choice names no tool")
		}
		return toolChoiceFunction{Type: "function", Name: tc.Name}, nil
	}
	return nil, fmt.Errorf("openai-responses: unknown tool choice mode %q", tc.Mode)
}
