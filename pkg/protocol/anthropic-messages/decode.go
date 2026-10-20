package anthropicmessages

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/sse"
	"github.com/nickheyer/icebath/pkg/protocol"
)

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
				yield(protocol.Event{}, &protocol.Error{Status: 200, Type: msg.Type, Message: msg.Message, Body: []byte(ev.Data), InStream: true, Transient: transientTypes[msg.Type]})
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
	e := &protocol.Error{Status: status, Body: body}
	var er errorResponse
	if err := json.Unmarshal(body, &er); err != nil || er.Error.Type == "" && er.Error.Message == "" {
		e.Message = strings.TrimSpace(string(body))
	} else {
		e.Type, e.Message = er.Error.Type, er.Error.Message
	}
	e.Transient = transientStatus(status) || transientTypes[e.Type]
	return e
}

// Reports statuses that denote a timeout, conflict, throttle or server failure
func transientStatus(status int) bool {
	return status == 408 || status == 409 || status == 429 || status >= 500
}
