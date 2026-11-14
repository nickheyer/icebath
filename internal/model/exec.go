package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/openai"
)

var errRunaway = errors.New("model: stream cut on a runaway repetition loop")

type exchange struct {
	s     *Session
	p     *Prepared
	rt    *Route
	o     CallOptions
	k     *knobs
	out   wire.Sink
	mode  wire.Mode
	call  *wire.Call
	ep    wire.Endpoint
	est   int
	model string
	local bool
	loop  bool
	llama bool
	trace []TraceEntry
	stale float64

	callerBase *wire.Call

	mu              sync.Mutex
	think           *thinkScrubber
	nativeReasoning bool
	visible         strings.Builder
	deltasSent      bool
	toolInFlight    bool
	toolStarted     bool
	partialTools    []string
	contentWatch    *RunawayWatch
	reasoningWatch  *RunawayWatch
	runaway         string
	progress        *wire.Progress
	progressAt      time.Time
	diag            *streamDiag
	fenced          bool
	sse             bool
	codexFrames     bool
	staleSuspended  bool
	attemptID       int
	attemptCancel   context.CancelCauseFunc
	released        int
	counted         map[int]bool
	attemptStart    time.Time
	connected       time.Time
	wrote           time.Time
	headersAt       time.Time
	lastByte        time.Time
	lastChunk       time.Time
	firstChunk      time.Time
	codex           codexWatchState
	codexEvents     int
	codexAttempt    int
	phaseAware      bool
	bufferTool      bool
	lastHeader      http.Header
	watchRunaway    bool
}

func (r *Router) injectedCaller(ctx context.Context, rt *Route) (wire.Caller, bool, error) {
	callers := r.c.o.Callers
	if rt.Provider == "moa" {
		if callers.MoA == nil {
			return nil, false, errors.New("model: no mixture-of-agents caller is registered for the moa provider")
		}
		return callers.MoA, true, nil
	}
	switch rt.Mode {
	case wire.ModeChat, wire.ModeResponses, wire.ModeAnthropic, wire.ModeBedrock, wire.ModeCodexAppServer, "":
	default:
		if c, ok := callers.Dialects[rt.Mode]; ok && c != nil {
			return c, true, nil
		}
		return nil, false, fmt.Errorf("model: no caller is registered for the %q dialect", rt.Mode)
	}
	base := strings.ToLower(rt.BaseURL)
	if rt.Command != "" || strings.HasPrefix(base, "acp://") || strings.HasPrefix(base, "acp+tcp://") {
		if callers.Process == nil {
			return nil, false, fmt.Errorf("model: no agent-program caller is registered for provider %s", rt.Provider)
		}
		c, err := callers.Process(ctx, rt)
		if err != nil {
			return nil, false, err
		}
		return c, true, nil
	}
	return nil, false, nil
}

func (s *Session) Do(ctx context.Context, p *Prepared, sink wire.Sink, o CallOptions) (*wire.Response, error) {
	if p == nil || p.Request == nil || (p.Call == nil && p.Caller == nil) {
		return nil, errors.New("model: a call needs a prepared request")
	}
	ctx = s.r.bind(ctx)
	if sink == nil {
		sink = func(wire.Event) {}
	}
	rt := s.Route()
	k, err := s.loadKnobs(ctx, rt)
	if err != nil {
		return nil, err
	}
	var sinkMu sync.Mutex
	serial := func(ev wire.Event) {
		sinkMu.Lock()
		defer sinkMu.Unlock()
		sink(ev)
	}
	x := &exchange{s: s, p: p, rt: rt, o: o, k: k, out: serial, mode: p.Mode, call: p.Call, ep: p.Endpoint,
		model: p.Request.Model, think: newThinkScrubber(), contentWatch: NewRunawayWatch(), reasoningWatch: NewRunawayWatch(),
		counted: map[int]bool{}, diag: newStreamDiag()}
	if x.model == "" {
		x.model = rt.Model
	}
	x.local = rt.BaseURL != "" && catalog.IsLocalEndpoint(rt.BaseURL)
	x.loop = rt.BaseURL != "" && httpclient.IsLoopbackHost(providers.URLHostname(rt.BaseURL))
	x.llama = s.streamsLlamaCpp(rt)
	if p.Caller != nil {
		base, err := (openai.ChatProtocol{}).Build(p.Request)
		if err != nil {
			return nil, err
		}
		x.call, x.callerBase = base, base
	}
	x.applyRequestMiddleware(ctx)
	x.est = RequestContextTokens(ctx, x.call.Body)
	p.Sent = x.call
	x.fireBefore(ctx)
	if k.dumpRequests {
		if _, err := s.dump(ctx, x, "preflight", nil); err != nil {
			slog.Warn(fmt.Sprintf("Failed to dump API request debug payload: %v", err))
		}
	}
	started := time.Now()
	resp, err := x.runExecution(ctx, func(ctx context.Context, call *wire.Call) (*wire.Response, error) {
		x.call = call
		p.Sent = call
		return x.execute(ctx)
	})
	if resp != nil {
		x.assemble(ctx, resp)
	}
	if err != nil && resp == nil {
		var ae *wire.APIError
		var ie *InterruptError
		x.mu.Lock()
		h := x.lastHeader
		x.mu.Unlock()
		if h != nil && !errors.As(err, &ae) && !errors.As(err, &ie) {
			err = &ResponseError{Header: h, Err: err}
		}
	}
	var cut *CutError
	if err == nil || errors.As(err, &cut) {
		x.fireAfter(ctx, resp, started)
	} else {
		x.fireFailed(ctx, err, started)
	}
	return resp, err
}

func (x *exchange) callerRequest() (*wire.Request, error) {
	req := *x.p.Request
	req.Messages = cloneMessages(x.p.Request.Messages)
	req.TopLevel = cloneRawMap(req.TopLevel)
	if x.callerBase == nil || bytes.Equal(x.call.Body, x.callerBase.Body) {
		return &req, nil
	}
	cv, err := wire.ParseValue(x.call.Body)
	if err != nil {
		return nil, err
	}
	bv, err := wire.ParseValue(x.callerBase.Body)
	if err != nil {
		return nil, err
	}
	cur, base := wire.DictOf(cv), wire.DictOf(bv)
	for _, mb := range cur.Members() {
		if base.Has(mb.Key) && bytes.Equal(base.Get(mb.Key).Raw(), mb.Value) {
			continue
		}
		v := cur.Get(mb.Key)
		switch mb.Key {
		case "messages":
			elems := v.Elems()
			msgs := make([]wire.Message, len(elems))
			for i, e := range elems {
				if err := msgs[i].UnmarshalJSON(e.Raw()); err != nil {
					return nil, fmt.Errorf("model: middleware message %d: %w", i, err)
				}
			}
			req.Messages = msgs
		case "model":
			if s, ok := v.Str(); ok {
				req.Model = s
			}
		case "max_tokens", "max_completion_tokens":
			if n, ok := v.Int(); ok {
				req.MaxTokens = int(n)
			}
		case "temperature":
			if f, ok := v.Float(); ok {
				req.Temperature, req.OmitTemperature = &f, false
			}
		case "stream", "stream_options":
		default:
			if req.TopLevel == nil {
				req.TopLevel = map[string]json.RawMessage{}
			}
			req.TopLevel[mb.Key] = append(json.RawMessage(nil), mb.Value...)
		}
	}
	for _, mb := range base.Members() {
		if cur.Has(mb.Key) {
			continue
		}
		switch mb.Key {
		case "tools":
			req.Tools = nil
		case "temperature":
			req.Temperature, req.OmitTemperature = nil, true
		case "max_tokens", "max_completion_tokens":
			req.MaxTokens = 0
		}
	}
	return &req, nil
}

func (x *exchange) execute(ctx context.Context) (*wire.Response, error) {
	stream := x.shouldStream(ctx)
	if ctx.Err() != nil {
		return nil, &InterruptError{Message: "Agent interrupted before streaming API call", Err: ctx.Err()}
	}
	if x.p.Caller != nil {
		if stream {
			return x.streamCall(ctx, x.callerAttempt)
		}
		return x.nonStream(ctx, x.callerDispatch)
	}
	switch x.mode {
	case wire.ModeResponses:
		if !stream {
			return x.nonStream(ctx, x.codexStream)
		}
		x.emit(wire.Event{Kind: wire.EventOpen})
		r, err := x.nonStream(ctx, x.codexStream)
		x.closeStream(r, err)
		return r, err
	case wire.ModeBedrock:
		if stream {
			return x.bedrockStream(ctx)
		}
		return x.nonStream(ctx, x.bedrockConverse)
	}
	if stream {
		return x.streamCall(ctx, x.httpStreamAttempt)
	}
	return x.nonStream(ctx, x.plainDispatch)
}

func (x *exchange) shouldStream(ctx context.Context) bool {
	if x.s.exec.streamOff() {
		return false
	}
	base := strings.ToLower(x.rt.BaseURL)
	if strings.HasPrefix(base, "acp://") || strings.HasPrefix(base, "acp+tcp://") {
		return false
	}
	reg := x.s.r.c.o.Providers
	streaming := providers.StreamingDefault
	if p, ok := reg.Get(ctx, x.rt.Provider); ok {
		streaming = p.Spec().Streaming
	}
	if streaming == providers.StreamingOff {
		return false
	}
	if x.externalProcess(ctx) && streaming != providers.StreamingOn {
		return false
	}
	if !x.o.Stream && x.rt.Provider == "moa" {
		return false
	}
	return true
}

func (x *exchange) externalProcess(ctx context.Context) bool {
	name := strings.ToLower(strings.TrimSpace(x.rt.Provider))
	if name == "" {
		return false
	}
	reg := x.s.r.c.o.Providers
	if row, ok := reg.RegistryRow(ctx, name); ok {
		return row.AuthType == "external_process"
	}
	p, ok := reg.Get(ctx, name)
	return ok && p.Spec().AuthType == "external_process"
}

func (x *exchange) facts() Facts {
	return Facts{Provider: x.rt.Provider, Model: x.rt.Model, BaseURL: x.rt.BaseURL, APIKey: x.rt.Credential.Key,
		ApproxTokens: x.o.Info.ApproxTokens, ContextLength: x.o.Info.ContextLength, Messages: len(x.p.Request.Messages)}
}

func (x *exchange) emit(ev wire.Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	x.out(ev)
}

func (x *exchange) closeStream(r *wire.Response, err error) {
	var cut *CutError
	if err != nil && (r == nil || !errors.As(err, &cut)) {
		x.emit(wire.Event{Kind: wire.EventClose, End: wire.StreamEnd{Error: err.Error()}})
		return
	}
	ev := wire.Event{Kind: wire.EventClose, End: wire.StreamEnd{Finished: true}}
	if r != nil {
		ev.Text = r.Message.FlatText("")
	}
	x.emit(ev)
}

func (x *exchange) waitNotice(text string) {
	x.emit(wire.Event{Kind: wire.EventNotice, Notice: wire.Notice{Kind: wire.NoticeWait, Text: text}})
}

func (x *exchange) diagWait(text string, elapsed time.Duration) {
	x.emit(wire.Event{Kind: wire.EventNotice, Notice: wire.Notice{Kind: wire.NoticeReconnect, Text: text, Elapsed: elapsed}})
}

func (x *exchange) bufferStatus(text string, attempt int, elapsed time.Duration) {
	x.emit(wire.Event{Kind: wire.EventNotice, Notice: wire.Notice{Kind: wire.NoticeDrop, Text: text, Attempt: attempt, Elapsed: elapsed}})
}

func (x *exchange) degrade(text string) {
	x.emit(wire.Event{Kind: wire.EventNotice, Notice: wire.Notice{Kind: wire.NoticeDegrade, Text: text}})
}

func (x *exchange) touch(text string) {
	x.emit(wire.Event{Kind: wire.EventKeepalive, Text: text})
}

func (x *exchange) requestURL() string {
	if x.call == nil {
		return x.ep.BaseURL
	}
	return strings.TrimRight(x.ep.BaseURL, "/") + x.call.Path
}

func (x *exchange) deliver(ev wire.Event) {
	now := time.Now()
	x.mu.Lock()
	if x.fenced {
		x.mu.Unlock()
		return
	}
	if !x.sse || ev.Kind != wire.EventKeepalive {
		x.lastChunk = now
	}
	if ev.Kind != wire.EventKeepalive && ev.Kind != wire.EventProgress && ev.Kind != wire.EventNotice && x.firstChunk.IsZero() {
		x.firstChunk = now
		x.diag.firstChunk = now
	}
	switch ev.Kind {
	case wire.EventProgress:
		pr := ev.Progress
		x.progress, x.progressAt = &pr, now
	case wire.EventReasoning:
		if !ev.Inline {
			x.nativeReasoning = true
		}
		if x.watchRunaway && x.reasoningWatch.Feed(ev.Text) {
			x.runaway = "reasoning"
		}
	case wire.EventToolStart:
		x.toolInFlight, x.toolStarted = true, true
		x.partialTools = append(x.partialTools, firstOf(ev.Tool.Name, "?"))
	case wire.EventToolArgs:
		x.toolInFlight = true
	case wire.EventText:
		x.mu.Unlock()
		x.textDelta(ev)
		return
	}
	cut := x.runaway != ""
	x.mu.Unlock()
	x.out(ev)
	if cut {
		x.cutRunaway()
	}
}

func (x *exchange) textDelta(ev wire.Event) {
	x.mu.Lock()
	if x.watchRunaway && x.contentWatch.Feed(ev.Text) {
		x.runaway = "content"
		x.mu.Unlock()
		x.cutRunaway()
		return
	}
	if x.toolStarted {
		if x.o.Display {
			x.visible.WriteString(ev.Text)
		}
		x.mu.Unlock()
		x.out(ev)
		return
	}
	x.deltasSent = true
	x.mu.Unlock()
	x.fireText(ev.Text, ev.At)
}

func (x *exchange) fireText(text string, at time.Time) {
	x.mu.Lock()
	visible := x.think.feed(text)
	hidden := x.think.lastHidden
	native := x.nativeReasoning
	if visible != "" && x.o.Display {
		x.visible.WriteString(visible)
	}
	x.mu.Unlock()
	if at.IsZero() {
		at = time.Now()
	}
	if hidden != "" && !native {
		x.out(wire.Event{Kind: wire.EventReasoning, Text: hidden, Inline: true, At: at})
	}
	if visible != "" {
		x.out(wire.Event{Kind: wire.EventText, Text: visible, At: at})
	}
}

func (x *exchange) resetDelivery() {
	x.mu.Lock()
	tail := x.think.flush()
	x.nativeReasoning = false
	x.visible.Reset()
	x.mu.Unlock()
	if tail != "" {
		x.emit(wire.Event{Kind: wire.EventText, Text: tail})
	}
}

func (x *exchange) flushThink() {
	x.mu.Lock()
	tail := x.think.flush()
	if tail != "" && x.o.Display {
		x.visible.WriteString(tail)
	}
	x.mu.Unlock()
	if tail != "" {
		x.emit(wire.Event{Kind: wire.EventText, Text: tail})
	}
}

func (x *exchange) cutRunaway() {
	x.mu.Lock()
	cancel := x.attemptCancel
	x.fenced = true
	x.mu.Unlock()
	if cancel != nil {
		cancel(errRunaway)
	}
}

func (x *exchange) visibleDelivered() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return strings.TrimSpace(x.visible.String()) != ""
}

func (x *exchange) startAttempt(cancel context.CancelCauseFunc) int {
	now := time.Now()
	x.mu.Lock()
	defer x.mu.Unlock()
	x.attemptID++
	x.attemptCancel = cancel
	x.fenced = false
	x.toolInFlight, x.toolStarted, x.progress, x.partialTools = false, false, nil, nil
	x.attemptStart, x.connected, x.wrote, x.headersAt = now, time.Time{}, time.Time{}, time.Time{}
	x.lastByte, x.lastChunk = now, now
	x.diag = newStreamDiag()
	x.codexEvents = 0
	return x.attemptID
}

func (x *exchange) killAttempt(cause error) {
	x.mu.Lock()
	cancel := x.attemptCancel
	x.fenced = true
	x.mu.Unlock()
	if cancel != nil {
		cancel(cause)
	}
}

func (x *exchange) release(id int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.released = id
	x.attemptCancel = nil
}

func causeOf(actx, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if actx.Err() != nil {
		if c := context.Cause(actx); c != nil && !errors.Is(c, context.Canceled) {
			return c
		}
	}
	return err
}

type watchedBody struct {
	io.ReadCloser
	x       *exchange
	sse     bool
	line    []byte
	cr      bool
	hasData bool
	event   string
	data    []byte
}

func (w *watchedBody) Read(p []byte) (int, error) {
	n, err := w.ReadCloser.Read(p)
	if n > 0 {
		now := time.Now()
		w.x.mu.Lock()
		w.x.lastByte = now
		w.x.diag.bytes += n
		w.x.mu.Unlock()
		if w.sse {
			w.scan(p[:n], now)
		}
	}
	return n, err
}

func (w *watchedBody) scan(b []byte, now time.Time) {
	for _, c := range b {
		if w.cr {
			w.cr = false
			if c == '\n' {
				continue
			}
		}
		if c == '\r' || c == '\n' {
			w.cr = c == '\r'
			w.endLine(now)
			continue
		}
		if len(w.line) < 64<<10 {
			w.line = append(w.line, c)
		}
	}
}

func (w *watchedBody) endLine(now time.Time) {
	line := w.line
	w.line = w.line[:0]
	if len(line) == 0 {
		if w.hasData {
			w.x.frame(w.event, w.data, now)
		}
		w.hasData, w.event, w.data = false, "", w.data[:0]
		return
	}
	if line[0] == ':' {
		return
	}
	field, value, found := bytes.Cut(line, []byte(":"))
	if found && len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	switch string(field) {
	case "data":
		w.hasData = true
		if w.x.codexFrames {
			if len(w.data) > 0 {
				w.data = append(w.data, '\n')
			}
			w.data = append(w.data, value...)
		}
	case "event":
		w.event = string(value)
	}
}

func (x *exchange) frame(event string, data []byte, now time.Time) {
	x.mu.Lock()
	if x.fenced {
		x.mu.Unlock()
		return
	}
	x.lastChunk = now
	x.diag.chunks++
	if !x.llama && x.firstChunk.IsZero() {
		x.firstChunk = now
		x.diag.firstChunk = now
	}
	codex := x.codexFrames
	x.mu.Unlock()
	if codex {
		x.codexEvent(codexEventHasContent(event, data), now)
	}
}

func codexEventHasContent(event string, data []byte) bool {
	v, err := wire.ParseValue(data)
	if err != nil || !v.IsObject() {
		return false
	}
	typ := v.Get("type").Text()
	if typ == "" {
		typ = event
	}
	switch typ {
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.text.delta", "response.audio.delta",
		"response.function_call_arguments.delta", "response.reasoning_text.delta", "response.refusal.delta":
		return v.Get("delta").Truthy()
	case "response.output_item.added":
		item := v.Get("item")
		if !strings.Contains(item.Get("type").Display(), "function_call") {
			return false
		}
		for _, f := range []string{"id", "call_id", "name", "arguments"} {
			if item.Get(f).Truthy() {
				return true
			}
		}
	}
	return false
}

func (x *exchange) send(ctx context.Context, call *wire.Call) (*http.Response, error) {
	sent := call
	if x.mode == wire.ModeResponses {
		c := *call
		c.Header = call.Header.Clone()
		c.Body = append([]byte(nil), call.Body...)
		if err := openai.CompressCodexCall(&c, x.ep.BaseURL); err != nil {
			return nil, err
		}
		sent = &c
	}
	target := strings.TrimRight(x.ep.BaseURL, "/") + sent.Path
	if _, err := url.Parse(target); err != nil {
		return nil, fmt.Errorf("model: request URL for %s is not valid: %w", x.rt.Provider, err)
	}
	trace := &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			x.mu.Lock()
			x.connected = time.Now()
			x.mu.Unlock()
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			x.mu.Lock()
			x.wrote = time.Now()
			x.mu.Unlock()
		},
	}
	method := sent.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), method, target, bytes.NewReader(sent.Body))
	if err != nil {
		return nil, err
	}
	for k, v := range x.ep.Header {
		req.Header[k] = append([]string(nil), v...)
	}
	for k, v := range sent.Header {
		req.Header[k] = append([]string(nil), v...)
	}
	if x.ep.Auth != nil {
		if err := x.ep.Auth.Authorize(ctx, req, sent.Body); err != nil {
			return nil, err
		}
	}
	client := x.ep.Client
	if client == nil {
		client = x.s.r.c.o.HTTP.Client()
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	x.mu.Lock()
	x.headersAt, x.lastByte = time.Now(), time.Now()
	x.diag.capture(resp)
	x.lastHeader = resp.Header.Clone()
	x.mu.Unlock()
	if resp.StatusCode >= 400 {
		body := wire.ReadErrorBody(ctx, resp.Body)
		return nil, sent.Decode.Error(resp.StatusCode, resp.Header, body)
	}
	resp.Body = &watchedBody{ReadCloser: resp.Body, x: x, sse: sent.Stream && x.sseProtocol()}
	return resp, nil
}

func (x *exchange) sseProtocol() bool {
	return x.mode != wire.ModeBedrock && !x.rt.BedrockAnthropic
}

func (x *exchange) stampResponse(r *wire.Response, h http.Header) {
	if r == nil {
		return
	}
	r.Header = h.Clone()
	if r.ServedModel == "" {
		r.ServedModel = wire.ServedModel(h)
	}
	x.mu.Lock()
	r.FirstByte = x.firstChunk
	x.mu.Unlock()
}

func (x *exchange) unary(ctx context.Context, call *wire.Call) (*wire.Response, error) {
	resp, err := x.send(ctx, call)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	x.mu.Lock()
	if x.firstChunk.IsZero() {
		x.firstChunk = time.Now()
	}
	x.mu.Unlock()
	r, err := call.Decode.Response(resp.Header, body)
	x.stampResponse(r, resp.Header)
	return r, err
}

func (x *exchange) unaryHops(ctx context.Context, call *wire.Call) (*wire.Response, error) {
	r, err := x.unary(ctx, call)
	for r != nil && r.Next != nil && ctx.Err() == nil {
		next := r.Next
		r, err = x.unary(ctx, next)
	}
	return r, err
}

func (x *exchange) plainDispatch(ctx context.Context) (*wire.Response, error) {
	return x.unaryHops(ctx, x.call)
}

func (x *exchange) variant(edit func(*wire.Request)) (*wire.Call, error) {
	base := *x.p.Request
	built, err := x.p.Protocol.Build(&base)
	if err != nil {
		return nil, err
	}
	changed := *x.p.Request
	changed.ExtraBody = cloneRawMap(changed.ExtraBody)
	changed.TopLevel = cloneRawMap(changed.TopLevel)
	edit(&changed)
	to, err := x.p.Protocol.Build(&changed)
	if err != nil {
		return nil, err
	}
	return rebase(x.call, built, to)
}

func rebase(cur, from, to *wire.Call) (*wire.Call, error) {
	out := *to
	if bytes.Equal(cur.Body, from.Body) {
		out.Body = to.Body
	} else {
		cv, err := wire.ParseValue(cur.Body)
		if err != nil {
			return nil, err
		}
		fv, err := wire.ParseValue(from.Body)
		if err != nil {
			return nil, err
		}
		tv, err := wire.ParseValue(to.Body)
		if err != nil {
			return nil, err
		}
		body, fromDict := wire.DictOf(cv), wire.DictOf(fv)
		toDict := wire.DictOf(tv)
		for _, mb := range toDict.Members() {
			if !fromDict.Has(mb.Key) || !bytes.Equal(fromDict.Get(mb.Key).Raw(), mb.Value) {
				body.Set(mb.Key, mb.Value)
			}
		}
		for _, mb := range fromDict.Members() {
			if !toDict.Has(mb.Key) {
				body.Delete(mb.Key)
			}
		}
		out.Body = wire.ScrubSurrogates(body.JSON())
	}
	out.Header = to.Header.Clone()
	for k, v := range cur.Header {
		if fv, ok := from.Header[k]; !ok || strings.Join(fv, "\x00") != strings.Join(v, "\x00") {
			out.Header[k] = append([]string(nil), v...)
		}
	}
	for k := range from.Header {
		if _, ok := cur.Header[k]; !ok {
			delete(out.Header, k)
		}
	}
	return &out, nil
}

func cloneRawMap[V any](m map[string]V) map[string]V {
	if m == nil {
		return nil
	}
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (x *exchange) streamVariant(includeUsage bool, bufferToolInput bool) (*wire.Call, error) {
	return x.variant(func(r *wire.Request) {
		r.Stream = true
		r.IncludeUsage = includeUsage
		if bufferToolInput {
			r.Anthropic.BufferToolInput = true
		}
		if x.llama {
			if r.ExtraBody == nil {
				r.ExtraBody = map[string]json.RawMessage{}
			}
			if _, ok := r.ExtraBody["return_progress"]; !ok {
				r.ExtraBody["return_progress"] = wire.Bool(true)
			}
		}
	})
}
