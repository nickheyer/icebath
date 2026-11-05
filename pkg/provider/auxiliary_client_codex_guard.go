package provider

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// InterruptedError is the Python InterruptedError a user interrupt raises inside a provider stream
type InterruptedError struct {
	Message string
}

func (e *InterruptedError) Error() string { return e.Message }
func (*InterruptedError) auxInterrupted() {}

// Implemented by a stream whose unreleased attempt connection a stranger goroutine may shut down
type auxAttemptConnHolder interface {
	auxAttemptConn() net.Conn
}

// Raw connection under an SDK event stream, nil when the stream has none or already released it
func auxAttemptStreamSocket(stream any) net.Conn {
	if s, ok := stream.(auxAttemptConnHolder); ok {
		return s.auxAttemptConn()
	}
	return nil
}

// Closes the target when it can be closed, a failure logged at debug under the note and silent without one
func auxCloseQuietly(target any, failureNote string) {
	closer, ok := target.(interface{ Close() error })
	if !ok {
		return
	}
	if err := closer.Close(); err != nil && failureNote != "" {
		slog.Debug("Codex auxiliary: "+failureNote, "error", err)
	}
}

// CodexStreamStallMarker is the text the context compressor reads as a retry ladder timeout
const CodexStreamStallMarker = "stream stalled"

// Progress aware deadline and connection safe timeout watchdog of one Codex auxiliary stream attempt
type auxCodexStreamGuard struct {
	ctx                   context.Context
	client                any
	totalTimeout          float64
	hasTotal              bool
	start                 time.Time
	noProgressTimeout     float64
	hardDeadline          time.Time
	deadlineMu            sync.Mutex
	progressDeadline      time.Time
	sawContent            atomic.Bool
	timedOut              atomic.Bool
	timeoutReleasePending atomic.Bool
	streamFinished        atomic.Bool
	timerMu               sync.Mutex
	timer                 *time.Timer
	protectedCancelCheck  AuxCancelCheck
	attemptMu             sync.Mutex
	attemptStream         any
}

// Guard whose first token and inter event window is the task override or the default, clamped by the total and host deadline
func newAuxCodexStreamGuard(ctx context.Context, client any, totalTimeout float64, hasTotal bool, noProgressTimeout float64) *auxCodexStreamGuard {
	g := &auxCodexStreamGuard{ctx: ctx, client: client, totalTimeout: totalTimeout, hasTotal: hasTotal, start: time.Now()}
	g.noProgressTimeout = auxStreamNoProgressTimeoutSeconds
	if noProgressTimeout > 0 {
		g.noProgressTimeout = noProgressTimeout
	}
	ceilingInput := 0.0
	if hasTotal {
		g.noProgressTimeout = min(g.noProgressTimeout, totalTimeout)
		ceilingInput = totalTimeout
	}
	g.hardDeadline = g.start.Add(auxSeconds(auxStreamTotalCeiling(ceilingInput)))
	if host, ok := auxCurrentAuxStreamDeadline(ctx); ok && host.Before(g.hardDeadline) {
		g.hardDeadline = host
	}
	g.progressDeadline = g.start.Add(auxSeconds(g.noProgressTimeout))
	if auxInterruptProtected(ctx) {
		g.protectedCancelCheck = auxCaptureAuxCancelCheck(ctx)
	}
	return g
}

func auxSeconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// Earlier of the hard ceiling and the current no progress deadline
func (g *auxCodexStreamGuard) effectiveDeadline() time.Time {
	g.deadlineMu.Lock()
	defer g.deadlineMu.Unlock()
	if g.progressDeadline.Before(g.hardDeadline) {
		return g.progressDeadline
	}
	return g.hardDeadline
}

// True when the frozen hard cancel source reports the owner already cancelled
func (g *auxCodexStreamGuard) cancelRequested() bool {
	return g.protectedCancelCheck != nil && auxCapturedAuxCancelRequested(g.protectedCancelCheck)
}

func (g *auxCodexStreamGuard) adoptStream(stream any) {
	g.attemptMu.Lock()
	g.attemptStream = stream
	g.attemptMu.Unlock()
}

// Closes the attempt stream silently on the owner side and forgets it
func (g *auxCodexStreamGuard) releaseStream(stream any) {
	auxCloseQuietly(stream, "")
	g.attemptMu.Lock()
	g.attemptStream = nil
	g.attemptMu.Unlock()
}

// Wakes only this attempt stream, a stranger goroutine shutting its connection down instead of closing it
func (g *auxCodexStreamGuard) closeAttemptStream(failureNote string, owner bool) {
	g.attemptMu.Lock()
	stream := g.attemptStream
	g.attemptMu.Unlock()
	if stream == nil {
		return
	}
	if !owner {
		if conn := auxAttemptStreamSocket(stream); conn != nil {
			auxShutdownConn(conn)
			return
		}
	}
	auxCloseQuietly(stream, failureNote)
}

// Substantive payload re-arms the no progress window while the hard ceiling never moves
func (g *auxCodexStreamGuard) recordProgress() {
	g.deadlineMu.Lock()
	g.progressDeadline = time.Now().Add(auxSeconds(g.noProgressTimeout))
	g.deadlineMu.Unlock()
}

// Hard ceiling, first token or mid stream stall wording of the timeout
func (g *auxCodexStreamGuard) timeoutMessage() string {
	elapsed := time.Since(g.start).Seconds()
	if !time.Now().Before(g.hardDeadline) {
		return fmt.Sprintf("Codex auxiliary Responses stream exceeded %.1fs hard ceiling", g.hardDeadline.Sub(g.start).Seconds())
	}
	if !g.sawContent.Load() {
		return fmt.Sprintf("Codex auxiliary Responses stream produced no output within %.1fs (no-progress timeout, %.1fs elapsed)",
			g.noProgressTimeout, elapsed)
	}
	return fmt.Sprintf("Codex auxiliary Responses %s: no new output for %.1fs (%.1fs elapsed)", CodexStreamStallMarker, g.noProgressTimeout, elapsed)
}

// Closes or shuts down the client once the timeout wins, waking only the attempt stream after an owner cancel
func (g *auxCodexStreamGuard) closeClientOnTimeout(owner bool) {
	var timeoutWon bool
	if d, ok := g.protectedCancelCheck.(interface{ BeginTimeoutCleanup() bool }); ok {
		timeoutWon = d.BeginTimeoutCleanup()
	} else {
		timeoutWon = !g.cancelRequested()
	}
	g.timedOut.Store(true)
	if !timeoutWon {
		g.closeAttemptStream("cancelled attempt stream close during timeout failed", owner)
		return
	}
	g.timeoutReleasePending.Store(true)
	if owner {
		auxCloseQuietly(g.client, "client close during timeout failed")
	} else {
		if err := auxRecoverRaise(func() {
			count := forceCloseTCPSockets(g.client)
			slog.Info(fmt.Sprintf("Codex auxiliary client aborted (timeout, tcp_force_closed=%d, deferred_close=stranger_thread)", count))
		}); err != nil {
			slog.Debug("Codex auxiliary: client abort during timeout failed", "error", err)
		}
		g.closeAttemptStream("attempt stream close during stranger-thread timeout failed", false)
	}
	if err := auxRecoverRaise(func() { auxEvictCachedClientInstance(g.client) }); err != nil {
		slog.Debug("Codex auxiliary: cache eviction on timeout failed", "error", err)
	}
}

// Timeout once the effective deadline passed, then explicit host cancel, then an unprotected user interrupt
func (g *auxCodexStreamGuard) checkCancelled() error {
	if g.hasTotal && !time.Now().Before(g.effectiveDeadline()) {
		if !g.timedOut.Load() {
			g.closeClientOnTimeout(true)
		}
		return &AuxTimeoutError{Message: g.timeoutMessage()}
	}
	if auxInterruptCancelRequested(g.ctx) {
		return &AuxiliaryExplicitCancellation{}
	}
	if tools.IsInterrupted(g.ctx) && !auxInterruptProtected(g.ctx) {
		return &InterruptedError{Message: "Codex auxiliary Responses stream interrupted"}
	}
	return nil
}

// Reschedules while progress moved the deadline forward, otherwise times the attempt out from the timer goroutine
func (g *auxCodexStreamGuard) watchdogFire() {
	if remaining := time.Until(g.effectiveDeadline()); remaining > 0 {
		if !(g.timedOut.Load() || g.streamFinished.Load()) {
			g.armTimer(remaining)
		}
		return
	}
	g.closeClientOnTimeout(false)
}

func (g *auxCodexStreamGuard) armTimer(delay time.Duration) {
	g.timerMu.Lock()
	g.timer = time.AfterFunc(delay, g.watchdogFire)
	g.timerMu.Unlock()
}

// Arms the watchdog when a total timeout exists and runs the first cancel check
func (g *auxCodexStreamGuard) begin() error {
	if g.hasTotal && g.totalTimeout != 0 {
		g.armTimer(max(time.Until(g.effectiveDeadline()), 0))
	}
	return g.checkCancelled()
}

// Substantive events re-arm the window and tick progress, other frames only count toward first response timing
func (g *auxCodexStreamGuard) onEvent(event any) error {
	if m, ok := event.(map[string]any); ok && codexEventHasContent(m) {
		g.recordProgress()
		g.sawContent.Store(true)
		auxNotifyAuxProviderResponse(g.ctx)
	} else {
		auxNotifyAuxTimingResponse(g.ctx)
	}
	return g.checkCancelled()
}

// Stops the watchdog and performs the client close a stranger goroutine timeout deferred to the owner
func (g *auxCodexStreamGuard) finish() {
	g.streamFinished.Store(true)
	g.timerMu.Lock()
	if g.timer != nil {
		g.timer.Stop()
	}
	g.timerMu.Unlock()
	if g.timeoutReleasePending.Load() {
		auxCloseQuietly(g.client, "owner-thread close after timeout failed")
	}
}
