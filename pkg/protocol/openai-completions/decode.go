package openaicompletions

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
	return &protocol.Error{Status: 200, Type: errType(e), Message: e.Message, Body: body, InStream: true, Transient: transientTypes[e.Type] || transientTypes[e.Code]}
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
	e := &protocol.Error{Status: status, Body: body, Transient: transientStatus(status)}
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
