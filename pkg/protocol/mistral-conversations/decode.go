package mistralconversations

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/protocol/internal/sse"
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
		switch e.Type {
		case "message.output":
			parts, err := decodeContent(e.Content)
			if err != nil {
				return protocol.Response{}, fmt.Errorf("mistral-conversations: output %d: %w", i, err)
			}
			out.Content = append(out.Content, parts...)
		case "function.call":
			args, err := decodeArguments(e.Arguments)
			if err != nil {
				return protocol.Response{}, fmt.Errorf("mistral-conversations: output %d: %w", i, err)
			}
			out.Content = append(out.Content, protocol.ToolCall{ID: e.ToolCallID, Name: e.Name, Args: args})
			out.StopReason = protocol.StopToolUse
		default:
			return protocol.Response{}, fmt.Errorf("mistral-conversations: output %d: cannot carry %q entries", i, e.Type)
		}
	}
	out.Usage = protocol.Usage{Input: resp.Usage.PromptTokens, Output: resp.Usage.CompletionTokens}
	return out, nil
}

// Turns a content string or chunk array into neutral parts, merging adjacent text
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
	for _, c := range chunks {
		p, err := decodeChunk(c)
		if err != nil {
			return nil, err
		}
		if t, ok := p.(protocol.Text); ok && len(parts) > 0 {
			if prev, ok := parts[len(parts)-1].(protocol.Text); ok {
				parts[len(parts)-1] = protocol.Text{Text: prev.Text + t.Text}
				continue
			}
		}
		parts = append(parts, p)
	}
	return parts, nil
}

// Renders reference and file chunks as text the way the official SDK does
func decodeChunk(c wireChunk) (protocol.Part, error) {
	switch c.Type {
	case "text":
		return protocol.Text{Text: c.Text}, nil
	case "thinking":
		var text strings.Builder
		for _, inner := range c.Thinking {
			t, err := chunkText(inner)
			if err != nil {
				return nil, fmt.Errorf("thinking: %w", err)
			}
			text.WriteString(t)
		}
		return protocol.Thinking{Text: text.String(), Signature: c.Signature}, nil
	}
	t, err := chunkText(c)
	if err != nil {
		return nil, err
	}
	return protocol.Text{Text: t}, nil
}

func chunkText(c wireChunk) (string, error) {
	switch c.Type {
	case "text":
		return c.Text, nil
	case "tool_reference":
		return "<Reference title=" + c.Title + ">", nil
	case "tool_file":
		return "<File id=" + c.FileID + " name=" + c.FileName + ">", nil
	}
	return "", fmt.Errorf("unsupported chunk type %q", c.Type)
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

func (Protocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		var b protocol.Builder
		var current string
		stop := protocol.StopEnd
		flush := func() bool {
			for _, e := range b.Events() {
				if !yield(e, nil) {
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
			var data event
			if err := json.Unmarshal([]byte(ev.Data), &data); err != nil {
				yield(protocol.Event{}, fmt.Errorf("mistral-conversations: event %s: %w", ev.Name, err))
				return
			}
			if data.Type == "" {
				data.Type = ev.Name
			}
			switch data.Type {
			case "conversation.response.started":
				b.Start(data.ConversationID, "")
			case "message.output.delta":
				b.Start("", data.Model)
				key := data.ID + "/" + strconv.Itoa(data.ContentIndex)
				if err := messageDelta(&b, &current, key, data.Content); err != nil {
					yield(protocol.Event{}, fmt.Errorf("mistral-conversations: %w", err))
					return
				}
			case "function.call.delta":
				b.Start("", data.Model)
				stop = protocol.StopToolUse
				if err := closeOther(&b, &current, data.ToolCallID); err != nil {
					yield(protocol.Event{}, err)
					return
				}
				if !b.IsOpen(data.ToolCallID) {
					b.OpenToolCall(data.ToolCallID, data.ToolCallID, data.Name)
				}
				if err := b.Delta(data.ToolCallID, data.Arguments); err != nil {
					yield(protocol.Event{}, err)
					return
				}
			case "conversation.response.done":
				var u protocol.Usage
				if data.Usage != nil {
					u = protocol.Usage{Input: data.Usage.PromptTokens, Output: data.Usage.CompletionTokens}
				}
				if err := b.Finish(stop, "", u); err != nil {
					yield(protocol.Event{}, err)
					return
				}
				flush()
				return
			case "conversation.response.error":
				yield(protocol.Event{}, &protocol.Error{Status: 200, Type: data.Type, Message: fmt.Sprintf("%s (code %d)", data.Message, data.Code)})
				return
			default:
				yield(protocol.Event{}, fmt.Errorf("mistral-conversations: cannot carry %q events", data.Type))
				return
			}
			if !flush() {
				return
			}
		}
		yield(protocol.Event{}, fmt.Errorf("mistral-conversations: stream ended before conversation.response.done"))
	}
}

// Closes the block that was receiving deltas when a different key starts
func closeOther(b *protocol.Builder, current *string, key string) error {
	if *current != "" && *current != key && b.IsOpen(*current) {
		if err := b.Close(*current); err != nil {
			return err
		}
	}
	*current = key
	return nil
}

// Feeds one delta, a string or a single chunk, into a text or thinking block under key
func messageDelta(b *protocol.Builder, current *string, key string, raw json.RawMessage) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return textDelta(b, current, key, s)
	}
	var c wireChunk
	if err := json.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("delta content: %w", err)
	}
	part, err := decodeChunk(c)
	if err != nil {
		return err
	}
	switch p := part.(type) {
	case protocol.Thinking:
		key += "/thinking"
		if err := closeOther(b, current, key); err != nil {
			return err
		}
		if !b.IsOpen(key) {
			b.OpenThinking(key, false)
		}
		if err := b.Delta(key, p.Text); err != nil {
			return err
		}
		if p.Signature != "" {
			return b.Signature(key, p.Signature)
		}
		return nil
	case protocol.Text:
		return textDelta(b, current, key, p.Text)
	}
	return fmt.Errorf("unsupported delta chunk %q", c.Type)
}

func textDelta(b *protocol.Builder, current *string, key, text string) error {
	if err := closeOther(b, current, key); err != nil {
		return err
	}
	if !b.IsOpen(key) {
		b.OpenText(key)
	}
	return b.Delta(key, text)
}

func (Protocol) DecodeError(status int, body []byte) *protocol.Error {
	e := &protocol.Error{Status: status, Body: body, Message: strings.TrimSpace(string(body))}
	var v validationError
	if err := json.Unmarshal(body, &v); err != nil || len(v.Detail) == 0 {
		return e
	}
	e.Type = v.Detail[0].Type
	var msgs []string
	for _, d := range v.Detail {
		loc := make([]string, len(d.Loc))
		for i, l := range d.Loc {
			loc[i] = fmt.Sprint(l)
		}
		msgs = append(msgs, strings.Join(loc, ".")+": "+d.Msg)
	}
	e.Message = strings.Join(msgs, ", ")
	return e
}
