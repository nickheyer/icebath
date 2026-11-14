package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth/aws"
	"github.com/nickheyer/icebath/internal/model/wire"
)

var ErrInvalidResponse = errors.New("bedrock: reply holds no output")

var finishReasons = map[string]wire.FinishReason{
	"end_turn": wire.FinishStop, "stop_sequence": wire.FinishStop, "tool_use": wire.FinishToolCalls, "max_tokens": wire.FinishLength,
	"content_filtered": wire.FinishContentFilter, "guardrail_intervened": wire.FinishContentFilter,
}

type decoder struct {
	model   string
	sink    wire.Sink
	emitted int
}

type reasoningPart struct {
	text      string
	signature string
	redacted  string
}

type orderedBlock struct {
	kind      string
	text      string
	reasoning reasoningPart
	toolID    string
	toolName  string
	input     json.RawMessage
}

type parts struct {
	texts    []string
	redacted []json.RawMessage
	calls    []wire.ToolCall
}

func (b *orderedBlock) json() json.RawMessage {
	switch b.kind {
	case "reasoning":
		r := &wire.Dict{}
		if b.reasoning.text != "" {
			r.SetString("text", b.reasoning.text)
		}
		if b.reasoning.signature != "" {
			r.SetString("signature", b.reasoning.signature)
		}
		if b.reasoning.redacted != "" {
			r.SetString("redactedContentBase64", b.reasoning.redacted)
		}
		return (&wire.Dict{}).Set("reasoningContent", r.JSON()).JSON()
	case "tool":
		use := (&wire.Dict{}).SetString("toolUseId", b.toolID).SetString("name", b.toolName).Set("input", b.input)
		return (&wire.Dict{}).Set("toolUse", use.JSON()).JSON()
	}
	return textBlock(b.text).JSON()
}

func (b *orderedBlock) empty() bool {
	return b.kind == "reasoning" && b.reasoning == (reasoningPart{})
}

func (p *parts) absorb(reasoning wire.Value, into *reasoningPart, onText func(string)) {
	if !reasoning.IsObject() {
		return
	}
	text, signature := reasoning.Get("text"), reasoning.Get("signature")
	if nested := reasoning.Get("reasoningText"); nested.IsObject() {
		if v := nested.Get("text"); v.Exists() {
			text = v
		}
		if v := nested.Get("signature"); v.Exists() {
			signature = v
		}
	}
	if text.Truthy() {
		s := text.Display()
		if onText != nil {
			onText(s)
		}
		into.text += s
	}
	if s, ok := signature.Str(); ok && s != "" {
		into.signature += s
	}
	if encoded, ok := reasoning.Get("redactedContent").Str(); ok && encoded != "" {
		p.redacted = append(p.redacted, (&wire.Dict{}).SetString("type", "redacted_thinking").SetString("data", encoded).JSON())
		into.redacted = encoded
	}
}

func (d *decoder) build(p *parts, ordered []*orderedBlock, usage wire.Value, stopReason, model string, header http.Header) *wire.Response {
	var blocks, details []json.RawMessage
	var reasoning []string
	for _, b := range ordered {
		blocks = append(blocks, b.json())
		if b.kind != "reasoning" {
			continue
		}
		if b.reasoning.text != "" {
			detail := (&wire.Dict{}).SetString("type", "thinking").SetString("thinking", b.reasoning.text)
			if b.reasoning.signature != "" {
				detail.SetString("signature", b.reasoning.signature)
			}
			details = append(details, detail.JSON())
			reasoning = append(reasoning, b.reasoning.text)
		}
		if b.reasoning.redacted != "" {
			details = append(details, (&wire.Dict{}).SetString("type", "redacted_thinking").SetString("data", b.reasoning.redacted).JSON())
		}
	}
	if len(details) == 0 {
		details = p.redacted
	}
	if len(details) > 0 && d.model != "" {
		details = append(details, (&wire.Dict{}).SetString("type", IssuerType).SetString("model", d.model).JSON())
	}
	m := wire.Message{Role: wire.RoleAssistant, Text: strings.Join(p.texts, "\n"), ToolCalls: p.calls, ReasoningDetails: details,
		Reasoning: strings.Join(reasoning, "\n\n"), BedrockBlocks: blocks}
	finish := wire.MapFinish(stopReason, finishReasons)
	if len(p.calls) > 0 && finish == wire.FinishStop {
		finish = wire.FinishToolCalls
	}
	count := func(key string) int {
		n, _ := usage.Get(key).Int()
		return int(n)
	}
	r := &wire.Response{Message: m, FinishReason: finish, RawFinish: stopReason, Model: model, Header: header, ServedModel: wire.ServedModel(header)}
	if r.Model == "" {
		r.Model = d.model
	}
	if usage.IsObject() {
		r.Usage = &wire.Usage{InputTokens: count("inputTokens"), OutputTokens: count("outputTokens"),
			CacheReadTokens: count("cacheReadInputTokens"), CacheWriteTokens: count("cacheWriteInputTokens"), Requests: 1}
	}
	return r
}

func (d *decoder) Response(header http.Header, body []byte) (*wire.Response, error) {
	resp, err := wire.ParseValue(body)
	if err != nil || !resp.IsObject() {
		return nil, wire.NewAPIErrorDetail(http.StatusOK, header, "bedrock_invalid_json", "Invalid JSON from the Bedrock Converse API", body)
	}
	if !resp.Get("output").Exists() {
		return nil, ErrInvalidResponse
	}
	p := &parts{}
	var ordered []*orderedBlock
	for _, block := range resp.Path("output", "message", "content").Elems() {
		switch {
		case block.Get("text").Exists():
			text := block.Get("text").Display()
			p.texts = append(p.texts, text)
			ordered = append(ordered, &orderedBlock{kind: "text", text: text})
		case block.Get("reasoningContent").Exists():
			b := &orderedBlock{kind: "reasoning"}
			p.absorb(block.Get("reasoningContent"), &b.reasoning, nil)
			if !b.empty() {
				ordered = append(ordered, b)
			}
		case block.Get("toolUse").Exists():
			use := block.Get("toolUse")
			input := json.RawMessage("{}")
			if v := use.Get("input"); v.Exists() {
				input = v.Raw()
			}
			b := &orderedBlock{kind: "tool", toolID: use.Get("toolUseId").Text(), toolName: use.Get("name").Text(), input: input}
			ordered = append(ordered, b)
			p.calls = append(p.calls, wire.BuildToolCall(b.toolID, b.toolName, input))
		}
	}
	stop := "end_turn"
	if v := resp.Get("stopReason"); v.Exists() {
		stop = v.Text()
	}
	usage := resp.Get("usage")
	if !usage.Exists() {
		usage, _ = wire.ParseValue([]byte("{}"))
	}
	return d.build(p, ordered, usage, stop, resp.Get("modelId").Text(), header), nil
}

func (d *decoder) Error(status int, header http.Header, body []byte) error {
	code := header.Get("x-amzn-errortype")
	if i := strings.IndexByte(code, ':'); i >= 0 {
		code = code[:i]
	}
	message := ""
	if v, err := wire.ParseValue(body); err == nil {
		message = v.Get("message").Text()
		if message == "" {
			message = v.Get("Message").Text()
		}
		if code == "" {
			code = v.Get("__type").Text()
		}
	}
	return wire.NewAPIErrorDetail(status, header, strings.TrimSpace(code), message, body)
}

func (d *decoder) emit(e wire.Event) {
	d.emitted++
	if d.sink != nil {
		e.At = time.Now()
		d.sink(e)
	}
}

type streamState struct {
	p           parts
	blocks      map[int]*orderedBlock
	current     int
	hasCurrent  bool
	tool        *orderedBlock
	toolIndex   int
	toolCount   int
	toolJSON    strings.Builder
	textBuffer  strings.Builder
	hasToolUse  bool
	stopReason  string
	stopped     bool
	usage       wire.Value
	sawEvent    bool
	bufferedAny bool
}

func (s *streamState) index(payload wire.Value, newBlock bool) int {
	if n, ok := payload.Get("contentBlockIndex").Int(); ok && payload.Get("contentBlockIndex").Kind() == wire.KindNumber {
		return int(n)
	}
	if newBlock || !s.hasCurrent {
		return len(s.blocks)
	}
	return s.current
}

func (s *streamState) flushText() {
	if s.bufferedAny {
		s.p.texts = append(s.p.texts, s.textBuffer.String())
		s.textBuffer.Reset()
		s.bufferedAny = false
	}
}

func (s *streamState) ordered() []*orderedBlock {
	indexes := make([]int, 0, len(s.blocks))
	for index := range s.blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	out := make([]*orderedBlock, 0, len(indexes))
	for _, index := range indexes {
		out = append(out, s.blocks[index])
	}
	return out
}

func (d *decoder) feed(s *streamState, kind string, payload wire.Value) {
	s.sawEvent = true
	switch kind {
	case "contentBlockStart":
		idx := s.index(payload, true)
		s.current, s.hasCurrent = idx, true
		if use := payload.Path("start", "toolUse"); use.Exists() {
			s.hasToolUse = true
			s.flushText()
			s.tool = &orderedBlock{kind: "tool", toolID: use.Get("toolUseId").Text(), toolName: use.Get("name").Text(), input: json.RawMessage("{}")}
			s.toolJSON.Reset()
			s.toolIndex = s.toolCount
			s.toolCount++
			s.blocks[idx] = s.tool
			d.emit(wire.Event{Kind: wire.EventToolStart, Tool: wire.ToolDelta{Index: s.toolIndex, ID: s.tool.toolID, Name: s.tool.toolName}})
		}
	case "contentBlockDelta":
		idx := s.index(payload, false)
		s.current, s.hasCurrent = idx, true
		delta := payload.Get("delta")
		switch {
		case delta.Get("text").Exists():
			text := delta.Get("text").Display()
			block := s.blocks[idx]
			if block == nil {
				block = &orderedBlock{kind: "text"}
				s.blocks[idx] = block
			}
			block.text += text
			s.textBuffer.WriteString(text)
			s.bufferedAny = true
			if !s.hasToolUse {
				d.emit(wire.Event{Kind: wire.EventText, Text: text})
			}
		case delta.Get("toolUse").Exists() && s.tool != nil:
			fragment := delta.Path("toolUse", "input").Text()
			s.toolJSON.WriteString(fragment)
			if fragment != "" {
				d.emit(wire.Event{Kind: wire.EventToolArgs, Tool: wire.ToolDelta{Index: s.toolIndex, ID: s.tool.toolID, Args: fragment}})
			}
		case delta.Get("reasoningContent").Exists():
			reasoning := delta.Get("reasoningContent")
			redacted, _ := reasoning.Get("redactedContent").Str()
			if !reasoning.IsObject() || !(reasoning.Get("text").Truthy() || reasoning.Get("signature").Truthy() || redacted != "") {
				return
			}
			block := s.blocks[idx]
			if block == nil {
				block = &orderedBlock{kind: "reasoning"}
				s.blocks[idx] = block
			}
			block.kind = "reasoning"
			s.p.absorb(reasoning, &block.reasoning, func(text string) { d.emit(wire.Event{Kind: wire.EventReasoning, Text: text}) })
		}
	case "contentBlockStop":
		s.hasCurrent = false
		if s.tool == nil {
			s.flushText()
			return
		}
		input := json.RawMessage("{}")
		if v, err := wire.ParseValue([]byte(s.toolJSON.String())); err == nil {
			input = v.Raw()
		}
		s.tool.input = input
		s.p.calls = append(s.p.calls, wire.BuildToolCall(s.tool.toolID, s.tool.toolName, input))
		s.tool = nil
	case "messageStop":
		s.stopped, s.stopReason = true, "end_turn"
		if v := payload.Get("stopReason"); v.Exists() {
			s.stopReason = v.Text()
		}
	case "metadata":
		meta := payload.Get("usage")
		usage := &wire.Dict{}
		for _, key := range []string{"inputTokens", "outputTokens", "cacheReadInputTokens", "cacheWriteInputTokens"} {
			n, _ := meta.Get(key).Int()
			usage.SetInt(key, int(n))
		}
		s.usage = usage.Value()
		count := func(key string) int {
			n, _ := s.usage.Get(key).Int()
			return int(n)
		}
		d.emit(wire.Event{Kind: wire.EventUsage, Usage: &wire.Usage{InputTokens: count("inputTokens"), OutputTokens: count("outputTokens"),
			CacheReadTokens: count("cacheReadInputTokens"), CacheWriteTokens: count("cacheWriteInputTokens"), Requests: 1}})
	}
}

func (d *decoder) finish(s *streamState, partial bool) *wire.Response {
	if partial && s.tool != nil {
		s.p.calls = append(s.p.calls, wire.ToolCall{ID: s.tool.toolID, Name: s.tool.toolName, Arguments: s.toolJSON.String()})
		s.tool = nil
	}
	s.flushText()
	usage := s.usage
	if !usage.Exists() {
		usage, _ = wire.ParseValue([]byte("{}"))
	}
	stop := s.stopReason
	if stop == "" {
		stop = "end_turn"
	}
	r := d.build(&s.p, s.ordered(), usage, stop, "", nil)
	r.Streamed = true
	if partial {
		r.FinishReason, r.RawFinish = "", ""
		if !s.usage.Exists() {
			r.Usage = nil
		}
	}
	return r
}

func streamFault(code, message string) error {
	inner := (&wire.Dict{}).SetString("message", message).SetString("code", code).SetString("type", code)
	payload := (&wire.Dict{}).Set("error", inner.JSON()).JSON()
	if apiErr := wire.StreamErrorFromPayload(payload, nil); apiErr != nil {
		return apiErr
	}
	return wire.NewStreamError(wire.StreamNonJSON, string(payload))
}

func (d *decoder) Stream(ctx context.Context, body io.Reader, sink wire.Sink) (*wire.Response, error) {
	d.sink = sink
	s := &streamState{blocks: map[int]*orderedBlock{}}
	reader := aws.NewFrameReader(body)
	for {
		frame, err := reader.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return d.finish(s, true), err
		}
		switch frame.MessageType() {
		case "exception":
			message := ""
			if v, perr := wire.ParseValue(frame.Payload); perr == nil {
				message = v.Get("message").Text()
			}
			return d.finish(s, true), streamFault(frame.ExceptionType(), message)
		case "error":
			return d.finish(s, true), streamFault(frame.ErrorCode(), frame.ErrorMessage())
		}
		payload, perr := wire.ParseValue(frame.Payload)
		if perr != nil {
			return d.finish(s, true), wire.NewStreamError(wire.StreamNonJSON, string(frame.Payload))
		}
		before := d.emitted
		d.feed(s, frame.EventType(), payload)
		if d.emitted == before {
			d.emit(wire.Event{Kind: wire.EventKeepalive})
		}
	}
	if !s.stopped {
		if !s.sawEvent {
			return nil, wire.ErrEmptyStream
		}
		return d.finish(s, true), wire.ErrStreamDropped
	}
	return d.finish(s, false), nil
}
