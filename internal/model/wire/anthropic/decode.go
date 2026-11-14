package anthropic

import (
	"context"
	"encoding/base64"
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

var (
	ErrInvalidResponse = errors.New("anthropic: reply holds no content blocks")
	ErrEventOrder      = errors.New("anthropic: unexpected event order, content arrived before message_start")
)

type ToolInputError struct {
	Tool string
}

func (e *ToolInputError) Error() string {
	return "anthropic: unable to parse the streamed tool parameter JSON for " + e.Tool
}

type decoder struct {
	model   string
	oauth   bool
	invoke  bool
	offered map[string]bool
	sink    wire.Sink

	started     bool
	stopped     bool
	sawEvent    bool
	hasToolUse  bool
	message     *wire.Dict
	usage       *wire.Dict
	startOutput json.RawMessage
	stopReason  json.RawMessage
	stopDetails json.RawMessage
	blocks      map[int]*streamBlock
	toolCount   int
	emitted     int
}

func (d *decoder) observe(event wire.Value, name string) error {
	before := d.emitted
	if err := d.feed(event, name); err != nil {
		return err
	}
	if d.emitted == before {
		d.emit(wire.Event{Kind: wire.EventKeepalive})
	}
	return nil
}

func payloadError(payload []byte, header http.Header) error {
	if apiErr := wire.StreamErrorFromPayload(payload, header); apiErr != nil {
		return apiErr
	}
	return wire.NewStreamError(wire.StreamNonJSON, string(payload))
}

type streamBlock struct {
	typ       string
	start     wire.Value
	text      strings.Builder
	signature string
	signed    bool
	partial   strings.Builder
	citations []json.RawMessage
	toolIndex int
}

func (d *decoder) unprefix(name string) string {
	bare := strings.TrimPrefix(name, mcpPrefix)
	for _, candidate := range []string{name, "mcp_" + bare, bare} {
		if d.offered[candidate] {
			return candidate
		}
	}
	if original, ok := oauthReverseAliases[bare]; ok {
		return original
	}
	return name
}

func GuardrailIntervened(message wire.Value) bool {
	return strings.ToUpper(message.Get(GuardrailActionField).Display()) == "INTERVENED"
}

func (d *decoder) normalize(msg wire.Value, header http.Header) (*wire.Response, error) {
	content := msg.Get("content")
	stop := msg.Get("stop_reason").Text()
	if !content.IsArray() || (len(content.Elems()) == 0 && stop != "end_turn" && stop != "refusal") {
		return nil, ErrInvalidResponse
	}
	var texts, reasoning []string
	var details, ordered []json.RawMessage
	var calls []wire.ToolCall
	signed, usesTool := false, false
	for _, block := range content.Elems() {
		clean := sanitizeReplayBlock(block)
		if clean != nil {
			ordered = append(ordered, clean.JSON())
		}
		btype := block.Get("type").Text()
		switch {
		case btype == "text":
			texts = append(texts, block.Get("text").Text())
		case isThinkingType(btype):
			if btype == "thinking" {
				reasoning = append(reasoning, block.Get("thinking").Text())
			}
			if clean != nil {
				details = append(details, clean.JSON())
				signed = signed || clean.Get("signature").Truthy() || clean.Get("data").Truthy()
			} else if block.IsObject() {
				details = append(details, block.Raw())
			}
		case btype == "tool_use":
			usesTool = true
			name := block.Get("name").Text()
			if d.oauth && strings.HasPrefix(name, mcpPrefix) {
				name = d.unprefix(name)
			}
			input := json.RawMessage("null")
			if v := block.Get("input"); v.Exists() {
				input = v.Raw()
			}
			calls = append(calls, wire.BuildToolCall(block.Get("id").Text(), name, input))
		}
	}
	m := wire.Message{Role: wire.RoleAssistant, Text: strings.Join(texts, "\n"), Reasoning: strings.Join(reasoning, "\n\n"),
		ReasoningDetails: details, ToolCalls: calls}
	if signed && usesTool {
		m.AnthropicBlocks = ordered
	}
	finish := wire.MapFinish(stop, finishReasons)
	if GuardrailIntervened(msg) {
		finish = wire.FinishContentFilter
	}
	r := &wire.Response{Message: m, FinishReason: finish, RawFinish: stop, ID: msg.Get("id").Text(), Model: msg.Get("model").Text(),
		Header: header, ServedModel: wire.ServedModel(header)}
	if r.Model == "" {
		r.Model = d.model
	}
	if details := msg.Get("stop_details"); details.Exists() && details.Kind() != wire.KindNull {
		r.StopDetails = details.Raw()
	}
	if usage := msg.Get("usage"); usage.IsObject() {
		r.Usage = wire.ParseUsage(usage.Raw(), wire.UsageAnthropic)
	}
	return r, nil
}

func (d *decoder) Response(header http.Header, body []byte) (*wire.Response, error) {
	msg, err := wire.ParseValue(body)
	if err != nil || !msg.IsObject() {
		return nil, wire.NewAPIErrorDetail(http.StatusOK, header, "anthropic_invalid_json", "Invalid JSON from the Anthropic Messages API", body)
	}
	if msg.Get("type").Text() == "error" || (msg.Get("error").IsObject() && !msg.Get("content").Exists()) {
		return nil, payloadError(body, header)
	}
	return d.normalize(msg, header)
}

func (d *decoder) Error(status int, header http.Header, body []byte) error {
	if !d.invoke {
		return wire.NewAPIError(status, header, body)
	}
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

func (b *streamBlock) finish(partialInput bool) *wire.Dict {
	out := wire.DictOf(b.start)
	if !out.Has("type") && b.typ != "" {
		out.SetString("type", b.typ)
	}
	switch b.typ {
	case "text":
		out.SetString("text", b.start.Get("text").Text()+b.text.String())
		if len(b.citations) > 0 {
			all := append([]json.RawMessage{}, b.citations...)
			if prior := b.start.Get("citations"); prior.IsArray() {
				all = nil
				for _, c := range prior.Elems() {
					all = append(all, c.Raw())
				}
				all = append(all, b.citations...)
			}
			out.Set("citations", wire.Array(all...))
		}
	case "thinking":
		out.SetString("thinking", b.start.Get("thinking").Text()+b.text.String())
		if b.signed {
			out.SetString("signature", b.signature)
		}
	case "tool_use", "server_tool_use":
		if b.partial.Len() > 0 {
			if v, err := wire.ParseValue([]byte(b.partial.String())); err == nil {
				out.Set("input", v.Raw())
			} else if partialInput {
				out.Set("input", wire.Quote(b.partial.String()))
			}
		}
	}
	return out
}

func (d *decoder) assembled(partialInput bool) wire.Value {
	indexes := make([]int, 0, len(d.blocks))
	for index := range d.blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	content := make([]json.RawMessage, 0, len(indexes))
	for _, index := range indexes {
		content = append(content, d.blocks[index].finish(partialInput).JSON())
	}
	msg := &wire.Dict{}
	if d.message != nil {
		msg = wire.DictOf(d.message.Value())
	}
	msg.Set("content", wire.Array(content...))
	if d.stopReason != nil {
		msg.Set("stop_reason", d.stopReason)
	}
	if d.stopDetails != nil {
		msg.Set("stop_details", d.stopDetails)
	}
	if d.usage != nil {
		msg.Set("usage", d.usage.JSON())
	}
	return msg.Value()
}

func (d *decoder) partial() *wire.Response {
	if !d.started && len(d.blocks) == 0 {
		return &wire.Response{Message: wire.Message{Role: wire.RoleAssistant}, Model: d.model, Streamed: true}
	}
	msg := wire.DictOf(d.assembled(true))
	if !msg.Get("stop_reason").Truthy() {
		msg.SetString("stop_reason", "end_turn")
	}
	r, err := d.normalize(msg.Value(), nil)
	if err != nil || r == nil {
		return &wire.Response{Message: wire.Message{Role: wire.RoleAssistant}, Model: d.model, Streamed: true}
	}
	for i := range r.Message.ToolCalls {
		call := &r.Message.ToolCalls[i]
		if s := ""; json.Unmarshal([]byte(call.Arguments), &s) == nil {
			call.Arguments = s
		}
	}
	r.Streamed = true
	r.FinishReason, r.RawFinish = "", ""
	return r
}

func (d *decoder) feed(event wire.Value, name string) error {
	kind := event.Get("type").Text()
	if kind == "" {
		kind = name
	}
	switch kind {
	case "ping":
		d.emit(wire.Event{Kind: wire.EventKeepalive})
		return nil
	case "error":
		return payloadError(event.Raw(), nil)
	case "message_start":
		d.sawEvent, d.started = true, true
		message := event.Get("message")
		d.message = &wire.Dict{}
		for _, key := range []string{"id", "type", "role", "model"} {
			if v := message.Get(key); v.Exists() {
				d.message.Set(key, v.Raw())
			}
		}
		if v := message.Get(GuardrailActionField); v.Exists() {
			d.message.Set(GuardrailActionField, v.Raw())
		}
		if usage := message.Get("usage"); usage.IsObject() {
			d.usage = wire.DictOf(usage)
		} else {
			d.usage = (&wire.Dict{}).SetInt("input_tokens", 0).SetInt("output_tokens", 0)
		}
		d.startOutput = json.RawMessage("0")
		if v := d.usage.Get("output_tokens"); v.Truthy() {
			d.startOutput = v.Raw()
		}
	case "content_block_start":
		d.sawEvent = true
		if !d.started {
			return ErrEventOrder
		}
		index, ok := event.Get("index").Int()
		block := event.Get("content_block")
		if !ok || !block.IsObject() {
			return nil
		}
		sb := &streamBlock{typ: block.Get("type").Text(), start: block}
		d.blocks[int(index)] = sb
		if sb.typ == "tool_use" {
			d.hasToolUse = true
			sb.toolIndex = d.toolCount
			d.toolCount++
			if name := block.Get("name").Text(); name != "" {
				if d.oauth && strings.HasPrefix(name, mcpPrefix) {
					name = d.unprefix(name)
				}
				d.emit(wire.Event{Kind: wire.EventToolStart, Tool: wire.ToolDelta{Index: sb.toolIndex, ID: block.Get("id").Text(), Name: name}})
			}
		}
	case "content_block_delta":
		d.sawEvent = true
		if !d.started {
			return ErrEventOrder
		}
		index, ok := event.Get("index").Int()
		delta := event.Get("delta")
		if !ok || !delta.IsObject() {
			return nil
		}
		sb := d.blocks[int(index)]
		if sb == nil {
			sb = &streamBlock{start: wire.Value{}}
			d.blocks[int(index)] = sb
		}
		switch delta.Get("type").Text() {
		case "text_delta":
			text := delta.Get("text").Text()
			if sb.typ == "" {
				sb.typ = "text"
			}
			sb.text.WriteString(text)
			if text != "" && !d.hasToolUse {
				d.emit(wire.Event{Kind: wire.EventText, Text: text})
			}
		case "thinking_delta":
			text := delta.Get("thinking").Text()
			if sb.typ == "" {
				sb.typ = "thinking"
			}
			sb.text.WriteString(text)
			if text != "" {
				d.emit(wire.Event{Kind: wire.EventReasoning, Text: text})
			}
		case "signature_delta":
			sb.signature, sb.signed = delta.Get("signature").Text(), true
		case "citations_delta":
			if citation := delta.Get("citation"); citation.Exists() {
				sb.citations = append(sb.citations, citation.Raw())
			}
		case "input_json_delta":
			fragment := delta.Get("partial_json").Text()
			sb.partial.WriteString(fragment)
			if fragment != "" && sb.typ == "tool_use" {
				d.emit(wire.Event{Kind: wire.EventToolArgs, Tool: wire.ToolDelta{Index: sb.toolIndex, ID: sb.start.Get("id").Text(), Args: fragment}})
			}
		}
	case "content_block_stop":
		d.sawEvent = true
		index, ok := event.Get("index").Int()
		if !ok {
			return nil
		}
		if sb := d.blocks[int(index)]; sb != nil && sb.partial.Len() > 0 {
			if _, err := wire.ParseValue([]byte(sb.partial.String())); err != nil {
				return &ToolInputError{Tool: sb.start.Get("name").Text()}
			}
		}
	case "message_delta":
		d.sawEvent = true
		if !d.started {
			return ErrEventOrder
		}
		delta := event.Get("delta")
		if v := delta.Get("stop_reason"); v.Exists() {
			d.stopReason = v.Raw()
		}
		if v := delta.Get("stop_details"); v.Exists() && v.Kind() != wire.KindNull {
			d.stopDetails = v.Raw()
		}
		usage := event.Get("usage")
		if !usage.IsObject() {
			d.usage.Set("output_tokens", d.startOutput)
		} else {
			if v := usage.Get("output_tokens"); v.Exists() && v.Kind() != wire.KindNull {
				d.usage.Set("output_tokens", v.Raw())
			}
			for _, key := range []string{"input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "server_tool_use"} {
				if v := usage.Get(key); v.Exists() && v.Kind() != wire.KindNull {
					d.usage.Set(key, v.Raw())
				}
			}
		}
		d.emit(wire.Event{Kind: wire.EventUsage, Usage: wire.ParseUsage(d.usage.JSON(), wire.UsageAnthropic)})
	case "message_stop":
		d.sawEvent, d.stopped = true, true
	}
	return nil
}

func (d *decoder) ended() (*wire.Response, error) {
	if !d.stopped {
		if !d.sawEvent {
			return nil, wire.ErrEmptyStream
		}
		return d.partial(), wire.ErrStreamDropped
	}
	r, err := d.normalize(d.assembled(false), nil)
	if err != nil {
		return d.partial(), err
	}
	r.Streamed = true
	return r, nil
}

func (d *decoder) Stream(ctx context.Context, body io.Reader, sink wire.Sink) (*wire.Response, error) {
	d.sink = sink
	d.blocks = map[int]*streamBlock{}
	if d.invoke {
		return d.streamFrames(ctx, body)
	}
	reader := wire.NewEventReader(body, 0)
	reader.OnComment = func(string) { d.emit(wire.Event{Kind: wire.EventKeepalive}) }
	for {
		ev, err := reader.Next(ctx)
		if err == io.EOF {
			return d.ended()
		}
		if err != nil {
			return d.partial(), err
		}
		if ev.Done() {
			return d.ended()
		}
		if len(strings.TrimSpace(string(ev.Data))) == 0 {
			continue
		}
		event, perr := wire.ParseValue(ev.Data)
		if perr != nil || !event.IsObject() {
			_, payloadErr := ev.Payload()
			if payloadErr == nil {
				payloadErr = wire.NewStreamError(wire.StreamNonJSON, string(ev.Data))
			}
			return d.partial(), payloadErr
		}
		if err := d.observe(event, ev.Event); err != nil {
			return d.partial(), err
		}
		if d.stopped {
			return d.ended()
		}
	}
}

func frameFault(code, message string) error {
	inner := (&wire.Dict{}).SetString("message", message).SetString("code", code).SetString("type", code)
	return payloadError((&wire.Dict{}).Set("error", inner.JSON()).JSON(), nil)
}

func (d *decoder) streamFrames(ctx context.Context, body io.Reader) (*wire.Response, error) {
	reader := aws.NewFrameReader(body)
	for {
		frame, err := reader.Next(ctx)
		if err == io.EOF {
			return d.ended()
		}
		if err != nil {
			return d.partial(), err
		}
		switch frame.MessageType() {
		case "exception":
			message := ""
			if v, perr := wire.ParseValue(frame.Payload); perr == nil {
				message = v.Get("message").Text()
			}
			return d.partial(), frameFault(frame.ExceptionType(), message)
		case "error":
			return d.partial(), frameFault(frame.ErrorCode(), frame.ErrorMessage())
		}
		if frame.EventType() != "chunk" {
			continue
		}
		chunk, perr := wire.ParseValue(frame.Payload)
		if perr != nil {
			return d.partial(), wire.NewStreamError(wire.StreamNonJSON, string(frame.Payload))
		}
		data, derr := base64.StdEncoding.DecodeString(chunk.Get("bytes").Text())
		if derr != nil {
			return d.partial(), wire.NewStreamError(wire.StreamNonJSON, chunk.Get("bytes").Text())
		}
		event, perr := wire.ParseValue(data)
		if perr != nil || !event.IsObject() {
			return d.partial(), wire.NewStreamError(wire.StreamNonJSON, string(data))
		}
		if err := d.observe(event, ""); err != nil {
			return d.partial(), err
		}
		if d.stopped {
			return d.ended()
		}
	}
}

func accessDenied(err error, text string) bool {
	var apiErr *wire.APIError
	if errors.As(err, &apiErr) && apiErr.Code() != "" {
		return apiErr.Code() == "AccessDeniedException" || apiErr.Code() == "UnauthorizedException"
	}
	return strings.Contains(text, "not authorized") || strings.Contains(text, "accessdenied")
}

func StreamUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrEventOrder) {
		return true
	}
	text := strings.ToLower(err.Error())
	var apiErr *wire.APIError
	if errors.As(err, &apiErr) {
		text += " " + strings.ToLower(apiErr.Message())
	}
	if strings.Contains(text, "stream") && strings.Contains(text, "not supported") {
		return true
	}
	if strings.Contains(text, "unexpected event order") {
		return true
	}
	if !strings.Contains(text, "invokemodelwithresponsestream") {
		return false
	}
	return accessDenied(err, text)
}

func IsToolInputError(err error) bool {
	var target *ToolInputError
	return errors.As(err, &target)
}
