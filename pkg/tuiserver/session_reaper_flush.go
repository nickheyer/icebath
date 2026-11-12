package tuiserver

import (
	"context"
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pysignal"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Seconds the exit flush may take and the seconds interrupted turns get to settle on the way out
var (
	exitFlushBudgetS = math.Max(0, envFloat("ICEBATH_TUI_EXIT_FLUSH_BUDGET_S", 5.0))
	exitTurnSettleS  = 0.5
)

// Seconds a signal sent to this process with the default action gets to end it
const exitSignalDeathWait = 2 * time.Second

// Handlers the exit flush replaced, and whether the flush handlers are installed
var (
	exitFlushMu                sync.Mutex
	exitFlushPrevHandlers      = map[os.Signal]*pysignal.Handler{}
	exitFlushHandlersInstalled bool
	exitFlushHandler           = pysignal.Func(handleExitFlushSignal)
)

// Agent surface a transcript flush drives, the in memory messages and the persist step
type flushableAgent interface {
	SessionMessages() []*state.Message
	PersistSession(messages *[]*state.Message, conversationHistory []*state.Message) error
}

// Agent of a session when it can persist a transcript, nil for a session with none
func sessionFlushAgent(session *tuigateway.Session) flushableAgent {
	if session == nil {
		return nil
	}
	slot := session.Get("agent")
	if built, ok := slot.(*agent.AIAgent); ok && built == nil {
		return nil
	}
	a, _ := slot.(flushableAgent)
	return a
}

// Whether a flush of the session would write anything
func sessionHasPendingFlush(session *tuigateway.Session) bool {
	a := sessionFlushAgent(session)
	return a != nil && len(a.SessionMessages()) > 0
}

// Persists one session transcript through its agent, false when there is nothing to write or the write fails
func flushSessionMessages(session *tuigateway.Session) (flushed bool) {
	a := sessionFlushAgent(session)
	if a == nil {
		return false
	}
	snapshot := a.SessionMessages()
	if len(snapshot) == 0 {
		return false
	}
	defer func() {
		if failure := recover(); failure != nil {
			logger().Debug("incremental session flush failed", "exc_info", failureText(failure))
			flushed = false
		}
	}()
	if err := a.PersistSession(&snapshot, nil); err != nil {
		logger().Debug("incremental session flush failed", "exc_info", err)
		return false
	}
	return true
}

// Live session records in registry order
func reaperSessionSnapshot() []*tuigateway.Session {
	live := snapshotSessions()
	out := make([]*tuigateway.Session, len(live))
	for i, entry := range live {
		out[i] = entry.Session
	}
	return out
}

// Flushes every in memory session within the budget on a worker and returns how many were persisted
func flushSessionsBeforeExit(budgetS *float64) int {
	budget := exitFlushBudgetS
	if budgetS != nil {
		budget = math.Max(0, *budgetS)
	}
	if budget <= 0 {
		return 0
	}
	sessions := reaperSessionSnapshot()
	flushable := 0
	for _, session := range sessions {
		if sessionHasPendingFlush(session) {
			flushable++
		}
	}
	var flushed atomic.Int64
	done := make(chan struct{})
	window := time.Duration(budget * float64(time.Second))
	go func() {
		defer threadExceptHook("hermes-exit-flush")
		defer close(done)
		deadline := time.Now().Add(window)
		for _, session := range sessions {
			if !time.Now().Before(deadline) {
				break
			}
			if flushSessionMessages(session) {
				flushed.Add(1)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(window):
	}
	count := int(flushed.Load())
	if count < flushable {
		logger().Warn(fmt.Sprintf("Exit flush persisted %d of %d in-memory session transcript(s) within %.1fs; the rest "+
			"may have been lost (ICEBATH_TUI_EXIT_FLUSH_BUDGET_S)", count, flushable, budget))
	}
	return count
}

// Interrupts every running turn, waits the settle budget and kills foreground commands still alive halfway through
func stopTurnsBeforeExit(ctx context.Context, budgetS *float64) {
	var running []tuigateway.LiveSession
	for _, live := range snapshotSessions() {
		if live.Session != nil && truthy(live.Session.Get("running")) {
			running = append(running, live)
		}
	}
	var threads []*memory.ContextThread
	for _, live := range running {
		suppress(func() { interruptSessionTurn(ctx, live.SID, live.Session, "", false) })
		if t, ok := live.Session.Get("_run_thread").(*memory.ContextThread); ok && t != nil && t != currentRunThread(ctx) {
			threads = append(threads, t)
		}
	}
	budget := exitTurnSettleS
	if budgetS != nil {
		budget = math.Max(0, *budgetS)
	}
	window := time.Duration(budget * float64(time.Second))
	deadline := time.Now().Add(window)
	join := func(until time.Time) {
		for _, t := range threads {
			t.Join(max(0, time.Until(until)))
		}
	}
	join(deadline.Add(-window / 2))
	tools.KillLiveForegroundProcesses(true)
	join(deadline)
}

// Flushes in memory sessions and stops turns, then hands the signal to the handler that was installed before
func handleExitFlushSignal(sig os.Signal) error {
	exitFlushMu.Lock()
	prev := exitFlushPrevHandlers[sig]
	exitFlushMu.Unlock()
	if prev == pysignal.Ignore {
		return nil
	}
	suppress(func() { flushSessionsBeforeExit(nil) })
	suppress(func() { stopTurnsBeforeExit(context.Background(), nil) })
	if prev.Callable() {
		return prev.Call(sig)
	}
	pysignal.Signal(sig, pysignal.Default)
	if err := pysignal.KillSelf(sig); err != nil {
		number, _ := sig.(syscall.Signal)
		return &pysignal.SystemExit{Code: 128 + int(number)}
	}
	time.Sleep(exitSignalDeathWait)
	return nil
}

// Installs the chaining SIGTERM and SIGINT flush handlers once, true when they are in place
func InstallExitFlushSignalHandlers() bool {
	loadServerModule()
	exitFlushMu.Lock()
	defer exitFlushMu.Unlock()
	if exitFlushHandlersInstalled {
		return true
	}
	for _, sig := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		exitFlushPrevHandlers[sig] = pysignal.Signal(sig, exitFlushHandler)
	}
	exitFlushHandlersInstalled = true
	return true
}
