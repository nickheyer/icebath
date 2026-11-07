package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Env var for the stale stream detector base in seconds
const StreamStaleTimeoutEnv = "ICEBATH_STREAM_STALE_TIMEOUT"

// Env var for the stale ceiling of a local endpoint in seconds
const LocalStreamStaleTimeoutEnv = "ICEBATH_LOCAL_STREAM_STALE_TIMEOUT"

// Env var for the socket read timeout of a streaming request in seconds
const StreamReadTimeoutEnv = "ICEBATH_STREAM_READ_TIMEOUT"

// Env var for the overall request timeout in seconds
const APITimeoutEnv = "ICEBATH_API_TIMEOUT"

const defaultAPITimeout = 1800.0
const defaultStreamReadTimeout = 120.0
const defaultStreamStaleTimeout = 180.0

// Upper bound on an error body copied into an APIError
const ccErrorBodyLimit = 1 << 20

// Socket timeouts of one request attempt
type ccTimeouts struct {
	base     time.Duration
	read     time.Duration
	connect  time.Duration
	stale    time.Duration
	lifetime time.Duration
}

func secondsDuration(f float64) time.Duration {
	if math.IsNaN(f) || f*float64(time.Second) >= math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(f * float64(time.Second))
}

// Seconds of a watchdog window, the unbounded window of an infinite timeout reads back as infinity
func durationSeconds(d time.Duration) float64 {
	if d == time.Duration(math.MaxInt64) {
		return math.Inf(1)
	}
	return d.Seconds()
}

// Overall request timeout and whether a per provider setting supplied it
func (c *chatCompletionsClient) requestTimeout(req *Request, settings wireSettings) (time.Duration, bool) {
	if req != nil && req.Timeout > 0 {
		return req.Timeout, true
	}
	if c.route.Timeout > 0 {
		return c.route.Timeout, true
	}
	model := ""
	if req != nil {
		model = req.Model
	}
	if d, ok := configuredProviderTimeout(settings.providers, c.cc.provider, model, "timeout_seconds", "request_timeout_seconds"); ok {
		return d, true
	}
	return secondsDuration(envFloat(APITimeoutEnv, defaultAPITimeout)), false
}

// Stale patience scaled for context size and floored for known reasoning models
func scaleStaleTimeoutForContext(base float64, estTokens int) float64 {
	if estTokens > 100000 && base < 300 {
		return 300
	}
	if estTokens > 50000 && base < 240 {
		return 240
	}
	return base
}

// Stale stream patience in seconds for the request
func (c *chatCompletionsClient) staleTimeout(req *Request, settings wireSettings, estTokens int) float64 {
	if d, ok := configuredProviderTimeout(settings.providers, c.cc.provider, req.Model, "stale_timeout_seconds", "stale_timeout_seconds"); ok {
		return d.Seconds()
	}
	base := settings.streamStale
	if base <= 0 {
		base = defaultStreamStaleTimeout
	}
	if _, explicit := os.LookupEnv(StreamStaleTimeoutEnv); explicit {
		base = envFloat(StreamStaleTimeoutEnv, base)
	}
	if base == defaultStreamStaleTimeout && c.cc.local {
		local := settings.localStreamStale
		if local <= 0 {
			local = 900
		}
		return envFloat(LocalStreamStaleTimeoutEnv, local)
	}
	timeout := scaleStaleTimeoutForContext(base, estTokens)
	if floor, ok := ReasoningStaleTimeoutFloor(req.Model); ok && floor.Seconds() > timeout {
		timeout = floor.Seconds()
	}
	return timeout
}

// Socket timeouts of a streaming attempt with the stale detector and lifetime cap
func (c *chatCompletionsClient) streamTimeouts(req *Request, settings wireSettings, estTokens int) ccTimeouts {
	base, configured := c.requestTimeout(req, settings)
	stale := c.staleTimeout(req, settings, estTokens)
	t := ccTimeouts{base: base, stale: secondsDuration(stale), lifetime: secondsDuration(resolveStreamMaxLifetime(settings.streamMaxLifetime, stale))}
	if configured {
		t.read = base
		t.connect = base
		if t.connect > 60*time.Second {
			t.connect = 60 * time.Second
		}
		return t
	}
	read := envFloat(StreamReadTimeoutEnv, defaultStreamReadTimeout)
	if read == defaultStreamReadTimeout && c.cc.local {
		read = base.Seconds()
	} else if read == defaultStreamReadTimeout && stale > read {
		read = stale
	}
	t.read = secondsDuration(read)
	t.connect = 30 * time.Second
	return t
}

// ---- partial stream stub ----

const ccRawDroppedToolNames = "_dropped_tool_names"
const ccRawCleanEOF = "_clean_eof"
const ccRawRunaway = "_runaway_repetition"
const ccRawContentFilter = "_content_filter_terminated"

const ccRawOverflowTerminal = "_overflow_terminal"

// How a partial stream stub is tagged, its wire shape following the api mode
type partialStubOptions struct {
	overflowTerminal bool
	cleanEOF         bool
	apiMode          APIMode
}

// Stub response for a stream that ended without a finish reason after delivering content
func buildPartialStreamStub(route Route, content, reasoning, model string, usage *Usage, droppedToolNames []string, o partialStubOptions) *Response {
	var dropped any
	if len(droppedToolNames) > 0 {
		dropped = droppedToolNames
	}
	var raw map[string]any
	if o.apiMode == ModeAnthropicMessages {
		raw = map[string]any{
			"id": PartialStreamStubID, "type": "message", "role": "assistant", "model": model,
			"content": []any{map[string]any{"type": "text", "text": content}}, "stop_reason": "max_tokens", "stop_sequence": nil, "usage": nil,
		}
	} else {
		msg := map[string]any{"role": "assistant", "content": nil, "tool_calls": nil, "reasoning_content": nil}
		if content != "" {
			msg["content"] = content
		}
		if reasoning != "" {
			msg["reasoning_content"] = reasoning
		}
		raw = map[string]any{
			"id": PartialStreamStubID, "model": model, "usage": nil,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "length"}},
		}
	}
	raw[ccRawDroppedToolNames], raw[ccRawOverflowTerminal], raw[ccRawCleanEOF] = dropped, o.overflowTerminal, o.cleanEOF
	if usage != nil {
		raw["usage"] = usage
	}
	m := &state.Message{Role: "assistant"}
	if content != "" {
		m.Content = content
	}
	if reasoning != "" {
		m.Reasoning, m.ReasoningContent = reasoning, reasoning
	}
	resp := &Response{Message: m, FinishReason: "length", Model: model, Provider: route.Provider, RequestID: PartialStreamStubID, Truncated: true}
	if usage != nil {
		resp.Usage = *usage
	}
	resp.Raw, _ = json.Marshal(raw)
	return resp
}

func rawResponseField(resp *Response, key string) any {
	if resp == nil || len(resp.Raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Raw, &m); err != nil {
		return nil
	}
	return m[key]
}

// IsPartialStreamStub reports a response built for a stream that died after delivering content
func IsPartialStreamStub(resp *Response) bool {
	return resp != nil && resp.RequestID == PartialStreamStubID
}

// PartialStreamDroppedToolNames lists tool calls whose arguments never completed before the stream ended
func PartialStreamDroppedToolNames(resp *Response) []string {
	var out []string
	if list, ok := rawResponseField(resp, ccRawDroppedToolNames).([]any); ok {
		for _, v := range list {
			out = append(out, stringOf(v))
		}
	}
	return out
}

// PartialStreamCleanEOF reports a stub built after a clean server close with no transport error
func PartialStreamCleanEOF(resp *Response) bool {
	v, _ := rawResponseField(resp, ccRawCleanEOF).(bool)
	return v
}

// PartialStreamOverflowTerminal reports a stub whose stream died on a context overflow so the turn must end
func PartialStreamOverflowTerminal(resp *Response) bool {
	v, _ := rawResponseField(resp, ccRawOverflowTerminal).(bool)
	return v
}

// ResponseRunawayRepetition reports a response cut because a channel became a repetition loop
func ResponseRunawayRepetition(resp *Response) bool {
	v, _ := rawResponseField(resp, ccRawRunaway).(bool)
	return v
}

// ResponseContentFilterTerminated reports a partial stub whose stream died on a content policy block
func ResponseContentFilterTerminated(resp *Response) bool {
	v, _ := rawResponseField(resp, ccRawContentFilter).(bool)
	return v
}

// ---- stream attempt ----

type ccFrameMsg struct {
	frame *sseFrame
	err   error
}

// Cancellation reason recorded by a watchdog before it aborts the attempt
type ccKill struct {
	mu      sync.Mutex
	reason  string
	elapsed float64
}

// Records a stream_max_lifetime kill, the latest cap kill names the attempt error over any other kill
func (k *ccKill) capKill(elapsed float64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.reason, k.elapsed = "lifetime", elapsed
}

func (k *ccKill) fire(reason string, elapsed float64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.reason == "" {
		k.reason, k.elapsed = reason, elapsed
	}
}

func (k *ccKill) get() (string, float64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.reason, k.elapsed
}

// Integer rendered with thousands separators
func commaInt(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	out := strings.Join(parts, ",")
	if neg {
		out = "-" + out
	}
	return out
}

// Live state of one streamed chat completion attempt
type ccStreamState struct {
	c                  *chatCompletionsClient
	diag               *StreamDiag
	wire               *FailedStreamRequest
	req                *Request
	fn                 StreamFunc
	headers            http.Header
	status             int
	estTokens          int
	contentParts       []string
	reasoningParts     []string
	detailDisplayParts []string
	refusalParts       []string
	pendingText        []string
	reasoningDetails   []map[string]any
	tools              *ccToolCallAccumulator
	partialToolNames   []string
	signature          string
	finishReason       string
	usage              map[string]any
	modelName          string
	responseID         string
	upstreamProvider   string
	scrubber           *StreamingThinkScrubber
	nativeReasoning    bool
	deltasSent         bool
	visibleStreamed    bool
	contentWatch       *RunawayStreamWatch
	reasoningWatch     *RunawayStreamWatch
	detailWatch        *RunawayStreamWatch
	runaway            string
	firstChunkAt       time.Time
	progress           map[string]any
	progressAt         time.Time
	lastProgressNotice time.Time
	mon                *streamWaitMonitor
	started            time.Time
	lifetimeCap        float64
	fence              any
	writerToken        int
	relay              *ccManagedRelay
}

func (s *ccStreamState) monitorModel() string {
	if s.req == nil {
		return ""
	}
	return s.req.Model
}

func (s *ccStreamState) monitorFirstChunk() time.Time { return s.firstChunkAt }

func (s *ccStreamState) monitorPromptProgress() bool { return s.progress != nil }

func (s *ccStreamState) monitorEstTokens() int { return s.estTokens }

// Decoded payload of a frame a killed attempt delivered, nil when it is not JSON
func decodedFrame(data string) any {
	var v any
	if json.Unmarshal([]byte(data), &v) != nil {
		return nil
	}
	return v
}

func (s *ccStreamState) emit(ev StreamEvent) error {
	if s.fn == nil {
		return nil
	}
	return s.fn(ev)
}

// Delivers visible text through the scrubber, forwarding hidden reasoning when none streams natively
func (s *ccStreamState) emitText(text string) error {
	s.deltasSent = true
	visible := s.scrubber.Feed(text)
	if hidden := s.scrubber.LastHidden(); hidden != "" && !s.nativeReasoning {
		if err := s.emit(StreamEvent{Kind: EventReasoningDelta, Text: hidden}); err != nil {
			return err
		}
	}
	if !s.visibleStreamed {
		visible = strings.TrimLeft(visible, "\n")
	}
	if visible == "" {
		return nil
	}
	s.visibleStreamed = true
	return s.emit(StreamEvent{Kind: EventTextDelta, Text: visible})
}

// Tool call turns route text through the scrubber for reasoning tag extraction only
func (s *ccStreamState) routeSuppressedText(text string) error {
	s.scrubber.Feed(text)
	if hidden := s.scrubber.LastHidden(); hidden != "" && !s.nativeReasoning {
		return s.emit(StreamEvent{Kind: EventReasoningDelta, Text: hidden})
	}
	return nil
}

func (s *ccStreamState) emitReasoning(text string) error {
	s.nativeReasoning = true
	return s.emit(StreamEvent{Kind: EventReasoningDelta, Text: text})
}

func (s *ccStreamState) flushPendingText() error {
	pending := s.pendingText
	s.pendingText = nil
	for _, text := range pending {
		var err error
		if s.tools.Len() > 0 {
			err = s.routeSuppressedText(text)
		} else {
			err = s.emitText(text)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Rate limited prompt reading notice from llama server progress chunks until the first token
func (s *ccStreamState) showPromptProgress(now time.Time) error {
	if s.progress == nil || !s.firstChunkAt.IsZero() {
		return nil
	}
	if now.Sub(s.progressAt) > 60*time.Second || now.Sub(s.lastProgressNotice) < time.Second {
		return nil
	}
	s.lastProgressNotice = now
	text := "⚙ processing prompt"
	if pct, ok := promptProgressPercent(s.progress); ok {
		text = fmt.Sprintf("⚙ processing prompt — %d%%", pct)
	}
	if s.mon != nil {
		return s.mon.showPromptProgress(now, text)
	}
	return s.emit(StreamEvent{Kind: EventStatus, Text: text})
}

// Usage and finish of a chunk without choices, or the in stream error some providers send that way
func (s *ccStreamState) choicelessChunk(chunk map[string]any) error {
	if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
		s.usage = u
	}
	errType, errMsg := chunk["error_type"], chunk["error_message"]
	if !detailValueEmpty(errType) || !detailValueEmpty(errMsg) {
		status := statusCodeFromPayload(map[string]any{"code": errType, "message": errMsg})
		if status == 0 {
			status = statusCodeFromValue(errType)
		}
		code := stringOf(errType)
		if code == "" {
			code = "provider_in_stream_error"
		}
		msg := stringOf(errMsg)
		if msg == "" {
			b, _ := json.Marshal(chunk)
			msg = string(b)
		}
		return &ProviderStreamError{StatusCode: status, Body: providerErrorBody(map[string]any{"code": code, "message": msg}, status),
			RawText: fmt.Sprintf("%v: %v", errType, errMsg), Headers: s.headers}
	}
	switch last := chunk["lastOne"].(type) {
	case bool:
		if last && s.finishReason == "" {
			s.finishReason = "stop"
		}
	case float64:
		if last == 1 && s.finishReason == "" {
			s.finishReason = "stop"
		}
	case string:
		if last == "true" && s.finishReason == "" {
			s.finishReason = "stop"
		}
	}
	return nil
}

// Folds one decoded chunk into the accumulators and emits its deltas, true once the stream must stop
func (s *ccStreamState) handleChunk(chunk map[string]any) (bool, error) {
	if m := stringOf(chunk["model"]); m != "" {
		s.modelName = m
	}
	if s.responseID == "" {
		if id, ok := chunk["id"].(string); ok && id != "" {
			s.responseID = id
		}
	}
	if s.upstreamProvider == "" {
		if p, ok := chunk["provider"].(string); ok && p != "" {
			s.upstreamProvider = p
			if s.diag != nil {
				s.diag.ServingProvider = pystr.Slice(pystr.Strip(p), 0, 64)
			}
		}
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return false, s.choicelessChunk(chunk)
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return false, nil
	}
	delta, _ := choice["delta"].(map[string]any)
	if fr, _ := NormalizeFinishReason(ccFinishReasonText(choice["finish_reason"])).(string); fr != "" {
		s.finishReason = fr
		if s.diag != nil {
			s.diag.FinishReasonSeen = true
		}
	}
	if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
		s.usage = u
	}
	if delta == nil {
		return false, nil
	}
	reasoningText := ""
	if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
		reasoningText = rc
	} else if r, ok := delta["reasoning"].(string); ok && r != "" {
		reasoningText = r
	}
	if sig := thoughtSignature(delta["extra_content"]); sig != "" {
		s.signature = sig
	}
	if reasoningText != "" {
		prev := ""
		if len(s.reasoningParts) > 0 {
			prev = s.reasoningParts[len(s.reasoningParts)-1]
		}
		reasoningText = separateGluedReasoningBlocks(prev, reasoningText)
		s.reasoningParts = append(s.reasoningParts, reasoningText)
	}
	var detailTextParts []string
	if rd, ok := delta["reasoning_details"].([]any); ok {
		for _, d := range rd {
			detailTextParts = append(detailTextParts, streamedReasoningDetailText(d))
			s.reasoningDetails = appendStreamedReasoningDetail(s.reasoningDetails, d)
		}
	}
	detailText := strings.Join(detailTextParts, "")
	if detailText != "" {
		prev := ""
		if len(s.detailDisplayParts) > 0 {
			prev = s.detailDisplayParts[len(s.detailDisplayParts)-1]
		}
		detailText = separateGluedReasoningBlocks(prev, detailText)
		s.detailDisplayParts = append(s.detailDisplayParts, detailText)
	}
	displayReasoning := detailText
	if displayReasoning == "" {
		displayReasoning = reasoningText
	}
	if displayReasoning != "" {
		if err := s.emitReasoning(displayReasoning); err != nil {
			return true, err
		}
		if s.reasoningWatch.Feed(reasoningText) || s.detailWatch.Feed(detailText) {
			s.runaway = "reasoning"
			return true, nil
		}
	}
	if refusal, ok := delta["refusal"].(string); ok && refusal != "" {
		s.refusalParts = append(s.refusalParts, refusal)
	}
	if deltaContent := flattenMessageText(delta["content"], ""); deltaContent != "" {
		s.contentParts = append(s.contentParts, deltaContent)
		if s.contentWatch.Feed(deltaContent) {
			s.runaway = "content"
			return true, nil
		}
		switch {
		case s.tools.Len() > 0:
			if err := s.routeSuppressedText(deltaContent); err != nil {
				return true, err
			}
		case len(s.pendingText) > 0 || providerStreamTextMayBeSSE(deltaContent) || (!s.deltasSent && routerTimeoutShimMayFollow(strings.Join(s.contentParts, ""))):
			s.pendingText = append(s.pendingText, deltaContent)
			pending := strings.Join(s.pendingText, "")
			if !(providerStreamTextMayBeSSE(pending) || routerTimeoutShimMayFollow(pending)) {
				if err := s.flushPendingText(); err != nil {
					return true, err
				}
			}
			return false, nil
		default:
			if err := s.emitText(deltaContent); err != nil {
				return true, err
			}
		}
	}
	if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
		if err := s.flushPendingText(); err != nil {
			return true, err
		}
		for _, raw := range tcs {
			tc, _ := raw.(map[string]any)
			if tc == nil {
				continue
			}
			name, slot := s.tools.Feed(tc)
			entry := s.tools.acc[slot]
			if name != "" {
				s.partialToolNames = append(s.partialToolNames, name)
				if err := s.emit(StreamEvent{Kind: EventToolCallStart, Index: slot, ToolCall: &state.ToolCall{ID: entry.ID, Type: "function", Function: state.ToolCallFunction{Name: name}}}); err != nil {
					return true, err
				}
			}
			if fn, _ := tc["function"].(map[string]any); fn != nil {
				if frag, ok := fn["arguments"].(string); ok && frag != "" {
					if err := s.emit(StreamEvent{Kind: EventToolCallDelta, Index: slot, ToolCall: &state.ToolCall{ID: entry.ID, Type: "function", Function: state.ToolCallFunction{Name: entry.Name, Arguments: frag}}}); err != nil {
						return true, err
					}
				}
			}
		}
	}
	return false, nil
}

// Finish reason text with integer reasons some providers send rendered as digits
func ccFinishReasonText(v any) string {
	switch fr := v.(type) {
	case string:
		return fr
	case float64:
		return strconv.FormatInt(int64(fr), 10)
	}
	return ""
}

// Assembled tool calls with the truncated flag for arguments that cannot be trusted
func assembleStreamedToolCalls(acc *ccToolCallAccumulator, finishReason string) ([]map[string]any, bool) {
	entries := acc.Materialize()
	var out []map[string]any
	truncated := false
	for _, idx := range acc.Slots() {
		tc := entries[idx]
		arguments := tc.Arguments
		name := tc.Name
		if name == "" {
			name = "?"
		}
		if strings.TrimSpace(arguments) != "" {
			if !jsonLoadsOK(arguments) {
				if finishReason == "" {
					truncated = true
				} else {
					repaired := RepairToolCallArguments(arguments, name)
					if repaired != "{}" {
						arguments = repaired
					} else {
						truncated = true
					}
				}
			}
			if finishReason == "" && IsRepetitionDominated(arguments) {
				catalogLog.Warn(fmt.Sprintf("Tool call '%s' has repetition-dominated arguments without a finish_reason; treating as a dropped tool call.", name))
				truncated = true
			}
		} else if finishReason == "" {
			truncated = true
		}
		fn := map[string]any{"name": tc.Name, "arguments": arguments, "args_repaired": arguments != tc.Arguments}
		if tc.ThoughtSignature != "" {
			fn["thought_signature"] = tc.ThoughtSignature
		}
		call := map[string]any{"id": tc.ID, "type": tc.Type, "function": fn}
		if tc.ExtraContent != nil {
			call["extra_content"] = tc.ExtraContent
		}
		out = append(out, call)
	}
	return out, truncated
}

// Assembles the non streaming shaped response once the chunk loop ends
func (s *ccStreamState) finish(latency time.Duration) (*Response, error) {
	if resp, adopted, err := s.adoptRelayFinal(latency); adopted {
		return resp, err
	}
	fullContent := strings.Join(s.contentParts, "")
	fullReasoning := strings.Join(s.reasoningParts, "")
	if fullReasoning == "" && fullContent != "" {
		fullReasoning = inlineReasoning(fullContent)
	}
	finishReason := s.finishReason
	if s.runaway != "" {
		finishReason = "length"
	}
	toolCalls, truncated := assembleStreamedToolCalls(s.tools, finishReason)
	if finishReason == "" && len(s.contentParts) == 0 && len(s.reasoningParts) == 0 && len(s.refusalParts) == 0 && s.tools.Len() == 0 {
		return nil, &EmptyStreamError{Message: "Provider returned an empty stream with no finish_reason (possible upstream error or malformed SSE response)."}
	}
	model := s.modelName
	if model == "" {
		model = s.req.Model
	}
	var usage *Usage
	if s.usage != nil {
		u := ccUsageFromMap(s.usage)
		usage = &u
	}
	if truncated && finishReason == "" {
		var dropped []string
		for _, idx := range s.tools.Slots() {
			name := s.tools.acc[idx].Name
			if name == "" {
				name = "?"
			}
			dropped = append(dropped, name)
		}
		catalogLog.Warn(fmt.Sprintf("Clean EOF, no finish_reason: server ended the stream (no transport exception) while a tool call's arguments were still incomplete (tools=%v). The server or a proxy closed the stream cleanly; not an output-length truncation.", dropped))
		return buildPartialStreamStub(s.c.route, fullContent, fullReasoning, model, usage, dropped, partialStubOptions{cleanEOF: true}), nil
	}
	if finishReason == "" && (len(s.contentParts) > 0 || len(s.reasoningParts) > 0) && s.tools.Len() == 0 && s.usage == nil {
		catalogLog.Warn("Clean EOF, no finish_reason: server ended the stream (no transport exception) after delivering text with no tool calls. The server or a proxy closed the stream cleanly.")
		return buildPartialStreamStub(s.c.route, fullContent, fullReasoning, model, usage, nil, partialStubOptions{cleanEOF: true}), nil
	}
	effective := finishReason
	if truncated {
		effective = "length"
	} else if effective == "" {
		effective = "stop"
	}
	if pse := providerStreamErrorFromText(fullContent, effective, s.headers); pse != nil {
		return nil, s.c.streamError(pse)
	}
	message := map[string]any{"role": "assistant", "content": nil, "tool_calls": nil, "reasoning_content": nil, "refusal": nil}
	if fullContent != "" {
		message["content"] = fullContent
	}
	if len(toolCalls) > 0 {
		list := make([]any, 0, len(toolCalls))
		for _, tc := range toolCalls {
			list = append(list, tc)
		}
		message["tool_calls"] = list
	}
	if fullReasoning != "" {
		message["reasoning_content"] = fullReasoning
	}
	if refusal := strings.Join(s.refusalParts, ""); refusal != "" {
		message["refusal"] = refusal
	}
	if len(s.reasoningDetails) > 0 {
		list := make([]any, 0, len(s.reasoningDetails))
		for _, d := range s.reasoningDetails {
			list = append(list, d)
		}
		message["reasoning_details"] = list
	}
	if s.signature != "" {
		message["extra_content"] = map[string]any{"google": map[string]any{"thought_signature": s.signature}}
	}
	responseID := s.responseID
	if responseID == "" {
		responseID = "stream-" + uuid.NewString()
	}
	payload := map[string]any{"id": responseID, "model": model, "usage": s.usage, "provider": s.upstreamProvider,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": effective}}}
	if s.runaway != "" {
		payload[ccRawRunaway] = true
		catalogLog.Warn(fmt.Sprintf("Stream cut: the %s turned into a runaway repetition loop (model=%s, provider=%s).", s.runaway, model, orUnknown2(s.c.route.Provider)))
	}
	outUsage := Usage{}
	if usage != nil {
		outUsage = *usage
	}
	if !isRouterTimeoutShim(fullContent, len(toolCalls), outUsage) {
		if err := s.flushPendingText(); err != nil {
			return nil, err
		}
	}
	resp, err := s.c.normalizeResponse(payload, s.status, s.headers, latency)
	if err != nil {
		return nil, err
	}
	if usage != nil {
		if err := s.emit(StreamEvent{Kind: EventUsage, Usage: usage}); err != nil {
			return nil, err
		}
	}
	for i := range resp.Message.ToolCalls {
		tc := resp.Message.ToolCalls[i]
		if err := s.emit(StreamEvent{Kind: EventToolCallDone, Index: i, ToolCall: &tc}); err != nil {
			return nil, err
		}
	}
	if err := s.emit(StreamEvent{Kind: EventDone}); err != nil {
		return nil, err
	}
	return resp, nil
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// Reads one streamed chat completion attempt from an open response through the chunk loop
func (c *chatCompletionsClient) consumeStream(requestCtx, ctx context.Context, cancel context.CancelFunc, kill *ccKill, src ccFrameSource, beat <-chan bool, s *ccStreamState, t ccTimeouts) (*Response, error) {
	frames := make(chan ccFrameMsg, 32)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer releaseFrameSource(src)
		for {
			f, err := src.Next()
			msg := ccFrameMsg{frame: f, err: err}
			select {
			case frames <- msg:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	w := newStreamWatchdog(requestCtx, cancel, kill, readerDone, s.emit, func(base error) error { return Classify(c.route.Provider, c.route.Model, 0, "", base) }, s.req.Model, s.estTokens, t, s.started)
	defer w.stop()
	defer w.logAbnormalExit()
	readTimer := time.NewTimer(t.read)
	defer readTimer.Stop()
	s.mon = newStreamWaitMonitor(s, c.route.BaseURL, durationSeconds(t.stale), time.Now())
	s.mon.w = w
	poll := time.NewTicker(streamMonitorPoll)
	defer poll.Stop()
	var unwindErr error
	for {
		select {
		case <-requestCtx.Done():
			return nil, w.interrupt(time.Now(), s.deltasSent)
		case <-w.unwound():
			return nil, c.attemptError(requestCtx, kill, unwindErr, s)
		case now := <-poll.C:
			moved, err := s.mon.tick(now, &w.lastChunk)
			if err != nil {
				cancel()
				return nil, err
			}
			if moved {
				w.moved(now)
			}
		case now := <-w.staleC():
			if w.onStale(now) {
				return nil, w.abandoned
			}
		case <-w.readC(readTimer.C):
			kill.fire("read", t.read.Seconds())
			cancel()
			return nil, c.attemptError(ctx, kill, errors.New("read timeout"), s)
		case now := <-w.lifeC():
			if w.onLifetime(now) {
				return nil, w.abandoned
			}
		case live := <-beat:
			resetTimer(readTimer, t.read)
			if live {
				w.chunk(time.Now())
			}
		case m := <-frames:
			var final *ccFinalError
			if w.killed {
				if errors.As(m.err, &final) {
					unwindErr = streamKillCause(final.err)
				} else if m.err != nil {
					unwindErr = streamKillCause(m.err)
				} else if m.frame != nil {
					w.call.discard(w.attempt, decodedFrame(m.frame.Data))
				}
				continue
			}
			if m.err != nil {
				if errors.Is(m.err, io.EOF) {
					return s.finish(time.Since(s.started))
				}
				if errors.As(m.err, &final) {
					return nil, final.err
				}
				return nil, c.attemptError(ctx, kill, m.err, s)
			}
			resetTimer(readTimer, t.read)
			chunk, skip, err := c.decodeFrame(m.frame, s.headers)
			if errors.Is(err, io.EOF) {
				return s.finish(time.Since(s.started))
			}
			if err != nil {
				return nil, err
			}
			if skip {
				continue
			}
			if !s.writerStillCurrent(chunk) {
				return s.finish(time.Since(s.started))
			}
			now := time.Now()
			w.chunk(now)
			if c.llamacpp {
				if progress := promptProgress(chunk); progress != nil {
					s.progress, s.progressAt = progress, now
					if err := s.showPromptProgress(now); err != nil {
						cancel()
						return nil, err
					}
					continue
				}
			}
			if s.firstChunkAt.IsZero() {
				s.firstChunkAt = now
			}
			if err := s.countChunk(now, estimateChunkBytes(chunk)); err != nil {
				cancel()
				return nil, err
			}
			stop, err := s.handleChunk(chunk)
			if err != nil {
				cancel()
				return nil, c.chunkError(err)
			}
			if stop {
				cancel()
				return s.finish(time.Since(s.started))
			}
		}
	}
}

// Chunk of one SSE frame, skip for frames the stream ignores and io.EOF at the done marker
func (c *chatCompletionsClient) decodeFrame(f *sseFrame, headers http.Header) (map[string]any, bool, error) {
	if f.Event == "" && f.ID == "" && f.Retry == "" && len(f.Fields) == 0 && f.Data == "" {
		return nil, true, nil
	}
	if strings.HasPrefix(f.Data, "[DONE]") {
		return nil, false, io.EOF
	}
	if strings.TrimSpace(f.Data) == "" {
		return nil, false, c.streamError(providerStreamErrorFromNonJSON("", headers))
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(f.Data), &chunk); err != nil {
		return nil, false, c.streamError(providerStreamErrorFromNonJSON(f.Data, headers))
	}
	if eo := chunk["error"]; eo != nil && !detailValueEmpty(eo) {
		if em, ok := eo.(map[string]any); ok && len(em) == 0 {
			return nil, true, nil
		}
		payload := map[string]any{}
		if em, ok := eo.(map[string]any); ok {
			payload = em
		} else {
			payload["message"] = stringOf(eo)
		}
		status := statusCodeFromPayload(chunk)
		if status == 0 {
			status = f.StatusCode
		}
		return nil, false, c.streamError(&ProviderStreamError{StatusCode: status, Body: providerErrorBody(payload, status), RawText: f.Data, Headers: headers})
	}
	if f.Event != "" && f.Event != "error" && !strings.HasPrefix(f.Event, "response.") && !strings.HasPrefix(f.Event, "transcript.") {
		if _, hasChoices := chunk["choices"]; !hasChoices {
			return nil, true, nil
		}
	}
	return chunk, false, nil
}

// Turns an in loop chunk error into the APIError shape the retry loop classifies
func (c *chatCompletionsClient) chunkError(err error) error {
	var pse *ProviderStreamError
	if errors.As(err, &pse) {
		return c.streamError(pse)
	}
	return err
}

// APIError for a provider error encoded inside the stream, keeping the synthetic code and headers
func (c *chatCompletionsClient) streamError(pse *ProviderStreamError) error {
	apiErr := Classify(c.route.Provider, c.route.Model, pse.StatusCode, pse.BodyText(), pse)
	if code := pse.Code(); code != "" {
		apiErr.Code = code
	}
	if apiErr.Message == "" {
		apiErr.Message = pse.Error()
	}
	if pse.Headers != nil && apiErr.RetryAfter == 0 {
		apiErr.RetryAfter = RetryAfterFrom(pse.Headers.Get("Retry-After"))
	}
	return apiErr
}

// Error of a cancelled or failed attempt, naming the watchdog that cut it
func (c *chatCompletionsClient) attemptError(ctx context.Context, kill *ccKill, err error, s *ccStreamState) error {
	reason, elapsed := kill.get()
	var headers http.Header
	if s != nil {
		headers = s.headers
	}
	var base error
	switch reason {
	case "stale":
		base = &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: &ccTimeoutError{err: fmt.Errorf("Stream timed out: no chunks received for %.0fs (stale stream threshold, model: %s). Connection killed.", elapsed, orUnknown2(c.route.Model)), headers: headers}}
	case "lifetime":
		cap := 0.0
		if s != nil {
			cap = s.lifetimeCap
		}
		base = &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: &ccTimeoutError{err: &StreamLifetimeExceededError{Elapsed: elapsed, Cap: cap, Cause: streamKillCause(err)}, headers: headers}}
	case "read":
		base = &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: &ccTimeoutError{err: fmt.Errorf("Read timed out after %.0fs waiting for the provider stream.", elapsed), headers: headers}}
	default:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		base = ccWrapTransport(err, TimeoutPhaseRead)
		if headers != nil {
			var tpe *TimeoutPhaseError
			if errors.As(base, &tpe) && tpe == base {
				base = &TimeoutPhaseError{Phase: tpe.Phase, Err: &ccHeadersError{err: tpe.Err, headers: headers}}
			} else {
				base = &ccHeadersError{err: base, headers: headers}
			}
		}
	}
	if s != nil && s.wire != nil {
		base = withWireRequest(base, s.wire)
	}
	return Classify(c.route.Provider, c.route.Model, 0, "", base)
}

// Counts a real chunk into the attempt diagnostics and refreshes the activity clock
func (s *ccStreamState) countChunk(now time.Time, size int) error {
	countDiagChunk(s.diag, now, size)
	return s.emit(StreamEvent{Kind: EventActivity, Text: "receiving stream response"})
}

// Watchdog kill of a stream attempt, a temporary timeout that carries the response headers
type ccTimeoutError struct {
	err     error
	headers http.Header
}

func (e *ccTimeoutError) Error() string { return e.err.Error() }

func (e *ccTimeoutError) Unwrap() error { return e.err }

// Timeout marks the kill as a timeout for the classifier
func (e *ccTimeoutError) Timeout() bool { return true }

// Temporary marks the kill as retryable transport trouble
func (e *ccTimeoutError) Temporary() bool { return true }

// ResponseHeaders exposes the headers of the killed attempt
func (e *ccTimeoutError) ResponseHeaders() http.Header { return e.headers }

// Wraps a transport failure so timeouts carry the phase the adapter observed
func ccWrapTransport(err error, defaultPhase string) error {
	if err == nil {
		return nil
	}
	var tpe *TimeoutPhaseError
	if errors.As(err, &tpe) {
		return err
	}
	timeout := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)
	if !timeout {
		var ne interface{ Timeout() bool }
		if errors.As(err, &ne) && ne.Timeout() {
			timeout = true
		}
	}
	if !timeout {
		return err
	}
	phase := TimeoutPhase(err)
	if phase == "" {
		phase = defaultPhase
	}
	return &TimeoutPhaseError{Phase: phase, Err: err}
}

// Error wrapper that carries the response headers of a failed attempt
type ccHeadersError struct {
	err     error
	headers http.Header
}

func (e *ccHeadersError) Error() string { return e.err.Error() }

func (e *ccHeadersError) Unwrap() error { return e.err }

// ResponseHeaders exposes the headers for reset and welcome tier parsing
func (e *ccHeadersError) ResponseHeaders() http.Header { return e.headers }

// Timeout reports whether the wrapped failure was a timeout
func (e *ccHeadersError) Timeout() bool {
	var ne interface{ Timeout() bool }
	return errors.As(e.err, &ne) && ne.Timeout()
}
