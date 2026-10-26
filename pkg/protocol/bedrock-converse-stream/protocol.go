// Amazon Bedrock Converse and ConverseStream wire protocol, paths relative to the regional bedrock-runtime host
package bedrockconversestream

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
)

type Protocol struct{}

var _ protocol.Protocol = Protocol{}

func init() {
	protocol.Register(Protocol{})
}

func (Protocol) ID() protocol.ID { return protocol.BedrockConverseStream }

type request struct {
	Messages        []message        `json:"messages,omitempty"`
	System          []systemBlock    `json:"system,omitempty"`
	InferenceConfig *inferenceConfig `json:"inferenceConfig,omitempty"`
	ToolConfig      *toolConfig      `json:"toolConfig,omitempty"`
}

type message struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

// Union with exactly one member set
type systemBlock struct {
	Text       *string     `json:"text,omitempty"`
	CachePoint *cachePoint `json:"cachePoint,omitempty"`
}

type cachePoint struct {
	Type string `json:"type"`
}

// Union with exactly one member set
type contentBlock struct {
	Text             *string           `json:"text,omitempty"`
	Image            *imageBlock       `json:"image,omitempty"`
	Document         *documentBlock    `json:"document,omitempty"`
	Video            *videoBlock       `json:"video,omitempty"`
	ToolUse          *toolUseBlock     `json:"toolUse,omitempty"`
	ToolResult       *toolResultBlock  `json:"toolResult,omitempty"`
	ReasoningContent *reasoningContent `json:"reasoningContent,omitempty"`
	CitationsContent *citationsContent `json:"citationsContent,omitempty"`
}

type imageBlock struct {
	Format string     `json:"format"`
	Source byteSource `json:"source"`
}

type documentBlock struct {
	Format string     `json:"format"`
	Name   string     `json:"name"`
	Source byteSource `json:"source"`
}

type videoBlock struct {
	Format string     `json:"format"`
	Source byteSource `json:"source"`
}

// Bytes serialize as base64 per the restJson1 blob rule
type byteSource struct {
	Bytes []byte `json:"bytes"`
}

type toolUseBlock struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type toolResultBlock struct {
	ToolUseID string              `json:"toolUseId"`
	Content   []toolResultContent `json:"content"`
	Status    string              `json:"status,omitempty"`
}

type toolResultContent struct {
	Text     *string        `json:"text,omitempty"`
	Image    *imageBlock    `json:"image,omitempty"`
	Document *documentBlock `json:"document,omitempty"`
}

type reasoningContent struct {
	ReasoningText   *reasoningText `json:"reasoningText,omitempty"`
	RedactedContent string         `json:"redactedContent,omitempty"`
}

type reasoningText struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
}

// Generated text with the citations backing it
type citationsContent struct {
	Content   []citationText `json:"content"`
	Citations []citation     `json:"citations"`
}

type citationText struct {
	Text string `json:"text"`
}

type citation struct {
	Title         string           `json:"title"`
	SourceContent []citationText   `json:"sourceContent"`
	Location      citationLocation `json:"location"`
}

// Union over the char, page and chunk location shapes
type citationLocation struct {
	DocumentChar  *documentSpan `json:"documentChar"`
	DocumentPage  *documentSpan `json:"documentPage"`
	DocumentChunk *documentSpan `json:"documentChunk"`
}

type documentSpan struct {
	DocumentIndex int `json:"documentIndex"`
	Start         int `json:"start"`
	End           int `json:"end"`
}

type inferenceConfig struct {
	MaxTokens     int      `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

type toolConfig struct {
	Tools      []tool      `json:"tools"`
	ToolChoice *toolChoice `json:"toolChoice,omitempty"`
}

// Union with exactly one member set
type tool struct {
	ToolSpec   *toolSpec   `json:"toolSpec,omitempty"`
	CachePoint *cachePoint `json:"cachePoint,omitempty"`
}

type toolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema toolInputSchema `json:"inputSchema"`
}

type toolInputSchema struct {
	JSON json.RawMessage `json:"json"`
}

type toolChoice struct {
	Auto *struct{}     `json:"auto,omitempty"`
	Any  *struct{}     `json:"any,omitempty"`
	Tool *specificTool `json:"tool,omitempty"`
}

type specificTool struct {
	Name string `json:"name"`
}

type response struct {
	Output     converseOutput `json:"output"`
	StopReason string         `json:"stopReason"`
	Usage      tokenUsage     `json:"usage"`
	Metrics    metrics        `json:"metrics"`
}

type converseOutput struct {
	Message message `json:"message"`
}

type tokenUsage struct {
	InputTokens           int `json:"inputTokens"`
	OutputTokens          int `json:"outputTokens"`
	CacheReadInputTokens  int `json:"cacheReadInputTokens"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
}

type metrics struct {
	LatencyMs int64 `json:"latencyMs"`
}

type contentBlockStartEvent struct {
	ContentBlockIndex int               `json:"contentBlockIndex"`
	Start             contentBlockStart `json:"start"`
}

type contentBlockStart struct {
	ToolUse *toolUseStart `json:"toolUse"`
}

type toolUseStart struct {
	ToolUseID string `json:"toolUseId"`
	Name      string `json:"name"`
}

type contentBlockDeltaEvent struct {
	ContentBlockIndex int               `json:"contentBlockIndex"`
	Delta             contentBlockDelta `json:"delta"`
}

type contentBlockDelta struct {
	Text             *string         `json:"text"`
	ToolUse          *toolUseDelta   `json:"toolUse"`
	ReasoningContent *reasoningDelta `json:"reasoningContent"`
	Citation         *citation       `json:"citation"`
}

type toolUseDelta struct {
	Input string `json:"input"`
}

type reasoningDelta struct {
	Text            *string `json:"text"`
	Signature       *string `json:"signature"`
	RedactedContent string  `json:"redactedContent"`
}

type contentBlockStopEvent struct {
	ContentBlockIndex int `json:"contentBlockIndex"`
}

type messageStopEvent struct {
	StopReason string `json:"stopReason"`
}

type metadataEvent struct {
	Usage   tokenUsage `json:"usage"`
	Metrics metrics    `json:"metrics"`
}

type exceptionBody struct {
	Message string `json:"message"`
}

type errorBody struct {
	Type    string `json:"__type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Extra key that turns on prompt caching, consumed rather than merged into the body
const cacheKey = "bedrock_cache"

var imageFormats = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpeg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

var documentFormats = map[string]string{
	"application/pdf":  "pdf",
	"text/plain":       "txt",
	"text/markdown":    "md",
	"text/csv":         "csv",
	"text/html":        "html",
	"application/json": "json",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "docx",
	"application/msword":       "doc",
	"application/vnd.ms-excel": "xls",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": "xlsx",
}

// Builds the Converse or ConverseStream call with the model id escaped into the path
func (Protocol) Encode(req protocol.Request) (protocol.Call, error) {
	if req.Model == "" {
		return protocol.Call{}, fmt.Errorf("bedrock-converse-stream: model is required")
	}
	cache, extra, err := consumeCache(req.Extra)
	if err != nil {
		return protocol.Call{}, err
	}
	wire := request{}
	if req.System != "" {
		wire.System = []systemBlock{{Text: &req.System}}
		if cache {
			wire.System = append(wire.System, systemBlock{CachePoint: &cachePoint{Type: "default"}})
		}
	}
	enc := &encoder{}
	for i, m := range req.Messages {
		msg, keep, err := enc.message(m)
		if err != nil {
			return protocol.Call{}, fmt.Errorf("bedrock-converse-stream: message %d: %w", i, err)
		}
		if keep {
			wire.Messages = append(wire.Messages, msg)
		}
	}
	if req.MaxTokens > 0 || req.Temperature != nil || len(req.StopSequences) > 0 {
		wire.InferenceConfig = &inferenceConfig{MaxTokens: req.MaxTokens, Temperature: req.Temperature, StopSequences: req.StopSequences}
	}
	cfg, err := encodeToolConfig(req.Tools, req.ToolChoice, cache)
	if err != nil {
		return protocol.Call{}, err
	}
	wire.ToolConfig = cfg
	body, err := protocol.MarshalBody(wire, extra)
	if err != nil {
		return protocol.Call{}, err
	}
	op := "converse"
	if req.Stream {
		op = "converse-stream"
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return protocol.Call{
		Method: http.MethodPost,
		Path:   "/model/" + escapePath(req.Model) + "/" + op,
		Header: header,
		Body:   body,
	}, nil
}

// Reads and removes the caching switch so the remaining extra fields merge into the body
func consumeCache(extra map[string]json.RawMessage) (bool, map[string]json.RawMessage, error) {
	raw, ok := extra[cacheKey]
	if !ok {
		return false, extra, nil
	}
	var on bool
	if err := json.Unmarshal(raw, &on); err != nil {
		return false, nil, fmt.Errorf("bedrock-converse-stream: %s must be a boolean: %w", cacheKey, err)
	}
	rest := make(map[string]json.RawMessage, len(extra)-1)
	for k, v := range extra {
		if k != cacheKey {
			rest[k] = v
		}
	}
	return on, rest, nil
}

// Numbers generated document names across one request
type encoder struct {
	docs int
}

// Maps one turn to a wire message, reporting false when the replay policy left no content
func (e *encoder) message(m protocol.Message) (message, bool, error) {
	if len(m.Content) == 0 {
		return message{}, false, fmt.Errorf("empty content")
	}
	switch m.Role {
	case protocol.RoleUser:
		msg, err := e.parts("user", m.Content, e.userPart)
		return msg, true, err
	case protocol.RoleAssistant:
		msg, err := e.parts("assistant", m.Content, e.assistantPart)
		return msg, len(msg.Content) > 0, err
	case protocol.RoleTool:
		msg, err := e.parts("user", m.Content, e.toolResultPart)
		return msg, true, err
	}
	return message{}, false, fmt.Errorf("unknown role %q", m.Role)
}

// Collects the blocks each part maps to, skipping parts the encoder reports as omitted
func (e *encoder) parts(role string, parts []protocol.Part, encode func(protocol.Part) (*contentBlock, error)) (message, error) {
	msg := message{Role: role}
	for _, p := range parts {
		block, err := encode(p)
		if err != nil {
			return message{}, err
		}
		if block != nil {
			msg.Content = append(msg.Content, *block)
		}
	}
	return msg, nil
}

func (e *encoder) userPart(p protocol.Part) (*contentBlock, error) {
	switch v := p.(type) {
	case protocol.Text:
		return &contentBlock{Text: &v.Text}, nil
	case protocol.Image:
		img, err := encodeImage(v)
		if err != nil {
			return nil, err
		}
		return &contentBlock{Image: img}, nil
	case protocol.Document:
		doc, err := e.document(v)
		if err != nil {
			return nil, err
		}
		return &contentBlock{Document: doc}, nil
	}
	return nil, fmt.Errorf("%T is not allowed in a user message", p)
}

func (e *encoder) assistantPart(p protocol.Part) (*contentBlock, error) {
	switch v := p.(type) {
	case protocol.Text:
		return &contentBlock{Text: &v.Text}, nil
	case protocol.Thinking:
		if !protocol.Replayable(v.Origin, protocol.BedrockConverseStream) {
			return nil, nil
		}
		if v.Redacted {
			if v.Signature == "" {
				return nil, fmt.Errorf("redacted thinking has no redactedContent payload")
			}
			return &contentBlock{ReasoningContent: &reasoningContent{RedactedContent: v.Signature}}, nil
		}
		return &contentBlock{ReasoningContent: &reasoningContent{ReasoningText: &reasoningText{Text: v.Text, Signature: v.Signature}}}, nil
	case protocol.ToolCall:
		args, err := protocol.ParseArgs(string(v.Args))
		if err != nil {
			return nil, fmt.Errorf("tool call %s: %w", v.Name, err)
		}
		return &contentBlock{ToolUse: &toolUseBlock{ToolUseID: v.ID, Name: v.Name, Input: args}}, nil
	case protocol.ServerToolCall, protocol.ServerToolResult:
		return nil, nil
	}
	return nil, fmt.Errorf("%T is not allowed in an assistant message", p)
}

func (e *encoder) toolResultPart(p protocol.Part) (*contentBlock, error) {
	r, ok := p.(protocol.ToolResult)
	if !ok {
		return nil, fmt.Errorf("%T is not allowed in a tool message", p)
	}
	block := &toolResultBlock{ToolUseID: r.CallID, Content: []toolResultContent{}, Status: "success"}
	if r.IsError {
		block.Status = "error"
	}
	for _, c := range r.Content {
		switch v := c.(type) {
		case protocol.Text:
			block.Content = append(block.Content, toolResultContent{Text: &v.Text})
		case protocol.Image:
			img, err := encodeImage(v)
			if err != nil {
				return nil, err
			}
			block.Content = append(block.Content, toolResultContent{Image: img})
		case protocol.Document:
			doc, err := e.document(v)
			if err != nil {
				return nil, err
			}
			block.Content = append(block.Content, toolResultContent{Document: doc})
		default:
			return nil, fmt.Errorf("%T is not allowed in a tool result", c)
		}
	}
	return &contentBlock{ToolResult: block}, nil
}

func encodeImage(img protocol.Image) (*imageBlock, error) {
	format, ok := imageFormats[img.MediaType]
	if !ok {
		return nil, fmt.Errorf("image media type %q has no ImageFormat value", img.MediaType)
	}
	if len(img.Data) == 0 {
		return nil, fmt.Errorf("image has no bytes")
	}
	return &imageBlock{Format: format, Source: byteSource{Bytes: img.Data}}, nil
}

// Maps the media type onto a DocumentFormat value and names unnamed documents by number
func (e *encoder) document(d protocol.Document) (*documentBlock, error) {
	format, ok := documentFormats[d.MediaType]
	if !ok {
		return nil, fmt.Errorf("document media type %q has no DocumentFormat value", d.MediaType)
	}
	if len(d.Data) == 0 {
		return nil, fmt.Errorf("document %q has no bytes", d.Name)
	}
	name := d.Name
	if name == "" {
		e.docs++
		name = fmt.Sprintf("document-%d", e.docs)
	}
	return &documentBlock{Format: format, Name: name, Source: byteSource{Bytes: d.Data}}, nil
}

func encodeToolConfig(tools []protocol.Tool, choice protocol.ToolChoice, cache bool) (*toolConfig, error) {
	if len(tools) == 0 {
		if choice.Mode != "" {
			return nil, fmt.Errorf("bedrock-converse-stream: tool choice %q without tools", choice.Mode)
		}
		return nil, nil
	}
	cfg := &toolConfig{}
	for _, t := range tools {
		if len(t.Parameters) == 0 {
			return nil, fmt.Errorf("bedrock-converse-stream: tool %q has no parameters schema", t.Name)
		}
		cfg.Tools = append(cfg.Tools, tool{ToolSpec: &toolSpec{Name: t.Name, Description: t.Description, InputSchema: toolInputSchema{JSON: t.Parameters}}})
	}
	if cache {
		cfg.Tools = append(cfg.Tools, tool{CachePoint: &cachePoint{Type: "default"}})
	}
	switch choice.Mode {
	case "":
	case protocol.ToolChoiceAuto:
		cfg.ToolChoice = &toolChoice{Auto: &struct{}{}}
	case protocol.ToolChoiceRequired:
		cfg.ToolChoice = &toolChoice{Any: &struct{}{}}
	case protocol.ToolChoiceTool:
		if choice.Name == "" {
			return nil, fmt.Errorf("bedrock-converse-stream: tool choice names no tool")
		}
		cfg.ToolChoice = &toolChoice{Tool: &specificTool{Name: choice.Name}}
	case protocol.ToolChoiceNone:
		return nil, fmt.Errorf("bedrock-converse-stream: ToolChoice has no none member")
	default:
		return nil, fmt.Errorf("bedrock-converse-stream: unknown tool choice mode %q", choice.Mode)
	}
	return cfg, nil
}

// Percent encodes every byte outside the unreserved set, matching smithy-go EscapePath for a non greedy label
func escapePath(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

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
		return &protocol.Error{Status: 200, Type: typ, Message: body.Message, Transient: transientTypes[typ]}
	case "error":
		code := f.String(":error-code")
		return &protocol.Error{Status: 200, Type: code, Message: f.String(":error-message"), Transient: transientTypes[code]}
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
	e := &protocol.Error{Status: status, Transient: transientStatus(status)}
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
