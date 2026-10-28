package protocol

import (
	"encoding/json"
	"fmt"
	"iter"
	"sort"
	"strings"
)

// Why the model stopped, normalized across protocols with the wire value kept beside it
type StopReason string

const (
	StopEnd           StopReason = "stop"
	StopLength        StopReason = "length"
	StopToolUse       StopReason = "tool_use"
	StopContentFilter StopReason = "content_filter"
	// Wire value with no neutral meaning, see Response.RawStopReason
	StopOther StopReason = "other"
)

// Token counts normalized so Input excludes cached tokens and Output includes reasoning
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
	Reasoning  int `json:"reasoning"`
}

// Tokens the request occupied on the input side, cached or not
func (u Usage) Context() int { return u.Input + u.CacheRead + u.CacheWrite }

// Every token the exchange consumed on either side
func (u Usage) Total() int { return u.Context() + u.Output }

// Sum of two usages field by field
func (u Usage) Add(o Usage) Usage {
	return Usage{
		Input:      u.Input + o.Input,
		Output:     u.Output + o.Output,
		CacheRead:  u.CacheRead + o.CacheRead,
		CacheWrite: u.CacheWrite + o.CacheWrite,
		Reasoning:  u.Reasoning + o.Reasoning,
	}
}

// Completed assistant turn
type Response struct {
	ID            string
	Model         string
	Content       []Part
	StopReason    StopReason
	RawStopReason string
	Usage         Usage
}

type EventType string

const (
	EventStart               EventType = "start"
	EventTextStart           EventType = "text_start"
	EventTextDelta           EventType = "text_delta"
	EventTextEnd             EventType = "text_end"
	EventThinkingStart       EventType = "thinking_start"
	EventThinkingDelta       EventType = "thinking_delta"
	EventThinkingEnd         EventType = "thinking_end"
	EventToolCallStart       EventType = "tool_call_start"
	EventToolCallDelta       EventType = "tool_call_delta"
	EventToolCallEnd         EventType = "tool_call_end"
	EventServerToolCallStart EventType = "server_tool_call_start"
	EventServerToolCallDelta EventType = "server_tool_call_delta"
	EventServerToolCallEnd   EventType = "server_tool_call_end"
	EventServerToolResult    EventType = "server_tool_result"
	EventCitation            EventType = "citation"
	EventUsage               EventType = "usage"
	EventDone                EventType = "done"
)

// Incremental update of an assistant turn with the response assembled so far
type Event struct {
	Type EventType
	// Position of the affected part in Response.Content
	Index int
	// Text, thinking or tool argument fragment on delta events
	Delta string
	// Every argument fragment received so far on tool call delta events
	Partial string
	// Initial part on start events and the complete part on end events
	Part Part
	// Source attached to the text part at Index on citation events
	Citation Citation
	Response Response
}

type blockKind int

const (
	blockText blockKind = iota
	blockThinking
	blockToolCall
	blockServerToolCall
)

type block struct {
	index int
	kind  blockKind
	args  strings.Builder
}

// Assembles neutral events and the final Response from protocol specific stream fragments
type Builder struct {
	Response Response
	// Protocol stamped on signed parts so other protocols know not to replay them
	Origin  ID
	started bool
	open    map[string]*block
	queue   []Event
}

// Records the response identity and emits the start event once
func (b *Builder) Start(id, model string) {
	if id != "" {
		b.Response.ID = id
	}
	if model != "" {
		b.Response.Model = model
	}
	if b.started {
		return
	}
	b.started = true
	b.emit(Event{Type: EventStart})
}

// Reports whether a block under key is still receiving deltas
func (b *Builder) IsOpen(key string) bool {
	_, ok := b.open[key]
	return ok
}

func (b *Builder) OpenText(key string) {
	i := b.add(key, blockText, Text{})
	b.emit(Event{Type: EventTextStart, Index: i, Part: b.Response.Content[i]})
}

func (b *Builder) OpenThinking(key string, redacted bool) {
	i := b.add(key, blockThinking, Thinking{Redacted: redacted, Origin: b.Origin})
	b.emit(Event{Type: EventThinkingStart, Index: i, Part: b.Response.Content[i]})
}

func (b *Builder) OpenToolCall(key, id, name string) {
	i := b.add(key, blockToolCall, ToolCall{ID: id, Name: name})
	b.emit(Event{Type: EventToolCallStart, Index: i, Part: b.Response.Content[i]})
}

func (b *Builder) OpenServerToolCall(key, id, name string) {
	i := b.add(key, blockServerToolCall, ServerToolCall{ID: id, Name: name, Origin: b.Origin})
	b.emit(Event{Type: EventServerToolCallStart, Index: i, Part: b.Response.Content[i]})
}

// Appends a vendor side tool outcome as a complete part
func (b *Builder) AddServerToolResult(p ServerToolResult) {
	b.Start("", "")
	p.Origin = b.Origin
	i := len(b.Response.Content)
	b.Response.Content = append(b.Response.Content, p)
	b.emit(Event{Type: EventServerToolResult, Index: i, Part: p})
}

// Appends a text, thinking or tool argument fragment to the block under key
func (b *Builder) Delta(key, delta string) error {
	blk, ok := b.open[key]
	if !ok {
		return fmt.Errorf("protocol: delta for unopened block %q", key)
	}
	i := blk.index
	switch blk.kind {
	case blockText:
		p := b.Response.Content[i].(Text)
		p.Text += delta
		b.Response.Content[i] = p
		b.emit(Event{Type: EventTextDelta, Index: i, Delta: delta})
	case blockThinking:
		p := b.Response.Content[i].(Thinking)
		p.Text += delta
		b.Response.Content[i] = p
		b.emit(Event{Type: EventThinkingDelta, Index: i, Delta: delta})
	case blockToolCall:
		blk.args.WriteString(delta)
		b.emit(Event{Type: EventToolCallDelta, Index: i, Delta: delta, Partial: blk.args.String()})
	case blockServerToolCall:
		blk.args.WriteString(delta)
		b.emit(Event{Type: EventServerToolCallDelta, Index: i, Delta: delta, Partial: blk.args.String()})
	}
	return nil
}

// Appends replay signature bytes to the thinking or tool call block under key
func (b *Builder) Signature(key, signature string) error {
	blk, ok := b.open[key]
	if !ok {
		return fmt.Errorf("protocol: signature for unopened block %q", key)
	}
	switch p := b.Response.Content[blk.index].(type) {
	case Thinking:
		p.Signature += signature
		p.Origin = b.Origin
		b.Response.Content[blk.index] = p
	case ToolCall:
		p.Signature += signature
		p.Origin = b.Origin
		b.Response.Content[blk.index] = p
	default:
		return fmt.Errorf("protocol: signature on text block %q", key)
	}
	return nil
}

// Attaches a source citation to the text block under key
func (b *Builder) Cite(key string, c Citation) error {
	blk, ok := b.open[key]
	if !ok {
		return fmt.Errorf("protocol: citation for unopened block %q", key)
	}
	p, isText := b.Response.Content[blk.index].(Text)
	if !isText {
		return fmt.Errorf("protocol: citation on non text block %q", key)
	}
	p.Citations = append(p.Citations, c)
	b.Response.Content[blk.index] = p
	b.emit(Event{Type: EventCitation, Index: blk.index, Citation: c})
	return nil
}

// Records usage known before the stream ends and emits a usage event
func (b *Builder) Usage(u Usage) {
	b.Start("", "")
	b.Response.Usage = u
	b.emit(Event{Type: EventUsage})
}

// Finalizes the block under key, parsing tool arguments into a JSON object
func (b *Builder) Close(key string) error {
	blk, ok := b.open[key]
	if !ok {
		return fmt.Errorf("protocol: close of unopened block %q", key)
	}
	delete(b.open, key)
	i := blk.index
	switch blk.kind {
	case blockText:
		b.emit(Event{Type: EventTextEnd, Index: i, Part: b.Response.Content[i]})
	case blockThinking:
		b.emit(Event{Type: EventThinkingEnd, Index: i, Part: b.Response.Content[i]})
	case blockToolCall:
		p := b.Response.Content[i].(ToolCall)
		args, err := ParseArgs(blk.args.String())
		if err != nil {
			return fmt.Errorf("protocol: tool call %s: %w", p.Name, err)
		}
		p.Args = args
		b.Response.Content[i] = p
		b.emit(Event{Type: EventToolCallEnd, Index: i, Part: p})
	case blockServerToolCall:
		p := b.Response.Content[i].(ServerToolCall)
		args, err := ParseArgs(blk.args.String())
		if err != nil {
			return fmt.Errorf("protocol: server tool call %s: %w", p.Name, err)
		}
		p.Args = args
		b.Response.Content[i] = p
		b.emit(Event{Type: EventServerToolCallEnd, Index: i, Part: p})
	}
	return nil
}

// Closes every open block in order, records the outcome and emits the done event
func (b *Builder) Finish(stop StopReason, raw string, usage Usage) error {
	b.Start("", "")
	keys := make([]string, 0, len(b.open))
	for k := range b.open {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return b.open[keys[i]].index < b.open[keys[j]].index })
	for _, k := range keys {
		if err := b.Close(k); err != nil {
			return err
		}
	}
	b.Response.StopReason = stop
	b.Response.RawStopReason = raw
	b.Response.Usage = usage
	b.emit(Event{Type: EventDone})
	return nil
}

// Returns and clears the events produced since the previous call
func (b *Builder) Events() []Event {
	out := b.queue
	b.queue = nil
	return out
}

func (b *Builder) add(key string, kind blockKind, p Part) int {
	b.Start("", "")
	if b.open == nil {
		b.open = map[string]*block{}
	}
	i := len(b.Response.Content)
	b.Response.Content = append(b.Response.Content, p)
	b.open[key] = &block{index: i, kind: kind}
	return i
}

func (b *Builder) emit(ev Event) {
	ev.Response = b.Response
	ev.Response.Content = append([]Part(nil), b.Response.Content...)
	b.queue = append(b.queue, ev)
}

// Validates streamed tool arguments as a JSON object, treating no text as empty arguments
func ParseArgs(raw string) (json.RawMessage, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return json.RawMessage("{}"), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return nil, fmt.Errorf("arguments are not a JSON object: %w", err)
	}
	if obj == nil {
		return nil, fmt.Errorf("arguments are not a JSON object: null")
	}
	return json.RawMessage(raw), nil
}

// Drains a stream and returns the response carried by its done event
func Collect(events iter.Seq2[Event, error]) (Response, error) {
	for ev, err := range events {
		if err != nil {
			return Response{}, err
		}
		if ev.Type == EventDone {
			return ev.Response, nil
		}
	}
	return Response{}, fmt.Errorf("protocol: stream ended without a done event")
}

// Concatenated text parts of a response in order
func (r Response) Text() string {
	var sb strings.Builder
	for _, p := range r.Content {
		if t, ok := p.(Text); ok {
			sb.WriteString(t.Text)
		}
	}
	return sb.String()
}

// Tokens an image or document is taken to cost in an estimate
const attachmentTokens = 1000

// Rough tokens the parts carry, text and arguments at four bytes a token and attachments at a flat figure
func EstimateTokens(parts []Part) int {
	chars := 0
	tokens := 0
	for _, p := range parts {
		switch v := p.(type) {
		case Text:
			chars += len(v.Text)
		case Thinking:
			chars += len(v.Text)
		case ToolCall:
			chars += len(v.Name) + len(v.Args)
		case ToolResult:
			tokens += EstimateTokens(v.Content)
		case Image, Document:
			tokens += attachmentTokens
		}
	}
	return tokens + chars/4
}

// Tool calls the model asked for in order
func (r Response) ToolCalls() []ToolCall {
	var out []ToolCall
	for _, p := range r.Content {
		if c, ok := p.(ToolCall); ok {
			out = append(out, c)
		}
	}
	return out
}
