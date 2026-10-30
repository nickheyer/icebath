// Anthropic Messages API wire protocol with paths relative to the api.anthropic.com origin
package anthropicmessages

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/sse"
	"github.com/nickheyer/icebath/pkg/protocol"
)

const (
	path    = "/v1/messages"
	version = "2023-06-01"
)

type Protocol struct{}

func (Protocol) ID() protocol.ID { return protocol.AnthropicMessages }

func (Protocol) MaxTokensRequired() bool { return true }

func init() { protocol.Register(Protocol{}) }

var _ protocol.Protocol = Protocol{}

// Request body for creating a message
type request struct {
	Model         string      `json:"model"`
	MaxTokens     int         `json:"max_tokens"`
	System        []textBlock `json:"system,omitempty"`
	Messages      []message   `json:"messages"`
	Tools         []tool      `json:"tools,omitempty"`
	ToolChoice    *toolChoice `json:"tool_choice,omitempty"`
	Temperature   *float64    `json:"temperature,omitempty"`
	StopSequences []string    `json:"stop_sequences,omitempty"`
	Stream        bool        `json:"stream,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

// Prompt cache breakpoint covering everything up to and including its block
type cacheControl struct {
	Type string `json:"type"`
}

type toolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type textBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type imageBlock struct {
	Type         string        `json:"type"`
	Source       imageSource   `json:"source"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type documentBlock struct {
	Type         string         `json:"type"`
	Source       documentSource `json:"source"`
	Title        string         `json:"title,omitempty"`
	CacheControl *cacheControl  `json:"cache_control,omitempty"`
}

type documentSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type thinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type redactedThinkingBlock struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

// Shared by tool_use and server_tool_use blocks
type toolUseBlock struct {
	Type         string          `json:"type"`
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Input        json.RawMessage `json:"input"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

type toolResultBlock struct {
	Type         string        `json:"type"`
	ToolUseID    string        `json:"tool_use_id"`
	Content      []any         `json:"content,omitempty"`
	IsError      bool          `json:"is_error,omitempty"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// Replay form of web_search_tool_result and every other vendor side result block
type serverToolResultBlock struct {
	Type      string          `json:"type"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

// Response content block of any type, read by switching on Type
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	Data      string          `json:"data"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	Citations []citation      `json:"citations"`
}

// Citation of any location type, flattened over the char, page, block and web shapes
type citation struct {
	Type            string `json:"type"`
	CitedText       string `json:"cited_text"`
	DocumentIndex   *int   `json:"document_index"`
	DocumentTitle   string `json:"document_title"`
	Title           string `json:"title"`
	URL             string `json:"url"`
	StartCharIndex  *int   `json:"start_char_index"`
	EndCharIndex    *int   `json:"end_char_index"`
	StartPageNumber *int   `json:"start_page_number"`
	EndPageNumber   *int   `json:"end_page_number"`
	StartBlockIndex *int   `json:"start_block_index"`
	EndBlockIndex   *int   `json:"end_block_index"`
}

type outputTokensDetails struct {
	ThinkingTokens int `json:"thinking_tokens"`
}

type usage struct {
	InputTokens              int                  `json:"input_tokens"`
	OutputTokens             int                  `json:"output_tokens"`
	CacheCreationInputTokens int                  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                  `json:"cache_read_input_tokens"`
	OutputTokensDetails      *outputTokensDetails `json:"output_tokens_details"`
}

// Non streaming message body, also carried by message_start
type response struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Model        string         `json:"model"`
	Content      []contentBlock `json:"content"`
	StopReason   string         `json:"stop_reason"`
	StopSequence string         `json:"stop_sequence"`
	Usage        usage          `json:"usage"`
}

// Delta of a content block or of the message, read by switching on the event
type delta struct {
	Type         string    `json:"type"`
	Text         string    `json:"text"`
	PartialJSON  string    `json:"partial_json"`
	Thinking     string    `json:"thinking"`
	Signature    string    `json:"signature"`
	Citation     *citation `json:"citation"`
	StopReason   string    `json:"stop_reason"`
	StopSequence string    `json:"stop_sequence"`
}

type streamEvent struct {
	Type         string        `json:"type"`
	Message      *response     `json:"message"`
	Index        int           `json:"index"`
	ContentBlock *contentBlock `json:"content_block"`
	Delta        *delta        `json:"delta"`
	Usage        *usage        `json:"usage"`
	Error        *errorObject  `json:"error"`
}

type errorObject struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type errorResponse struct {
	Type  string      `json:"type"`
	Error errorObject `json:"error"`
}

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
		return protocol.Call{}, fmt.Errorf("anthropic-messages: the API requires max_tokens and the output limit of %s is unknown, set max_tokens", req.Model)
	}
	body := request{
		Model:         req.Model,
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		StopSequences: req.StopSequences,
		Stream:        req.Stream,
	}
	if req.System != "" {
		body.System = []textBlock{{Type: "text", Text: req.System, CacheControl: ephemeral()}}
	}
	for _, t := range req.Tools {
		if len(t.Parameters) == 0 {
			return protocol.Call{}, fmt.Errorf("anthropic-messages: tool %q has no input schema", t.Name)
		}
		body.Tools = append(body.Tools, tool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters})
	}
	if n := len(body.Tools); n > 0 {
		body.Tools[n-1].CacheControl = ephemeral()
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
	markTail(body.Messages)
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

func ephemeral() *cacheControl { return &cacheControl{Type: "ephemeral"} }

// Puts the conversation breakpoint on the newest block that may carry one, skipping thinking blocks
func markTail(msgs []message) {
	for i := len(msgs) - 1; i >= 0; i-- {
		for j := len(msgs[i].Content) - 1; j >= 0; j-- {
			switch b := msgs[i].Content[j].(type) {
			case textBlock:
				b.CacheControl = ephemeral()
				msgs[i].Content[j] = b
			case imageBlock:
				b.CacheControl = ephemeral()
				msgs[i].Content[j] = b
			case documentBlock:
				b.CacheControl = ephemeral()
				msgs[i].Content[j] = b
			case toolUseBlock:
				b.CacheControl = ephemeral()
				msgs[i].Content[j] = b
			case toolResultBlock:
				b.CacheControl = ephemeral()
				msgs[i].Content[j] = b
			default:
				continue
			}
			return
		}
	}
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

const resultSuffix = "_tool_result"

var transientTypes = map[string]bool{
	"overloaded_error": true,
	"rate_limit_error": true,
	"api_error":        true,
}

func (Protocol) Decode(r io.Reader) (protocol.Response, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return protocol.Response{}, err
	}
	var msg response
	if err := json.Unmarshal(data, &msg); err != nil {
		return protocol.Response{}, fmt.Errorf("anthropic-messages: %w", err)
	}
	if msg.Type != "message" {
		return protocol.Response{}, fmt.Errorf("anthropic-messages: body type %q is not a message", msg.Type)
	}
	parts, err := decodeBlocks(msg.Content)
	if err != nil {
		return protocol.Response{}, err
	}
	stop, err := mapStop(msg.StopReason)
	if err != nil {
		return protocol.Response{}, err
	}
	return protocol.Response{
		ID:            msg.ID,
		Model:         msg.Model,
		Content:       parts,
		StopReason:    stop,
		RawStopReason: rawStop(msg.StopReason, msg.StopSequence),
		Usage:         mapUsage(msg.Usage),
	}, nil
}

func decodeBlocks(blocks []contentBlock) ([]protocol.Part, error) {
	parts := make([]protocol.Part, 0, len(blocks))
	for i, b := range blocks {
		switch {
		case b.Type == "text":
			t := protocol.Text{Text: b.Text}
			for _, c := range b.Citations {
				t.Citations = append(t.Citations, mapCitation(c))
			}
			parts = append(parts, t)
		case b.Type == "thinking":
			parts = append(parts, protocol.Thinking{Text: b.Thinking, Signature: b.Signature, Origin: protocol.AnthropicMessages})
		case b.Type == "redacted_thinking":
			parts = append(parts, protocol.Thinking{Redacted: true, Signature: b.Data, Origin: protocol.AnthropicMessages})
		case b.Type == "tool_use":
			args, err := compactArgs(b.Input)
			if err != nil {
				return nil, fmt.Errorf("anthropic-messages: block %d: %w", i, err)
			}
			parts = append(parts, protocol.ToolCall{ID: b.ID, Name: b.Name, Args: args})
		case b.Type == "server_tool_use":
			args, err := compactArgs(b.Input)
			if err != nil {
				return nil, fmt.Errorf("anthropic-messages: block %d: %w", i, err)
			}
			parts = append(parts, protocol.ServerToolCall{ID: b.ID, Name: b.Name, Args: args, Origin: protocol.AnthropicMessages})
		case strings.HasSuffix(b.Type, resultSuffix):
			parts = append(parts, protocol.ServerToolResult{CallID: b.ToolUseID, Name: b.Type, Content: b.Content, Origin: protocol.AnthropicMessages})
		default:
			return nil, fmt.Errorf("anthropic-messages: block %d has unsupported type %q", i, b.Type)
		}
	}
	return parts, nil
}

// Flattens the char, page, block and web location shapes onto one neutral citation
func mapCitation(c citation) protocol.Citation {
	out := protocol.Citation{Title: c.DocumentTitle, URL: c.URL, Cited: c.CitedText}
	if out.Title == "" {
		out.Title = c.Title
	}
	if c.DocumentIndex != nil {
		out.Document = strconv.Itoa(*c.DocumentIndex)
	}
	switch {
	case c.StartCharIndex != nil:
		out.Start, out.End = *c.StartCharIndex, deref(c.EndCharIndex)
	case c.StartPageNumber != nil:
		out.Start, out.End = *c.StartPageNumber, deref(c.EndPageNumber)
	case c.StartBlockIndex != nil:
		out.Start, out.End = *c.StartBlockIndex, deref(c.EndBlockIndex)
	}
	return out
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// Re-marshals a tool_use input object without whitespace, treating absence as empty
func compactArgs(input json.RawMessage) (json.RawMessage, error) {
	if len(input) == 0 {
		return json.RawMessage("{}"), nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, input); err != nil {
		return nil, err
	}
	return protocol.ParseArgs(buf.String())
}

func mapStop(raw string) (protocol.StopReason, error) {
	switch raw {
	case "end_turn", "stop_sequence":
		return protocol.StopEnd, nil
	case "max_tokens", "model_context_window_exceeded":
		return protocol.StopLength, nil
	case "tool_use":
		return protocol.StopToolUse, nil
	case "refusal":
		return protocol.StopContentFilter, nil
	case "":
		return "", fmt.Errorf("anthropic-messages: missing stop_reason")
	}
	return protocol.StopOther, nil
}

// Records the matched stop sequence beside its reason so callers can tell which one fired
func rawStop(reason, sequence string) string {
	if sequence != "" {
		return "stop_sequence:" + sequence
	}
	return reason
}

func mapUsage(u usage) protocol.Usage {
	out := protocol.Usage{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: u.CacheCreationInputTokens,
	}
	if u.OutputTokensDetails != nil {
		out.Reasoning = u.OutputTokensDetails.ThinkingTokens
	}
	return out
}

// Overlays the cumulative counts of a message_delta onto the message_start usage
func mergeUsage(base *usage, delta usage) {
	base.OutputTokens = delta.OutputTokens
	if delta.InputTokens != 0 {
		base.InputTokens = delta.InputTokens
	}
	if delta.CacheCreationInputTokens != 0 {
		base.CacheCreationInputTokens = delta.CacheCreationInputTokens
	}
	if delta.CacheReadInputTokens != 0 {
		base.CacheReadInputTokens = delta.CacheReadInputTokens
	}
	if delta.OutputTokensDetails != nil {
		base.OutputTokensDetails = delta.OutputTokensDetails
	}
}

func (Protocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		b := protocol.Builder{Origin: protocol.AnthropicMessages}
		var total usage
		var stop, sequence string
		results := map[string]bool{}
		for ev, err := range sse.Read(r) {
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			var e streamEvent
			if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
				yield(protocol.Event{}, fmt.Errorf("anthropic-messages: event %q: %w", ev.Name, err))
				return
			}
			switch e.Type {
			case "message_start":
				if e.Message == nil {
					yield(protocol.Event{}, fmt.Errorf("anthropic-messages: message_start without message"))
					return
				}
				b.Start(e.Message.ID, e.Message.Model)
				total = e.Message.Usage
				b.Usage(mapUsage(total))
			case "content_block_start":
				err = openBlock(&b, strconv.Itoa(e.Index), e.ContentBlock, results)
			case "content_block_delta":
				err = applyDelta(&b, strconv.Itoa(e.Index), e.Delta)
			case "content_block_stop":
				key := strconv.Itoa(e.Index)
				if results[key] {
					delete(results, key)
				} else {
					err = b.Close(key)
				}
			case "message_delta":
				if e.Delta != nil {
					if e.Delta.StopReason != "" {
						stop = e.Delta.StopReason
					}
					if e.Delta.StopSequence != "" {
						sequence = e.Delta.StopSequence
					}
				}
				if e.Usage != nil {
					mergeUsage(&total, *e.Usage)
				}
			case "message_stop":
				reason, err := mapStop(stop)
				if err == nil {
					err = b.Finish(reason, rawStop(stop, sequence), mapUsage(total))
				}
				if err != nil {
					yield(protocol.Event{}, err)
					return
				}
				for _, out := range b.Events() {
					if !yield(out, nil) {
						return
					}
				}
				return
			case "error":
				msg := errorObject{}
				if e.Error != nil {
					msg = *e.Error
				}
				yield(protocol.Event{}, protocol.Classify(&protocol.Error{Status: 200, Type: msg.Type, Message: msg.Message, Transient: transientTypes[msg.Type]}))
				return
			}
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			for _, out := range b.Events() {
				if !yield(out, nil) {
					return
				}
			}
		}
		yield(protocol.Event{}, fmt.Errorf("anthropic-messages: stream ended before message_stop"))
	}
}

// Opens the builder block matching a content_block_start payload, adding result blocks whole
func openBlock(b *protocol.Builder, key string, cb *contentBlock, results map[string]bool) error {
	if cb == nil {
		return fmt.Errorf("anthropic-messages: content_block_start %s without content_block", key)
	}
	switch {
	case cb.Type == "text":
		b.OpenText(key)
		if cb.Text != "" {
			return b.Delta(key, cb.Text)
		}
	case cb.Type == "thinking":
		b.OpenThinking(key, false)
		if cb.Thinking != "" {
			return b.Delta(key, cb.Thinking)
		}
	case cb.Type == "redacted_thinking":
		b.OpenThinking(key, true)
		return b.Signature(key, cb.Data)
	case cb.Type == "tool_use":
		b.OpenToolCall(key, cb.ID, cb.Name)
	case cb.Type == "server_tool_use":
		b.OpenServerToolCall(key, cb.ID, cb.Name)
	case strings.HasSuffix(cb.Type, resultSuffix):
		results[key] = true
		b.AddServerToolResult(protocol.ServerToolResult{CallID: cb.ToolUseID, Name: cb.Type, Content: cb.Content})
	default:
		return fmt.Errorf("anthropic-messages: block %s has unsupported type %q", key, cb.Type)
	}
	return nil
}

// Feeds a content_block_delta payload to the builder block under key
func applyDelta(b *protocol.Builder, key string, d *delta) error {
	if d == nil {
		return fmt.Errorf("anthropic-messages: content_block_delta %s without delta", key)
	}
	switch d.Type {
	case "text_delta":
		return b.Delta(key, d.Text)
	case "thinking_delta":
		return b.Delta(key, d.Thinking)
	case "input_json_delta":
		return b.Delta(key, d.PartialJSON)
	case "signature_delta":
		return b.Signature(key, d.Signature)
	case "citations_delta":
		if d.Citation == nil {
			return fmt.Errorf("anthropic-messages: citations_delta %s without citation", key)
		}
		return b.Cite(key, mapCitation(*d.Citation))
	}
	return fmt.Errorf("anthropic-messages: block %s has unsupported delta type %q", key, d.Type)
}

func (Protocol) DecodeError(status int, body []byte) *protocol.Error {
	e := &protocol.Error{Status: status}
	var er errorResponse
	if err := json.Unmarshal(body, &er); err != nil || er.Error.Type == "" && er.Error.Message == "" {
		e.Message = strings.TrimSpace(string(body))
	} else {
		e.Type, e.Message = er.Error.Type, er.Error.Message
	}
	e.Transient = transientStatus(status) || transientTypes[e.Type]
	return protocol.Classify(e)
}

// Reports statuses that denote a timeout, conflict, throttle or server failure
func transientStatus(status int) bool {
	return status == 408 || status == 409 || status == 429 || status >= 500
}
