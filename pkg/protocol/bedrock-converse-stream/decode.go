package bedrockconversestream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
)

var stopReasons = map[string]protocol.StopReason{
	"end_turn":                      protocol.StopEnd,
	"stop_sequence":                 protocol.StopEnd,
	"max_tokens":                    protocol.StopLength,
	"model_context_window_exceeded": protocol.StopLength,
	"tool_use":                      protocol.StopToolUse,
	"guardrail_intervened":          protocol.StopContentFilter,
	"content_filtered":              protocol.StopContentFilter,
	"malformed_model_output":        protocol.StopOther,
	"malformed_tool_use":            protocol.StopOther,
}

func mapStop(raw string) protocol.StopReason {
	if s, ok := stopReasons[raw]; ok {
		return s
	}
	return protocol.StopOther
}

func mapUsage(u tokenUsage) protocol.Usage {
	return protocol.Usage{Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheWriteInputTokens}
}

// Parses a ConverseResponse body
func (Protocol) Decode(r io.Reader) (protocol.Response, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return protocol.Response{}, err
	}
	var wire response
	if err := json.Unmarshal(body, &wire); err != nil {
		return protocol.Response{}, fmt.Errorf("bedrock-converse-stream: response: %w", err)
	}
	resp := protocol.Response{StopReason: mapStop(wire.StopReason), RawStopReason: wire.StopReason, Usage: mapUsage(wire.Usage)}
	for i, block := range wire.Output.Message.Content {
		part, err := decodeBlock(block)
		if err != nil {
			return protocol.Response{}, fmt.Errorf("bedrock-converse-stream: content block %d: %w", i, err)
		}
		resp.Content = append(resp.Content, part)
	}
	return resp, nil
}

func decodeBlock(block contentBlock) (protocol.Part, error) {
	switch {
	case block.Text != nil:
		return protocol.Text{Text: *block.Text}, nil
	case block.ReasoningContent != nil:
		rc := block.ReasoningContent
		if rc.ReasoningText != nil {
			return protocol.Thinking{Text: rc.ReasoningText.Text, Signature: rc.ReasoningText.Signature}, nil
		}
		if rc.RedactedContent != "" {
			return protocol.Thinking{Redacted: true, Signature: rc.RedactedContent}, nil
		}
		return nil, fmt.Errorf("reasoningContent has no member")
	case block.ToolUse != nil:
		var compact bytes.Buffer
		if err := json.Compact(&compact, block.ToolUse.Input); err != nil {
			return nil, fmt.Errorf("toolUse %s input: %w", block.ToolUse.Name, err)
		}
		args, err := protocol.ParseArgs(compact.String())
		if err != nil {
			return nil, fmt.Errorf("toolUse %s: %w", block.ToolUse.Name, err)
		}
		return protocol.ToolCall{ID: block.ToolUse.ToolUseID, Name: block.ToolUse.Name, Args: args}, nil
	}
	return nil, fmt.Errorf("unsupported content block")
}

type blockKind int

const (
	kindText blockKind = iota + 1
	kindThinking
	kindToolUse
)

type openBlock struct {
	kind     blockKind
	redacted bool
}

// Tracks open blocks and the stop reason while frames arrive
type streamState struct {
	b          protocol.Builder
	blocks     map[int]openBlock
	stopReason string
	stopped    bool
	finished   bool
}

// Parses an event stream body, finishing on the metadata event
func (Protocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		st := &streamState{blocks: map[int]openBlock{}}
		for f, err := range ReadFrames(r) {
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			if err := st.handle(f); err != nil {
				yield(protocol.Event{}, err)
				return
			}
			for _, ev := range st.b.Events() {
				if !yield(ev, nil) {
					return
				}
			}
			if st.finished {
				return
			}
		}
		yield(protocol.Event{}, fmt.Errorf("bedrock-converse-stream: stream ended before the metadata event"))
	}
}

func (st *streamState) handle(f Frame) error {
	switch f.String(":message-type") {
	case "event":
		return st.event(f.String(":event-type"), f.Payload)
	case "exception":
		var body exceptionBody
		if err := json.Unmarshal(f.Payload, &body); err != nil {
			return fmt.Errorf("bedrock-converse-stream: exception payload: %w", err)
		}
		return &protocol.Error{Status: 200, Type: f.String(":exception-type"), Message: body.Message, Body: f.Payload}
	case "error":
		return &protocol.Error{Status: 200, Type: f.String(":error-code"), Message: f.String(":error-message"), Body: f.Payload}
	}
	return fmt.Errorf("bedrock-converse-stream: frame has no event, exception or error message type")
}

func (st *streamState) event(name string, payload []byte) error {
	switch name {
	case "messageStart":
		st.b.Start("", "")
		return nil
	case "contentBlockStart":
		var ev contentBlockStartEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			return fmt.Errorf("bedrock-converse-stream: contentBlockStart: %w", err)
		}
		if ev.Start.ToolUse == nil {
			return fmt.Errorf("bedrock-converse-stream: contentBlockStart %d is not a toolUse", ev.ContentBlockIndex)
		}
		if _, open := st.blocks[ev.ContentBlockIndex]; open {
			return fmt.Errorf("bedrock-converse-stream: contentBlockStart %d repeated", ev.ContentBlockIndex)
		}
		st.blocks[ev.ContentBlockIndex] = openBlock{kind: kindToolUse}
		st.b.OpenToolCall(key(ev.ContentBlockIndex), ev.Start.ToolUse.ToolUseID, ev.Start.ToolUse.Name)
		return nil
	case "contentBlockDelta":
		var ev contentBlockDeltaEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			return fmt.Errorf("bedrock-converse-stream: contentBlockDelta: %w", err)
		}
		return st.delta(ev.ContentBlockIndex, ev.Delta)
	case "contentBlockStop":
		var ev contentBlockStopEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			return fmt.Errorf("bedrock-converse-stream: contentBlockStop: %w", err)
		}
		if _, open := st.blocks[ev.ContentBlockIndex]; !open {
			return nil
		}
		delete(st.blocks, ev.ContentBlockIndex)
		return st.b.Close(key(ev.ContentBlockIndex))
	case "messageStop":
		var ev messageStopEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			return fmt.Errorf("bedrock-converse-stream: messageStop: %w", err)
		}
		st.stopReason, st.stopped = ev.StopReason, true
		return nil
	case "metadata":
		var ev metadataEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			return fmt.Errorf("bedrock-converse-stream: metadata: %w", err)
		}
		if !st.stopped {
			return fmt.Errorf("bedrock-converse-stream: metadata arrived before messageStop")
		}
		st.finished = true
		return st.b.Finish(mapStop(st.stopReason), st.stopReason, mapUsage(ev.Usage))
	}
	return fmt.Errorf("bedrock-converse-stream: unknown event type %q", name)
}

func (st *streamState) delta(index int, d contentBlockDelta) error {
	k := key(index)
	switch {
	case d.Text != nil:
		if err := st.open(index, kindText, false); err != nil {
			return err
		}
		return st.b.Delta(k, *d.Text)
	case d.ToolUse != nil:
		if st.blocks[index].kind != kindToolUse {
			return fmt.Errorf("bedrock-converse-stream: toolUse delta for block %d without contentBlockStart", index)
		}
		return st.b.Delta(k, d.ToolUse.Input)
	case d.ReasoningContent != nil:
		rc := d.ReasoningContent
		redacted := rc.RedactedContent != ""
		if err := st.open(index, kindThinking, redacted); err != nil {
			return err
		}
		if redacted != st.blocks[index].redacted {
			return fmt.Errorf("bedrock-converse-stream: block %d mixes redacted and plain reasoning", index)
		}
		if redacted {
			return st.b.Signature(k, rc.RedactedContent)
		}
		if rc.Text != nil {
			if err := st.b.Delta(k, *rc.Text); err != nil {
				return err
			}
		}
		if rc.Signature != nil {
			return st.b.Signature(k, *rc.Signature)
		}
		return nil
	}
	return fmt.Errorf("bedrock-converse-stream: delta for block %d has no supported member", index)
}

// Opens block index as kind when new and rejects a kind change on an open block
func (st *streamState) open(index int, kind blockKind, redacted bool) error {
	if have, ok := st.blocks[index]; ok {
		if have.kind != kind {
			return fmt.Errorf("bedrock-converse-stream: block %d changed kind mid stream", index)
		}
		return nil
	}
	st.blocks[index] = openBlock{kind: kind, redacted: redacted}
	switch kind {
	case kindText:
		st.b.OpenText(key(index))
	case kindThinking:
		st.b.OpenThinking(key(index), redacted)
	}
	return nil
}

func key(index int) string { return strconv.Itoa(index) }

// Parses a non 2xx body, taking the sanitized __type or code as the error type
func (Protocol) DecodeError(status int, body []byte) *protocol.Error {
	e := &protocol.Error{Status: status, Body: body}
	var wire errorBody
	if err := json.Unmarshal(body, &wire); err != nil {
		e.Message = strings.TrimSpace(string(body))
		return e
	}
	code := wire.Code
	if code == "" {
		code = wire.Type
	}
	e.Type = sanitizeErrorCode(code)
	e.Message = wire.Message
	return e
}

// Strips the namespace prefix and URI suffix the way the AWS SDK restjson decoder does
func sanitizeErrorCode(code string) string {
	if i := strings.Index(code, ":"); i >= 0 {
		code = code[:i]
	}
	if i := strings.Index(code, "#"); i >= 0 {
		code = code[i+1:]
	}
	return code
}
