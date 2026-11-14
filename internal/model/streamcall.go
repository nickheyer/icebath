package model

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/anthropic"
	"github.com/nickheyer/icebath/internal/model/wire/gemini"
)

const streamProbeWindow = 60 * time.Second

var sseConnPhrases = []string{"connection lost", "connection reset", "connection closed", "connection terminated",
	"network error", "network connection", "terminated", "peer closed", "broken pipe", "upstream connect error"}

type attemptFunc func(actx context.Context) (*wire.Response, *CutError, error)

func (x *exchange) streamCall(ctx context.Context, attempt attemptFunc) (*wire.Response, error) {
	if err := x.s.checkStaleGiveup(x.k); err != nil {
		return nil, err
	}
	x.stale = x.streamStaleTimeout()
	x.mu.Lock()
	x.watchRunaway = true
	x.mu.Unlock()
	defer func() {
		x.mu.Lock()
		x.watchRunaway = false
		x.mu.Unlock()
	}()
	write, read, conn := x.streamTimeouts(x.stale)
	maxRetries := x.k.streamRetries.orInt(2)
	mon := x.startStreamMonitor(ctx, write, read, conn)
	defer mon.halt()
	compat := 0
	var lastErr error
	for n := 0; n <= maxRetries+compat; n++ {
		actx, cancel := context.WithCancelCause(ctx)
		id := x.startAttempt(cancel)
		if ctx.Err() != nil {
			cancel(nil)
			x.release(id)
			return nil, &InterruptError{Message: "Agent interrupted before stream retry", Err: ctx.Err()}
		}
		x.emit(wire.Event{Kind: wire.EventOpen})
		r, cut, err := attempt(actx)
		err = causeOf(actx, ctx, err)
		x.closeStream(r, err)
		cancel(nil)
		x.release(id)
		if ctx.Err() != nil {
			return nil, x.interruptedStream(ctx)
		}
		if err == nil {
			x.flushThink()
			x.s.exec.resetStreak()
			if cut != nil {
				return r, cut
			}
			return r, nil
		}
		lastErr = err
		retry, final, ferr := x.handleStreamError(ctx, err, n, maxRetries, &compat)
		if final != nil {
			return final, nil
		}
		if !retry {
			return x.streamFailure(ctx, ferr)
		}
		if x.routeSwitched() {
			return x.streamFailure(ctx, err)
		}
	}
	return x.streamFailure(ctx, lastErr)
}

func (x *exchange) streamFailure(ctx context.Context, err error) (*wire.Response, error) {
	if ctx.Err() != nil {
		return nil, x.interruptedStream(ctx)
	}
	x.mu.Lock()
	sent := x.deltasSent
	x.mu.Unlock()
	if sent {
		return x.partialStub(ctx, err)
	}
	return nil, err
}

func (x *exchange) interruptedStream(ctx context.Context) error {
	x.mu.Lock()
	staleElapsed := time.Since(x.lastChunk).Seconds()
	attempt, counted, started := x.attemptID, x.counted[x.attemptID], x.deltasSent
	x.mu.Unlock()
	if attempt > 0 && !counted && staleElapsed <= x.stale && x.recordInterruptedWait(staleElapsed, started) {
		x.mu.Lock()
		x.counted[attempt] = true
		x.mu.Unlock()
	}
	return &InterruptError{Message: "Agent interrupted during streaming API call", Err: ctx.Err()}
}

func (x *exchange) recordInterruptedWait(elapsed float64, responseStarted bool) bool {
	if responseStarted || elapsed < interruptStaleWait {
		return false
	}
	streak := x.s.exec.bumpStreak()
	slog.Warn(fmt.Sprintf("Interrupted provider wait counted as stale after %.0fs with no output; consecutive stale attempts=%d.", elapsed, streak))
	return true
}

func (x *exchange) routeSwitched() bool {
	live := x.s.Route()
	if live == x.rt || (live.Model == x.rt.Model && live.Provider == x.rt.Provider && live.BaseURL == x.rt.BaseURL && live.Mode == x.rt.Mode) {
		return false
	}
	slog.Warn(fmt.Sprintf("Stream retry skipped: model/provider switched mid-request (%s via %s -> %s via %s); handing back to the turn loop to rebuild the request for the current route.",
		x.rt.Model, firstOf(x.rt.Provider, x.rt.BaseURL), live.Model, firstOf(live.Provider, live.BaseURL)))
	return true
}

func (x *exchange) includeUsage() bool {
	_, native := x.p.Protocol.(gemini.Protocol)
	return !native && !x.s.exec.streamOptionsOff()
}

func (x *exchange) httpStreamAttempt(actx context.Context) (*wire.Response, *CutError, error) {
	x.mu.Lock()
	buffer := x.bufferTool
	x.sse = x.sseProtocol()
	x.mu.Unlock()
	call, err := x.streamVariant(x.includeUsage(), buffer)
	if err != nil {
		return nil, nil, err
	}
	r, err := x.streamHops(actx, call)
	return x.finishStream(r, err)
}

func (x *exchange) streamOnce(actx context.Context, call *wire.Call) (*wire.Response, error) {
	resp, err := x.send(actx, call)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	r, err := call.Decode.Stream(actx, resp.Body, x.deliver)
	x.stampResponse(r, resp.Header)
	if r != nil {
		x.mu.Lock()
		if r.Upstream != "" {
			x.diag.serving = truncateRunes(strings.TrimSpace(r.Upstream), 64)
		}
		x.diag.finishSeen = x.diag.finishSeen || r.RawFinish != ""
		x.mu.Unlock()
	}
	return r, err
}

func (x *exchange) streamHops(actx context.Context, call *wire.Call) (*wire.Response, error) {
	r, err := x.streamOnce(actx, call)
	for r != nil && r.Next != nil && actx.Err() == nil {
		next := r.Next
		if next.Stream {
			r, err = x.streamOnce(actx, next)
		} else {
			r, err = x.unary(actx, next)
		}
	}
	return r, err
}

func (x *exchange) callerAttempt(actx context.Context) (*wire.Response, *CutError, error) {
	req, err := x.callerRequest()
	if err != nil {
		return nil, nil, err
	}
	req.Stream, req.IncludeUsage = true, !x.s.exec.streamOptionsOff()
	x.mu.Lock()
	x.sse = false
	x.mu.Unlock()
	r, err := x.p.Caller.Call(actx, req, x.deliver)
	return x.finishStream(r, err)
}

func (x *exchange) callerDispatch(actx context.Context) (*wire.Response, error) {
	req, err := x.callerRequest()
	if err != nil {
		return nil, err
	}
	req.Stream, req.IncludeUsage = false, false
	return x.p.Caller.Call(actx, req, x.deliver)
}

func (x *exchange) runawayCut() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.runaway
}

func (x *exchange) finishStream(r *wire.Response, err error) (*wire.Response, *CutError, error) {
	if channel := x.runawayCut(); channel != "" {
		slog.Warn(fmt.Sprintf("Stream cut: the %s turned into a runaway repetition loop (model=%s, provider=%s).",
			channel, firstOf(x.model, "unknown"), firstOf(x.rt.Provider, "unknown")))
		if r == nil {
			r = &wire.Response{Message: wire.Message{Role: wire.RoleAssistant}, Model: x.model, Streamed: true}
		}
		r.FinishReason, r.RawFinish, r.Next = wire.FinishLength, x.lengthWord(), nil
		return r, &CutError{Runaway: true}, nil
	}
	if err != nil {
		if x.mode == wire.ModeChat && errors.Is(err, wire.ErrStreamDropped) && r != nil {
			return x.cleanEOFStub(r)
		}
		return r, nil, err
	}
	if r == nil {
		return nil, nil, wire.ErrEmptyStream
	}
	if x.mode == wire.ModeChat && r.RawFinish == "" && len(r.Message.ToolCalls) > 0 {
		dropped := false
		for _, c := range r.Message.ToolCalls {
			if IsRepetitionDominated(c.Arguments) {
				slog.Warn(fmt.Sprintf("Tool call '%s' has repetition-dominated arguments without a finish_reason; treating as a dropped tool call.", firstOf(c.Name, "?")))
				dropped = true
			}
		}
		if dropped {
			return x.cleanEOFStub(r)
		}
	}
	return r, nil, nil
}

func (x *exchange) lengthWord() string {
	if x.mode == wire.ModeAnthropic {
		return "max_tokens"
	}
	return string(wire.FinishLength)
}

func (x *exchange) cleanEOFStub(r *wire.Response) (*wire.Response, *CutError, error) {
	var names []string
	for _, c := range r.Message.ToolCalls {
		names = append(names, firstOf(c.Name, "?"))
	}
	if len(names) > 0 {
		slog.Warn(fmt.Sprintf("Clean EOF, no finish_reason: server ended the stream (no transport exception) while a tool call's arguments were still incomplete (tools=%v). The server or a proxy closed the stream cleanly; not an output-length truncation.", names))
	} else {
		slog.Warn("Clean EOF, no finish_reason: server ended the stream (no transport exception) after delivering text with no tool calls. The server or a proxy closed the stream cleanly.")
	}
	reasoning := r.Message.ReasoningContent
	if (reasoning == nil || *reasoning == "") && r.Message.Text != "" {
		if inline := extractReasoning(&wire.Message{Text: r.Message.Text}); inline != "" {
			reasoning = &inline
		}
	}
	stub := x.stub(r.Message.Text, reasoning, r.Model, r.Usage)
	stub.Header, stub.ServedModel, stub.Upstream, stub.FirstByte = r.Header, r.ServedModel, r.Upstream, r.FirstByte
	return stub, &CutError{DroppedTools: names, CleanEOF: true}, nil
}

func (x *exchange) stub(text string, reasoning *string, model string, usage *wire.Usage) *wire.Response {
	return &wire.Response{ID: PartialStreamStubID, Model: model, FinishReason: wire.FinishLength, RawFinish: x.lengthWord(),
		Message: wire.Message{Role: wire.RoleAssistant, Text: text, ReasoningContent: reasoning}, Usage: usage, Streamed: true}
}

func (x *exchange) partialStub(ctx context.Context, err error) (*wire.Response, error) {
	x.mu.Lock()
	text := strings.TrimSpace(x.visible.String())
	names := append([]string(nil), x.partialTools...)
	x.mu.Unlock()
	if len(names) > 0 {
		shown := strings.Join(names[:min(3, len(names))], ", ")
		if len(names) > 3 {
			shown += fmt.Sprintf(", +%d more", len(names)-3)
		}
		warn := fmt.Sprintf("\n\n⚠ Stream stalled mid tool-call (%s); the action was not executed. Ask me to retry if you want to continue.", shown)
		text += warn
		if x.o.Warnings {
			x.fireText(warn, time.Time{})
		}
		slog.Warn(fmt.Sprintf("Partial stream dropped tool call(s) %v after %d chars of text; surfaced warning to user: %v", names, len([]rune(text)), err))
	}
	cls := x.s.r.c.classifier.Classify(ctx, err, x.facts())
	x.s.exec.resetStreak()
	model := firstOf(x.rt.Model, "unknown")
	cut := &CutError{DroppedTools: names, Cause: err}
	if cls != nil && cls.Class == ClassContextOverflow {
		slog.Warn(fmt.Sprintf("Partial stream ended on a context-overflow error after %d chars; NOT seeding a continuation stub (transcript is already over budget): %v", len([]rune(text)), err))
		cut.OverflowTerminal = true
		return x.stub("", nil, model, nil), cut
	}
	if len(names) == 0 {
		slog.Warn(fmt.Sprintf("Partial stream delivered before error; returning length-truncated stub with %d chars of recovered content so the loop can continue from where the stream died: %v", len([]rune(text)), err))
	}
	if cls != nil && cls.Class == ClassContentPolicyBlocked {
		cut.ContentFilter = true
	}
	return x.stub(text, nil, model, nil), cut
}

func timedOut(err error) bool {
	var te *DeadlineError
	var lifetime *StreamLifetimeError
	var he *httpclient.Error
	if errors.As(err, &te) || errors.As(err, &lifetime) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, httpclient.ErrTimeout) {
		return true
	}
	if errors.As(err, &he) && he.Timeout() {
		return true
	}
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

func connError(err error) bool {
	var ae *wire.APIError
	if errors.As(err, &ae) {
		return false
	}
	return connectionDropped(err) || transportFailure(err)
}

func emptyStream(err error, mode wire.Mode) bool {
	if errors.Is(err, wire.ErrEmptyStream) {
		return true
	}
	return mode == wire.ModeAnthropic && errors.Is(err, wire.ErrStreamDropped)
}

func sseConnError(err error) bool {
	var ae *wire.APIError
	if !errors.As(err, &ae) || ae.Status != 0 {
		return false
	}
	return containsAny(strings.ToLower(err.Error()), sseConnPhrases)
}

func statusOf(err error) int {
	var ae *wire.APIError
	if errors.As(err, &ae) && ae.Status != 0 {
		return ae.Status
	}
	var se *httpclient.StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

func rejectsStreamOptions(err error) bool {
	var ae *wire.APIError
	if !errors.As(err, &ae) || (ae.Status != 400 && ae.Status != 422) {
		return false
	}
	body := strings.ToLower(string(ae.Body()) + " " + err.Error())
	return strings.Contains(body, "stream_options") &&
		containsAny(body, []string{"extra", "not supported", "unrecognized", "unexpected", "unknown"})
}

func emptyFrameError(err error) bool {
	var se *wire.StreamError
	if errors.As(err, &se) && se.Code == wire.StreamEmptyFrame {
		return true
	}
	var ae *wire.APIError
	return errors.As(err, &ae) && ae.Code() == wire.StreamEmptyFrame
}

func (x *exchange) forgetInvisible(err error) bool {
	x.mu.Lock()
	tool := len(x.partialTools) > 0 || x.toolInFlight
	sent := x.deltasSent
	x.mu.Unlock()
	if sent && !tool && !x.visibleDelivered() {
		slog.Warn(fmt.Sprintf("Stream died after deltas but before any visible text was delivered (0 chars, no tool call in flight); treating as an undelivered stream failure: %v", err))
		x.resetDelivery()
		x.mu.Lock()
		x.deltasSent = false
		x.mu.Unlock()
	}
	return tool
}

func (x *exchange) handleStreamError(ctx context.Context, e error, attempt, maxRetries int, compat *int) (bool, *wire.Response, error) {
	isTimeout := timedOut(e)
	isConn := !isTimeout && connError(e)
	isParse := IsStreamParseError(e, x.mode)
	isEmpty := emptyStream(e, x.mode)
	isSSEConn := !isTimeout && !isConn && sseConnError(e)
	transient := isTimeout || isConn || isSSEConn || isParse
	x.mu.Lock()
	sent := x.deltasSent
	x.mu.Unlock()
	if !sent && !x.s.exec.streamOptionsOff() && rejectsStreamOptions(e) {
		x.s.exec.disableStreamOptions()
		*compat = 1
		slog.Info(fmt.Sprintf("Endpoint rejected stream_options (HTTP %d); retrying without it for this session.", statusOf(e)))
		return true, nil, nil
	}
	tool := x.forgetInvisible(e)
	x.mu.Lock()
	sent = x.deltasSent
	x.mu.Unlock()
	if sent {
		if !(tool && transient && attempt < maxRetries) {
			slog.Warn(fmt.Sprintf("Streaming failed after partial delivery, not retrying: %v", e))
			return false, nil, e
		}
		if x.o.Warnings {
			x.fireText("\n\n⚠ Connection dropped mid tool-call; reconnecting…\n\n", time.Time{})
		}
		x.resetDelivery()
		x.mu.Lock()
		x.deltasSent = false
		x.mu.Unlock()
		x.retryAfterDrop(ctx, e, attempt, maxRetries, true)
		return true, nil, nil
	}
	if transient || isEmpty {
		if attempt < maxRetries {
			x.retryAfterDrop(ctx, e, attempt, maxRetries, false)
			return true, nil, nil
		}
		x.mu.Lock()
		d := x.diag
		x.mu.Unlock()
		x.logStreamRetry("exhausted", e, maxRetries+1, maxRetries+1, false, d)
		if isParse || isEmpty {
			what := "Provider returned an empty response stream after"
			if isParse {
				what = "Provider returned malformed streaming data after"
			}
			x.bufferStatus(fmt.Sprintf("❌ %s %d attempts. The provider may be experiencing issues — try again in a moment.", what, maxRetries+1), maxRetries+1, 0)
		} else {
			x.bufferConnectExhausted(e, maxRetries+1)
		}
		return false, nil, e
	}
	x.maybeDisableStreaming(e)
	slog.Error(fmt.Sprintf("Streaming failed before delivery: %v", e))
	if handled, r, err := x.unmask(ctx, e); handled {
		return false, r, err
	}
	return false, nil, e
}

func (x *exchange) retryAfterDrop(ctx context.Context, e error, attempt, maxRetries int, midToolCall bool) {
	x.mu.Lock()
	d := x.diag
	x.mu.Unlock()
	x.emitStreamDrop(e, attempt+1, maxRetries+1, midToolCall, d)
	if IsStreamParseError(e, x.mode) {
		x.mu.Lock()
		x.bufferTool = true
		x.mu.Unlock()
	}
	wait := httpclient.Backoff(attempt+1, time.Second, 4*time.Second, 0)
	t := time.NewTimer(wait)
	select {
	case <-ctx.Done():
	case <-t.C:
	}
	t.Stop()
	x.mu.Lock()
	x.lastChunk = time.Now()
	x.mu.Unlock()
}

func (x *exchange) maybeDisableStreaming(e error) {
	if x.llama {
		return
	}
	model, provider := firstOf(x.rt.Model, "unknown"), firstOf(x.rt.Provider, "unknown")
	if emptyFrameError(e) {
		x.s.exec.disableStream()
		slog.Warn(fmt.Sprintf("Provider stream returned an empty SSE frame (keepalive, no payload) before any delta — switching %s/%s to non-streaming for this session.", provider, model))
		x.degrade(warnEmoji + " Provider stream returned an empty keepalive frame — retrying this turn without streaming (streaming stays off for this session).")
		return
	}
	if !anthropic.StreamUnavailable(e) {
		return
	}
	lower := strings.ToLower(e.Error())
	unsupported := strings.Contains(lower, "stream") && strings.Contains(lower, "not supported")
	if strings.Contains(lower, "unexpected event order") && !unsupported {
		if x.mode != wire.ModeAnthropic || x.rt.Provider == "bedrock" {
			return
		}
		x.s.exec.disableStream()
		x.degrade("\n⚠  Provider sent Anthropic stream events out of order. Switching to non-streaming.\n")
		return
	}
	x.s.exec.disableStream()
	if unsupported {
		x.degrade("\n⚠  Streaming is not supported for this model/provider. Switching to non-streaming.\n   To avoid this delay, set display.streaming: false in config.yaml\n")
		return
	}
	x.degrade("\n⚠  AWS IAM denied bedrock:InvokeModelWithResponseStream. Switching to non-streaming.\n   Grant that action to restore streaming output.\n")
}

func (x *exchange) unmask(ctx context.Context, e error) (bool, *wire.Response, error) {
	if ctx.Err() != nil || x.llama {
		return false, nil, nil
	}
	x.mu.Lock()
	sent := x.deltasSent
	x.mu.Unlock()
	status := statusOf(e)
	if status == 0 || status < 500 || sent || x.mode != wire.ModeChat {
		return false, nil, nil
	}
	if !x.s.exec.claimProbe(time.Now(), streamProbeWindow) {
		return false, nil, nil
	}
	dispatch := x.plainDispatch
	if x.p.Caller != nil {
		dispatch = x.callerDispatch
	}
	x.setSuspended(true)
	probe, perr := x.nonStream(ctx, dispatch)
	x.setSuspended(false)
	if perr != nil {
		var ie *InterruptError
		if errors.As(perr, &ie) {
			return true, nil, perr
		}
		if ps := statusOf(perr); ps != 0 && ps < 500 {
			slog.Info(fmt.Sprintf("Non-streaming unmask probe surfaced the underlying error: %v", perr))
			return true, nil, perr
		}
		slog.Info(fmt.Sprintf("Non-streaming unmask probe failed: %v", perr))
		return false, nil, nil
	}
	slog.Info(fmt.Sprintf("Streaming 5xx re-issued non-streaming successfully for %s/%s (not latched: the 5xx may be transient).",
		firstOf(x.rt.Provider, "unknown"), firstOf(x.rt.Model, "unknown")))
	x.bufferStatus("⚠  Streaming failed with a provider server error; the non-streaming retry succeeded.", 0, 0)
	x.emit(wire.Event{Kind: wire.EventOpen})
	x.replayFinal(probe)
	x.closeStream(probe, nil)
	return true, probe, nil
}

func (x *exchange) replayFinal(r *wire.Response) {
	if r == nil {
		return
	}
	reasoning := ""
	if r.Message.ReasoningContent != nil {
		reasoning = *r.Message.ReasoningContent
	}
	if reasoning == "" {
		reasoning = r.Message.Reasoning
	}
	if reasoning != "" {
		x.mu.Lock()
		x.nativeReasoning = true
		x.mu.Unlock()
		x.emit(wire.Event{Kind: wire.EventReasoning, Text: reasoning})
	}
	if r.Message.Parts == nil && r.Message.Text != "" {
		x.fireText(r.Message.Text, time.Time{})
		x.flushThink()
	}
}

func (x *exchange) setSuspended(on bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.staleSuspended = on
	x.watchRunaway = !on
}

type streamMonitor struct {
	x             *exchange
	ctx           context.Context
	stop          chan struct{}
	done          chan struct{}
	write         float64
	read          float64
	conn          float64
	lifetime      float64
	lifeAttempt   int
	lifeStarted   time.Time
	lastHeartbeat time.Time
	lastLoadPoll  time.Time
	loadShown     bool
	loadMisses    int
	noticeStarted time.Time
	notice        waitNoticeState
	wall          time.Time
}

func resolveStreamLifetime(configured, stale float64) float64 {
	if configured <= 0 || !finite(configured) {
		return 0
	}
	if finite(stale) {
		return math.Max(configured, stale)
	}
	return configured
}

func (x *exchange) startStreamMonitor(ctx context.Context, write, read, conn float64) *streamMonitor {
	now := time.Now()
	m := &streamMonitor{x: x, ctx: ctx, stop: make(chan struct{}), done: make(chan struct{}), write: write, read: read, conn: conn,
		lifetime: resolveStreamLifetime(x.k.lifetimeCfg, x.stale), lifeStarted: now, lastHeartbeat: now, wall: now}
	slog.Debug(fmt.Sprintf("stream watchdog armed: stale=%.0fs cap=%.0fs model=%s", x.stale, m.lifetime, firstOf(x.model, "unknown")))
	go m.run()
	return m
}

func (m *streamMonitor) halt() {
	close(m.stop)
	<-m.done
}

func (m *streamMonitor) run() {
	defer close(m.done)
	ticker := time.NewTicker(monitorPoll)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.tick()
		}
	}
}

func (m *streamMonitor) tick() {
	x := m.x
	if x.loop {
		m.skipSleep()
	}
	now := time.Now()
	if x.local && m.pollLocalLoad(now) {
		return
	}
	x.mu.Lock()
	lastChunk, suspended := x.lastChunk, x.staleSuspended
	x.mu.Unlock()
	if !m.noticeStarted.IsZero() && lastChunk.After(m.noticeStarted) {
		x.waitNotice("")
		m.noticeStarted = time.Time{}
		m.notice.reset()
	}
	if now.Sub(m.lastHeartbeat) >= heartbeatInterval {
		m.lastHeartbeat = now
		m.heartbeat(int(now.Sub(lastChunk).Seconds()))
	}
	if suspended {
		return
	}
	if elapsed := time.Since(lastChunk).Seconds(); elapsed > x.stale {
		m.noticeStarted = time.Time{}
		m.notice.reset()
		m.killStale(elapsed)
	}
	m.checkLifetime()
	if x.p.Caller == nil {
		m.checkPhases()
	}
}

func (m *streamMonitor) skipSleep() {
	x := m.x
	wall := time.Now()
	stopped := wall.Sub(m.wall).Seconds() - monitorPoll.Seconds()
	m.wall = wall
	if stopped < 5 {
		return
	}
	slept := seconds(stopped)
	x.mu.Lock()
	x.lastChunk = minTime(x.lastChunk.Add(slept), wall)
	x.mu.Unlock()
	m.lastHeartbeat = minTime(m.lastHeartbeat.Add(slept), wall)
	slog.Info(fmt.Sprintf("Computer slept for %.0fs during a model request; the stall timer skips that time.", stopped))
	x.touch(fmt.Sprintf("resumed after the computer slept for %ds", int(stopped)))
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (m *streamMonitor) showPromptProgress(now time.Time) bool {
	x := m.x
	x.mu.Lock()
	progress, at, first := x.progress, x.progressAt, !x.firstChunk.IsZero()
	x.mu.Unlock()
	if progress == nil {
		return false
	}
	if first {
		if m.loadShown {
			m.loadShown, m.loadMisses = false, 0
			x.waitNotice("")
		}
		return false
	}
	if now.Sub(at) > 60*time.Second || now.Sub(m.lastLoadPoll) < time.Second {
		return false
	}
	m.lastLoadPoll = now
	m.noticeStarted = time.Time{}
	m.notice.reset()
	if progress.Total > 0 {
		percent := max(0, min(100, int(math.RoundToEven(float64(progress.Done)/float64(progress.Total)*100))))
		x.waitNotice(fmt.Sprintf("⚙ processing prompt — %d%%", percent))
	} else {
		x.waitNotice("⚙ processing prompt")
	}
	m.loadShown, m.loadMisses, m.lastHeartbeat = true, 0, now
	return true
}

func (m *streamMonitor) pollLocalLoad(now time.Time) bool {
	x := m.x
	if m.showPromptProgress(now) {
		return true
	}
	x.mu.Lock()
	lastChunk := x.lastChunk
	x.mu.Unlock()
	if now.Sub(lastChunk) < 2*time.Second || now.Sub(m.lastLoadPoll) < time.Second {
		return false
	}
	m.lastLoadPoll = now
	if text := x.managedLoadNotice(m.ctx); text != "" {
		m.noticeStarted = time.Time{}
		m.notice.reset()
		x.waitNotice(text)
		x.touch("local model loading")
		m.loadShown, m.loadMisses, m.lastHeartbeat = true, 0, now
		return true
	}
	if m.loadShown {
		m.loadMisses++
		if m.loadMisses >= 3 {
			m.loadShown, m.loadMisses = false, 0
			x.waitNotice("")
		}
	}
	return false
}

func (x *exchange) managedLoadNotice(ctx context.Context) string {
	if x.rt.BaseURL == "" {
		return ""
	}
	ep := x.s.r.c.o.Local.ManagedEndpoint()
	if ep == nil {
		return ""
	}
	managed, err := url.Parse(ep.BaseURL)
	if err != nil || managed.Host == "" {
		return ""
	}
	base, err := url.Parse(x.rt.BaseURL)
	if err != nil || !strings.EqualFold(base.Host, managed.Host) {
		return ""
	}
	if progress, ok := x.s.r.c.o.Local.LoadingProgress()[x.model]; ok {
		return fmt.Sprintf("⏳ loading %s into memory — %d%% (responses start once the model is loaded)", x.model, progress.Percent)
	}
	processed, ok := x.s.r.c.o.Local.PrefillProgress(ctx, x.model)
	if !ok {
		return ""
	}
	if total := int64(x.est); total > 0 && total >= processed {
		return fmt.Sprintf("⚙ processing prompt — %d%%", max(0, min(100, int(math.RoundToEven(float64(processed)/float64(total)*100)))))
	}
	return "⚙ processing prompt"
}

func (m *streamMonitor) heartbeat(waiting int) {
	x := m.x
	if waiting >= int(waitNoticeSilence) {
		var w watchdogDeadline
		if finite(x.stale) {
			w = watchdogDeadline{label: "stream stale", remaining: x.stale - float64(waiting), set: true}
		}
		x.mu.Lock()
		phase := "first_chunk"
		if !x.firstChunk.IsZero() {
			phase = "post_chunk"
		}
		x.mu.Unlock()
		if !m.notice.shouldEmit(phase, w) {
			x.touch(fmt.Sprintf("waiting for stream response (%ds, %s)", waiting, phase))
			return
		}
		m.noticeStarted = m.lastHeartbeat
		x.waitNotice(waitNoticeText(firstOf(x.model, "the provider"), float64(waiting), phase, w))
		return
	}
	x.touch(fmt.Sprintf("waiting for stream response (%ds, no chunks yet)", waiting))
}

func (m *streamMonitor) killStale(elapsed float64) {
	x := m.x
	model := firstOf(x.model, "unknown")
	slog.Warn(fmt.Sprintf("Stream stale for %.0fs (threshold %.0fs) — no chunks received. model=%s context=~%s tokens. Killing connection.",
		elapsed, x.stale, model, thousands(x.est)))
	x.bufferStatus(fmt.Sprintf("%s No response from provider for %ds (model: %s, context: ~%s tokens). Reconnecting...",
		warnEmoji, int(elapsed), model, thousands(x.est)), 0, seconds(elapsed))
	x.killAttempt(&DeadlineError{Message: fmt.Sprintf("Stream stale for %ds (threshold %ds): no chunks received, connection killed", int(elapsed), int(x.stale))})
	x.mu.Lock()
	attempt := x.attemptID
	count := attempt > 0 && !x.counted[attempt]
	if count {
		x.counted[attempt] = true
	}
	now := time.Now()
	x.lastChunk = now
	x.mu.Unlock()
	if count {
		x.s.exec.bumpStreak()
	}
	m.lifeStarted = now
	x.diagWait(fmt.Sprintf("⚠ no output from provider for %ds — reconnecting...", int(elapsed)), seconds(elapsed))
	x.touch(fmt.Sprintf("stale stream detected after %ds, reconnecting", int(elapsed)))
}

func (m *streamMonitor) checkLifetime() {
	x := m.x
	if m.lifetime == 0 {
		return
	}
	x.mu.Lock()
	attempt, released, active := x.attemptID, x.released, x.attemptCancel != nil
	x.mu.Unlock()
	if attempt == 0 || attempt == released || !active {
		return
	}
	now := time.Now()
	if attempt != m.lifeAttempt {
		m.lifeAttempt, m.lifeStarted = attempt, now
		return
	}
	elapsed := now.Sub(m.lifeStarted)
	if elapsed.Seconds() <= m.lifetime {
		return
	}
	m.lifeStarted = now
	model := firstOf(x.model, "unknown")
	slog.Warn(fmt.Sprintf("Stream still running after %.0fs (stream_max_lifetime %.0fs) — killing connection. model=%s", elapsed.Seconds(), m.lifetime, model))
	x.bufferStatus(fmt.Sprintf("%s Response still streaming after %ds (stream_max_lifetime %ds, model: %s). Aborting...",
		warnEmoji, int(elapsed.Seconds()), int(m.lifetime), model), 0, elapsed)
	x.killAttempt(&StreamLifetimeError{Elapsed: elapsed, Cap: seconds(m.lifetime)})
	x.mu.Lock()
	x.counted[attempt] = true
	x.lastChunk = time.Now()
	x.mu.Unlock()
}

func (m *streamMonitor) checkPhases() {
	if err := m.x.phaseTimeout(m.conn, m.write, m.read); err != nil {
		m.x.killAttempt(err)
	}
}

func (x *exchange) phaseTimeout(conn, write, read float64) error {
	now := time.Now()
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.attemptCancel == nil || x.attemptStart.IsZero() {
		return nil
	}
	host := hostOfURL(x.ep.BaseURL)
	switch {
	case x.connected.IsZero():
		if finite(conn) && conn > 0 && now.Sub(x.attemptStart).Seconds() > conn {
			return &httpclient.Error{Phase: httpclient.PhaseConnect, Host: host, Err: httpclient.ErrTimeout}
		}
	case x.wrote.IsZero():
		if finite(write) && write > 0 && now.Sub(x.connected).Seconds() > write {
			return &httpclient.Error{Phase: httpclient.PhaseWrite, Host: host, Err: httpclient.ErrTimeout}
		}
	default:
		since := x.lastByte
		if x.wrote.After(since) {
			since = x.wrote
		}
		if finite(read) && read > 0 && now.Sub(since).Seconds() > read {
			phase := httpclient.PhaseBody
			if x.headersAt.IsZero() {
				phase = httpclient.PhaseHeader
			}
			return &httpclient.Error{Phase: phase, Host: host, Err: httpclient.ErrTimeout}
		}
	}
	return nil
}

func hostOfURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
