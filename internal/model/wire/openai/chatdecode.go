package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/model/wire"
)

// Sole message of a router that answers an upstream connect timeout with HTTP 200
const routerTimeoutShim = "Connect timeout, please try again later."

// Reads chat completions replies with the aliases and carrier keys its request used
type chatDecoder struct {
	aliases  map[string]string
	carriers wire.CarrierKeys
}

// Reports whether streamed text is still a prefix of the router timeout notice and must be held back
func RouterTimeoutShimMayFollow(text string) bool {
	return text != "" && strings.HasPrefix(routerTimeoutShim, strings.TrimLeftFunc(text, unicode.IsSpace))
}

// Reports whether a reply is the router timeout notice with no tool calls and no proof of generated tokens
func IsRouterTimeoutShim(r *wire.Response) bool {
	if r == nil || strip(r.Message.Text) != routerTimeoutShim || r.Message.Parts != nil || len(r.Message.ToolCalls) > 0 {
		return false
	}
	return r.Usage == nil || r.Usage.OutputTokens <= 0
}

// Typed failure for a 4xx or 5xx chat reply
func (d *chatDecoder) Error(status int, header http.Header, body []byte) error {
	return wire.NewAPIError(status, header, body)
}

// Text of message content a server sent as an object or a list instead of a string
func coerceContent(v wire.Value) string {
	switch v.Kind() {
	case wire.KindObject:
		for _, key := range [...]string{"text", "content"} {
			if f := v.Get(key); f.Truthy() {
				return f.Display()
			}
		}
		text, _ := wire.FormatJSON(v.Raw(), wire.TextLayout{Spaced: true})
		return text
	case wire.KindArray:
		var parts []string
		for _, p := range v.Elems() {
			if s, ok := p.Str(); ok {
				parts = append(parts, s)
			} else if p.IsObject() && (p.Get("type").Text() == "text" || p.Get("text").Exists()) {
				parts = append(parts, p.Get("text").Display())
			}
		}
		return strings.Join(parts, "\n")
	}
	return v.Display()
}

// One tool call of a reply message, false for an entry without a function name
func (d *chatDecoder) toolCall(tc wire.Value) (wire.ToolCall, bool) {
	fn := tc.Get("function")
	name := fn.Get("name")
	if fn.Kind() <= wire.KindNull || name.Kind() <= wire.KindNull {
		return wire.ToolCall{}, false
	}
	call := wire.ToolCall{Name: d.unalias(name.Display()), Arguments: "{}"}
	if id := tc.Get("id"); id.Kind() > wire.KindNull {
		call.ID = id.Display()
	}
	if args := fn.Get("arguments"); args.IsString() {
		call.Arguments = args.Text()
	} else if args.Kind() > wire.KindNull {
		call.Arguments = wire.BuildToolCall("", "", args.Raw()).Arguments
	}
	if extra := tc.Get("extra_content"); extra.Kind() > wire.KindNull {
		call.ExtraContent = slices.Clone(extra.Raw())
	}
	if sig, ok := fn.Get("thought_signature").Str(); ok {
		call.FunctionSignature = sig
	}
	return call, true
}

// Name the agent knows a tool under, undoing only an alias this request issued
func (d *chatDecoder) unalias(name string) string {
	if original, ok := d.aliases[name]; ok {
		return original
	}
	return name
}

// Finish word of a choice, a numeric reason as its digits, stop when the provider sent none
func finishOf(v wire.Value) (wire.FinishReason, string) {
	raw := ""
	if v.Kind() > wire.KindNull {
		raw = v.Display()
	}
	if fr := wire.NormalizeFinish(raw); fr != "" {
		return fr, raw
	}
	return wire.FinishStop, raw
}

// Normalized reply for one message object with its finish reason, usage and identity
func (d *chatDecoder) normalize(msg wire.Value, finish wire.FinishReason, rawFinish string, calls []wire.ToolCall) *wire.Response {
	m := wire.Message{Role: wire.RoleAssistant, ToolCalls: calls}
	content := msg.Get("content")
	if content.Kind() > wire.KindNull {
		m.Text = coerceContent(content)
	}
	if rc, ok := msg.Get("reasoning_content").Str(); ok {
		m.ReasoningContent = &rc
	}
	if details := msg.Get("reasoning_details"); details.IsArray() && len(details.Elems()) > 0 {
		for _, e := range details.Elems() {
			m.ReasoningDetails = append(m.ReasoningDetails, slices.Clone(e.Raw()))
		}
	}
	readable := d.carriers.Capture(&m, msg.Raw())
	if refusal, ok := msg.Get("refusal").Str(); ok && strip(refusal) != "" {
		m.Refusal = refusal
		if strip(m.Text) == "" && len(calls) == 0 {
			m.Text = refusal
			if finish == wire.FinishStop {
				finish = wire.FinishContentFilter
			}
		}
	}
	if reasoning := msg.Get("reasoning"); reasoning.Truthy() {
		m.Reasoning = reasoning.Display()
	} else {
		m.Reasoning = readable
	}
	return &wire.Response{Message: m, FinishReason: finish, RawFinish: rawFinish}
}

// Assembles a complete chat completions reply body
func (d *chatDecoder) Response(header http.Header, body []byte) (*wire.Response, error) {
	v, err := wire.ParseValue(body)
	if err != nil {
		return nil, wire.NewStreamError(wire.StreamNonJSON, string(body))
	}
	choices := v.Get("choices").Elems()
	if len(choices) == 0 {
		if v.Get("error").Truthy() {
			return nil, errors.Join(ErrInvalidResponse, wire.StreamErrorFromPayload(v.Raw(), header))
		}
		return nil, ErrInvalidResponse
	}
	msg := choices[0].Get("message")
	var calls []wire.ToolCall
	for _, tc := range msg.Get("tool_calls").Elems() {
		if call, ok := d.toolCall(tc); ok {
			calls = append(calls, call)
		}
	}
	finish, raw := finishOf(choices[0].Get("finish_reason"))
	r := d.normalize(msg, finish, raw, calls)
	r.Usage = wire.ParseUsage(v.Get("usage").Raw(), wire.UsageChat)
	r.ID, r.Model, r.Upstream = v.Get("id").Text(), v.Get("model").Text(), v.Get("provider").Text()
	r.Header, r.ServedModel = header, wire.ServedModel(header)
	if len(choices) == 1 && IsRouterTimeoutShim(r) {
		return r, ErrRouterTimeoutShim
	}
	return r, nil
}

// One tool call under assembly from streamed deltas
type callSlot struct {
	id, name  string
	args      []string
	extra     json.RawMessage
	signature string
	notified  bool
	scanned   int
	depth     int
	inString  bool
	escaped   bool
	started   bool
}

// Assembles streamed tool call deltas, a fresh id at a reused index opening a slot only for a new call
type callAccumulator struct {
	slots  map[int]*callSlot
	lastID map[int]string
	active map[int]int
}

// Reports whether the arguments of a slot have balanced brackets outside strings
func (s *callSlot) closed() bool {
	for _, part := range s.args[s.scanned:] {
		for i := 0; i < len(part); i++ {
			c := part[i]
			switch {
			case s.inString && s.escaped:
				s.escaped = false
			case s.inString && c == '\\':
				s.escaped = true
			case s.inString:
				s.inString = c != '"'
			case c == '"':
				s.inString = true
			case c == '{' || c == '[':
				s.depth, s.started = s.depth+1, true
			case c == '}' || c == ']':
				s.depth--
			}
		}
	}
	s.scanned = len(s.args)
	return s.started && s.depth == 0 && !s.inString
}

// Merges one delta and returns its slot index, with the tool name the first time the call has one
func (a *callAccumulator) feed(tc wire.Value) (slot int, started string, fragment string) {
	raw := 0
	if n, ok := tc.Get("index").Int(); ok {
		raw = int(n)
	}
	id := ""
	if v := tc.Get("id"); v.Truthy() {
		id = v.Display()
	}
	fn := tc.Get("function")
	name := ""
	if v := fn.Get("name"); v.Truthy() {
		name = v.Display()
	}
	if _, seen := a.active[raw]; !seen {
		a.active[raw] = raw
	}
	if last, seen := a.lastID[raw]; id != "" && seen && id != last && name != "" {
		cur := a.slots[a.active[raw]]
		if cur == nil || name != cur.name || cur.closed() {
			next := 0
			for idx := range a.slots {
				next = max(next, idx+1)
			}
			a.active[raw] = next
		}
	}
	if id != "" {
		a.lastID[raw] = id
	}
	slot = a.active[raw]
	s := a.slots[slot]
	if s == nil {
		s = &callSlot{}
		a.slots[slot] = s
	}
	if id != "" {
		s.id = id
	}
	if name != "" {
		s.name = name
	}
	if args, ok := fn.Get("arguments").Str(); ok && args != "" {
		s.args = append(s.args, args)
		fragment = args
	}
	if sig, ok := fn.Get("thought_signature").Str(); ok && sig != "" {
		s.signature = sig
	}
	if extra := tc.Get("extra_content"); extra.Kind() > wire.KindNull {
		s.extra = slices.Clone(extra.Raw())
	}
	if s.name != "" && !s.notified {
		s.notified = true
		started = s.name
	}
	return slot, started, fragment
}

// Finished tool calls in slot order, flagging arguments a dropped or capped stream left incomplete
func (a *callAccumulator) assemble(d *chatDecoder, finished bool) (calls []wire.ToolCall, truncated bool) {
	order := make([]int, 0, len(a.slots))
	for idx := range a.slots {
		order = append(order, idx)
	}
	slices.Sort(order)
	for _, idx := range order {
		s := a.slots[idx]
		args := strings.Join(s.args, "")
		repaired := false
		switch {
		case strip(args) != "":
			if !json.Valid([]byte(args)) {
				if !finished {
					truncated = true
				} else if fixed := wire.RepairArguments(args, orUnknown(s.name)); fixed != "{}" {
					args, repaired = fixed, true
				} else {
					truncated = true
				}
			}
		case !finished:
			truncated = true
		}
		calls = append(calls, wire.ToolCall{ID: s.id, Name: d.unalias(s.name), Arguments: args, ExtraContent: s.extra,
			FunctionSignature: s.signature, Repaired: repaired})
	}
	return calls, truncated
}

// Tool name for a log line, a question mark for a call that never named one
func orUnknown(name string) string {
	if name == "" {
		return "?"
	}
	return name
}

// Assembly state of one chat completions stream
type chatStream struct {
	d         *chatDecoder
	sink      wire.Sink
	content   []string
	reasoning []string
	display   []string
	refusal   []string
	pending   []string
	details   wire.DetailAccumulator
	calls     callAccumulator
	carriers  *wire.StreamCarriers
	finish    wire.FinishReason
	rawFinish string
	model     string
	id        string
	upstream  string
	usage     *wire.Usage
	sentText  bool
}

// Delivers one event stamped with the time it was read
func (s *chatStream) emit(e wire.Event) {
	if s.sink != nil {
		e.At = time.Now()
		s.sink(e)
	}
}

// Releases held-back text to the sink
func (s *chatStream) flush() {
	for _, text := range s.pending {
		s.emit(wire.Event{Kind: wire.EventText, Text: text})
		s.sentText = true
	}
	s.pending = nil
}

// Prompt reading progress of a chunk that carries nothing else, false for a real chunk
func promptProgress(v wire.Value) (wire.Progress, bool) {
	p := v.Get("prompt_progress")
	if !p.IsObject() {
		return wire.Progress{}, false
	}
	for _, choice := range v.Get("choices").Elems() {
		if choice.Get("finish_reason").Truthy() {
			return wire.Progress{}, false
		}
		delta := choice.Get("delta")
		for _, field := range [...]string{"content", "tool_calls", "reasoning_content", "reasoning", "refusal"} {
			if delta.Get(field).Truthy() {
				return wire.Progress{}, false
			}
		}
	}
	out := wire.Progress{Stage: "prompt"}
	out.Done, _ = p.Get("processed").Int()
	out.Total, _ = p.Get("total").Int()
	if out.Total > 0 {
		out.Percent = math.Max(0, math.Min(100, math.RoundToEven(float64(out.Done)/float64(out.Total)*100)))
	}
	if ms, ok := p.Get("time_ms").Int(); ok {
		out.ServerTime, out.Timed = time.Duration(ms)*time.Millisecond, true
	}
	return out, true
}

// Absorbs one stream chunk, returning a provider failure the chunk carried
func (s *chatStream) feed(v wire.Value) error {
	if v.Get("error").Truthy() {
		return wire.StreamErrorFromPayload(v.Raw(), nil)
	}
	if progress, ok := promptProgress(v); ok {
		s.emit(wire.Event{Kind: wire.EventProgress, Progress: progress})
		return nil
	}
	if model := v.Get("model"); model.Truthy() {
		s.model = model.Display()
	}
	if id, ok := v.Get("id").Str(); ok && id != "" && s.id == "" {
		s.id = id
	}
	if provider, ok := v.Get("provider").Str(); ok && provider != "" && s.upstream == "" {
		s.upstream = provider
	}
	usage := wire.ParseUsage(v.Get("usage").Raw(), wire.UsageChat)
	if usage != nil {
		s.usage = usage
		s.emit(wire.Event{Kind: wire.EventUsage, Usage: usage})
	}
	choices := v.Get("choices").Elems()
	if len(choices) == 0 {
		errType, errMsg := v.Get("error_type"), v.Get("error_message")
		if errType.Truthy() || errMsg.Truthy() {
			code, message := errType.Display(), errMsg.Display()
			if !errType.Truthy() {
				code = "provider_in_stream_error"
			}
			if !errMsg.Truthy() {
				message = string(v.Raw())
			}
			body := (&wire.Dict{}).SetString("code", code).SetString("message", message)
			return wire.StreamErrorFromPayload(body.JSON(), nil)
		}
		last := v.Get("lastOne")
		if n, _ := last.Int(); (last.True() || n == 1 || last.Text() == "true") && s.rawFinish == "" {
			s.finish, s.rawFinish = wire.FinishStop, "stop"
		}
		return nil
	}
	choice := choices[0]
	delta := choice.Get("delta")
	if fr := choice.Get("finish_reason"); fr.Kind() > wire.KindNull {
		if normalized := wire.NormalizeFinish(fr.Display()); normalized != "" {
			s.finish, s.rawFinish = normalized, fr.Display()
		}
	}
	reasoning, unset := delta.Get("reasoning_content").Text(), false
	if reasoning == "" {
		plain := delta.Get("reasoning")
		reasoning, unset = plain.Text(), plain.Kind() <= wire.KindNull
	}
	if readable := s.carriers.Feed(delta.Raw()); unset {
		reasoning = readable
	}
	if reasoning != "" {
		previous := ""
		if n := len(s.reasoning); n > 0 {
			previous = s.reasoning[n-1]
		}
		reasoning = wire.SeparateGluedReasoning(previous, reasoning)
		s.reasoning = append(s.reasoning, reasoning)
	}
	var detailText strings.Builder
	for _, rd := range delta.Get("reasoning_details").Elems() {
		detailText.WriteString(wire.DetailText(rd.Raw()))
		s.details.Add(rd.Raw())
	}
	shown := detailText.String()
	if shown != "" {
		previous := ""
		if n := len(s.display); n > 0 {
			previous = s.display[n-1]
		}
		shown = wire.SeparateGluedReasoning(previous, shown)
		s.display = append(s.display, shown)
	} else {
		shown = reasoning
	}
	if shown != "" {
		s.emit(wire.Event{Kind: wire.EventReasoning, Text: shown})
	}
	if refusal, ok := delta.Get("refusal").Str(); ok && refusal != "" {
		s.refusal = append(s.refusal, refusal)
		s.emit(wire.Event{Kind: wire.EventRefusal, Text: refusal})
	}
	held := false
	if text := wire.FlattenContent(delta.Get("content").Raw(), ""); delta.Get("content").Kind() > wire.KindNull && text != "" {
		s.content = append(s.content, text)
		switch {
		case len(s.calls.slots) > 0:
			s.emit(wire.Event{Kind: wire.EventText, Text: text})
		case len(s.pending) > 0 || wire.StreamTextMayBeSSE(text) ||
			(!s.sentText && RouterTimeoutShimMayFollow(strings.Join(s.content, ""))):
			s.pending = append(s.pending, text)
			joined := strings.Join(s.pending, "")
			if !wire.StreamTextMayBeSSE(joined) && !RouterTimeoutShimMayFollow(joined) {
				s.flush()
			}
			held = true
		default:
			s.emit(wire.Event{Kind: wire.EventText, Text: text})
			s.sentText = true
		}
	}
	if deltas := delta.Get("tool_calls").Elems(); len(deltas) > 0 && !held {
		s.flush()
		for _, tc := range deltas {
			slot, started, fragment := s.calls.feed(tc)
			if started != "" {
				s.emit(wire.Event{Kind: wire.EventToolStart, Tool: wire.ToolDelta{Index: slot, ID: s.calls.slots[slot].id, Name: started}})
			}
			if fragment != "" {
				s.emit(wire.Event{Kind: wire.EventToolArgs, Tool: wire.ToolDelta{Index: slot, Args: fragment}})
			}
		}
	}
	return nil
}

// Reply assembled from what arrived so far, for a stream that failed or ended early
func (s *chatStream) partial() *wire.Response {
	calls, _ := s.calls.assemble(s.d, false)
	m := wire.Message{Role: wire.RoleAssistant, Text: strings.Join(s.content, ""), ToolCalls: calls}
	if reasoning := strings.Join(s.reasoning, ""); reasoning != "" {
		m.ReasoningContent = &reasoning
	}
	return &wire.Response{Message: m, Usage: s.usage, ID: s.id, Model: s.model, Upstream: s.upstream, Streamed: true}
}

// Final reply of a stream that reached its end, or the partial one with the reason the turn is unfinished
func (s *chatStream) result() (*wire.Response, error) {
	finished := s.rawFinish != ""
	calls, truncated := s.calls.assemble(s.d, finished)
	hasText := len(s.content) > 0 || len(s.reasoning) > 0
	if !finished && !hasText && len(s.refusal) == 0 && len(s.calls.slots) == 0 {
		return nil, wire.ErrEmptyStream
	}
	if truncated && !finished {
		names := make([]string, len(calls))
		for i := range calls {
			names[i] = orUnknown(calls[i].Name)
		}
		slog.Warn("stream ended cleanly without a finish reason while a tool call's arguments were incomplete", "tools", names)
		return s.partial(), wire.ErrStreamDropped
	}
	if !finished && hasText && len(s.calls.slots) == 0 && s.usage == nil {
		slog.Warn("stream ended cleanly without a finish reason after delivering text with no tool calls")
		return s.partial(), wire.ErrStreamDropped
	}
	finish, raw := s.finish, s.rawFinish
	switch {
	case truncated:
		finish = wire.FinishLength
	case !finished:
		finish = wire.FinishStop
	}
	content := strings.Join(s.content, "")
	if apiErr := wire.StreamErrorFromText(content, string(finish), nil); apiErr != nil {
		return s.partial(), apiErr
	}
	msg := &wire.Dict{}
	if content != "" {
		msg.SetString("content", content)
	}
	if reasoning := strings.Join(s.reasoning, ""); reasoning != "" {
		msg.SetString("reasoning_content", reasoning)
	}
	if refusal := strings.Join(s.refusal, ""); refusal != "" {
		msg.SetString("refusal", refusal)
	}
	r := s.d.normalize(msg.Value(), finish, raw, calls)
	r.Message.ReasoningDetails = s.details.Details()
	s.carriers.Apply(&r.Message)
	for _, key := range s.d.carriers.Text {
		if text := r.Message.Carriers[key]; r.Message.Reasoning == "" && text != "" {
			r.Message.Reasoning = text
		}
	}
	r.Usage, r.Model, r.Upstream, r.Streamed = s.usage, s.model, s.upstream, true
	if r.ID = s.id; r.ID == "" {
		r.ID = "stream-" + uuid4()
	}
	if IsRouterTimeoutShim(r) {
		return r, ErrRouterTimeoutShim
	}
	s.flush()
	return r, nil
}

// Assembles a streamed chat completions reply, delivering events to sink as chunks arrive
func (d *chatDecoder) Stream(ctx context.Context, body io.Reader, sink wire.Sink) (*wire.Response, error) {
	s := &chatStream{d: d, sink: sink, carriers: d.carriers.Stream(),
		calls: callAccumulator{slots: map[int]*callSlot{}, lastID: map[int]string{}, active: map[int]int{}}}
	reader := wire.NewEventReader(body, 0)
	reader.OnComment = func(string) { s.emit(wire.Event{Kind: wire.EventKeepalive}) }
	for {
		ev, err := reader.Next(ctx)
		if err == io.EOF || (err == nil && ev.Done()) {
			return s.result()
		}
		if err != nil {
			return s.partial(), err
		}
		payload, err := ev.Payload()
		if err != nil {
			return s.partial(), err
		}
		v, err := wire.ParseValue(payload)
		if err != nil {
			return s.partial(), err
		}
		if err := s.feed(v); err != nil {
			return s.partial(), err
		}
	}
}
