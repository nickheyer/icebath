// Ollama native chat wire protocol, POST /api/chat relative to the Ollama host such as http://localhost:11434
package ollamachat

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
)

const path = "/api/chat"

type Protocol struct{}

var _ protocol.Protocol = Protocol{}

func init() {
	protocol.Register(Protocol{})
}

func (Protocol) ID() protocol.ID { return protocol.OllamaChat }

type request struct {
	Model    string         `json:"model"`
	Messages []message      `json:"messages"`
	Tools    []tool         `json:"tools,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
	Stream   bool           `json:"stream"`
}

// Chat message for every role, images and tool calls riding beside the content string
type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Thinking   string     `json:"thinking,omitempty"`
	Images     []string   `json:"images,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string           `json:"id,omitempty"`
	Index    int              `json:"index,omitempty"`
	Function toolCallFunction `json:"function"`
}

// Arguments travel as a JSON object, or as string fragments from servers that stream them
type toolCallFunction struct {
	Index     int             `json:"index,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type tool struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ChatResponse, one per NDJSON line when streaming, with the error line sharing the shape
type response struct {
	Model              string  `json:"model"`
	Message            message `json:"message"`
	Done               bool    `json:"done"`
	DoneReason         string  `json:"done_reason"`
	PromptEvalCount    int     `json:"prompt_eval_count"`
	EvalCount          int     `json:"eval_count"`
	TotalDuration      int64   `json:"total_duration"`
	LoadDuration       int64   `json:"load_duration"`
	PromptEvalDuration int64   `json:"prompt_eval_duration"`
	EvalDuration       int64   `json:"eval_duration"`
	Error              string  `json:"error"`
}

type errorBody struct {
	Error string `json:"error"`
}

const errorPrefix = "[error] "

// Builds POST /api/chat with a leading system message and sampling options under options
func (Protocol) Encode(req protocol.Request) (protocol.Call, error) {
	if req.Model == "" {
		return protocol.Call{}, fmt.Errorf("ollama-chat: model is required")
	}
	wire := request{Model: req.Model, Messages: []message{}, Stream: req.Stream}
	if req.System != "" || req.KeepSystem {
		wire.Messages = append(wire.Messages, message{Role: "system", Content: req.System})
	}
	for i, m := range req.Messages {
		msgs, err := encodeMessage(m)
		if err != nil {
			return protocol.Call{}, fmt.Errorf("ollama-chat: message %d: %w", i, err)
		}
		wire.Messages = append(wire.Messages, msgs...)
	}
	for _, t := range req.Tools {
		if len(t.Parameters) == 0 {
			return protocol.Call{}, fmt.Errorf("ollama-chat: tool %q has no parameters schema", t.Name)
		}
		wire.Tools = append(wire.Tools, tool{Type: "function", Function: toolFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters}})
	}
	switch req.ToolChoice.Mode {
	case "", protocol.ToolChoiceAuto:
	case protocol.ToolChoiceNone, protocol.ToolChoiceRequired, protocol.ToolChoiceTool:
		return protocol.Call{}, fmt.Errorf("ollama-chat: the wire has no tool choice, cannot force %q", req.ToolChoice.Mode)
	default:
		return protocol.Call{}, fmt.Errorf("ollama-chat: unknown tool choice mode %q", req.ToolChoice.Mode)
	}
	wire.Options = encodeOptions(req)
	body, err := protocol.MarshalBody(wire, req.Extra)
	if err != nil {
		return protocol.Call{}, err
	}
	header := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}
	if req.Stream {
		header.Set("Accept", "application/x-ndjson")
	}
	return protocol.Call{Method: http.MethodPost, Path: path, Header: header, Body: body}, nil
}

func encodeOptions(req protocol.Request) map[string]any {
	opts := map[string]any{}
	if req.MaxTokens > 0 {
		opts["num_predict"] = req.MaxTokens
	}
	if req.Temperature != nil {
		opts["temperature"] = *req.Temperature
	}
	if len(req.StopSequences) > 0 {
		opts["stop"] = req.StopSequences
	}
	return opts
}

func encodeMessage(m protocol.Message) ([]message, error) {
	if len(m.Content) == 0 {
		return nil, fmt.Errorf("empty %s message", m.Role)
	}
	switch m.Role {
	case protocol.RoleUser:
		msg, err := encodeUser(m.Content)
		return []message{msg}, err
	case protocol.RoleAssistant:
		msg, err := encodeAssistant(m.Content)
		return []message{msg}, err
	case protocol.RoleTool:
		return encodeToolResults(m.Content)
	}
	return nil, fmt.Errorf("unknown role %q", m.Role)
}

// Joins text parts with newlines because a message carries one content string
func encodeUser(parts []protocol.Part) (message, error) {
	msg := message{Role: "user"}
	var texts []string
	for _, p := range parts {
		switch v := p.(type) {
		case protocol.Text:
			texts = append(texts, v.Text)
		case protocol.Image:
			img, err := encodeImage(v)
			if err != nil {
				return message{}, err
			}
			msg.Images = append(msg.Images, img)
		case protocol.Document:
			return message{}, fmt.Errorf("the wire has no document part for %q", v.Name)
		default:
			return message{}, fmt.Errorf("user message cannot carry %T", p)
		}
	}
	msg.Content = strings.Join(texts, "\n")
	return msg, nil
}

// Replays text, replayable thinking and tool calls, sending an empty turn when policy drops everything
func encodeAssistant(parts []protocol.Part) (message, error) {
	msg := message{Role: "assistant"}
	var texts, thoughts []string
	for _, p := range parts {
		switch v := p.(type) {
		case protocol.Text:
			texts = append(texts, v.Text)
		case protocol.Thinking:
			if v.Redacted || !protocol.Replayable(v.Origin, protocol.OllamaChat) {
				continue
			}
			thoughts = append(thoughts, v.Text)
		case protocol.ToolCall:
			args, err := protocol.ParseArgs(string(v.Args))
			if err != nil {
				return message{}, fmt.Errorf("tool call %s: %w", v.Name, err)
			}
			msg.ToolCalls = append(msg.ToolCalls, toolCall{ID: v.ID, Function: toolCallFunction{Name: v.Name, Arguments: args}})
		case protocol.ServerToolCall, protocol.ServerToolResult:
		default:
			return message{}, fmt.Errorf("assistant message cannot carry %T", p)
		}
	}
	msg.Content = strings.Join(texts, "\n")
	msg.Thinking = strings.Join(thoughts, "\n")
	return msg, nil
}

// Emits one tool message per result, linked by tool_call_id and tool_name and flagged on failure
func encodeToolResults(parts []protocol.Part) ([]message, error) {
	var out []message
	for _, p := range parts {
		r, ok := p.(protocol.ToolResult)
		if !ok {
			return nil, fmt.Errorf("tool message cannot carry %T", p)
		}
		if r.CallID == "" && r.Name == "" {
			return nil, fmt.Errorf("tool result has neither a call id nor a tool name")
		}
		msg := message{Role: "tool", ToolName: r.Name, ToolCallID: r.CallID}
		var texts []string
		for _, c := range r.Content {
			switch v := c.(type) {
			case protocol.Text:
				texts = append(texts, v.Text)
			case protocol.Image:
				img, err := encodeImage(v)
				if err != nil {
					return nil, fmt.Errorf("tool result %s: %w", r.CallID, err)
				}
				msg.Images = append(msg.Images, img)
			default:
				return nil, fmt.Errorf("tool result %s cannot carry %T", r.CallID, c)
			}
		}
		msg.Content = strings.Join(texts, "\n")
		if r.IsError {
			msg.Content = errorPrefix + msg.Content
		}
		out = append(out, msg)
	}
	return out, nil
}

// Checks the media type names an image before sending the bare base64 the wire expects
func encodeImage(img protocol.Image) (string, error) {
	if !strings.HasPrefix(img.MediaType, "image/") {
		return "", fmt.Errorf("image media type %q is not an image type", img.MediaType)
	}
	if len(img.Data) == 0 {
		return "", fmt.Errorf("image has no data")
	}
	return base64.StdEncoding.EncodeToString(img.Data), nil
}

var transientHints = []string{"timeout", "overloaded", "too many"}

// Reads the single ChatResponse of a non streaming call
func (Protocol) Decode(r io.Reader) (protocol.Response, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return protocol.Response{}, err
	}
	var wire response
	if err := json.Unmarshal(body, &wire); err != nil {
		return protocol.Response{}, fmt.Errorf("ollama-chat: response: %w", err)
	}
	if wire.Error != "" {
		return protocol.Response{}, &protocol.Error{Status: 200, Message: wire.Error, Transient: transientMessage(wire.Error)}
	}
	if !wire.Done {
		return protocol.Response{}, fmt.Errorf("ollama-chat: response is not done")
	}
	out := protocol.Response{Model: wire.Model, Usage: usage(wire)}
	if wire.Message.Thinking != "" {
		out.Content = append(out.Content, protocol.Thinking{Text: wire.Message.Thinking, Origin: protocol.OllamaChat})
	}
	if wire.Message.Content != "" {
		out.Content = append(out.Content, protocol.Text{Text: wire.Message.Content})
	}
	for _, tc := range wire.Message.ToolCalls {
		args, err := completeArgs(tc.Function.Arguments)
		if err != nil {
			return protocol.Response{}, fmt.Errorf("ollama-chat: tool call %s: %w", tc.Function.Name, err)
		}
		out.Content = append(out.Content, protocol.ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: args})
	}
	out.StopReason, out.RawStopReason = stopReason(wire.DoneReason, len(wire.Message.ToolCalls) > 0)
	return out, nil
}

// Maps done_reason, which the server leaves empty on a natural stop, onto the neutral set
func stopReason(reason string, hasCalls bool) (protocol.StopReason, string) {
	switch reason {
	case "", "stop":
		if hasCalls {
			return protocol.StopToolUse, reason
		}
		return protocol.StopEnd, reason
	case "length":
		return protocol.StopLength, reason
	}
	return protocol.StopOther, reason
}

func usage(wire response) protocol.Usage {
	return protocol.Usage{Input: wire.PromptEvalCount, Output: wire.EvalCount}
}

// Maps the nanosecond duration fields onto the neutral timings

// Reports whether an error message describes a timeout or an overloaded server
func transientMessage(msg string) bool {
	lower := strings.ToLower(msg)
	for _, hint := range transientHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// Validates complete arguments arriving as an object or as a JSON encoded string
func completeArgs(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return protocol.ParseArgs("")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return protocol.ParseArgs(s)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, err
	}
	return protocol.ParseArgs(buf.String())
}

// Tool call whose arguments may arrive as string fragments or object fragments across lines
type pendingCall struct {
	key     string
	name    string
	text    bool
	objects bool
	fields  map[string]json.RawMessage
}

// Tracks the block receiving fragments and the tool call accumulating by index across NDJSON lines
type streamState struct {
	b        protocol.Builder
	open     string
	kind     string
	n        int
	hasCalls bool
	calls    map[int]*pendingCall
	current  int
}

// Yields neutral events from NDJSON lines until the line with done set
func (Protocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		st := &streamState{b: protocol.Builder{Origin: protocol.OllamaChat}, calls: map[int]*pendingCall{}, current: -1}
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadString('\n')
			if err != nil && err != io.EOF {
				yield(protocol.Event{}, err)
				return
			}
			line = strings.TrimSpace(line)
			if line != "" {
				done, perr := st.apply(line)
				if perr != nil {
					yield(protocol.Event{}, perr)
					return
				}
				for _, ev := range st.b.Events() {
					if !yield(ev, nil) {
						return
					}
				}
				if done {
					return
				}
			}
			if err == io.EOF {
				yield(protocol.Event{}, fmt.Errorf("ollama-chat: stream ended before a done line"))
				return
			}
		}
	}
}

func (st *streamState) apply(line string) (bool, error) {
	var chunk response
	if err := json.Unmarshal([]byte(line), &chunk); err != nil {
		return false, fmt.Errorf("ollama-chat: stream line: %w", err)
	}
	if chunk.Error != "" {
		return false, &protocol.Error{Status: 200, Message: chunk.Error, Transient: transientMessage(chunk.Error)}
	}
	st.b.Start("", chunk.Model)
	if chunk.Message.Thinking != "" {
		if err := st.fragment("thinking", chunk.Message.Thinking); err != nil {
			return false, err
		}
	}
	if chunk.Message.Content != "" {
		if err := st.fragment("text", chunk.Message.Content); err != nil {
			return false, err
		}
	}
	for _, tc := range chunk.Message.ToolCalls {
		if err := st.toolCall(tc); err != nil {
			return false, err
		}
	}
	if !chunk.Done {
		return false, nil
	}
	if err := st.closeOpen(); err != nil {
		return false, err
	}
	if err := st.closeCall(); err != nil {
		return false, err
	}
	stop, raw := stopReason(chunk.DoneReason, st.hasCalls)
	return true, st.b.Finish(stop, raw, usage(chunk))
}

// Appends to the open block of kind, closing a block of another kind or a pending call first
func (st *streamState) fragment(kind, s string) error {
	if err := st.closeCall(); err != nil {
		return err
	}
	if st.open == "" || st.kind != kind {
		if err := st.closeOpen(); err != nil {
			return err
		}
		st.n++
		st.open, st.kind = kind+"-"+strconv.Itoa(st.n), kind
		if kind == "thinking" {
			st.b.OpenThinking(st.open, false)
		} else {
			st.b.OpenText(st.open)
		}
	}
	return st.b.Delta(st.open, s)
}

// Accumulates a tool call by index, closing the previous index or a renamed call at the same index
func (st *streamState) toolCall(tc toolCall) error {
	if err := st.closeOpen(); err != nil {
		return err
	}
	idx := tc.Function.Index
	if idx == 0 {
		idx = tc.Index
	}
	pc := st.calls[idx]
	if pc != nil && tc.Function.Name != "" && pc.name != tc.Function.Name {
		if err := st.closeCall(); err != nil {
			return err
		}
		pc = nil
	}
	if pc == nil {
		if err := st.closeCall(); err != nil {
			return err
		}
		st.n++
		pc = &pendingCall{key: "call-" + strconv.Itoa(st.n), name: tc.Function.Name, fields: map[string]json.RawMessage{}}
		st.calls[idx] = pc
		st.current = idx
		st.hasCalls = true
		st.b.OpenToolCall(pc.key, tc.ID, tc.Function.Name)
	}
	return st.arguments(pc, tc.Function.Arguments)
}

// Appends string fragments as deltas and merges object fragments for delivery at close
func (st *streamState) arguments(pc *pendingCall, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if pc.objects {
			return fmt.Errorf("ollama-chat: tool call %s mixes string and object arguments", pc.name)
		}
		pc.text = true
		return st.b.Delta(pc.key, s)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return fmt.Errorf("ollama-chat: tool call %s: arguments are not a JSON object", pc.name)
	}
	if pc.text {
		return fmt.Errorf("ollama-chat: tool call %s mixes string and object arguments", pc.name)
	}
	pc.objects = true
	for k, v := range obj {
		pc.fields[k] = v
	}
	return nil
}

// Finalizes the call at the current index, delivering merged object arguments as one delta
func (st *streamState) closeCall() error {
	pc, ok := st.calls[st.current]
	if !ok {
		return nil
	}
	delete(st.calls, st.current)
	st.current = -1
	if pc.objects {
		raw, err := json.Marshal(pc.fields)
		if err != nil {
			return fmt.Errorf("ollama-chat: tool call %s: %w", pc.name, err)
		}
		if err := st.b.Delta(pc.key, string(raw)); err != nil {
			return err
		}
	}
	return st.b.Close(pc.key)
}

func (st *streamState) closeOpen() error {
	if st.open == "" {
		return nil
	}
	key := st.open
	st.open, st.kind = "", ""
	return st.b.Close(key)
}

// Parses the error string envelope, otherwise reporting the trimmed body text
func (Protocol) DecodeError(status int, body []byte) *protocol.Error {
	e := &protocol.Error{Status: status, Transient: transientStatus(status)}
	var wire errorBody
	if err := json.Unmarshal(body, &wire); err == nil && wire.Error != "" {
		e.Message = wire.Error
	} else {
		e.Message = strings.TrimSpace(string(body))
	}
	e.Transient = e.Transient || transientMessage(e.Message)
	return e
}

// Reports statuses that denote a timeout, conflict, throttle or server failure
func transientStatus(status int) bool {
	return status == 408 || status == 409 || status == 429 || status >= 500
}
