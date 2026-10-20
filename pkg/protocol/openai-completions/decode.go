package openaicompletions

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
	out := protocol.Response{ID: wire.ID, Model: wire.Model, RawStopReason: c.FinishReason, StopReason: stopReason(c.FinishReason)}
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

func stopReason(finish string) protocol.StopReason {
	switch finish {
	case "stop":
		return protocol.StopEnd
	case "length":
		return protocol.StopLength
	case "tool_calls", "function_call":
		return protocol.StopToolUse
	case "content_filter":
		return protocol.StopContentFilter
	}
	return protocol.StopOther
}

func mapUsage(u usage) protocol.Usage {
	out := protocol.Usage{Input: u.PromptTokens, Output: u.CompletionTokens}
	if u.PromptTokensDetails != nil {
		out.CacheRead = u.PromptTokensDetails.CachedTokens
		out.CacheWrite = u.PromptTokensDetails.CacheWriteTokens
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
		var b protocol.Builder
		var usage protocol.Usage
		var finish string
		finished := false
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
			if ev.Data == "[DONE]" {
				break
			}
			var c chunk
			if err := json.Unmarshal([]byte(ev.Data), &c); err != nil {
				yield(protocol.Event{}, fmt.Errorf("openai-completions: chunk: %w", err))
				return
			}
			if c.Error != nil {
				yield(protocol.Event{}, &protocol.Error{Status: 200, Type: errType(*c.Error), Message: c.Error.Message, Body: []byte(ev.Data)})
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
		if err := b.Finish(stopReason(finish), finish, usage); err != nil {
			yield(protocol.Event{}, err)
			return
		}
		flush()
	}
}

// Routes content, refusal and tool call fragments into builder blocks following accumulate_delta
func applyDelta(b *protocol.Builder, d delta, calls map[int]*pendingCall, current *int) error {
	if d.Content != nil && *d.Content != "" {
		if !b.IsOpen("content") {
			b.OpenText("content")
		}
		if err := b.Delta("content", *d.Content); err != nil {
			return err
		}
	}
	if d.Refusal != nil && *d.Refusal != "" {
		if !b.IsOpen("refusal") {
			b.OpenText("refusal")
		}
		if err := b.Delta("refusal", *d.Refusal); err != nil {
			return err
		}
	}
	for _, tc := range d.ToolCalls {
		if tc.Type != "" && tc.Type != "function" {
			return fmt.Errorf("openai-completions: tool call at index %d has type %q", tc.Index, tc.Type)
		}
		if *current != tc.Index {
			for _, k := range []string{"content", "refusal"} {
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
	var wire errorResponse
	if err := json.Unmarshal(body, &wire); err == nil && wire.Error != nil && wire.Error.Message != "" {
		return &protocol.Error{Status: status, Type: errType(*wire.Error), Message: wire.Error.Message, Body: body}
	}
	return &protocol.Error{Status: status, Message: string(bytes.TrimSpace(body)), Body: body}
}

func errType(e apiError) string {
	if e.Type != "" {
		return e.Type
	}
	return e.Code
}
