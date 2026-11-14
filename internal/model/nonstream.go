package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/bedrock"
	"github.com/nickheyer/icebath/internal/model/wire/openai"
)

const (
	codexStreamRetries  = 1
	zeroEventTurnBudget = 200000
	zeroEventResultSize = 100000
)

type dispatchResult struct {
	r   *wire.Response
	err error
}

func (x *exchange) nonStream(ctx context.Context, dispatch func(context.Context) (*wire.Response, error)) (*wire.Response, error) {
	if err := x.s.checkStaleGiveup(x.k); err != nil {
		return nil, err
	}
	wd := x.nonStreamWatchdogs()
	callStart := time.Now()
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	x.mu.Lock()
	x.codex, x.phaseAware, x.codexFrames, x.fenced = codexWatchState{}, wd.idleNeedProgress, wd.codex, false
	x.attemptCancel, x.attemptStart, x.lastByte = cancel, callStart, callStart
	x.connected, x.wrote, x.headersAt = time.Time{}, time.Time{}, time.Time{}
	x.mu.Unlock()
	x.touch("waiting for non-streaming API response")
	done := make(chan dispatchResult, 1)
	go func() {
		r, err := dispatch(actx)
		done <- dispatchResult{r, causeOf(actx, ctx, err)}
	}()
	ticker := time.NewTicker(monitorPoll)
	defer ticker.Stop()
	base := x.p.Request.Timeout.Seconds()
	polls := 0
	var notice waitNoticeState
	var noticeStarted time.Time
	finish := func(res dispatchResult) (*wire.Response, error) {
		x.mu.Lock()
		x.attemptCancel = nil
		x.mu.Unlock()
		if res.err != nil && ctx.Err() != nil {
			return nil, &InterruptError{Message: "Agent interrupted during API call", Err: ctx.Err()}
		}
		if res.err != nil {
			return nil, res.err
		}
		if res.r != nil {
			x.flushThink()
			x.s.exec.resetStreak()
		}
		return res.r, nil
	}
	kill := func(err error) (*wire.Response, error) {
		x.mu.Lock()
		x.fenced = true
		x.mu.Unlock()
		cancel(err)
		res := <-done
		if res.err == nil && res.r != nil {
			return finish(res)
		}
		return finish(dispatchResult{err: err})
	}
	for {
		select {
		case res := <-done:
			return finish(res)
		case <-ctx.Done():
			x.mu.Lock()
			started := wd.codex && !x.codex.lastEvent.IsZero()
			x.mu.Unlock()
			x.recordInterruptedWait(time.Since(callStart).Seconds(), started)
			slog.Debug("Force-closing the request due to interrupt (not a network error).")
			cancel(ctx.Err())
			<-done
			x.mu.Lock()
			x.attemptCancel = nil
			x.mu.Unlock()
			return nil, &InterruptError{Message: "Agent interrupted during API call", Err: ctx.Err()}
		case <-ticker.C:
		}
		polls++
		now := time.Now()
		elapsed := now.Sub(callStart).Seconds()
		x.nonStreamNotice(wd, callStart, elapsed, polls%100 == 0, &notice, &noticeStarted)
		x.mu.Lock()
		st := x.codex
		x.mu.Unlock()
		attemptStart := callStart
		if !st.retryStarted.IsZero() {
			attemptStart = st.retryStarted
		}
		attemptElapsed := now.Sub(attemptStart).Seconds()
		preProgress := wd.progress > 0 && !st.lastEvent.IsZero() && st.lastProgress.IsZero()
		switch {
		case wd.ttfbOn && st.lastEvent.IsZero() && attemptElapsed > wd.ttfb:
			return kill(x.ttfbKill(wd, attemptElapsed))
		case preProgress && attemptElapsed > wd.progress:
			return kill(x.progressKill(wd, attemptElapsed))
		case st.retryStarted.IsZero() && wd.idleOn && !st.lastEvent.IsZero() && (!wd.idleNeedProgress || !st.lastProgress.IsZero()) &&
			now.Sub(st.lastEvent).Seconds() > wd.idle:
			return kill(x.idleKill(wd, now.Sub(st.lastEvent).Seconds()))
		case elapsed > wd.stale:
			return kill(x.staleNonStreamKill(wd, elapsed))
		}
		if x.p.Caller == nil {
			if err := x.phaseTimeout(base, base, base); err != nil {
				return kill(err)
			}
		}
	}
}

func (x *exchange) nonStreamNotice(wd watchdogs, callStart time.Time, elapsed float64, heartbeat bool, notice *waitNoticeState, noticeStarted *time.Time) {
	x.mu.Lock()
	st := x.codex
	x.mu.Unlock()
	attemptStarted := callStart
	if !st.retryStarted.IsZero() {
		attemptStarted = st.retryStarted
	}
	pre := wd.progress > 0 && !st.lastEvent.IsZero() && st.lastProgress.IsZero()
	activity := st.lastEvent
	switch {
	case pre:
		activity = attemptStarted
	case !st.retryStarted.IsZero():
		activity = st.retryStarted
	}
	if !noticeStarted.IsZero() && !activity.IsZero() && activity.After(*noticeStarted) {
		x.waitNotice("")
		*noticeStarted = time.Time{}
		notice.reset()
	}
	if !heartbeat {
		return
	}
	silence := elapsed
	if !activity.IsZero() {
		silence = elapsed - activity.Sub(callStart).Seconds()
	}
	if silence < waitNoticeSilence {
		if !st.retryStarted.IsZero() {
			x.touch("waiting for first stream event after reconnect")
		} else {
			x.touch("waiting for provider response")
		}
		return
	}
	phase := "first_event"
	switch {
	case pre:
		phase = "pre_progress"
	case !st.retryStarted.IsZero():
		phase = "reconnect"
	case !st.lastEvent.IsZero():
		phase = "post_event"
	}
	w := codexWatchdogDeadline(wd, st, callStart, elapsed)
	if !notice.shouldEmit(phase, w) {
		x.touch(fmt.Sprintf("waiting for provider response (%ds, %s)", int(silence), phase))
		return
	}
	x.waitNotice(waitNoticeText(firstOf(x.model, "the provider"), silence, phase, w))
	*noticeStarted = callStart.Add(seconds(elapsed))
}

func withHint(text, hint, sep string) string {
	if hint == "" {
		return text
	}
	return text + sep + hint
}

func (x *exchange) ttfbKill(wd watchdogs, elapsed float64) error {
	hint := x.codexSilentHangHint()
	model := firstOf(x.model, "unknown")
	slog.Warn(fmt.Sprintf("Codex stream produced no parsed stream event within TTFB cutoff (%.0fs > %.0fs, model=%s). Backend accepted the connection but sent no stream events. Killing connection so the retry loop can reconnect.",
		elapsed, wd.ttfb, model))
	x.bufferStatus(withHint(fmt.Sprintf("%s No first stream event from provider in %ds (codex stream, model: %s). Reconnecting.", warnEmoji, int(elapsed), model), hint, " "), 0, seconds(elapsed))
	x.diagWait(fmt.Sprintf("⚠ no response from provider in %ds — reconnecting...", int(elapsed)), seconds(elapsed))
	x.touch(fmt.Sprintf("codex stream killed after %ds with no first stream event", int(elapsed)))
	return &DeadlineError{Message: withHint(fmt.Sprintf("Codex stream produced no parsed stream event within %ds (TTFB threshold: %ds)", int(elapsed), int(wd.ttfb)), hint, ". ")}
}

func (x *exchange) progressKill(wd watchdogs, elapsed float64) error {
	model := firstOf(x.model, "unknown")
	slog.Warn(fmt.Sprintf("Codex stream produced lifecycle events but no substantive model progress for %.0fs (threshold %.0fs, model=%s, context=~%s tokens). Reconnecting.",
		elapsed, wd.progress, model, thousands(x.est)))
	x.bufferStatus(fmt.Sprintf("%s Codex stream opened but made no model progress for %ds (model: %s). Reconnecting.", warnEmoji, int(elapsed), model), 0, seconds(elapsed))
	x.touch(fmt.Sprintf("codex stream killed after %ds without model progress", int(elapsed)))
	return &DeadlineError{Message: fmt.Sprintf("Codex stream produced no substantive model progress for %ds (progress threshold: %ds)", int(elapsed), int(wd.progress))}
}

func (x *exchange) idleKill(wd watchdogs, elapsed float64) error {
	model := firstOf(x.model, "unknown")
	arm := "the first parsed event"
	if wd.idleNeedProgress {
		arm = "model progress began"
	}
	slog.Warn(fmt.Sprintf("Codex stream produced no SSE events for %.0fs after %s (threshold %.0fs, model=%s, context=~%s tokens). Killing connection so the retry loop can reconnect.",
		elapsed, arm, wd.idle, model, thousands(x.est)))
	x.bufferStatus(fmt.Sprintf("%s Codex stream sent no events for %ds after %s (model: %s). Reconnecting.", warnEmoji, int(elapsed), arm, model), 0, seconds(elapsed))
	x.touch(fmt.Sprintf("codex stream killed after %ds with no SSE events", int(elapsed)))
	return &DeadlineError{Message: fmt.Sprintf("Codex stream produced no SSE events for %ds after %s (threshold: %ds)", int(elapsed), arm, int(wd.idle))}
}

func (x *exchange) staleNonStreamKill(wd watchdogs, elapsed float64) error {
	hint := x.codexSilentHangHint()
	model := firstOf(x.model, "unknown")
	slog.Warn(fmt.Sprintf("Non-streaming API call stale for %.0fs (threshold %.0fs). model=%s context=~%s tokens. Killing connection.",
		elapsed, wd.stale, model, thousands(x.est)))
	tail := hint
	if tail == "" {
		tail = "Aborting call."
	}
	x.bufferStatus(fmt.Sprintf("%s No response from provider for %ds (non-streaming, model: %s). %s", warnEmoji, int(elapsed), model, tail), 0, seconds(elapsed))
	x.s.exec.bumpStreak()
	x.touch(fmt.Sprintf("stale non-streaming call killed after %ds", int(elapsed)))
	return &DeadlineError{Message: withHint(fmt.Sprintf("Non-streaming API call timed out after %ds with no response (threshold: %ds)", int(elapsed), int(wd.stale)), hint, ". ")}
}

func (x *exchange) codexEvent(progress bool, now time.Time) {
	x.mu.Lock()
	first := x.codex.lastEvent.IsZero()
	x.codex.lastEvent = now
	x.codexEvents++
	firstProgress := false
	if progress {
		firstProgress = x.codex.lastProgress.IsZero()
		x.codex.lastProgress = now
		if x.phaseAware {
			x.codex.retryStarted = time.Time{}
		}
	}
	attempt := x.codexAttempt
	x.mu.Unlock()
	if first {
		slog.Info(fmt.Sprintf("Codex stream first parsed event at %.3f (attempt=%d/%d, model=%s)", unixSeconds(now), attempt+1, codexStreamRetries+1, x.model))
	}
	if firstProgress {
		slog.Info(fmt.Sprintf("Codex stream first substantive progress at %.3f (attempt=%d/%d, model=%s)", unixSeconds(now), attempt+1, codexStreamRetries+1, x.model))
	}
}

func unixSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func codexTransport(err error) bool {
	var ae *wire.APIError
	if errors.As(err, &ae) {
		return false
	}
	return connectionDropped(err) || transportFailure(err)
}

func (x *exchange) codexStream(actx context.Context) (*wire.Response, error) {
	if x.s.r.c.o.Providers.IsActualRoute(actx, x.rt.Provider, x.ep.BaseURL) {
		return nil, errors.New("Actual requests require Chat Completions; refusing to call /responses.")
	}
	call, err := x.variant(func(r *wire.Request) { r.Stream = true })
	if err != nil {
		return nil, err
	}
	if x.s.codexBackendOf(x.rt) {
		if v, perr := wire.ParseValue(call.Body); perr == nil && v.IsObject() && wire.DictOf(v).Has("prompt_cache_retention") {
			body := wire.DictOf(v)
			body.Delete("prompt_cache_retention")
			sanitized := *call
			sanitized.Body = wire.ScrubSurrogates(body.JSON())
			call = &sanitized
			slog.Warn(fmt.Sprintf("Dropped unsupported prompt_cache_retention at consumer Codex wire boundary (model=%s, via top-level).",
				firstOf(v.Get("model").Text(), x.rt.Model, "unknown")))
		}
	}
	x.mu.Lock()
	x.sse = true
	x.mu.Unlock()
	for attempt := 0; attempt <= codexStreamRetries; attempt++ {
		if actx.Err() != nil {
			return nil, &DeadlineError{Message: "Codex Responses stream request retired before retry"}
		}
		x.mu.Lock()
		if attempt > 0 && x.phaseAware {
			x.codex = codexWatchState{retryStarted: time.Now()}
			slog.Info(fmt.Sprintf("Codex physical stream retry at %.3f (attempt=%d/%d, model=%s)", unixSeconds(x.codex.retryStarted), attempt+1, codexStreamRetries+1, x.model))
		}
		x.codexEvents, x.codexAttempt = 0, attempt
		now := time.Now()
		x.attemptStart, x.lastByte, x.connected, x.wrote, x.headersAt = now, now, time.Time{}, time.Time{}, time.Time{}
		x.mu.Unlock()
		resp, err := x.send(actx, call)
		if err != nil {
			if actx.Err() != nil {
				return nil, err
			}
			if !codexTransport(err) {
				return nil, err
			}
			if attempt < codexStreamRetries {
				slog.Debug(fmt.Sprintf("Codex Responses stream connect failed (attempt %d/%d); retrying. error=%v", attempt+1, codexStreamRetries+1, err))
				call = x.pruneZeroEvent(actx, call, attempt+1)
				continue
			}
			x.logCodexFailure(err, false, attempt)
			return nil, err
		}
		slog.Debug(fmt.Sprintf("Codex stream opened (attempt=%d/%d, model=%s)", attempt+1, codexStreamRetries+1, x.model))
		r, derr := call.Decode.Stream(actx, resp.Body, x.deliver)
		if derr != nil {
			resp.Body.Close()
			if actx.Err() != nil {
				return nil, derr
			}
			if codexTransport(derr) && attempt < codexStreamRetries {
				slog.Debug(fmt.Sprintf("Codex Responses stream transport failed mid-iteration (attempt %d/%d); retrying. error=%v", attempt+1, codexStreamRetries+1, derr))
				x.mu.Lock()
				zero := x.codexEvents == 0
				x.mu.Unlock()
				if zero {
					call = x.pruneZeroEvent(actx, call, attempt+1)
				}
				continue
			}
			if codexTransport(derr) && !errors.Is(derr, openai.ErrNoTerminal) {
				x.logCodexFailure(derr, true, attempt)
			}
			return nil, derr
		}
		x.drain(resp.Body)
		resp.Body.Close()
		x.stampResponse(r, resp.Header)
		if r != nil && r.Incomplete != "" {
			slog.Warn(fmt.Sprintf("Codex Responses stream terminal status=incomplete (incomplete_details=%s, streamed_chars=%d).", r.Incomplete, len([]rune(r.Message.Text))))
		}
		return r, nil
	}
	return nil, &DeadlineError{Message: "Codex Responses stream request retired before retry"}
}

func (x *exchange) logCodexFailure(err error, opened bool, attempt int) {
	slog.Warn(fmt.Sprintf("Codex Responses request failed: serialized_request_body_bytes=%d stream_opened=%t exception_chain=%s model=%s attempt=%d/%d",
		len(x.call.Body), opened, flattenErrorChain(err), firstOf(x.rt.Model, "unknown"), attempt+1, codexStreamRetries+1))
	if !opened {
		x.bufferConnectExhausted(err, attempt+1)
	}
}

func (x *exchange) drain(body io.ReadCloser) {
	budget := x.k.drainCfg
	if budget <= 0 {
		return
	}
	var timedOut atomic.Bool
	timer := time.AfterFunc(seconds(budget), func() {
		timedOut.Store(true)
		body.Close()
	})
	_, err := io.Copy(io.Discard, body)
	timer.Stop()
	switch {
	case timedOut.Load():
		slog.Warn(fmt.Sprintf("Codex Responses stream remained open %.1fs after a terminal response (agent.stream_drain_timeout); shut down its connection and completed cleanup.", budget))
	case err != nil && codexTransport(err):
		slog.Warn(fmt.Sprintf("Codex Responses stream transport finalization failed after a terminal response was already received; returning the completed response instead of retrying. error=%v", err))
	case err != nil:
		slog.Debug(fmt.Sprintf("Codex Responses stream finalization failed after a terminal response: %v", err))
	}
}

func spacedLen(raw []byte) int {
	text, ok := wire.FormatJSON(raw, wire.TextLayout{Spaced: true})
	if !ok {
		return len(raw)
	}
	return len(text)
}

func (x *exchange) pruneZeroEvent(ctx context.Context, call *wire.Call, attempt int) *wire.Call {
	v, err := wire.ParseValue(call.Body)
	if err != nil || !v.IsObject() {
		return call
	}
	items := v.Get("input")
	if !items.IsArray() {
		return call
	}
	before := spacedLen(items.Raw())
	if before <= zeroEventTurnBudget {
		return call
	}
	elems := items.Elems()
	out := make([]json.RawMessage, len(elems))
	pruned := 0
	for i, item := range elems {
		out[i] = item.Raw()
		if !item.IsObject() || item.Get("type").Text() != "function_call_output" {
			continue
		}
		output, ok := item.Get("output").Str()
		if !ok {
			continue
		}
		callID := item.Get("call_id").Display()
		if !item.Get("call_id").Truthy() {
			callID = "call"
		} else if s, isText := item.Get("call_id").Str(); isText {
			callID = s
		}
		replaced := x.s.r.c.o.SpillToolOutput(ctx, output, "codex_zero_event_retry", callID, zeroEventResultSize)
		if replaced != output {
			out[i] = wire.DictOf(item).SetString("output", replaced).JSON()
			pruned++
		}
	}
	model := x.p.Request.Model
	if pruned == 0 {
		slog.Warn(fmt.Sprintf("Codex zero-event retry (attempt %d/%d): no prunable tool output; resending payload unchanged (serialized_input_bytes=%d, model=%s)",
			attempt, codexStreamRetries+1, before, model))
		return call
	}
	input := wire.Array(out...)
	slog.Warn(fmt.Sprintf("Codex zero-event retry (attempt %d/%d): spilled %d oversized tool output(s) before reconnect, serialized_input_bytes=%d -> %d (model=%s)",
		attempt, codexStreamRetries+1, pruned, before, spacedLen(input), model))
	next := *call
	next.Body = wire.ScrubSurrogates(wire.DictOf(v).Set("input", input).JSON())
	return &next
}

func (x *exchange) invalidateClient() {
	if c := x.ep.Client; c != nil {
		c.CloseIdleConnections()
	}
}

func (x *exchange) bedrockRecover(err error, call *wire.Call) *wire.Call {
	if retry := x.s.r.c.bedrock.RecoverCachePoint(err, call, x.model); retry != nil {
		return retry
	}
	return bedrock.RecoverRedactedReasoning(err, call, x.model)
}

func (x *exchange) bedrockConverse(actx context.Context) (*wire.Response, error) {
	r, err := x.unary(actx, x.call)
	if err == nil || actx.Err() != nil {
		return r, err
	}
	if retry := x.bedrockRecover(err, x.call); retry != nil {
		return x.unary(actx, retry)
	}
	if bedrock.IsStaleConnection(err) {
		x.invalidateClient()
	}
	return r, err
}

func (x *exchange) bedrockStreamWorker(actx context.Context) (*wire.Response, error) {
	call, err := x.variant(func(r *wire.Request) { r.Stream = true })
	if err != nil {
		return nil, err
	}
	resp, err := x.send(actx, call)
	if err == nil {
		defer resp.Body.Close()
		r, derr := call.Decode.Stream(actx, resp.Body, x.deliver)
		x.stampResponse(r, resp.Header)
		return r, derr
	}
	if actx.Err() != nil {
		return nil, err
	}
	if retry := x.bedrockRecover(err, call); retry != nil {
		return x.streamOnce(actx, retry)
	}
	if bedrock.IsStreamingAccessDenied(err) {
		x.s.exec.disableStream()
		x.degrade("\n⚠  AWS IAM denied bedrock:InvokeModelWithResponseStream — falling back to non-streaming InvokeModel.\n   Grant that action to restore streaming output.\n")
		slog.Info(fmt.Sprintf("bedrock: converse_stream denied by IAM (%s) — using non-streaming converse() for this session.", errorTypeName(err)))
		return x.unary(actx, x.call)
	}
	if bedrock.IsStaleConnection(err) {
		x.invalidateClient()
	}
	return nil, err
}

func (x *exchange) bedrockStream(ctx context.Context) (out *wire.Response, failure error) {
	if err := x.s.checkStaleGiveup(x.k); err != nil {
		return nil, err
	}
	stale := x.cloudStaleTimeout()
	x.emit(wire.Event{Kind: wire.EventOpen})
	defer func() { x.closeStream(out, failure) }()
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	x.startAttempt(cancel)
	x.mu.Lock()
	x.sse = false
	x.mu.Unlock()
	started := time.Now()
	done := make(chan dispatchResult, 1)
	go func() {
		r, err := x.bedrockStreamWorker(actx)
		done <- dispatchResult{r, causeOf(actx, ctx, err)}
	}()
	ticker := time.NewTicker(monitorPoll)
	defer ticker.Stop()
	model := firstOf(x.model, "unknown")
	for {
		select {
		case res := <-done:
			if ctx.Err() != nil {
				return nil, &InterruptError{Message: "Agent interrupted during Bedrock API call (post-worker)", Err: ctx.Err()}
			}
			if res.err != nil {
				return nil, res.err
			}
			if res.r != nil {
				x.flushThink()
				x.s.exec.resetStreak()
			}
			return res.r, nil
		case <-ctx.Done():
			x.mu.Lock()
			responded := !x.firstChunk.IsZero()
			x.mu.Unlock()
			x.recordInterruptedWait(time.Since(started).Seconds(), responded)
			<-done
			return nil, &InterruptError{Message: "Agent interrupted during Bedrock API call", Err: ctx.Err()}
		case <-ticker.C:
		}
		x.mu.Lock()
		staleElapsed := time.Since(x.lastChunk).Seconds()
		x.mu.Unlock()
		if staleElapsed <= stale {
			continue
		}
		slog.Warn(fmt.Sprintf("Bedrock stream stale for %.0fs (threshold %.0fs) — no events received. region=%s model=%s. Aborting call.",
			staleElapsed, stale, x.rt.Region, model))
		x.bufferStatus(fmt.Sprintf("%s No events from Bedrock for %ds (model: %s). Aborting...", warnEmoji, int(staleElapsed), model), 0, seconds(staleElapsed))
		x.s.exec.bumpStreak()
		x.invalidateClient()
		x.mu.Lock()
		x.lastChunk, x.fenced = time.Now(), true
		x.mu.Unlock()
		if gerr := x.s.checkStaleGiveup(x.k); gerr != nil {
			cancel(gerr)
			<-done
			return nil, gerr
		}
		err := &DeadlineError{Message: fmt.Sprintf("Bedrock stream produced no events for %ds (threshold %ds) — aborting stalled stream so the retry/fallback path can recover.",
			int(staleElapsed), int(stale))}
		cancel(err)
		<-done
		return nil, err
	}
}
