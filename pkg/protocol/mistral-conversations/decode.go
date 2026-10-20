package mistralconversations

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/sse"
	"github.com/nickheyer/icebath/pkg/protocol"
)

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
			done, err := st.apply(data, []byte(ev.Data))
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

func (st *streamState) apply(data event, body []byte) (bool, error) {
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
		return false, &protocol.Error{Status: 200, Type: data.Type, Message: fmt.Sprintf("%s (code %d)", data.Message, data.Code), Body: body, InStream: true, Transient: transient}
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
	e := &protocol.Error{Status: status, Body: body, Message: strings.TrimSpace(string(body)), Transient: transientStatus(status)}
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
