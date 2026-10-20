package bedrockconversestream

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"sort"
	"strconv"
	"strings"
	"time"

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

var transientTypes = map[string]bool{
	"throttlingException":         true,
	"serviceUnavailableException": true,
	"modelStreamErrorException":   true,
	"modelTimeoutException":       true,
	"internalServerException":     true,
}

var imageMediaTypes = map[string]string{
	"png":  "image/png",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
	"webp": "image/webp",
}

var videoMediaTypes = map[string]string{
	"three_gp": "video/3gpp",
	"mpg":      "video/mpeg",
	"mkv":      "video/x-matroska",
	"mov":      "video/quicktime",
	"flv":      "video/x-flv",
	"wmv":      "video/x-ms-wmv",
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

func mapMetrics(m metrics) protocol.Metrics {
	return protocol.Metrics{Latency: time.Duration(m.LatencyMs) * time.Millisecond}
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
	resp := protocol.Response{StopReason: mapStop(wire.StopReason), RawStopReason: wire.StopReason, Usage: mapUsage(wire.Usage), Metrics: mapMetrics(wire.Metrics)}
	for i, block := range wire.Output.Message.Content {
		if block.CitationsContent != nil {
			resp.Content = attachCitations(resp.Content, *block.CitationsContent)
			continue
		}
		part, err := decodeBlock(block)
		if err != nil {
			return protocol.Response{}, fmt.Errorf("bedrock-converse-stream: content block %d: %w", i, err)
		}
		resp.Content = append(resp.Content, part)
	}
	return resp, nil
}

// Appends cited text and its citations to the preceding text part, or starts one
func attachCitations(parts []protocol.Part, cc citationsContent) []protocol.Part {
	var sb strings.Builder
	for _, c := range cc.Content {
		sb.WriteString(c.Text)
	}
	cites := make([]protocol.Citation, 0, len(cc.Citations))
	for _, c := range cc.Citations {
		cites = append(cites, mapCitation(c))
	}
	if n := len(parts); n > 0 {
		if t, ok := parts[n-1].(protocol.Text); ok {
			t.Text += sb.String()
			t.Citations = append(t.Citations, cites...)
			parts[n-1] = t
			return parts
		}
	}
	return append(parts, protocol.Text{Text: sb.String(), Citations: cites})
}

// Flattens the char, page and chunk location shapes onto one neutral citation
func mapCitation(c citation) protocol.Citation {
	out := protocol.Citation{Title: c.Title}
	var quoted []string
	for _, s := range c.SourceContent {
		quoted = append(quoted, s.Text)
	}
	out.Cited = strings.Join(quoted, "")
	for _, span := range []*documentSpan{c.Location.DocumentChar, c.Location.DocumentPage, c.Location.DocumentChunk} {
		if span != nil {
			out.Document = strconv.Itoa(span.DocumentIndex)
			out.Start, out.End = span.Start, span.End
			break
		}
	}
	return out
}

func decodeBlock(block contentBlock) (protocol.Part, error) {
	switch {
	case block.Text != nil:
		return protocol.Text{Text: *block.Text}, nil
	case block.ReasoningContent != nil:
		rc := block.ReasoningContent
		if rc.ReasoningText != nil {
			return protocol.Thinking{Text: rc.ReasoningText.Text, Signature: rc.ReasoningText.Signature, Origin: protocol.BedrockConverseStream}, nil
		}
		if rc.RedactedContent != "" {
			if err := validRedacted(rc.RedactedContent); err != nil {
				return nil, err
			}
			return protocol.Thinking{Redacted: true, Signature: rc.RedactedContent, Origin: protocol.BedrockConverseStream}, nil
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
	case block.Image != nil:
		mediaType, ok := imageMediaTypes[block.Image.Format]
		if !ok {
			return nil, fmt.Errorf("image format %q has no media type", block.Image.Format)
		}
		if len(block.Image.Source.Bytes) == 0 {
			return nil, fmt.Errorf("image source has no bytes")
		}
		return protocol.Image{MediaType: mediaType, Data: block.Image.Source.Bytes}, nil
	case block.Document != nil:
		mediaType, ok := documentMediaType(block.Document.Format)
		if !ok {
			return nil, fmt.Errorf("document format %q has no media type", block.Document.Format)
		}
		if len(block.Document.Source.Bytes) == 0 {
			return nil, fmt.Errorf("document %q source has no bytes", block.Document.Name)
		}
		return protocol.Document{MediaType: mediaType, Data: block.Document.Source.Bytes, Name: block.Document.Name}, nil
	case block.Video != nil:
		if len(block.Video.Source.Bytes) == 0 {
			return nil, fmt.Errorf("video source has no bytes")
		}
		return protocol.Document{MediaType: videoMediaType(block.Video.Format), Data: block.Video.Source.Bytes}, nil
	}
	return nil, fmt.Errorf("unsupported content block")
}

// Reverses the DocumentFormat mapping used on encode
func documentMediaType(format string) (string, bool) {
	for mediaType, f := range documentFormats {
		if f == format {
			return mediaType, true
		}
	}
	return "", false
}

func videoMediaType(format string) string {
	if mediaType, ok := videoMediaTypes[format]; ok {
		return mediaType
	}
	return "video/" + format
}

// Rejects a redactedContent payload that is not base64 the way the wire defines the blob
func validRedacted(s string) error {
	if _, err := base64.StdEncoding.DecodeString(s); err != nil {
		return fmt.Errorf("redactedContent is not base64: %w", err)
	}
	return nil
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

// Tracks open blocks, pending redacted fragments and the stop reason while frames arrive
type streamState struct {
	b          protocol.Builder
	blocks     map[int]openBlock
	redacted   map[int]*strings.Builder
	stopReason string
	stopped    bool
	finished   bool
}

// Parses an event stream body, finishing on the metadata event
func (Protocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		st := &streamState{b: protocol.Builder{Origin: protocol.BedrockConverseStream}, blocks: map[int]openBlock{}, redacted: map[int]*strings.Builder{}}
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
		typ := f.String(":exception-type")
		return &protocol.Error{Status: 200, Type: typ, Message: body.Message, Body: f.Payload, InStream: true, Transient: transientTypes[typ]}
	case "error":
		code := f.String(":error-code")
		return &protocol.Error{Status: 200, Type: code, Message: f.String(":error-message"), Body: f.Payload, InStream: true, Transient: transientTypes[code]}
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
		return st.closeBlock(ev.ContentBlockIndex)
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
		if err := st.closeAll(); err != nil {
			return err
		}
		st.finished = true
		st.b.Metrics(mapMetrics(ev.Metrics))
		return st.b.Finish(mapStop(st.stopReason), st.stopReason, mapUsage(ev.Usage))
	}
	return fmt.Errorf("bedrock-converse-stream: unknown event type %q", name)
}

// Finalizes block index, validating joined redacted fragments as base64 before signing
func (st *streamState) closeBlock(index int) error {
	delete(st.blocks, index)
	if frag, ok := st.redacted[index]; ok {
		delete(st.redacted, index)
		if err := validRedacted(frag.String()); err != nil {
			return fmt.Errorf("bedrock-converse-stream: block %d: %w", index, err)
		}
		if err := st.b.Signature(key(index), frag.String()); err != nil {
			return err
		}
	}
	return st.b.Close(key(index))
}

func (st *streamState) closeAll() error {
	indexes := make([]int, 0, len(st.blocks))
	for i := range st.blocks {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		if err := st.closeBlock(i); err != nil {
			return err
		}
	}
	return nil
}

func (st *streamState) delta(index int, d contentBlockDelta) error {
	k := key(index)
	switch {
	case d.Text != nil:
		if err := st.open(index, kindText, false); err != nil {
			return err
		}
		return st.b.Delta(k, *d.Text)
	case d.Citation != nil:
		if err := st.open(index, kindText, false); err != nil {
			return err
		}
		return st.b.Cite(k, mapCitation(*d.Citation))
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
			frag, ok := st.redacted[index]
			if !ok {
				frag = &strings.Builder{}
				st.redacted[index] = frag
			}
			frag.WriteString(rc.RedactedContent)
			return nil
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
	e := &protocol.Error{Status: status, Body: body, Transient: transientStatus(status)}
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
	e.Transient = e.Transient || transientTypes[lowerFirst(e.Type)]
	return e
}

// Reports statuses that denote a timeout, conflict, throttle or server failure
func transientStatus(status int) bool {
	return status == 408 || status == 409 || status == 429 || status >= 500
}

// Matches the camel cased exception names the stream uses against the PascalCase HTTP forms
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
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
