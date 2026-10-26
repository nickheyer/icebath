// OpenAI Chat Completions wire protocol with paths relative to a base URL ending in /v1
package openaicompletions

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

type Protocol struct{}

var _ protocol.Protocol = Protocol{}

func init() {
	protocol.Register(Protocol{})
}

func (Protocol) ID() protocol.ID { return protocol.OpenAICompletions }

type request struct {
	Model               string         `json:"model"`
	Messages            []message      `json:"messages"`
	Tools               []tool         `json:"tools,omitempty"`
	ToolChoice          any            `json:"tool_choice,omitempty"`
	MaxCompletionTokens int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64       `json:"temperature,omitempty"`
	Stop                []string       `json:"stop,omitempty"`
	Stream              bool           `json:"stream,omitempty"`
	StreamOptions       *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Request message whose content is a string, a part list, or absent beside tool calls
type message struct {
	Role             string     `json:"role"`
	Content          any        `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type tool struct {
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type namedToolChoice struct {
	Type     string        `json:"type"`
	Function namedFunction `json:"function"`
}

type namedFunction struct {
	Name string `json:"name"`
}

type toolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function toolCallFunction `json:"function"`
}

// Arguments travel as a JSON encoded string rather than an object
type toolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type response struct {
	ID      string   `json:"id"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   *usage   `json:"usage"`
}

type choice struct {
	Index        int             `json:"index"`
	Message      responseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

// Assistant message with the two reasoning field names compatible vendors use
type responseMessage struct {
	Content          *string    `json:"content"`
	Refusal          *string    `json:"refusal"`
	ReasoningContent *string    `json:"reasoning_content"`
	Reasoning        *string    `json:"reasoning"`
	ToolCalls        []toolCall `json:"tool_calls"`
}

type usage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	PromptTokensDetails     *promptTokensDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails *completionTokensDetails `json:"completion_tokens_details"`
}

type promptTokensDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type completionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// Streamed frame, either a completion chunk or an error object
type chunk struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	Usage   *usage         `json:"usage"`
	Error   *apiError      `json:"error"`
}

type streamChoice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type delta struct {
	Content          *string         `json:"content"`
	Refusal          *string         `json:"refusal"`
	ReasoningContent *string         `json:"reasoning_content"`
	Reasoning        *string         `json:"reasoning"`
	ToolCalls        []toolCallChunk `json:"tool_calls"`
}

// Fragment of one tool call, keyed by index with every other field optional
type toolCallChunk struct {
	Index    int               `json:"index"`
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Function *toolCallFunction `json:"function"`
}

type errorResponse struct {
	Error *apiError `json:"error"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

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
	if req.System != "" || req.KeepSystem {
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

const (
	keyContent   = "content"
	keyRefusal   = "refusal"
	keyReasoning = "reasoning"
	doneSentinel = "[DONE]"
)

var transientTypes = map[string]bool{
	"server_error":        true,
	"rate_limit_exceeded": true,
	"slow_down":           true,
	"overloaded":          true,
}

// Reads a chat.completion object and maps its first choice to the neutral response
func (Protocol) Decode(r io.Reader) (protocol.Response, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return protocol.Response{}, err
	}
	var wire response
	if err := json.Unmarshal(body, &wire); err != nil {
		return protocol.Response{}, fmt.Errorf("openai-completions: response: %w", err)
	}
	if len(wire.Choices) == 0 {
		return protocol.Response{}, fmt.Errorf("openai-completions: response has no choices")
	}
	c := wire.Choices[0]
	out := protocol.Response{ID: wire.ID, Model: wire.Model, RawStopReason: c.FinishReason, StopReason: stopReason(c.FinishReason, len(c.Message.ToolCalls) > 0)}
	if thought := reasoningText(c.Message.ReasoningContent, c.Message.Reasoning); thought != "" {
		out.Content = append(out.Content, protocol.Thinking{Text: thought, Origin: protocol.OpenAICompletions})
	}
	if c.Message.Content != nil && *c.Message.Content != "" {
		out.Content = append(out.Content, protocol.Text{Text: *c.Message.Content})
	}
	if c.Message.Refusal != nil && *c.Message.Refusal != "" {
		out.Content = append(out.Content, protocol.Text{Text: *c.Message.Refusal})
	}
	for _, tc := range c.Message.ToolCalls {
		if tc.Type != "function" {
			return protocol.Response{}, fmt.Errorf("openai-completions: tool call %s has type %q", tc.ID, tc.Type)
		}
		args, err := protocol.ParseArgs(tc.Function.Arguments)
		if err != nil {
			return protocol.Response{}, fmt.Errorf("openai-completions: tool call %s: %w", tc.ID, err)
		}
		out.Content = append(out.Content, protocol.ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: args})
	}
	if wire.Usage != nil {
		out.Usage = mapUsage(*wire.Usage)
	}
	return out, nil
}

// Picks the reasoning text from whichever of the two vendor field names is populated
func reasoningText(content, reasoning *string) string {
	if content != nil && *content != "" {
		return *content
	}
	if reasoning != nil {
		return *reasoning
	}
	return ""
}

// Maps finish_reason onto the neutral set, inferring from tool calls when the wire sent none
func stopReason(finish string, hasCalls bool) protocol.StopReason {
	switch finish {
	case "stop":
		return protocol.StopEnd
	case "length":
		return protocol.StopLength
	case "tool_calls", "function_call":
		return protocol.StopToolUse
	case "content_filter":
		return protocol.StopContentFilter
	case "":
		if hasCalls {
			return protocol.StopToolUse
		}
		return protocol.StopEnd
	}
	return protocol.StopOther
}

// Takes cached tokens out of the prompt count so Input holds only the uncached share
func mapUsage(u usage) protocol.Usage {
	out := protocol.Usage{Input: u.PromptTokens, Output: u.CompletionTokens}
	if u.PromptTokensDetails != nil {
		out.CacheRead = u.PromptTokensDetails.CachedTokens
		out.CacheWrite = u.PromptTokensDetails.CacheWriteTokens
		out.Input -= out.CacheRead
	}
	if u.CompletionTokensDetails != nil {
		out.Reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	return out
}

// Pending tool call whose block opens once both id and name have arrived
type pendingCall struct {
	key  string
	id   string
	name string
	args strings.Builder
	open bool
}

// Yields neutral events from chat.completion.chunk frames until [DONE] or EOF after finish_reason
func (Protocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		b := protocol.Builder{Origin: protocol.OpenAICompletions}
		var usage protocol.Usage
		var finish string
		finished, hasCalls := false, false
		calls := map[int]*pendingCall{}
		current := -1
		flush := func() bool {
			for _, ev := range b.Events() {
				if !yield(ev, nil) {
					return false
				}
			}
			return true
		}
		for ev, err := range sse.Read(r) {
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			if ev.Data == doneSentinel {
				finished = true
				break
			}
			var c chunk
			if err := json.Unmarshal([]byte(ev.Data), &c); err != nil {
				yield(protocol.Event{}, fmt.Errorf("openai-completions: chunk: %w", err))
				return
			}
			if c.Error != nil {
				yield(protocol.Event{}, streamError(*c.Error, []byte(ev.Data)))
				return
			}
			b.Start(c.ID, c.Model)
			if c.Usage != nil {
				usage = mapUsage(*c.Usage)
			}
			for _, ch := range c.Choices {
				if ch.Index != 0 {
					yield(protocol.Event{}, fmt.Errorf("openai-completions: unexpected choice index %d", ch.Index))
					return
				}
				hasCalls = hasCalls || len(ch.Delta.ToolCalls) > 0
				if err := applyDelta(&b, ch.Delta, calls, &current); err != nil {
					yield(protocol.Event{}, err)
					return
				}
				if ch.FinishReason != nil && *ch.FinishReason != "" {
					finish = *ch.FinishReason
					finished = true
					if err := closeCall(&b, calls, current); err != nil {
						yield(protocol.Event{}, err)
						return
					}
					current = -1
				}
			}
			if !flush() {
				return
			}
		}
		if !finished {
			yield(protocol.Event{}, fmt.Errorf("openai-completions: stream ended before finish_reason"))
			return
		}
		if err := closeCall(&b, calls, current); err != nil {
			yield(protocol.Event{}, err)
			return
		}
		if err := b.Finish(stopReason(finish, hasCalls), finish, usage); err != nil {
			yield(protocol.Event{}, err)
			return
		}
		flush()
	}
}

func streamError(e apiError, body []byte) *protocol.Error {
	return &protocol.Error{Status: 200, Type: errType(e), Message: e.Message, Transient: transientTypes[e.Type] || transientTypes[e.Code]}
}

// Routes reasoning, content, refusal and tool call fragments into builder blocks following accumulate_delta
func applyDelta(b *protocol.Builder, d delta, calls map[int]*pendingCall, current *int) error {
	if thought := reasoningText(d.ReasoningContent, d.Reasoning); thought != "" {
		if !b.IsOpen(keyReasoning) {
			b.OpenThinking(keyReasoning, false)
		}
		if err := b.Delta(keyReasoning, thought); err != nil {
			return err
		}
	}
	for _, f := range []struct {
		key  string
		text *string
	}{{keyContent, d.Content}, {keyRefusal, d.Refusal}} {
		key, text := f.key, f.text
		if text == nil || *text == "" {
			continue
		}
		if b.IsOpen(keyReasoning) {
			if err := b.Close(keyReasoning); err != nil {
				return err
			}
		}
		if !b.IsOpen(key) {
			b.OpenText(key)
		}
		if err := b.Delta(key, *text); err != nil {
			return err
		}
	}
	for _, tc := range d.ToolCalls {
		if tc.Type != "" && tc.Type != "function" {
			return fmt.Errorf("openai-completions: tool call at index %d has type %q", tc.Index, tc.Type)
		}
		if *current != tc.Index {
			for _, k := range []string{keyReasoning, keyContent, keyRefusal} {
				if b.IsOpen(k) {
					if err := b.Close(k); err != nil {
						return err
					}
				}
			}
			if err := closeCall(b, calls, *current); err != nil {
				return err
			}
			*current = tc.Index
		}
		pc, ok := calls[tc.Index]
		if !ok {
			pc = &pendingCall{key: "tool:" + strconv.Itoa(tc.Index)}
			calls[tc.Index] = pc
		}
		pc.id += tc.ID
		if tc.Function != nil {
			pc.name += tc.Function.Name
			pc.args.WriteString(tc.Function.Arguments)
		}
		if !pc.open && pc.id != "" && pc.name != "" {
			if err := openCall(b, pc); err != nil {
				return err
			}
			continue
		}
		if pc.open && tc.Function != nil && tc.Function.Arguments != "" {
			if err := b.Delta(pc.key, tc.Function.Arguments); err != nil {
				return err
			}
		}
	}
	return nil
}

func openCall(b *protocol.Builder, pc *pendingCall) error {
	b.OpenToolCall(pc.key, pc.id, pc.name)
	pc.open = true
	if pc.args.Len() == 0 {
		return nil
	}
	return b.Delta(pc.key, pc.args.String())
}

// Finalizes the tool call at index, opening it first when identity never fully arrived
func closeCall(b *protocol.Builder, calls map[int]*pendingCall, index int) error {
	pc, ok := calls[index]
	if !ok {
		return nil
	}
	if !pc.open {
		if err := openCall(b, pc); err != nil {
			return err
		}
	}
	delete(calls, index)
	return b.Close(pc.key)
}

// Parses the error object envelope, otherwise reporting the trimmed body text
func (Protocol) DecodeError(status int, body []byte) *protocol.Error {
	e := &protocol.Error{Status: status, Transient: transientStatus(status)}
	var wire errorResponse
	if err := json.Unmarshal(body, &wire); err == nil && wire.Error != nil && wire.Error.Message != "" {
		e.Type, e.Message = errType(*wire.Error), wire.Error.Message
		e.Transient = e.Transient || transientTypes[wire.Error.Type] || transientTypes[wire.Error.Code]
		return e
	}
	e.Message = string(bytes.TrimSpace(body))
	return e
}

func errType(e apiError) string {
	if e.Type != "" {
		return e.Type
	}
	return e.Code
}

// Reports statuses that denote a timeout, conflict, throttle or server failure
func transientStatus(status int) bool {
	return status == 408 || status == 409 || status == 429 || status >= 500
}
