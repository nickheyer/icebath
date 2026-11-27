package transport

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
)

// Event that came before message_start, which a stream never sends but a broken endpoint does
type orderError struct{ event string }

func (e *orderError) Error() string {
	return fmt.Sprintf("The reply came out of order: %s arrived before it began. Send your message again.", e.event)
}

// Deltas appended onto a text field of the open block, by their type
var appended = map[string]string{"text_delta": "text", "thinking_delta": "thinking", "signature_delta": "signature"}

// Message put together from the events of a stream
type accumulator struct {
	started bool
	message map[string]any
	blocks  map[int]map[string]any
	partial map[int]*strings.Builder
	// Output tokens message_start gave, for a message_delta whose usage is null
	output any
}

func newAccumulator() *accumulator {
	return &accumulator{message: map[string]any{}, blocks: map[int]map[string]any{}, partial: map[int]*strings.Builder{}, output: float64(0)}
}

func index(payload map[string]any) (int, bool) {
	n, ok := payload["index"].(float64)
	return int(n), ok
}

// Folds one event into the message
func (a *accumulator) observe(event string, payload map[string]any) {
	switch event {
	case "message_start":
		a.started = true
		message, _ := payload["message"].(map[string]any)
		if usage, ok := message["usage"].(map[string]any); ok {
			if out, ok := usage["output_tokens"].(float64); ok && out != 0 {
				a.output = out
			}
		} else if message != nil {
			message["usage"] = map[string]any{"input_tokens": float64(0), "output_tokens": float64(0)}
		}
		for _, k := range []string{"id", "type", "role", "model", "usage"} {
			if v, has := message[k]; has {
				a.message[k] = v
			}
		}
	case "content_block_start":
		i, ok := index(payload)
		if block, isMap := payload["content_block"].(map[string]any); ok && isMap {
			a.blocks[i] = maps.Clone(block)
		}
	case "content_block_delta":
		i, ok := index(payload)
		delta, isMap := payload["delta"].(map[string]any)
		if !ok || !isMap {
			return
		}
		block := a.blocks[i]
		if block == nil {
			block = map[string]any{}
			a.blocks[i] = block
		}
		kind, _ := delta["type"].(string)
		if field, ok := appended[kind]; ok {
			block[field] = transport.Text(block[field]) + transport.Text(delta[field])
		} else if kind == "input_json_delta" {
			if a.partial[i] == nil {
				a.partial[i] = &strings.Builder{}
			}
			a.partial[i].WriteString(transport.Text(delta["partial_json"]))
		} else if kind == "citations_delta" {
			if citation, ok := delta["citation"]; ok {
				list, _ := block["citations"].([]any)
				block["citations"] = append(list, citation)
			}
		}
	case "message_delta":
		if delta, ok := payload["delta"].(map[string]any); ok {
			for _, k := range []string{"stop_reason", "stop_sequence", "stop_details"} {
				if v, has := delta[k]; has {
					a.message[k] = v
				}
			}
		}
		usage, has := payload["usage"]
		if !has || usage == nil {
			usage = map[string]any{"output_tokens": a.output}
		}
		current, curOK := a.message["usage"].(map[string]any)
		incoming, inOK := usage.(map[string]any)
		if !curOK || !inOK {
			a.message["usage"] = usage
			return
		}
		merged := maps.Clone(current)
		for k, v := range incoming {
			if v != nil || k == "output_tokens" {
				merged[k] = v
			}
		}
		a.message["usage"] = merged
	}
}

// Error of a tool input that wasn't JSON once the stream ended
type inputError struct {
	name, detail string
}

func (e *inputError) Error() string {
	return fmt.Sprintf("expected value at line 1 column 1: tool_use %q input is not valid JSON (%s)", e.name, e.detail)
}

// Message with its tool inputs decoded; a strict pass reports the first input that isn't JSON
func (a *accumulator) finalize(strict bool) (map[string]any, error) {
	var failure error
	indexes := slices.Sorted(maps.Keys(a.blocks))
	blocks := make([]any, 0, len(indexes))
	for _, i := range indexes {
		block := maps.Clone(a.blocks[i])
		if partial := a.partial[i]; partial != nil {
			var decoded any = map[string]any{}
			if raw := partial.String(); strings.TrimSpace(raw) != "" {
				if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
					if strict && failure == nil {
						failure = &inputError{name: transport.Text(block["name"]), detail: err.Error()}
					}
					decoded = map[string]any{}
				}
			}
			block["input"] = decoded
		} else if block["type"] == "tool_use" && block["input"] == nil {
			block["input"] = map[string]any{}
		}
		blocks = append(blocks, block)
	}
	out := maps.Clone(a.message)
	out["content"] = blocks
	return out, failure
}

// Live state of one streamed reply
type stream struct {
	c      *client
	p      *prepared
	fn     provider.StreamFunc
	acc    *accumulator
	header http.Header
	status int

	tools, sawEvent, sawStop bool
	textWatch, thinkWatch    transport.RunawayWatch
	runaway                  string
	// Something of the reply reached the person
	delivered bool
	first     time.Time
}

func (s *stream) emit(kind provider.EventKind, text string) error {
	if s.fn == nil {
		return nil
	}
	if err := s.fn(provider.StreamEvent{Kind: kind, Text: text}); err != nil {
		return &stopped{err}
	}
	return nil
}

// Error the consumer of the stream's events returned, which ends the stream as it is
type stopped struct{ err error }

func (e *stopped) Error() string { return e.err.Error() }

func (e *stopped) Unwrap() error { return e.err }

// One frame of the stream: whether it was output, whether reading stops, and the error it carries
func (s *stream) frame(f *transport.Frame) (bool, bool, error) {
	if f.Empty() || f.Event == "ping" {
		return false, false, nil
	}
	if strings.TrimSpace(f.Data) == "" {
		if f.Event == "" {
			return false, false, nil
		}
		return false, false, transport.FrameError("", s.header)
	}
	var payload map[string]any
	if json.Unmarshal([]byte(f.Data), &payload) != nil {
		return false, false, transport.FrameError(f.Data, s.header)
	}
	event := f.Event
	if event == "" {
		event, _ = payload["type"].(string)
	}
	if event == "ping" {
		return false, false, nil
	}
	if s.first.IsZero() {
		s.first = time.Now()
	}
	if err := s.emit(provider.EventActivity, "receiving the reply"); err != nil {
		return true, true, err
	}
	stop, err := s.event(event, payload, f.Data)
	return true, stop, err
}

// Folds one event into the reply and sends its deltas on, true once message_stop or a runaway cut came
func (s *stream) event(event string, payload map[string]any, raw string) (bool, error) {
	s.sawEvent = true
	if !s.acc.started && event != "message_start" && event != "error" {
		return true, &orderError{event: event}
	}
	switch event {
	case "error":
		inner, ok := payload["error"].(map[string]any)
		if !ok {
			inner = map[string]any{}
			if payload["error"] != nil {
				inner["message"] = fmt.Sprint(payload["error"])
			}
		}
		status := transport.PayloadStatus(payload)
		return true, &transport.StreamError{Status: status, Body: transport.ErrorBody(inner, status), Text: raw, Header: s.header}
	case "message_stop":
		s.sawStop = true
		return true, nil
	case "content_block_start":
		s.acc.observe(event, payload)
		if block, _ := payload["content_block"].(map[string]any); block["type"] == "tool_use" {
			s.tools = true
			if name := s.p.inbound(transport.Text(block["name"])); name != "" {
				if err := s.emit(provider.EventToolStart, name); err != nil {
					return true, err
				}
			}
		}
		return false, nil
	case "content_block_delta":
		s.acc.observe(event, payload)
		delta, _ := payload["delta"].(map[string]any)
		switch delta["type"] {
		case "text_delta":
			piece := transport.Text(delta["text"])
			if piece != "" && !s.tools {
				s.delivered = true
				if err := s.emit(provider.EventTextDelta, piece); err != nil {
					return true, err
				}
			}
			if s.textWatch.Feed(piece) {
				s.runaway = "content"
				return true, nil
			}
		case "thinking_delta":
			if piece := transport.Text(delta["thinking"]); piece != "" {
				s.delivered = true
				if err := s.emit(provider.EventReasoningDelta, piece); err != nil {
					return true, err
				}
				if s.thinkWatch.Feed(piece) {
					s.runaway = "reasoning"
					return true, nil
				}
			}
		}
		return false, nil
	}
	s.acc.observe(event, payload)
	return false, nil
}

// Reply of a stream that ended: the message put together, cut at its length after a runaway loop
func (s *stream) finish() (*provider.Response, error) {
	if s.runaway != "" {
		slog.Warn(fmt.Sprintf("Runaway repetition detected in streamed %s; cutting the stream. model=%s", s.runaway, s.p.model))
		message, _ := s.acc.finalize(false)
		message["stop_reason"] = "max_tokens"
		return s.c.normalize(s.p, message, s.header, s.status)
	}
	if !s.sawStop {
		if s.sawEvent {
			return nil, s.p.refused.Empty("The reply stopped before it ended, perhaps because the connection dropped. Send your message again.", nil)
		}
		return nil, s.p.refused.Empty("The provider sent back an empty reply, perhaps because of a failure on its side. Send your message again.", nil)
	}
	message, err := s.acc.finalize(true)
	if err != nil {
		s.c.r.buffer.Store(s.c.key(), true)
		return nil, s.p.refused.Empty(err.Error(), err)
	}
	content, _ := message["content"].([]any)
	if message["stop_reason"] == nil {
		if len(content) == 0 {
			return nil, s.p.refused.Empty("The provider sent back an empty reply that never ended, perhaps because of a failure on its side. Send your message again.", nil)
		}
		if hasKind(content, []string{"tool_use"}) {
			return nil, s.p.refused.Empty("The reply stopped while the model was still writing a tool call, so the connection probably dropped. "+
				"Send your message again.", nil)
		}
	}
	return s.c.normalize(s.p, message, s.header, s.status)
}
