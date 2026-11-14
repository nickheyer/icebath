package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
)

const PartialStreamStubID = "partial-stream-stub"

type CallInfo struct {
	TaskID        string
	TurnID        string
	RequestID     string
	Platform      string
	UserMessage   string
	History       []wire.Message
	CallCount     int
	RetryCount    int
	MaxRetries    int
	ApproxTokens  int
	TotalChars    int
	MaxTokens     int
	ToolCount     int
	ContextLength int
	StartedAt     time.Time
	MoAReferences json.RawMessage
}

type CallOptions struct {
	Stream        bool
	Display       bool
	Warnings      bool
	RunBudget     time.Duration
	RunStarted    time.Time
	SubagentID    string
	DelegateDepth int
	Info          CallInfo
}

type CallView struct {
	Method string
	Path   string
	Header http.Header
	Body   json.RawMessage
}

type TraceEntry struct {
	Source string
	Reason string
	Name   string
}

type MiddlewareInput struct {
	Call      CallView
	Original  CallView
	SessionID string
	Provider  string
	Model     string
	BaseURL   string
	Mode      wire.Mode
	Info      CallInfo
	Trace     []TraceEntry
}

type MiddlewareResult struct {
	Call   CallView
	Source string
	Reason string
	Name   string
}

type RequestMiddleware interface {
	RewriteRequest(ctx context.Context, in MiddlewareInput) (MiddlewareResult, bool, error)
}

type ExecutionMiddleware interface {
	ExecuteCall(ctx context.Context, in MiddlewareInput, next func(ctx context.Context, call CallView) (*wire.Response, error)) (*wire.Response, error)
}

type CallEvent struct {
	SessionID       string
	Provider        string
	Model           string
	BaseURL         string
	Mode            wire.Mode
	Info            CallInfo
	Request         json.RawMessage
	RequestMessages json.RawMessage
	SystemPrompt    string
	MessageCount    int
	Trace           []TraceEntry
	StartedAt       time.Time
	EndedAt         time.Time
	FirstChunkAt    time.Time
	FinishReason    string
	ResponseModel   string
	Response        json.RawMessage
	Usage           json.RawMessage
	Assistant       *wire.Message
	AssistantChars  int
	ToolCallCount   int
	Status          int
	Retryable       bool
	Reason          string
	ErrorType       string
	ErrorMessage    string
}

type CallObserver interface {
	BeforeCall(ctx context.Context, e CallEvent)
	AfterCall(ctx context.Context, e CallEvent)
	CallFailed(ctx context.Context, e CallEvent)
}

type CutError struct {
	DroppedTools     []string
	OverflowTerminal bool
	CleanEOF         bool
	ContentFilter    bool
	Runaway          bool
	Cause            error
}

func (e *CutError) Error() string {
	switch {
	case e.Runaway:
		return "stream cut: the reply turned into a runaway repetition loop"
	case e.OverflowTerminal:
		return "stream ended on a context overflow after partial delivery"
	case e.CleanEOF:
		return "stream ended cleanly without a finish reason"
	case e.Cause != nil:
		return "stream failed after partial delivery: " + e.Cause.Error()
	}
	return "stream ended before the turn finished"
}

func (e *CutError) Unwrap() error { return e.Cause }

type InterruptError struct {
	Message string
	Err     error
}

func (e *InterruptError) Error() string { return e.Message }

func (e *InterruptError) Unwrap() error { return e.Err }

type ResponseError struct {
	Header http.Header
	Err    error
}

func (e *ResponseError) Error() string { return e.Err.Error() }

func (e *ResponseError) Unwrap() error { return e.Err }

type DeadlineError struct {
	Message string
}

func (e *DeadlineError) Error() string { return e.Message }

func (e *DeadlineError) Timeout() bool { return true }

func (e *DeadlineError) Temporary() bool { return true }

func viewOf(c *wire.Call) CallView {
	return CallView{Method: c.Method, Path: c.Path, Header: c.Header.Clone(), Body: json.RawMessage(append([]byte(nil), c.Body...))}
}

func (x *exchange) middlewareInput(current, original CallView, trace []TraceEntry) MiddlewareInput {
	return MiddlewareInput{Call: current, Original: original, SessionID: x.p.Request.SessionID, Provider: x.rt.Provider,
		Model: x.rt.Model, BaseURL: x.rt.BaseURL, Mode: x.rt.Mode, Info: x.o.Info, Trace: append([]TraceEntry(nil), trace...)}
}

func objectBody(body []byte) bool {
	v, err := wire.ParseValue(body)
	return err == nil && v.IsObject()
}

func (x *exchange) applyRequestMiddleware(ctx context.Context) {
	hooks := x.s.r.c.o.Hooks.Middleware
	if len(hooks) == 0 {
		return
	}
	original := viewOf(x.call)
	current := viewOf(x.call)
	for _, h := range hooks {
		res, ok, err := h.RewriteRequest(ctx, x.middlewareInput(current, original, x.trace))
		if err != nil {
			slog.Warn(fmt.Sprintf("LLM request middleware %T failed; skipping it: %v", h, err))
			continue
		}
		if !ok || !objectBody(res.Call.Body) {
			continue
		}
		next := res.Call
		if next.Header == nil {
			next.Header = current.Header.Clone()
		}
		next.Method, next.Path = current.Method, current.Path
		current = CallView{Method: next.Method, Path: next.Path, Header: next.Header.Clone(), Body: append(json.RawMessage(nil), next.Body...)}
		entry := TraceEntry{Source: res.Source, Reason: res.Reason, Name: res.Name}
		if entry == (TraceEntry{}) {
			entry.Source = "plugin"
		}
		x.trace = append(x.trace, entry)
	}
	if len(x.trace) > 0 {
		x.call = x.withView(x.call, current)
	}
}

func (x *exchange) withView(base *wire.Call, v CallView) *wire.Call {
	out := *base
	out.Header = v.Header.Clone()
	out.Body = wire.ScrubSurrogates(append([]byte(nil), v.Body...))
	return &out
}

type downstreamError struct{ err error }

func (e *downstreamError) Error() string { return e.err.Error() }

func (e *downstreamError) Unwrap() error { return e.err }

func (x *exchange) runExecution(ctx context.Context, terminal func(ctx context.Context, call *wire.Call) (*wire.Response, error)) (*wire.Response, error) {
	chain := x.s.r.c.o.Hooks.Execution
	original := viewOf(x.call)
	var callAt func(ctx context.Context, index int, view CallView) (*wire.Response, error)
	callAt = func(ctx context.Context, index int, view CallView) (*wire.Response, error) {
		if index >= len(chain) {
			return terminal(ctx, x.withView(x.call, view))
		}
		mw := chain[index]
		called, succeeded := false, false
		var result *wire.Response
		next := func(ctx context.Context, v CallView) (*wire.Response, error) {
			if called {
				return nil, fmt.Errorf("model: middleware %T called next more than once; downstream execution is single-use", mw)
			}
			called = true
			if v.Body == nil {
				v = view
			} else if !objectBody(v.Body) {
				return nil, fmt.Errorf("model: middleware %T passed a request body that is not a JSON object", mw)
			}
			if v.Header == nil {
				v.Header = view.Header.Clone()
			}
			v.Method, v.Path = view.Method, view.Path
			r, err := callAt(ctx, index+1, v)
			if err != nil {
				return r, &downstreamError{err}
			}
			succeeded, result = true, r
			return r, nil
		}
		r, err := mw.ExecuteCall(ctx, x.middlewareInput(view, original, x.trace), next)
		var de *downstreamError
		if errors.As(err, &de) {
			return r, de.err
		}
		if err == nil {
			return r, nil
		}
		slog.Warn(fmt.Sprintf("LLM execution middleware %T failed: %v", mw, err))
		if succeeded {
			return result, nil
		}
		if called {
			return r, err
		}
		return callAt(ctx, index+1, view)
	}
	return callAt(ctx, 0, viewOf(x.call))
}

var sensitiveHookKeys = map[string]bool{"api_key": true, "authorization": true, "proxy_authorization": true, "cookie": true, "set_cookie": true}

func sensitiveHookKey(key string) bool {
	lowered := strings.ReplaceAll(strings.ToLower(key), "-", "_")
	return sensitiveHookKeys[lowered] || strings.HasSuffix(lowered, "_api_key")
}

func hookJSONable(v wire.Value, depth, maxString, maxSequence int) json.RawMessage {
	if depth > 8 {
		return wire.Quote("<" + jsonTypeName(v) + " depth limit>")
	}
	switch {
	case v.IsObject():
		d := &wire.Dict{}
		fields := v.Fields()
		for i, f := range fields {
			if i >= maxSequence {
				d.SetInt("_truncated_items", len(fields)-maxSequence)
				break
			}
			if sensitiveHookKey(f.Key) {
				d.SetString(f.Key, "<redacted>")
				continue
			}
			d.Set(f.Key, hookJSONable(f.Value, depth+1, maxString, maxSequence))
		}
		return d.JSON()
	case v.IsArray():
		elems := v.Elems()
		var out []json.RawMessage
		for i, e := range elems {
			if i >= maxSequence {
				break
			}
			out = append(out, hookJSONable(e, depth+1, maxString, maxSequence))
		}
		if len(elems) > maxSequence {
			out = append(out, (&wire.Dict{}).SetInt("_truncated_items", len(elems)-maxSequence).JSON())
		}
		return wire.Array(out...)
	}
	if s, ok := v.Str(); ok {
		runes := []rune(s)
		if len(runes) > maxString {
			return wire.Quote(string(runes[:maxString]) + "...[truncated " + strconv.Itoa(len(runes)-maxString) + " chars]")
		}
		return wire.Quote(s)
	}
	return v.Raw()
}

func jsonTypeName(v wire.Value) string {
	switch {
	case v.IsObject():
		return "object"
	case v.IsArray():
		return "array"
	}
	if _, ok := v.Str(); ok {
		return "string"
	}
	if _, ok := v.Bool(); ok {
		return "boolean"
	}
	if _, ok := v.Float(); ok {
		return "number"
	}
	return "null"
}

func (x *exchange) sanitizeHookPayload(raw json.RawMessage) json.RawMessage {
	limit := max(1000, x.k.payloadMax.orInt(50000))
	v, err := wire.ParseValue(raw)
	if err != nil {
		return wire.Quote(truncateRunes(string(raw), limit))
	}
	encoded := ""
	for _, caps := range [][2]int{{8000, 200}, {1000, 50}} {
		payload := hookJSONable(v, 0, caps[0], caps[1])
		encoded, _ = wire.FormatJSON(payload, wire.TextLayout{Spaced: true, UTF8: true})
		if len([]rune(encoded)) <= limit {
			return payload
		}
	}
	return (&wire.Dict{}).SetBool("_truncated", true).SetString("original_type", jsonTypeName(v)).
		SetString("preview", string([]rune(encoded)[:limit])).JSON()
}

func (x *exchange) requestPayloadForHook(body []byte) json.RawMessage {
	wrapper := (&wire.Dict{}).SetString("method", "POST").Set("body", body)
	return x.sanitizeHookPayload(wrapper.JSON())
}

func usageSummary(u *wire.Usage) json.RawMessage {
	if u == nil {
		return nil
	}
	requests := u.Requests
	if requests == 0 {
		requests = 1
	}
	return (&wire.Dict{}).SetInt("input_tokens", u.InputTokens).SetInt("output_tokens", u.OutputTokens).
		SetInt("cache_read_tokens", u.CacheReadTokens).SetInt("cache_write_tokens", u.CacheWriteTokens).
		SetInt("reasoning_tokens", u.ReasoningTokens).SetInt("request_count", requests).
		SetInt("prompt_tokens", u.PromptTokens()).SetInt("total_tokens", u.TotalTokens()).JSON()
}

func (x *exchange) event() CallEvent {
	return CallEvent{SessionID: x.p.Request.SessionID, Provider: x.rt.Provider, Model: x.rt.Model, BaseURL: x.rt.BaseURL,
		Mode: x.rt.Mode, Info: x.o.Info, Trace: append([]TraceEntry(nil), x.trace...), MessageCount: len(x.p.Request.Messages)}
}

func requestMessagesOf(body []byte, msgs []wire.Message) json.RawMessage {
	if v, err := wire.ParseValue(body); err == nil && v.IsObject() {
		for _, key := range []string{"messages", "input"} {
			if f := v.Get(key); f.IsArray() {
				return f.Raw()
			}
		}
	}
	return wire.AppendMessages(nil, msgs)
}

func systemPromptOf(body []byte, msgs []wire.Message) string {
	if v, err := wire.ParseValue(body); err == nil && v.IsObject() {
		for _, key := range []string{"system", "instructions"} {
			f := v.Get(key)
			if s, ok := f.Str(); ok && s != "" {
				return s
			}
			if f.IsArray() {
				if text := wire.FlattenContent(f.Raw(), "\n"); text != "" {
					return text
				}
			}
		}
	}
	for i := range msgs {
		if msgs[i].Role == wire.RoleSystem || msgs[i].Role == wire.RoleDeveloper {
			return msgs[i].FlatText("\n")
		}
	}
	return ""
}

func (x *exchange) fireBefore(ctx context.Context) {
	observers := x.s.r.c.o.Hooks.Observers
	if len(observers) == 0 {
		return
	}
	e := x.event()
	e.Request = x.requestPayloadForHook(x.call.Body)
	e.RequestMessages = requestMessagesOf(x.call.Body, x.p.Request.Messages)
	e.SystemPrompt = systemPromptOf(x.call.Body, x.p.Request.Messages)
	e.StartedAt = x.o.Info.StartedAt
	for _, o := range observers {
		o.BeforeCall(ctx, e)
	}
}

func (x *exchange) fireAfter(ctx context.Context, resp *wire.Response, started time.Time) {
	observers := x.s.r.c.o.Hooks.Observers
	if len(observers) == 0 || resp == nil {
		return
	}
	e := x.event()
	e.StartedAt, e.EndedAt, e.FirstChunkAt = started, time.Now(), resp.FirstByte
	e.FinishReason, e.ResponseModel = string(resp.FinishReason), resp.Model
	e.Usage = usageSummary(resp.Usage)
	m := resp.Message.Clone()
	e.Assistant = &m
	e.AssistantChars = len([]rune(m.Text))
	e.ToolCallCount = len(m.ToolCalls)
	calls := make([]json.RawMessage, len(m.ToolCalls))
	for i := range m.ToolCalls {
		calls[i] = m.ToolCalls[i].JSON()
	}
	assistant := (&wire.Dict{}).SetString("role", string(firstOf(string(m.Role), string(wire.RoleAssistant))))
	if m.Parts == nil && m.Text == "" {
		assistant.Set("content", json.RawMessage("null"))
	} else {
		assistant.SetString("content", m.FlatText(""))
	}
	assistant.Set("tool_calls", wire.Array(calls...))
	payload := (&wire.Dict{}).Set("model", nullableString(resp.Model)).Set("upstream_provider", nullableString(resp.Upstream)).
		Set("finish_reason", nullableString(string(resp.FinishReason))).Set("assistant_message", assistant.JSON())
	if summary := usageSummary(resp.Usage); summary != nil {
		payload.Set("usage", summary)
	} else {
		payload.Set("usage", json.RawMessage("null"))
	}
	e.Response = x.sanitizeHookPayload(payload.JSON())
	for _, o := range observers {
		o.AfterCall(ctx, e)
	}
}

func nullableString(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("null")
	}
	return wire.Quote(s)
}

func (x *exchange) fireFailed(ctx context.Context, err error, started time.Time) {
	observers := x.s.r.c.o.Hooks.Observers
	if len(observers) == 0 || err == nil {
		return
	}
	e := x.event()
	e.StartedAt, e.EndedAt = started, time.Now()
	e.Request = x.requestPayloadForHook(x.call.Body)
	e.ErrorType, e.ErrorMessage = errorTypeName(err), err.Error()
	var ae *wire.APIError
	if errors.As(err, &ae) {
		e.Status = ae.Status
	}
	if c := x.s.r.c.classifier.Classify(ctx, err, x.facts()); c != nil {
		e.Retryable, e.Reason = c.Retryable, c.Class.String()
	}
	for _, o := range observers {
		o.CallFailed(ctx, e)
	}
}
