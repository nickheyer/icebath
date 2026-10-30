// Mistral Conversations API, POST /v1/conversations under the api.mistral.ai base URL
package mistralconversations

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
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/sse"
	"github.com/nickheyer/icebath/pkg/protocol"
)

const (
	path       = "/v1/conversations"
	objectName = "entry"
)

type Protocol struct{}

var _ protocol.Protocol = Protocol{}

func init() {
	protocol.Register(Protocol{})
}

func (Protocol) ID() protocol.ID { return protocol.MistralConversations }

type request struct {
	Model          string          `json:"model"`
	Instructions   string          `json:"instructions,omitempty"`
	Inputs         []any           `json:"inputs"`
	Tools          []functionTool  `json:"tools,omitempty"`
	CompletionArgs *completionArgs `json:"completion_args,omitempty"`
	Stream         bool            `json:"stream"`
	Store          bool            `json:"store"`
}

type messageEntry struct {
	Object  string `json:"object"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type functionCallEntry struct {
	Object     string `json:"object"`
	Type       string `json:"type"`
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Arguments  string `json:"arguments"`
}

type functionResultEntry struct {
	Object     string `json:"object"`
	Type       string `json:"type"`
	ToolCallID string `json:"tool_call_id"`
	Result     string `json:"result"`
}

type textChunk struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type imageChunk struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
}

type thinkChunk struct {
	Type      string      `json:"type"`
	Thinking  []textChunk `json:"thinking"`
	Signature string      `json:"signature,omitempty"`
}

type functionTool struct {
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type completionArgs struct {
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	ToolChoice  string   `json:"tool_choice,omitempty"`
}

type response struct {
	ConversationID string      `json:"conversation_id"`
	Outputs        []wireEntry `json:"outputs"`
	Usage          usage       `json:"usage"`
}

// Output entry of any type, with content and arguments kept raw for their two wire forms
type wireEntry struct {
	Type               string          `json:"type"`
	ID                 string          `json:"id"`
	Model              string          `json:"model"`
	Content            json.RawMessage `json:"content"`
	ToolCallID         string          `json:"tool_call_id"`
	Name               string          `json:"name"`
	Arguments          json.RawMessage `json:"arguments"`
	ConfirmationStatus json.RawMessage `json:"confirmation_status"`
	AgentID            json.RawMessage `json:"agent_id"`
	Info               json.RawMessage `json:"info"`
	PreviousAgentID    string          `json:"previous_agent_id"`
	PreviousAgentName  string          `json:"previous_agent_name"`
	NextAgentID        string          `json:"next_agent_id"`
	NextAgentName      string          `json:"next_agent_name"`
}

type wireChunk struct {
	Type      string      `json:"type"`
	Text      string      `json:"text"`
	Thinking  []wireChunk `json:"thinking"`
	Signature string      `json:"signature"`
	Tool      string      `json:"tool"`
	Title     string      `json:"title"`
	URL       string      `json:"url"`
	FileID    string      `json:"file_id"`
	FileName  string      `json:"file_name"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Data payload of any streaming event
type event struct {
	Type               string          `json:"type"`
	ConversationID     string          `json:"conversation_id"`
	ID                 string          `json:"id"`
	ContentIndex       int             `json:"content_index"`
	Model              string          `json:"model"`
	Content            json.RawMessage `json:"content"`
	Name               string          `json:"name"`
	ToolCallID         string          `json:"tool_call_id"`
	Arguments          string          `json:"arguments"`
	ConfirmationStatus json.RawMessage `json:"confirmation_status"`
	AgentID            json.RawMessage `json:"agent_id"`
	Info               json.RawMessage `json:"info"`
	PreviousAgentID    string          `json:"previous_agent_id"`
	PreviousAgentName  string          `json:"previous_agent_name"`
	NextAgentID        string          `json:"next_agent_id"`
	NextAgentName      string          `json:"next_agent_name"`
	Usage              *usage          `json:"usage"`
	Message            string          `json:"message"`
	Code               int             `json:"code"`
}

// Error body of the object error shape, code arriving as a number or a string
type errorObject struct {
	Object  string          `json:"object"`
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
}

// Error body of the detail shape, a plain string or a validation list
type detailError struct {
	Detail json.RawMessage `json:"detail"`
}

type validationItem struct {
	Loc  []any  `json:"loc"`
	Msg  string `json:"msg"`
	Type string `json:"type"`
}

const (
	errorPrefix       = "[error] "
	typeToolExecution = "tool.execution"
	typeHandoff       = "agent.handoff"
	// Neutral name for agent.handoff entries, which carry no tool name of their own
	nameHandoff = "handoff"
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
		case protocol.Document:
			return nil, fmt.Errorf("the wire has no document chunk for %q", p.Name)
		default:
			return nil, fmt.Errorf("%T is not user content", p)
		}
	}
	return content, nil
}

// Emits a message.output entry per run of text and thinking, a function.call per tool call and replays vendor entries
func encodeAssistant(parts []protocol.Part) ([]any, error) {
	var entries []any
	var pending []any
	executions := map[string]map[string]json.RawMessage{}
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
			if p.Redacted || !replayable(p.Origin) {
				continue
			}
			pending = append(pending, thinkChunk{Type: "thinking", Thinking: []textChunk{{Type: "text", Text: p.Text}}, Signature: p.Signature})
		case protocol.ToolCall:
			args, err := protocol.ParseArgs(string(p.Args))
			if err != nil {
				return nil, fmt.Errorf("tool call %s: %w", p.ID, err)
			}
			flush()
			entries = append(entries, functionCallEntry{Object: objectName, Type: "function.call", ToolCallID: p.ID, Name: p.Name, Arguments: string(args)})
		case protocol.ServerToolCall:
			if !replayable(p.Origin) {
				continue
			}
			entry, err := encodeServerToolCall(p)
			if err != nil {
				return nil, err
			}
			flush()
			entries = append(entries, entry)
			if p.Name != nameHandoff {
				executions[p.ID] = entry
			}
		case protocol.ServerToolResult:
			if entry, ok := executions[p.CallID]; ok && replayable(p.Origin) && len(p.Content) > 0 {
				entry["info"] = p.Content
			}
		default:
			return nil, fmt.Errorf("%T is not assistant content", p)
		}
	}
	flush()
	return entries, nil
}

func replayable(origin protocol.ID) bool {
	return protocol.Replayable(origin, protocol.MistralConversations)
}

// Rebuilds a tool.execution or agent.handoff entry from the fields kept in Args
func encodeServerToolCall(c protocol.ServerToolCall) (map[string]json.RawMessage, error) {
	args, err := protocol.ParseArgs(string(c.Args))
	if err != nil {
		return nil, fmt.Errorf("server tool call %s: %w", c.ID, err)
	}
	entry := map[string]json.RawMessage{}
	if err := json.Unmarshal(args, &entry); err != nil {
		return nil, fmt.Errorf("server tool call %s: %w", c.ID, err)
	}
	entry["object"] = quote(objectName)
	if c.ID != "" {
		entry["id"] = quote(c.ID)
	}
	if c.Name == nameHandoff {
		entry["type"] = quote(typeHandoff)
		return entry, nil
	}
	if c.Name == "" {
		return nil, fmt.Errorf("server tool call %s has no tool name", c.ID)
	}
	entry["type"] = quote(typeToolExecution)
	entry["name"] = quote(c.Name)
	return entry, nil
}

func quote(s string) json.RawMessage {
	raw, _ := json.Marshal(s)
	return raw
}

// Joins text parts with newlines because function.result carries a single string, flagging failures
func encodeToolResults(parts []protocol.Part) ([]any, error) {
	var entries []any
	for _, p := range parts {
		r, ok := p.(protocol.ToolResult)
		if !ok {
			return nil, fmt.Errorf("%T is not a tool result", p)
		}
		var texts []string
		for _, c := range r.Content {
			switch c := c.(type) {
			case protocol.Text:
				texts = append(texts, c.Text)
			case protocol.Image:
				texts = append(texts, fmt.Sprintf("[image %s, %d bytes omitted]", c.MediaType, len(c.Data)))
			default:
				return nil, fmt.Errorf("tool result %s: function.result carries only text, not %T", r.CallID, c)
			}
		}
		result := strings.Join(texts, "\n")
		if r.IsError {
			result = errorPrefix + result
		}
		entries = append(entries, functionResultEntry{Object: objectName, Type: "function.result", ToolCallID: r.CallID, Result: result})
	}
	return entries, nil
}

func encodeImage(img protocol.Image) (imageChunk, error) {
	if img.MediaType == "" {
		return imageChunk{}, fmt.Errorf("image without media type")
	}
	if len(img.Data) == 0 {
		return imageChunk{}, fmt.Errorf("image without data")
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

func (Protocol) Decode(r io.Reader) (protocol.Response, error) {
	var resp response
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
		return protocol.Response{}, fmt.Errorf("mistral-conversations: response: %w", err)
	}
	out := protocol.Response{ID: resp.ConversationID, StopReason: protocol.StopEnd}
	for i, e := range resp.Outputs {
		if out.Model == "" {
			out.Model = e.Model
		}
		var parts []protocol.Part
		var err error
		switch e.Type {
		case "message.output":
			parts, err = decodeContent(e.Content)
		case "function.call":
			var args json.RawMessage
			args, err = decodeArguments(e.Arguments)
			parts = []protocol.Part{protocol.ToolCall{ID: e.ToolCallID, Name: e.Name, Args: args}}
			out.StopReason = protocol.StopToolUse
		case typeToolExecution:
			parts, err = decodeExecution(e)
		case typeHandoff:
			parts = []protocol.Part{protocol.ServerToolCall{ID: e.ID, Name: nameHandoff, Args: handoffArgs(e.PreviousAgentID, e.PreviousAgentName, e.NextAgentID, e.NextAgentName), Origin: protocol.MistralConversations}}
		default:
			err = fmt.Errorf("cannot carry %q entries", e.Type)
		}
		if err != nil {
			return protocol.Response{}, fmt.Errorf("mistral-conversations: output %d: %w", i, err)
		}
		out.Content = append(out.Content, parts...)
	}
	out.Usage = protocol.Usage{Input: resp.Usage.PromptTokens, Output: resp.Usage.CompletionTokens}
	return out, nil
}

// Keeps the arguments, confirmation status and agent of a vendor run tool, plus its info as the result
func decodeExecution(e wireEntry) ([]protocol.Part, error) {
	if e.Name == "" {
		return nil, fmt.Errorf("tool.execution %s has no name", e.ID)
	}
	fields := map[string]json.RawMessage{}
	if present(e.Arguments) {
		fields["arguments"] = e.Arguments
	}
	if present(e.ConfirmationStatus) {
		fields["confirmation_status"] = e.ConfirmationStatus
	}
	if present(e.AgentID) {
		fields["agent_id"] = e.AgentID
	}
	args, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	parts := []protocol.Part{protocol.ServerToolCall{ID: e.ID, Name: e.Name, Args: args, Origin: protocol.MistralConversations}}
	if present(e.Info) {
		var compact bytes.Buffer
		if err := json.Compact(&compact, e.Info); err != nil {
			return nil, fmt.Errorf("tool.execution %s info: %w", e.ID, err)
		}
		parts = append(parts, protocol.ServerToolResult{CallID: e.ID, Name: e.Name, Content: compact.Bytes(), Origin: protocol.MistralConversations})
	}
	return parts, nil
}

func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

func handoffArgs(prevID, prevName, nextID, nextName string) json.RawMessage {
	fields := map[string]json.RawMessage{}
	for key, v := range map[string]string{"previous_agent_id": prevID, "previous_agent_name": prevName, "next_agent_id": nextID, "next_agent_name": nextName} {
		if v != "" {
			fields[key] = quote(v)
		}
	}
	raw, _ := json.Marshal(fields)
	return raw
}

// Turns a content string or chunk array into parts, merging adjacent text and attaching references as citations
func decodeContent(raw json.RawMessage) ([]protocol.Part, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []protocol.Part{protocol.Text{Text: s}}, nil
	}
	var chunks []wireChunk
	if err := json.Unmarshal(raw, &chunks); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	var parts []protocol.Part
	var pending []protocol.Citation
	for _, c := range chunks {
		switch c.Type {
		case "text":
			if n := len(parts); n > 0 {
				if prev, ok := parts[n-1].(protocol.Text); ok {
					prev.Text += c.Text
					parts[n-1] = prev
					continue
				}
			}
			parts = append(parts, protocol.Text{Text: c.Text, Citations: pending})
			pending = nil
		case "thinking":
			th, err := decodeThinking(c)
			if err != nil {
				return nil, err
			}
			parts = append(parts, th)
		case "tool_reference", "tool_file":
			cite := chunkCitation(c)
			if n := len(parts); n > 0 {
				if prev, ok := parts[n-1].(protocol.Text); ok {
					cite.Start = utf8.RuneCountInString(prev.Text)
					cite.End = cite.Start
					prev.Citations = append(prev.Citations, cite)
					parts[n-1] = prev
					continue
				}
			}
			pending = append(pending, cite)
		default:
			return nil, fmt.Errorf("unsupported chunk type %q", c.Type)
		}
	}
	if len(pending) > 0 {
		parts = append(parts, protocol.Text{Citations: pending})
	}
	return parts, nil
}

func decodeThinking(c wireChunk) (protocol.Thinking, error) {
	var text strings.Builder
	for _, inner := range c.Thinking {
		if inner.Type != "text" {
			return protocol.Thinking{}, fmt.Errorf("thinking: unsupported chunk type %q", inner.Type)
		}
		text.WriteString(inner.Text)
	}
	return protocol.Thinking{Text: text.String(), Signature: c.Signature, Origin: protocol.MistralConversations}, nil
}

// Maps a tool_reference to a web citation and a tool_file to a document citation
func chunkCitation(c wireChunk) protocol.Citation {
	if c.Type == "tool_file" {
		return protocol.Citation{Title: c.FileName, Document: c.FileID}
	}
	return protocol.Citation{Title: c.Title, URL: c.URL}
}

// Accepts the string and object forms of function.call arguments
func decodeArguments(raw json.RawMessage) (json.RawMessage, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return protocol.ParseArgs(s)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, fmt.Errorf("arguments: %w", err)
	}
	return protocol.ParseArgs(buf.String())
}

// Arguments fragments and fixed fields of a vendor run tool collected until its done event
type execution struct {
	name   string
	args   strings.Builder
	fields map[string]json.RawMessage
}

// Tracks the block receiving deltas, the last text block and pending vendor tool runs
type streamState struct {
	b        protocol.Builder
	current  string
	textKey  string
	textLen  map[string]int
	stop     protocol.StopReason
	execs    map[string]*execution
	handoffs map[string]map[string]json.RawMessage
}

func (Protocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		st := &streamState{b: protocol.Builder{Origin: protocol.MistralConversations}, textLen: map[string]int{}, stop: protocol.StopEnd, execs: map[string]*execution{}, handoffs: map[string]map[string]json.RawMessage{}}
		for ev, err := range sse.Read(r) {
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			var data event
			if err := json.Unmarshal([]byte(ev.Data), &data); err != nil {
				yield(protocol.Event{}, fmt.Errorf("mistral-conversations: event %s: %w", ev.Name, err))
				return
			}
			if data.Type == "" {
				data.Type = ev.Name
			}
			done, err := st.apply(data)
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			for _, e := range st.b.Events() {
				if !yield(e, nil) {
					return
				}
			}
			if done {
				return
			}
		}
		yield(protocol.Event{}, fmt.Errorf("mistral-conversations: stream ended before conversation.response.done"))
	}
}

func (st *streamState) apply(data event) (bool, error) {
	switch data.Type {
	case "conversation.response.started":
		st.b.Start(data.ConversationID, "")
	case "message.output.delta":
		st.b.Start("", data.Model)
		key := data.ID + "/" + strconv.Itoa(data.ContentIndex)
		if err := st.messageDelta(key, data.Content); err != nil {
			return false, fmt.Errorf("mistral-conversations: %w", err)
		}
	case "function.call.delta":
		st.b.Start("", data.Model)
		st.stop = protocol.StopToolUse
		if err := st.closeOther(data.ToolCallID); err != nil {
			return false, err
		}
		if !st.b.IsOpen(data.ToolCallID) {
			st.b.OpenToolCall(data.ToolCallID, data.ToolCallID, data.Name)
		}
		return false, st.b.Delta(data.ToolCallID, data.Arguments)
	case "tool.execution.started":
		if err := st.closeOther(data.ID); err != nil {
			return false, err
		}
		ex := &execution{name: data.Name, fields: map[string]json.RawMessage{}}
		if present(data.ConfirmationStatus) {
			ex.fields["confirmation_status"] = data.ConfirmationStatus
		}
		if present(data.AgentID) {
			ex.fields["agent_id"] = data.AgentID
		}
		ex.args.WriteString(data.Arguments)
		st.execs[data.ID] = ex
		st.b.OpenServerToolCall(data.ID, data.ID, data.Name)
	case "tool.execution.delta":
		ex, ok := st.execs[data.ID]
		if !ok {
			return false, fmt.Errorf("mistral-conversations: tool.execution.delta for unknown run %q", data.ID)
		}
		ex.args.WriteString(data.Arguments)
	case "tool.execution.done":
		return false, st.executionDone(data)
	case "agent.handoff.started":
		if err := st.closeOther(data.ID); err != nil {
			return false, err
		}
		st.handoffs[data.ID] = map[string]json.RawMessage{}
		mergeHandoff(st.handoffs[data.ID], data)
		st.b.OpenServerToolCall(data.ID, data.ID, nameHandoff)
	case "agent.handoff.done":
		fields, ok := st.handoffs[data.ID]
		if !ok {
			return false, fmt.Errorf("mistral-conversations: agent.handoff.done for unknown handoff %q", data.ID)
		}
		delete(st.handoffs, data.ID)
		mergeHandoff(fields, data)
		raw, _ := json.Marshal(fields)
		if err := st.b.Delta(data.ID, string(raw)); err != nil {
			return false, err
		}
		return false, st.b.Close(data.ID)
	case "conversation.response.done":
		var u protocol.Usage
		if data.Usage != nil {
			u = protocol.Usage{Input: data.Usage.PromptTokens, Output: data.Usage.CompletionTokens}
		}
		return true, st.b.Finish(st.stop, "", u)
	case "conversation.response.error":
		transient := data.Code == 429 || data.Code >= 500 && data.Code < 600
		return false, &protocol.Error{Status: 200, Type: data.Type, Message: fmt.Sprintf("%s (code %d)", data.Message, data.Code), Transient: transient}
	default:
		return false, fmt.Errorf("mistral-conversations: cannot carry %q events", data.Type)
	}
	return false, nil
}

// Delivers the collected arguments as one delta and the info object as the run result
func (st *streamState) executionDone(data event) error {
	ex, ok := st.execs[data.ID]
	if !ok {
		return fmt.Errorf("mistral-conversations: tool.execution.done for unknown run %q", data.ID)
	}
	delete(st.execs, data.ID)
	ex.args.WriteString(data.Arguments)
	ex.fields["arguments"] = quote(ex.args.String())
	raw, _ := json.Marshal(ex.fields)
	if err := st.b.Delta(data.ID, string(raw)); err != nil {
		return err
	}
	if err := st.b.Close(data.ID); err != nil {
		return err
	}
	if present(data.Info) {
		var compact bytes.Buffer
		if err := json.Compact(&compact, data.Info); err != nil {
			return fmt.Errorf("mistral-conversations: tool.execution %s info: %w", data.ID, err)
		}
		st.b.AddServerToolResult(protocol.ServerToolResult{CallID: data.ID, Name: ex.name, Content: compact.Bytes()})
	}
	return nil
}

func mergeHandoff(fields map[string]json.RawMessage, data event) {
	for key, v := range map[string]string{"previous_agent_id": data.PreviousAgentID, "previous_agent_name": data.PreviousAgentName, "next_agent_id": data.NextAgentID, "next_agent_name": data.NextAgentName} {
		if v != "" {
			fields[key] = quote(v)
		}
	}
}

// Closes the block that was receiving deltas when a different key starts
func (st *streamState) closeOther(key string) error {
	if st.current != "" && st.current != key && st.b.IsOpen(st.current) {
		if err := st.b.Close(st.current); err != nil {
			return err
		}
	}
	st.current = key
	return nil
}

// Feeds one delta, a string or a single chunk, into a text, thinking or citation under key
func (st *streamState) messageDelta(key string, raw json.RawMessage) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return st.textDelta(key, s)
	}
	var c wireChunk
	if err := json.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("delta content: %w", err)
	}
	switch c.Type {
	case "text":
		return st.textDelta(key, c.Text)
	case "thinking":
		th, err := decodeThinking(c)
		if err != nil {
			return err
		}
		key += "/thinking"
		if err := st.closeOther(key); err != nil {
			return err
		}
		if !st.b.IsOpen(key) {
			st.b.OpenThinking(key, false)
		}
		if err := st.b.Delta(key, th.Text); err != nil {
			return err
		}
		if th.Signature != "" {
			return st.b.Signature(key, th.Signature)
		}
		return nil
	case "tool_reference", "tool_file":
		return st.cite(key, chunkCitation(c))
	}
	return fmt.Errorf("unsupported delta chunk %q", c.Type)
}

func (st *streamState) textDelta(key, text string) error {
	if err := st.closeOther(key); err != nil {
		return err
	}
	if !st.b.IsOpen(key) {
		st.b.OpenText(key)
	}
	st.textKey = key
	st.textLen[key] += utf8.RuneCountInString(text)
	return st.b.Delta(key, text)
}

// Attaches a reference to the open text block, opening one under key when none is open
func (st *streamState) cite(key string, c protocol.Citation) error {
	target := st.textKey
	if target == "" || !st.b.IsOpen(target) {
		if err := st.closeOther(key); err != nil {
			return err
		}
		if !st.b.IsOpen(key) {
			st.b.OpenText(key)
		}
		st.textKey, target = key, key
	}
	c.Start = st.textLen[target]
	c.End = c.Start
	return st.b.Cite(target, c)
}

// Parses the object error, string detail and validation list shapes, otherwise the trimmed body
func (Protocol) DecodeError(status int, body []byte) *protocol.Error {
	e := &protocol.Error{Status: status, Message: strings.TrimSpace(string(body)), Transient: transientStatus(status)}
	var obj errorObject
	if err := json.Unmarshal(body, &obj); err == nil && obj.Message != "" {
		e.Message, e.Type = obj.Message, obj.Type
		if e.Type == "" && present(obj.Code) {
			e.Type = strings.Trim(string(obj.Code), `"`)
		}
		return e
	}
	var d detailError
	if err := json.Unmarshal(body, &d); err != nil || !present(d.Detail) {
		return e
	}
	var s string
	if err := json.Unmarshal(d.Detail, &s); err == nil {
		e.Message = s
		return e
	}
	var items []validationItem
	if err := json.Unmarshal(d.Detail, &items); err != nil || len(items) == 0 {
		return e
	}
	e.Type = items[0].Type
	var msgs []string
	for _, it := range items {
		loc := make([]string, len(it.Loc))
		for i, l := range it.Loc {
			loc[i] = fmt.Sprint(l)
		}
		msgs = append(msgs, strings.Join(loc, ".")+": "+it.Msg)
	}
	e.Message = strings.Join(msgs, ", ")
	return e
}

// Reports statuses that denote a timeout, conflict, throttle or server failure
func transientStatus(status int) bool {
	return status == 408 || status == 409 || status == 429 || status >= 500
}
