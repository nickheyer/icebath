package transport

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
)

// Sentinel some routers send as the whole reply after their upstream timed out connecting
const routerShim = "Connect timeout, please try again later."

// Live state of one streamed reply
type stream struct {
	c      *client
	p      *prepared
	fn     provider.StreamFunc
	header http.Header
	status int

	content, reasoning, display, refusal []string
	// Text held back while it may still be an error a relay sends as text, or the router's timeout sentinel
	pending   []string
	details   []map[string]any
	calls     *accumulator
	signature string
	// Declared fields the reply carried, opaque ones by name and the parts of those holding reasoning
	fields map[string]string
	texts  map[string][]string
	reason string
	usage  map[string]any
	tier   string
	model  string

	scrub *transport.Scrubber
	// Reasoning streamed in its own fields, which the text's reasoning blocks then don't repeat
	native bool
	// Something of the reply reached the person, and visible text did
	delivered bool
	visible   bool

	textWatch, reasoningWatch, detailWatch transport.RunawayWatch
	runaway                                string

	first time.Time
	// llama.cpp prompt progress: the latest, when it came and when it was last shown
	progress           map[string]any
	progressAt, showed time.Time
	waiting            bool
	started            time.Time
}

func newStream(c *client, p *prepared, fn provider.StreamFunc, response *http.Response) *stream {
	return &stream{c: c, p: p, fn: fn, header: response.Header.Clone(), status: response.StatusCode, calls: newAccumulator(),
		fields: map[string]string{}, texts: map[string][]string{}, scrub: transport.NewScrubber(), started: time.Now()}
}

// Error the consumer of the stream's events returned, which ends the stream as it is
type stopped struct {
	err error
}

func (e *stopped) Error() string { return e.err.Error() }

func (e *stopped) Unwrap() error { return e.err }

func (s *stream) emit(kind provider.EventKind, text string) error {
	if s.fn == nil {
		return nil
	}
	if err := s.fn(provider.StreamEvent{Kind: kind, Text: text}); err != nil {
		return &stopped{err}
	}
	return nil
}

// One frame of the stream: whether it was output, whether reading stops, and the error it carries
func (s *stream) frame(f *transport.Frame) (bool, bool, error) {
	if f.Empty() {
		return false, false, nil
	}
	if strings.HasPrefix(f.Data, "[DONE]") {
		return false, true, nil
	}
	if strings.TrimSpace(f.Data) == "" {
		return false, false, transport.FrameError("", s.header)
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(f.Data), &chunk) != nil {
		return false, false, transport.FrameError(f.Data, s.header)
	}
	if object := chunk["error"]; !empty(object) {
		inner, isObject := object.(map[string]any)
		if isObject && len(inner) == 0 {
			return false, false, nil
		}
		if !isObject {
			inner = map[string]any{"message": fmt.Sprint(object)}
		}
		status := transport.PayloadStatus(chunk)
		if status == 0 {
			status = f.Status
		}
		return false, false, &transport.StreamError{Status: status, Body: transport.ErrorBody(inner, status), Text: f.Data, Header: s.header}
	}
	if _, choices := chunk["choices"]; f.Event != "" && f.Event != "error" && !strings.HasPrefix(f.Event, "response.") &&
		!strings.HasPrefix(f.Event, "transcript.") && !choices {
		return false, false, nil
	}
	now := time.Now()
	if s.p.llamacpp {
		if progress := promptProgress(chunk); progress != nil {
			s.progress, s.progressAt = progress, now
			return true, false, s.showProgress(now)
		}
	}
	if s.first.IsZero() {
		s.first = now
		if s.waiting {
			s.waiting = false
			if err := s.emit(provider.EventWait, ""); err != nil {
				return true, true, err
			}
		}
	}
	if err := s.emit(provider.EventActivity, "receiving the reply"); err != nil {
		return true, true, err
	}
	stop, err := s.chunk(chunk)
	return true, stop, err
}

// Fields a llama.cpp chunk carrying output has
var outputFields = []string{"content", "tool_calls", "reasoning_content", "reasoning", "refusal"}

// Prompt progress of a llama.cpp chunk that carries nothing else, nil for one with output or a finish
func promptProgress(chunk map[string]any) map[string]any {
	progress, ok := chunk["prompt_progress"].(map[string]any)
	if !ok {
		return nil
	}
	choices, _ := chunk["choices"].([]any)
	for _, c := range choices {
		choice, _ := c.(map[string]any)
		if choice == nil {
			continue
		}
		if !empty(choice["finish_reason"]) {
			return nil
		}
		delta, _ := choice["delta"].(map[string]any)
		for _, field := range outputFields {
			switch v := delta[field].(type) {
			case nil:
			case string:
				if v != "" {
					return nil
				}
			case []any:
				if len(v) > 0 {
					return nil
				}
			default:
				return nil
			}
		}
	}
	return progress
}

// Line of how much of the prompt llama.cpp has read, at most once a second until the first token
func (s *stream) showProgress(now time.Time) error {
	if !s.first.IsZero() || now.Sub(s.progressAt) > time.Minute || now.Sub(s.showed) < time.Second {
		return nil
	}
	s.showed, s.waiting = now, true
	text := "⚙ processing prompt"
	total, okTotal := s.progress["total"].(float64)
	processed, okProcessed := s.progress["processed"].(float64)
	if okTotal && okProcessed && total > 0 {
		text = fmt.Sprintf("⚙ processing prompt — %d%%", min(max(int(math.Round(processed/total*100)), 0), 100))
	}
	return s.emit(provider.EventWait, text)
}

// Visible text of a delta through the scrubber, its reasoning blocks sent as reasoning when none streams in its own fields
func (s *stream) text(text string) error {
	visible := s.scrub.Feed(text)
	if hidden := s.scrub.Hidden(); hidden != "" && !s.native {
		s.delivered = true
		if err := s.emit(provider.EventReasoningDelta, hidden); err != nil {
			return err
		}
	}
	if !s.visible {
		visible = strings.TrimLeft(visible, "\n")
	}
	if visible == "" {
		return nil
	}
	s.visible, s.delivered = true, true
	return s.emit(provider.EventTextDelta, visible)
}

// Text of a reply making tool calls, read only for the reasoning blocks in it
func (s *stream) suppressed(text string) error {
	s.scrub.Feed(text)
	if hidden := s.scrub.Hidden(); hidden != "" && !s.native {
		s.delivered = true
		return s.emit(provider.EventReasoningDelta, hidden)
	}
	return nil
}

func (s *stream) flush() error {
	pending := s.pending
	s.pending = nil
	for _, text := range pending {
		var err error
		if s.calls.Len() > 0 {
			err = s.suppressed(text)
		} else {
			err = s.text(text)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Finish reason as OpenAI names it, numbers as their digits
func finishOf(v any) string {
	var text string
	switch f := v.(type) {
	case string:
		text = f
	case float64:
		text = strconv.FormatInt(int64(f), 10)
	}
	text = strings.ToLower(text)
	switch text {
	case "max_tokens":
		return "length"
	case "end":
		return "stop"
	case "function_call":
		return "tool_calls"
	}
	return text
}

// Usage and finish of a chunk without choices, or the error a provider sends that way
func (s *stream) choiceless(chunk map[string]any) error {
	if usage, ok := chunk["usage"].(map[string]any); ok && len(usage) > 0 {
		s.usage = usage
	}
	kind, message := chunk["error_type"], chunk["error_message"]
	if !empty(kind) || !empty(message) {
		status := transport.PayloadStatus(map[string]any{"code": kind, "message": message})
		if status == 0 {
			status = transport.StatusOf(kind)
		}
		code, _ := kind.(string)
		code = cmp.Or(code, "provider_in_stream_error")
		text, _ := message.(string)
		if text == "" {
			raw, _ := json.Marshal(chunk)
			text = string(raw)
		}
		return &transport.StreamError{Status: status, Body: transport.ErrorBody(map[string]any{"code": code, "message": text}, status),
			Text: fmt.Sprintf("%v: %v", kind, message), Header: s.header}
	}
	switch last := chunk["lastOne"].(type) {
	case bool:
		if last && s.reason == "" {
			s.reason = "stop"
		}
	case float64:
		if last == 1 && s.reason == "" {
			s.reason = "stop"
		}
	case string:
		if last == "true" && s.reason == "" {
			s.reason = "stop"
		}
	}
	return nil
}

// Folds one chunk into the reply and sends its deltas on, true once the stream must stop
func (s *stream) chunk(chunk map[string]any) (bool, error) {
	if model, _ := chunk["model"].(string); model != "" {
		s.model = model
	}
	if tier, _ := chunk["service_tier"].(string); tier != "" {
		s.tier = strings.ToLower(tier)
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return false, s.choiceless(chunk)
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return false, nil
	}
	if finish := finishOf(choice["finish_reason"]); finish != "" {
		s.reason = finish
	}
	if usage, ok := chunk["usage"].(map[string]any); ok && len(usage) > 0 {
		s.usage = usage
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return false, nil
	}
	reasoning, _ := delta[reasoningContent].(string)
	if reasoning == "" {
		reasoning, _ = delta[reasoningField].(string)
	}
	if signature := thoughtSignature(delta["extra_content"]); signature != "" {
		s.signature = signature
	}
	for _, key := range s.p.opaque {
		if value, ok := delta[key].(string); ok && value != "" {
			s.fields[key] = value
		}
	}
	for _, key := range s.p.texts {
		if value, ok := delta[key].(string); ok && value != "" {
			s.texts[key] = append(s.texts[key], value)
			reasoning = cmp.Or(reasoning, value)
		}
	}
	if reasoning != "" {
		reasoning = separated(last(s.reasoning), reasoning)
		s.reasoning = append(s.reasoning, reasoning)
	}
	var parts []string
	if entries, ok := delta[reasoningDetails].([]any); ok {
		for _, detail := range entries {
			parts = append(parts, detailText(detail))
			s.details = appendDetail(s.details, detail)
		}
	}
	detail := strings.Join(parts, "")
	if detail != "" {
		detail = separated(last(s.display), detail)
		s.display = append(s.display, detail)
	}
	if shown := cmp.Or(detail, reasoning); shown != "" {
		s.native, s.delivered = true, true
		if err := s.emit(provider.EventReasoningDelta, shown); err != nil {
			return true, err
		}
		if s.reasoningWatch.Feed(reasoning) || s.detailWatch.Feed(detail) {
			s.runaway = "reasoning"
			return true, nil
		}
	}
	if refusal, ok := delta["refusal"].(string); ok && refusal != "" {
		s.refusal = append(s.refusal, refusal)
	}
	if text := flatten(delta["content"]); text != "" {
		s.content = append(s.content, text)
		if s.textWatch.Feed(text) {
			s.runaway = "content"
			return true, nil
		}
		switch {
		case s.calls.Len() > 0:
			if err := s.suppressed(text); err != nil {
				return true, err
			}
		case len(s.pending) > 0 || transport.MaybeEvents(text) || !s.delivered && shimFollows(strings.Join(s.content, "")):
			s.pending = append(s.pending, text)
			held := strings.Join(s.pending, "")
			if !transport.MaybeEvents(held) && !shimFollows(held) {
				if err := s.flush(); err != nil {
					return true, err
				}
			}
			return false, nil
		default:
			if err := s.text(text); err != nil {
				return true, err
			}
		}
	}
	if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
		if err := s.flush(); err != nil {
			return true, err
		}
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			if call == nil {
				continue
			}
			if name, _ := s.calls.Feed(call); name != "" {
				if err := s.emit(provider.EventToolStart, s.p.unaliased(name)); err != nil {
					return true, err
				}
			}
		}
	}
	return false, nil
}

func last(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// Whether text streamed so far may still be the router's timeout sentinel
func shimFollows(text string) bool {
	return text != "" && strings.HasPrefix(routerShim, strings.TrimLeft(text, " \t\r\n"))
}

// Part types that carry no text
var mediaParts = []string{"image", "image_url", "input_image", "audio", "input_audio"}

// Visible text of content in any shape: a string, or the text of its parts joined
func flatten(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var parts []string
		for _, part := range c {
			if text := partText(part); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	}
	if text := partText(content); text != "" {
		return text
	}
	raw, _ := json.Marshal(content)
	return string(raw)
}

func partText(part any) string {
	switch p := part.(type) {
	case string:
		return p
	case map[string]any:
		kind, _ := p["type"].(string)
		for _, media := range mediaParts {
			if strings.EqualFold(strings.TrimSpace(kind), media) {
				return ""
			}
		}
		for _, key := range []string{"text", "content", "input_text", "output_text", "summary_text"} {
			if text, ok := p[key].(string); ok {
				return text
			}
		}
	}
	return ""
}

// Tool calls of the stream put together, and whether any was cut short so its arguments can't be trusted
func (s *stream) assembled() ([]map[string]any, bool) {
	calls := s.calls.Calls()
	var out []map[string]any
	truncated := false
	for _, slot := range s.calls.Slots() {
		call := calls[slot]
		arguments := call.Arguments
		name := cmp.Or(call.Name, "?")
		if strings.TrimSpace(arguments) != "" {
			if !json.Valid([]byte(arguments)) {
				if s.reason == "" {
					truncated = true
				} else if repaired := transport.RepairArguments(arguments, name); repaired != "{}" {
					arguments = repaired
				} else {
					truncated = true
				}
			}
			if s.reason == "" && transport.Repetitive(arguments) {
				slog.Warn(fmt.Sprintf("Tool call '%s' has repetition-dominated arguments without a finish_reason; treating as a dropped tool call.", name))
				truncated = true
			}
		} else if s.reason == "" {
			truncated = true
		}
		function := map[string]any{"name": call.Name, "arguments": arguments}
		if call.Signature != "" {
			function["thought_signature"] = call.Signature
		}
		entry := map[string]any{"id": call.ID, "type": "function", "function": function}
		if call.Extra != nil {
			entry["extra_content"] = call.Extra
		}
		out = append(out, entry)
	}
	return out, truncated
}

// Reply of the stream once it ended
func (s *stream) finish() (*provider.Response, error) {
	if s.runaway != "" {
		s.reason = "length"
	}
	content := strings.Join(s.content, "")
	reasoning := strings.Join(s.reasoning, "")
	if reasoning == "" && content != "" {
		reasoning = transport.InlineReasoning(content)
	}
	calls, truncated := s.assembled()
	if s.reason == "" && len(s.content) == 0 && len(s.reasoning) == 0 && len(s.refusal) == 0 && s.calls.Len() == 0 {
		return nil, s.p.refused.Empty("The provider sent back an empty reply that never ended, perhaps because of a failure on its side. Send your message again.", nil)
	}
	model := cmp.Or(s.model, s.p.request.Model)
	if truncated && s.reason == "" {
		var dropped []string
		for _, slot := range s.calls.Slots() {
			dropped = append(dropped, cmp.Or(s.calls.calls[slot].Name, "?"))
		}
		slog.Warn(fmt.Sprintf("Clean EOF, no finish_reason: server ended the stream (no transport exception) while a tool call's arguments were "+
			"still incomplete (tools=%v). The server or a proxy closed the stream cleanly; not an output-length truncation.", dropped))
		return s.partial(content, reasoning)
	}
	if s.reason == "" && (len(s.content) > 0 || len(s.reasoning) > 0) && s.calls.Len() == 0 && s.usage == nil {
		slog.Warn("Clean EOF, no finish_reason: server ended the stream (no transport exception) after delivering text with no tool calls. " +
			"The server or a proxy closed the stream cleanly.")
		return s.partial(content, reasoning)
	}
	effective := s.reason
	switch {
	case truncated:
		effective = "length"
	case effective == "":
		effective = "stop"
	}
	if failure := transport.TextError(content, effective, s.header); failure != nil {
		return nil, failure
	}
	message := map[string]any{"role": "assistant"}
	if content != "" {
		message["content"] = content
	}
	if len(calls) > 0 {
		list := make([]any, len(calls))
		for i, call := range calls {
			list[i] = call
		}
		message["tool_calls"] = list
	}
	if reasoning != "" {
		message[reasoningContent] = reasoning
	}
	if refusal := strings.Join(s.refusal, ""); refusal != "" {
		message["refusal"] = refusal
	}
	if len(s.details) > 0 {
		list := make([]any, len(s.details))
		for i, detail := range s.details {
			list[i] = detail
		}
		message[reasoningDetails] = list
	}
	if s.signature != "" {
		message["extra_content"] = map[string]any{"google": map[string]any{"thought_signature": s.signature}}
	}
	for key, value := range s.fields {
		message[key] = value
	}
	for key, parts := range s.texts {
		message[key] = strings.Join(parts, "")
	}
	payload := map[string]any{"model": model, "service_tier": s.tier,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": effective}}}
	if s.usage != nil {
		payload["usage"] = s.usage
	}
	if s.runaway != "" {
		slog.Warn(fmt.Sprintf("Stream cut: the %s turned into a runaway repetition loop (model=%s, provider=%s).", s.runaway, model,
			cmp.Or(s.c.route.Provider, "unknown")))
	}
	if !shim(content, len(calls), s.usage) {
		if err := s.flush(); err != nil {
			return nil, err
		}
	}
	if tail := s.scrub.Flush(); tail != "" && s.calls.Len() == 0 {
		if err := s.emit(provider.EventTextDelta, tail); err != nil {
			return nil, err
		}
	}
	return s.c.normalize(s.p, payload, s.header)
}

// Reply of a stream that ended without saying it finished after it delivered text: what came, as cut off at its length
func (s *stream) partial(content, reasoning string) (*provider.Response, error) {
	if err := s.flush(); err != nil {
		return nil, err
	}
	message := s.c.message(s.p, content, reasoning, nil, carried{})
	var usage *provider.Usage
	if s.usage != nil {
		usage = usageOf(s.usage, s.tier)
	}
	return &provider.Response{Message: message, FinishReason: "length", Usage: usage, Header: s.header}, nil
}
