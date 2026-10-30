// OpenAI Responses API wire protocol with paths relative to the api.openai.com/v1 base URL
package openairesponses

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/sse"
	"github.com/nickheyer/icebath/pkg/protocol"
)

type Protocol struct{}

func (Protocol) ID() protocol.ID { return protocol.OpenAIResponses }

func init() { protocol.Register(Protocol{}) }

var _ protocol.Protocol = Protocol{}

const (
	itemMessage            = "message"
	itemFunctionCall       = "function_call"
	itemFunctionCallOutput = "function_call_output"
	itemReasoning          = "reasoning"
	partInputText          = "input_text"
	partInputImage         = "input_image"
	partInputFile          = "input_file"
	partOutputText         = "output_text"
	partRefusal            = "refusal"
	partReasoningText      = "reasoning_text"
	partSummaryText        = "summary_text"
	annotationURL          = "url_citation"
	annotationFile         = "file_citation"
)

// CreateResponse body limited to the fields the neutral model drives
type request struct {
	Model           string         `json:"model"`
	Instructions    string         `json:"instructions,omitempty"`
	Input           []any          `json:"input"`
	Tools           []functionTool `json:"tools,omitempty"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
	MaxOutputTokens int            `json:"max_output_tokens,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	Stream          bool           `json:"stream,omitempty"`
	Store           bool           `json:"store"`
	Include         []string       `json:"include"`
}

type functionTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type toolChoiceFunction struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// Message input item for user turns and replayed assistant text
type messageItem struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []any  `json:"content"`
	Status  string `json:"status,omitempty"`
}

type inputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type inputImage struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
	Detail   string `json:"detail"`
}

type inputFile struct {
	Type     string `json:"type"`
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
}

type outputText struct {
	Type        string            `json:"type"`
	Text        string            `json:"text"`
	Annotations []json.RawMessage `json:"annotations"`
	Logprobs    []json.RawMessage `json:"logprobs"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionCallOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output any    `json:"output"`
}

type reasoningItem struct {
	Type             string        `json:"type"`
	ID               string        `json:"id"`
	Summary          []summaryPart `json:"summary"`
	EncryptedContent string        `json:"encrypted_content,omitempty"`
}

type summaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Replay token carried in Thinking.Signature so a reasoning item can be sent back
type reasoningSignature struct {
	ID               string `json:"id"`
	EncryptedContent string `json:"encrypted_content,omitempty"`
}

// Response object fields read by the decoder, output items kept raw for verbatim replay
type response struct {
	ID                string            `json:"id"`
	Model             string            `json:"model"`
	Status            string            `json:"status"`
	Error             *responseError    `json:"error"`
	IncompleteDetails *incompleteInfo   `json:"incomplete_details"`
	Output            []json.RawMessage `json:"output"`
	Usage             *usage            `json:"usage"`
}

type responseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type incompleteInfo struct {
	Reason string `json:"reason"`
}

// Output item union flattened over message, function_call and reasoning fields
type outputItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Content          []outputPart    `json:"content"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	Arguments        string          `json:"arguments"`
	Summary          []summaryPart   `json:"summary"`
	EncryptedContent string          `json:"encrypted_content"`
	Raw              json.RawMessage `json:"-"`
}

type outputPart struct {
	Type        string       `json:"type"`
	Text        string       `json:"text"`
	Refusal     string       `json:"refusal"`
	Annotations []annotation `json:"annotations"`
}

// Annotation union flattened over the url_citation and file_citation shapes
type annotation struct {
	Type       string `json:"type"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
	FileID     string `json:"file_id"`
	Filename   string `json:"filename"`
	Index      int    `json:"index"`
}

type usage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// ResponseStreamEvent union flattened over every field the decoder reads
type streamEvent struct {
	Type         string          `json:"type"`
	Response     *response       `json:"response"`
	Item         json.RawMessage `json:"item"`
	ItemID       string          `json:"item_id"`
	ContentIndex int             `json:"content_index"`
	SummaryIndex int             `json:"summary_index"`
	Part         *outputPart     `json:"part"`
	Annotation   *annotation     `json:"annotation"`
	Delta        string          `json:"delta"`
	Text         string          `json:"text"`
	Refusal      string          `json:"refusal"`
	Arguments    string          `json:"arguments"`
	Code         string          `json:"code"`
	Message      string          `json:"message"`
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

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

const (
	statusCompleted  = "completed"
	statusIncomplete = "incomplete"
	statusFailed     = "failed"
	partSeparator    = "\n\n"
	doneSentinel     = "[DONE]"
)

var transientTypes = map[string]bool{
	"server_error":        true,
	"rate_limit_exceeded": true,
	"slow_down":           true,
	"overloaded":          true,
}

// Keys under which vendor tool items carry their outcome, checked in this order
var resultKeys = []string{"results", "outputs", "output", "result", "tools"}

// Parses a complete Response body
func (Protocol) Decode(r io.Reader) (protocol.Response, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return protocol.Response{}, err
	}
	var resp response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return protocol.Response{}, fmt.Errorf("openai-responses: response body: %w", err)
	}
	return convert(&resp)
}

func convert(resp *response) (protocol.Response, error) {
	if err := terminal(resp); err != nil {
		return protocol.Response{}, err
	}
	out := protocol.Response{ID: resp.ID, Model: resp.Model, Usage: usageOf(resp.Usage)}
	toolCall := false
	for i, rawItem := range resp.Output {
		item, err := parseItem(rawItem)
		if err != nil {
			return protocol.Response{}, fmt.Errorf("openai-responses: output item %d: %w", i, err)
		}
		parts, err := itemParts(item)
		if err != nil {
			return protocol.Response{}, fmt.Errorf("openai-responses: output item %d: %w", i, err)
		}
		toolCall = toolCall || item.Type == itemFunctionCall
		out.Content = append(out.Content, parts...)
	}
	out.StopReason, out.RawStopReason = stopReason(resp, toolCall)
	return out, nil
}

func parseItem(raw json.RawMessage) (*outputItem, error) {
	if len(raw) == 0 {
		return nil, errors.New("missing item")
	}
	var item outputItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	item.Raw = raw
	return &item, nil
}

// Rejects statuses that cannot end a turn, surfacing a failed response as an error
func terminal(resp *response) error {
	switch resp.Status {
	case statusCompleted, statusIncomplete:
		return nil
	case statusFailed:
		e := &protocol.Error{Status: 200, Message: "response failed"}
		if resp.Error != nil {
			e.Type, e.Message = resp.Error.Code, resp.Error.Message
			e.Transient = transientTypes[resp.Error.Code]
		}
		return e
	}
	return fmt.Errorf("openai-responses: response status %q is not terminal", resp.Status)
}

func itemParts(item *outputItem) ([]protocol.Part, error) {
	switch item.Type {
	case itemMessage:
		parts := make([]protocol.Part, 0, len(item.Content))
		for _, c := range item.Content {
			switch c.Type {
			case partOutputText:
				parts = append(parts, protocol.Text{Text: c.Text, Citations: citationsOf(c.Annotations)})
			case partRefusal:
				parts = append(parts, protocol.Text{Text: c.Refusal})
			default:
				return nil, fmt.Errorf("message part type %q has no neutral form", c.Type)
			}
		}
		return parts, nil
	case itemFunctionCall:
		args, err := protocol.ParseArgs(item.Arguments)
		if err != nil {
			return nil, fmt.Errorf("function call %s: %w", item.Name, err)
		}
		return []protocol.Part{protocol.ToolCall{ID: item.CallID, Name: item.Name, Args: args}}, nil
	case itemReasoning:
		return []protocol.Part{protocol.Thinking{Text: reasoningText(item), Signature: signatureOf(item), Origin: protocol.OpenAIResponses}}, nil
	case itemFunctionCallOutput, "":
		return nil, fmt.Errorf("output item type %q has no neutral form", item.Type)
	}
	args, err := serverToolArgs(item)
	if err != nil {
		return nil, err
	}
	parts := []protocol.Part{protocol.ServerToolCall{ID: item.ID, Name: item.Type, Args: args, Origin: protocol.OpenAIResponses}}
	if result, ok := serverToolResult(item); ok {
		parts = append(parts, protocol.ServerToolResult{CallID: item.ID, Name: item.Type, Content: result, Origin: protocol.OpenAIResponses})
	}
	return parts, nil
}

func citationsOf(annotations []annotation) []protocol.Citation {
	var out []protocol.Citation
	for _, a := range annotations {
		if c, ok := mapAnnotation(a); ok {
			out = append(out, c)
		}
	}
	return out
}

// Maps url_citation and file_citation annotations, reporting false for every other kind
func mapAnnotation(a annotation) (protocol.Citation, bool) {
	switch a.Type {
	case annotationURL:
		return protocol.Citation{Title: a.Title, URL: a.URL, Start: a.StartIndex, End: a.EndIndex}, true
	case annotationFile:
		doc := a.Filename
		if doc == "" {
			doc = a.FileID
		}
		return protocol.Citation{Title: a.Filename, Document: doc, Start: a.Index, End: a.Index}, true
	}
	return protocol.Citation{}, false
}

// Keeps every field of a vendor tool item except its id and type as the call arguments
func serverToolArgs(item *outputItem) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(item.Raw, &fields); err != nil {
		return nil, fmt.Errorf("%s item: %w", item.Type, err)
	}
	delete(fields, "id")
	delete(fields, "type")
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// Finds the outcome a vendor tool item embeds under its result key
func serverToolResult(item *outputItem) (json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(item.Raw, &fields); err != nil {
		return nil, false
	}
	for _, key := range resultKeys {
		if v, ok := fields[key]; ok && string(v) != "null" {
			return v, true
		}
	}
	return nil, false
}

// Joins summary and reasoning texts of a reasoning item with blank lines
func reasoningText(item *outputItem) string {
	texts := make([]string, 0, len(item.Summary)+len(item.Content))
	for _, s := range item.Summary {
		texts = append(texts, s.Text)
	}
	for _, c := range item.Content {
		texts = append(texts, c.Text)
	}
	return strings.Join(texts, partSeparator)
}

func signatureOf(item *outputItem) string {
	raw, _ := json.Marshal(reasoningSignature{ID: item.ID, EncryptedContent: item.EncryptedContent})
	return string(raw)
}

func stopReason(resp *response, toolCall bool) (protocol.StopReason, string) {
	if resp.Status == statusCompleted {
		if toolCall {
			return protocol.StopToolUse, statusCompleted
		}
		return protocol.StopEnd, statusCompleted
	}
	reason := ""
	if resp.IncompleteDetails != nil {
		reason = resp.IncompleteDetails.Reason
	}
	raw := statusIncomplete
	if reason != "" {
		raw += "/" + reason
	}
	switch reason {
	case "max_output_tokens":
		return protocol.StopLength, raw
	case "content_filter":
		return protocol.StopContentFilter, raw
	}
	return protocol.StopOther, raw
}

// Takes cached tokens out of the input count so Input holds only the uncached share
func usageOf(u *usage) protocol.Usage {
	if u == nil {
		return protocol.Usage{}
	}
	return protocol.Usage{
		Input:      u.InputTokens - u.InputTokensDetails.CachedTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.InputTokensDetails.CachedTokens,
		CacheWrite: u.InputTokensDetails.CacheWriteTokens,
		Reasoning:  u.OutputTokensDetails.ReasoningTokens,
	}
}

// Parses an error body, falling back to the raw text when it is not the wire error shape
func (Protocol) DecodeError(status int, body []byte) *protocol.Error {
	e := &protocol.Error{Status: status, Transient: transientStatus(status)}
	var wire errorBody
	if err := json.Unmarshal(body, &wire); err == nil && wire.Error.Message != "" {
		e.Type = wire.Error.Code
		if e.Type == "" {
			e.Type = wire.Error.Type
		}
		e.Message = wire.Error.Message
		e.Transient = e.Transient || transientTypes[wire.Error.Code] || transientTypes[wire.Error.Type]
		return e
	}
	e.Message = strings.TrimSpace(string(body))
	return e
}

// Reports statuses that denote a timeout, conflict, throttle or server failure
func transientStatus(status int) bool {
	return status == 408 || status == 409 || status == 429 || status >= 500
}

// Turns the SSE event stream into neutral events, ending at the terminal response event
func (Protocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		st := streamState{b: protocol.Builder{Origin: protocol.OpenAIResponses}, acc: map[string]string{}, parts: map[string]int{}}
		for ev, err := range sse.Read(r) {
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			if ev.Data == doneSentinel {
				continue
			}
			var e streamEvent
			if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
				yield(protocol.Event{}, fmt.Errorf("openai-responses: event %q: %w", ev.Name, err))
				return
			}
			done, err := st.apply(&e)
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			for _, out := range st.b.Events() {
				if !yield(out, nil) {
					return
				}
			}
			if done {
				return
			}
		}
		yield(protocol.Event{}, errors.New("openai-responses: stream ended before a terminal response event"))
	}
}

// Stream decoder state keyed by item id, with accumulated text per builder key
type streamState struct {
	b        protocol.Builder
	acc      map[string]string
	parts    map[string]int
	toolCall bool
}

func textKey(itemID string, index int) string {
	return fmt.Sprintf("%s/%d", itemID, index)
}

func (st *streamState) apply(e *streamEvent) (bool, error) {
	switch e.Type {
	case "response.created", "response.in_progress", "response.queued":
		if e.Response != nil {
			st.b.Start(e.Response.ID, e.Response.Model)
		}
	case "response.output_item.added":
		return false, st.itemAdded(e.Item)
	case "response.content_part.added":
		return false, st.partAdded(e)
	case "response.output_text.delta", "response.refusal.delta":
		return false, st.delta(textKey(e.ItemID, e.ContentIndex), e.Delta)
	case "response.output_text.done":
		return false, st.reconcile(textKey(e.ItemID, e.ContentIndex), e.Text)
	case "response.refusal.done":
		return false, st.reconcile(textKey(e.ItemID, e.ContentIndex), e.Refusal)
	case "response.output_text.annotation.added":
		return false, st.annotation(e)
	case "response.content_part.done":
		return false, st.partDone(e)
	case "response.reasoning_summary_part.added":
		return false, st.startPart(e.ItemID, "summary", e.SummaryIndex)
	case "response.reasoning_summary_text.delta":
		return false, st.partDelta(e.ItemID, "summary", e.SummaryIndex, e.Delta)
	case "response.reasoning_summary_text.done":
		return false, st.partFinal(e.ItemID, "summary", e.SummaryIndex, e.Text)
	case "response.reasoning_summary_part.done":
		if e.Part == nil {
			return false, errors.New("openai-responses: reasoning_summary_part.done without a part")
		}
		return false, st.partFinal(e.ItemID, "summary", e.SummaryIndex, e.Part.Text)
	case "response.reasoning_text.delta":
		return false, st.partDelta(e.ItemID, "content", e.ContentIndex, e.Delta)
	case "response.reasoning_text.done":
		return false, st.partFinal(e.ItemID, "content", e.ContentIndex, e.Text)
	case "response.function_call_arguments.delta":
		return false, st.delta(e.ItemID, e.Delta)
	case "response.function_call_arguments.done":
		return false, st.reconcile(e.ItemID, e.Arguments)
	case "response.output_item.done":
		return false, st.itemDone(e.Item)
	case "response.completed", "response.incomplete":
		return true, st.finish(e.Response)
	case "response.failed":
		if e.Response == nil {
			return false, &protocol.Error{Status: 200, Message: "response failed"}
		}
		e.Response.Status = statusFailed
		return false, terminal(e.Response)
	case "error":
		return false, &protocol.Error{Status: 200, Type: e.Code, Message: e.Message, Transient: transientTypes[e.Code]}
	}
	return false, nil
}

func (st *streamState) itemAdded(raw json.RawMessage) error {
	item, err := parseItem(raw)
	if err != nil {
		return fmt.Errorf("openai-responses: output_item.added: %w", err)
	}
	switch item.Type {
	case itemFunctionCall:
		st.b.OpenToolCall(item.ID, item.CallID, item.Name)
	case itemReasoning:
		st.b.OpenThinking(item.ID, false)
	case itemMessage:
	case itemFunctionCallOutput, "":
		return fmt.Errorf("openai-responses: output item type %q has no neutral form", item.Type)
	default:
		st.b.OpenServerToolCall(item.ID, item.ID, item.Type)
	}
	return nil
}

func (st *streamState) partAdded(e *streamEvent) error {
	if e.Part == nil {
		return errors.New("openai-responses: content_part.added without a part")
	}
	switch e.Part.Type {
	case partOutputText, partRefusal:
		st.b.OpenText(textKey(e.ItemID, e.ContentIndex))
	case partReasoningText:
	default:
		return fmt.Errorf("openai-responses: content part type %q has no neutral form", e.Part.Type)
	}
	return nil
}

// Attaches a url or file citation to the text part it annotates, ignoring other annotation kinds
func (st *streamState) annotation(e *streamEvent) error {
	if e.Annotation == nil {
		return nil
	}
	c, ok := mapAnnotation(*e.Annotation)
	if !ok {
		return nil
	}
	return st.b.Cite(textKey(e.ItemID, e.ContentIndex), c)
}

func (st *streamState) partDone(e *streamEvent) error {
	if e.Part == nil {
		return errors.New("openai-responses: content_part.done without a part")
	}
	key := textKey(e.ItemID, e.ContentIndex)
	switch e.Part.Type {
	case partOutputText:
		if err := st.reconcile(key, e.Part.Text); err != nil {
			return err
		}
	case partRefusal:
		if err := st.reconcile(key, e.Part.Refusal); err != nil {
			return err
		}
	case partReasoningText:
		return nil
	default:
		return fmt.Errorf("openai-responses: content part type %q has no neutral form", e.Part.Type)
	}
	return st.b.Close(key)
}

// Appends a fragment to the block under key and remembers it for later reconciliation
func (st *streamState) delta(key, s string) error {
	if err := st.b.Delta(key, s); err != nil {
		return err
	}
	st.acc[key] += s
	return nil
}

// Fills in any tail the deltas missed and rejects a final text that contradicts them
func (st *streamState) reconcile(key, full string) error {
	cur := st.acc[key]
	if !strings.HasPrefix(full, cur) {
		return fmt.Errorf("openai-responses: final text of %s diverges from its streamed deltas", key)
	}
	if len(full) == len(cur) {
		return nil
	}
	return st.delta(key, full[len(cur):])
}

// Separates reasoning parts of one item with a blank line the first time each part appears
func (st *streamState) startPart(itemID, kind string, index int) error {
	key := partKey(itemID, kind, index)
	if _, seen := st.acc[key]; seen {
		return nil
	}
	st.acc[key] = ""
	if st.parts[itemID] > 0 {
		if err := st.delta(itemID, partSeparator); err != nil {
			return err
		}
	}
	st.parts[itemID]++
	return nil
}

func partKey(itemID, kind string, index int) string {
	return fmt.Sprintf("%s/%s/%d", itemID, kind, index)
}

// Appends a reasoning fragment to its item and to the part it belongs to
func (st *streamState) partDelta(itemID, kind string, index int, s string) error {
	if err := st.startPart(itemID, kind, index); err != nil {
		return err
	}
	if s == "" {
		return nil
	}
	if err := st.delta(itemID, s); err != nil {
		return err
	}
	st.acc[partKey(itemID, kind, index)] += s
	return nil
}

// Fills in the tail of a reasoning part its deltas missed and rejects a contradicting final text
func (st *streamState) partFinal(itemID, kind string, index int, full string) error {
	if err := st.startPart(itemID, kind, index); err != nil {
		return err
	}
	cur := st.acc[partKey(itemID, kind, index)]
	if !strings.HasPrefix(full, cur) {
		return fmt.Errorf("openai-responses: final text of %s diverges from its streamed deltas", partKey(itemID, kind, index))
	}
	return st.partDelta(itemID, kind, index, full[len(cur):])
}

func (st *streamState) itemDone(raw json.RawMessage) error {
	item, err := parseItem(raw)
	if err != nil {
		return fmt.Errorf("openai-responses: output_item.done: %w", err)
	}
	switch item.Type {
	case itemFunctionCall:
		st.toolCall = true
		if err := st.reconcile(item.ID, item.Arguments); err != nil {
			return err
		}
		return st.b.Close(item.ID)
	case itemReasoning:
		if err := st.reconcile(item.ID, reasoningText(item)); err != nil {
			return err
		}
		if err := st.b.Signature(item.ID, signatureOf(item)); err != nil {
			return err
		}
		return st.b.Close(item.ID)
	case itemMessage:
		return nil
	case itemFunctionCallOutput, "":
		return fmt.Errorf("openai-responses: output item type %q has no neutral form", item.Type)
	}
	return st.serverToolDone(item)
}

// Delivers a finished vendor tool item as one arguments delta plus its embedded result
func (st *streamState) serverToolDone(item *outputItem) error {
	args, err := serverToolArgs(item)
	if err != nil {
		return fmt.Errorf("openai-responses: %w", err)
	}
	if err := st.b.Delta(item.ID, string(args)); err != nil {
		return err
	}
	if err := st.b.Close(item.ID); err != nil {
		return err
	}
	if result, ok := serverToolResult(item); ok {
		st.b.AddServerToolResult(protocol.ServerToolResult{CallID: item.ID, Name: item.Type, Content: result})
	}
	return nil
}

func (st *streamState) finish(resp *response) error {
	if resp == nil {
		return errors.New("openai-responses: terminal event without a response")
	}
	st.b.Start(resp.ID, resp.Model)
	toolCall := st.toolCall
	for _, raw := range resp.Output {
		item, err := parseItem(raw)
		if err != nil {
			return fmt.Errorf("openai-responses: terminal output: %w", err)
		}
		toolCall = toolCall || item.Type == itemFunctionCall
	}
	stop, raw := stopReason(resp, toolCall)
	return st.b.Finish(stop, raw, usageOf(resp.Usage))
}
