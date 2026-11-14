package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	warnEmoji          = "⚠\xef\xb8\x8f"
	nearDeadline       = 15.0
	largeRequestHint   = 256 * 1024
	waitNoticeSilence  = 60.0
	heartbeatInterval  = 30 * time.Second
	monitorPoll        = 300 * time.Millisecond
	interruptStaleWait = 30.0
)

var (
	phaseTexts = map[string]string{
		"first_event":  "%ds waiting for the first provider event",
		"reconnect":    "%ds waiting for the first provider event after reconnect",
		"pre_progress": "provider stream open; %ds without substantive model progress",
		"post_event":   "provider stream active; %ds without stream events",
		"first_chunk":  "%ds waiting for the first stream chunk",
		"post_chunk":   "stream open; %ds without stream output",
	}
	streamDiagHeaders = []string{"cf-ray", "cf-cache-status", "x-openrouter-provider", "x-openrouter-model", "x-openrouter-id",
		"x-request-id", "x-vercel-id", "via", "server", "x-forwarded-for"}
)

type watchdogDeadline struct {
	label     string
	remaining float64
	set       bool
}

func (w watchdogDeadline) near() bool { return w.set && w.remaining <= nearDeadline }

func waitNoticeText(model string, silence float64, phase string, w watchdogDeadline) string {
	lead := "waiting on"
	if w.near() {
		lead = "still waiting on"
	}
	text := "⏳ " + lead + " " + model + " — " + fmt.Sprintf(phaseTexts[phase], int(silence))
	if w.set {
		text += fmt.Sprintf(" (auto-reconnect: %s watchdog in %ds)", w.label, max(0, int(w.remaining)))
	}
	return text
}

type codexWatchState struct {
	lastEvent, lastProgress, retryStarted time.Time
}

func codexWatchdogDeadline(wd watchdogs, st codexWatchState, callStart time.Time, elapsed float64) watchdogDeadline {
	type deadline struct {
		label string
		at    float64
	}
	var all []deadline
	if finite(wd.stale) {
		all = append(all, deadline{"wall-clock stale", wd.stale})
	}
	offset := 0.0
	if !st.retryStarted.IsZero() {
		offset = math.Max(0, st.retryStarted.Sub(callStart).Seconds())
	}
	switch {
	case st.lastEvent.IsZero():
		if wd.ttfbOn && finite(wd.ttfb) {
			all = append(all, deadline{"TTFB", offset + wd.ttfb})
		}
	case wd.progress > 0 && st.lastProgress.IsZero():
		all = append(all, deadline{"first progress", offset + wd.progress})
	case (!wd.idleNeedProgress || !st.lastProgress.IsZero()) && wd.idleOn && finite(wd.idle):
		all = append(all, deadline{"stream idle", math.Max(0, st.lastEvent.Sub(callStart).Seconds()) + wd.idle})
	}
	if len(all) == 0 {
		return watchdogDeadline{}
	}
	best := all[0]
	for _, d := range all[1:] {
		if d.at < best.at {
			best = d
		}
	}
	if best.at <= elapsed {
		return watchdogDeadline{}
	}
	return watchdogDeadline{label: best.label, remaining: best.at - elapsed, set: true}
}

type waitNoticeState struct {
	phase     string
	label     string
	nearShown bool
}

func (w *waitNoticeState) reset() { *w = waitNoticeState{} }

func (w *waitNoticeState) shouldEmit(phase string, d watchdogDeadline) bool {
	label := ""
	if d.set {
		label = d.label
	}
	near := d.near()
	emit := w.phase != phase || w.label != label || (near && !w.nearShown)
	w.phase, w.label = phase, label
	if near {
		w.nearShown = true
	}
	return emit
}

type streamDiag struct {
	started    time.Time
	firstChunk time.Time
	chunks     int
	bytes      int
	headers    []string
	status     int
	serving    string
	finishSeen bool
}

func newStreamDiag() *streamDiag { return &streamDiag{started: time.Now()} }

func (d *streamDiag) capture(resp *http.Response) {
	if resp == nil {
		return
	}
	d.status = resp.StatusCode
	d.headers = d.headers[:0]
	for _, name := range streamDiagHeaders {
		if v := resp.Header.Get(name); v != "" {
			d.headers = append(d.headers, name+"="+truncateRunes(v, 120))
		}
	}
}

func (d *streamDiag) fields() (status string, bytes, chunks int, elapsed float64, ttfb, serving string, finishSeen bool, upstream string) {
	status, ttfb, serving, upstream = "-", "-", "-", "-"
	if d == nil {
		return
	}
	bytes, chunks = d.bytes, d.chunks
	elapsed = math.Max(0, time.Since(d.started).Seconds())
	if !d.firstChunk.IsZero() {
		ttfb = fmt.Sprintf("%.2fs", math.Max(0, d.firstChunk.Sub(d.started).Seconds()))
	}
	if len(d.headers) > 0 {
		upstream = strings.Join(d.headers, " ")
	}
	if d.status != 0 {
		status = fmt.Sprint(d.status)
	}
	if d.serving != "" {
		serving = d.serving
	}
	finishSeen = d.finishSeen
	return
}

func phaseWord(p httpclient.Phase) string {
	switch p {
	case httpclient.PhasePool:
		return "pool"
	case httpclient.PhaseConnect:
		return "connect"
	case httpclient.PhaseProxy:
		return "proxy"
	case httpclient.PhaseTLS:
		return "tls"
	case httpclient.PhaseWrite:
		return "write"
	}
	return "read"
}

func errorTypeName(err error) string {
	var he *httpclient.Error
	if errors.As(err, &he) && err == error(he) {
		if he.Timeout() {
			return phaseWord(he.Phase) + " timeout"
		}
		return phaseWord(he.Phase) + " failure"
	}
	switch {
	case errors.Is(err, wire.ErrEmptyStream):
		return "empty stream"
	case errors.Is(err, wire.ErrStreamDropped):
		return "stream dropped"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected EOF"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	}
	t := reflect.TypeOf(err)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Name() == "" {
		return "Error"
	}
	name := t.Name()
	if name == "errorString" || name == "wrapError" || name == "joinError" {
		return "Error"
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func flattenErrorChain(err error) string {
	var parts []string
	seen := map[error]bool{}
	for link := err; link != nil && len(parts) < 4; link = errors.Unwrap(link) {
		if comparable(link) {
			if seen[link] {
				break
			}
			seen[link] = true
		}
		msg := strings.ReplaceAll(strings.TrimSpace(link.Error()), "\n", " ")
		if len([]rune(msg)) > 140 {
			msg = string([]rune(msg)[:140]) + "…"
		}
		if msg == "" {
			parts = append(parts, errorTypeName(link))
		} else {
			parts = append(parts, errorTypeName(link)+"("+msg+")")
		}
	}
	if len(parts) == 0 {
		return errorTypeName(err)
	}
	return strings.Join(parts, " <- ")
}

func comparable(err error) bool {
	t := reflect.TypeOf(err)
	return t != nil && t.Comparable()
}

func connectExhaustedNotice(err error, attempts int, requestURL string, bodyBytes int) string {
	plural := "s"
	if attempts == 1 {
		plural = ""
	}
	var lifetime *StreamLifetimeError
	if errors.As(err, &lifetime) {
		return fmt.Sprintf("❌ %s (%d attempt%s)", err.Error(), attempts, plural)
	}
	host := "the endpoint"
	if u, perr := url.Parse(requestURL); perr == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	line := fmt.Sprintf("❌ Could not open a stream to %s after %d attempt%s", host, attempts, plural)
	line += fmt.Sprintf(" (request %d KB)", max(1, int(math.RoundToEven(float64(bodyBytes)/1024))))
	if bodyBytes >= largeRequestHint {
		return line + "; the endpoint or a proxy in front of it may reject requests this large."
	}
	return line + "; the endpoint looks unreachable — try again in a moment."
}

func (x *exchange) logStreamRetry(kind string, err error, attempt, maxAttempts int, midToolCall bool, d *streamDiag) {
	summary := Summary(err, x.rt.Provider, x.mode)
	if len([]rune(summary)) > 240 {
		summary = string([]rune(summary)[:240]) + "…"
	}
	status, bytes, chunks, elapsed, ttfb, serving, finishSeen, upstream := d.fields()
	slog.Warn(fmt.Sprintf("Stream %s on attempt %d/%d — retrying. subagent_id=%s depth=%d provider=%s base_url=%s "+
		"error_type=%s error=%s chain=%s http_status=%s bytes=%d chunks=%d elapsed=%.2fs ttfb=%s "+
		"serving_provider=%s finish_reason_seen=%t upstream=[%s]",
		kind, attempt, maxAttempts, firstOf(x.o.SubagentID, "-"), x.o.DelegateDepth, firstOf(x.rt.Provider, "-"),
		firstOf(x.rt.BaseURL, "-"), errorTypeName(err), summary, flattenErrorChain(err), status, bytes, chunks, elapsed, ttfb,
		serving, finishSeen, upstream), "mid_tool_call", midToolCall)
}

func (x *exchange) emitStreamDrop(err error, attempt, maxAttempts int, midToolCall bool, d *streamDiag) {
	kind := "drop"
	if midToolCall {
		kind = "drop mid tool-call"
	}
	x.logStreamRetry(kind, err, attempt, maxAttempts, midToolCall, d)
	provider := firstOf(x.rt.Provider, "provider")
	suffix := ""
	var elapsed time.Duration
	if d != nil {
		elapsed = time.Since(d.started)
		suffix = fmt.Sprintf(" after %.1fs", math.Max(0, elapsed.Seconds()))
	}
	name := errorTypeName(err)
	x.bufferStatus(fmt.Sprintf("%s %s stream %s (%s)%s — attempt %d/%d dropped, reconnecting", warnEmoji, provider, kind, name,
		suffix, attempt, maxAttempts), attempt, elapsed)
	x.touch(fmt.Sprintf("stream retry %d/%d after %s", attempt, maxAttempts, name))
}

func (x *exchange) bufferConnectExhausted(err error, attempts int) {
	text := connectExhaustedNotice(err, attempts, x.requestURL(), len(x.call.Body))
	if !x.s.exec.firstNotice(text) {
		return
	}
	x.bufferStatus(text, attempts, 0)
}
