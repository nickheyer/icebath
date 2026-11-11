package gatewayrun

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Interrupt reason and tool reason of the inactivity timeout
const (
	interruptReasonTimeout     = "Execution timed out (inactivity)"
	interruptToolReasonTimeout = "gateway inactivity watchdog"
)

// Executor task and inactivity watchdog handles of one agent run
type runAgentWorker struct {
	executorTask    *gatewayTask
	agentTimeout    *float64
	agentWarning    *float64
	taskID          string
	processBaseline map[string]bool
	workerDone      *pythread.Event
	timeoutFired    *pythread.Event
	cleanupLock     sync.Mutex
	isCurrent       func() bool
}

// Whether the worker's executor thread returned
func (w *runAgentWorker) WorkerDone() bool { return w.workerDone.IsSet() }

// Agent holder of a turn a profile stop interrupts
type turnAgentHolder struct{ tc *TurnContext }

// Agent the turn runs, nil before one was built
func (h turnAgentHolder) HolderAgent() any {
	if a := h.tc.currentAgent(); a != nil {
		return a
	}
	return nil
}

// Go frame names that mark goroutines doing turn work
var turnStackDumpFrameMarkers = []string{"RunConversation", "runSync", "runSyncWithTimeoutLifecycle", "finalizeTurn", "endTurn", "runInSession"}

// Logs the stack of every goroutine doing turn work before the hard interrupt frees it
func dumpWedgedTurnStacks(taskID string) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("Wedged-turn stack dump failed", "error", rec)
		}
	}()
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	dumped := 0
	for i, block := range strings.Split(string(buf), "\n\n") {
		if i == 0 {
			continue
		}
		matched := false
		for _, marker := range turnStackDumpFrameMarkers {
			if strings.Contains(block, marker) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		dumped++
		if dumped > 8 {
			runLog().Error(fmt.Sprintf("Wedged-turn stack dump for task %s truncated: more than 8 candidate threads", taskID))
			break
		}
		lines := strings.Split(block, "\n")
		header, frames := lines[0], lines[1:]
		if len(frames) > 50 {
			frames = frames[len(frames)-50:]
		}
		runLog().Error(fmt.Sprintf("Wedged-turn stack dump (task=%s thread=%s):\n%s", taskID, header, strings.Join(frames, "\n")))
	}
	if dumped == 0 {
		runLog().Error(fmt.Sprintf("Wedged-turn stack dump for task %s: no thread with turn-machinery frames found (worker may have already exited)", taskID))
	}
}

// Interrupts one timed out turn and reaps only the processes it created, false when it already ended
func abandonTimedOutGatewayTurn(ctx context.Context, tc *TurnContext, w *runAgentWorker) bool {
	w.cleanupLock.Lock()
	if w.workerDone.IsSet() || w.timeoutFired.IsSet() {
		w.cleanupLock.Unlock()
		return false
	}
	w.timeoutFired.Set()
	w.cleanupLock.Unlock()
	dumpWedgedTurnStacks(w.taskID)
	if a := tc.currentAgent(); a != nil {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug("Timed-out agent interrupt failed", "error", rec)
				}
			}()
			agent.RequestHardInterrupt(a, interruptReasonTimeout, interruptToolReasonTimeout)
		}()
		a.RecordWatchdogTurnAbort(ctx)
	}
	reapGatewayTurnProcesses(ctx, w.taskID, w.processBaseline, "gateway_turn_timeout", w.isCurrent)
	return true
}

// Watchdog goroutine that abandons the turn once its agent stays idle past the timeout
func watchGatewayTurnInactivity(ctx context.Context, tc *TurnContext, w *runAgentWorker, timeout, pollInterval float64) {
	activityOrigin := pythread.Monotonic()
	awakeIdle := state.NewAwakeIdleMeter()
	for !w.workerDone.Wait(math.Max(0.01, pollInterval)) {
		now := pythread.Monotonic()
		idleSeconds := now - activityOrigin
		if a := tc.currentAgent(); a != nil {
			func() {
				defer func() { _ = recover() }()
				if reported, ok := numberOf(a.GetActivitySummary()["seconds_since_activity"]); ok {
					idleSeconds = awakeIdle.Measure(math.Max(0, reported))
					activityOrigin = now - idleSeconds
				}
			}()
		}
		if idleSeconds < timeout {
			continue
		}
		abandonTimedOutGatewayTurn(ctx, tc, w)
		return
	}
}

// Float value of a JSON number, false for anything else
func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// Timeout of an env var in seconds, none for zero or less
func positiveEnvSeconds(name string, def float64) *float64 {
	if v := floatEnv(name, def); v > 0 {
		return &v
	}
	return nil
}

// Schedules run_sync on the executor with the inactivity watchdog beside it
func (r *GatewayRunner) runAgentStartTurnWorker(ctx context.Context, tc *TurnContext, runSync func(ctx context.Context) (map[string]any, error)) (*runAgentWorker, error) {
	taskID := tc.sessionID
	baseline := tools.DefaultProcessRegistry().SnapshotRunningIDs(taskID)
	tc.processTaskID, tc.processBaseline = taskID, baseline
	w := &runAgentWorker{
		agentTimeout: positiveEnvSeconds("ICEBATH_AGENT_TIMEOUT", 1800), agentWarning: positiveEnvSeconds("ICEBATH_AGENT_TIMEOUT_WARNING", 900),
		taskID: taskID, processBaseline: baseline, workerDone: &pythread.Event{}, timeoutFired: &pythread.Event{},
		isCurrent: func() bool { return true },
	}
	if tc.runGeneration != nil {
		sessionKey, generation := tc.sessionKey, *tc.runGeneration
		w.isCurrent = func() bool { return r.isSessionRunCurrent(sessionKey, generation) }
	}
	runSyncWithTimeoutLifecycle := func(execCtx context.Context) (any, error) {
		defer func() {
			w.workerDone.Set()
			if finished := tc.currentAgent(); finished != nil {
				r.clearTurnProcess(finished)
			}
		}()
		return runSync(execCtx)
	}
	if w.agentTimeout != nil {
		go watchGatewayTurnInactivity(context.WithoutCancel(ctx), tc, w, *w.agentTimeout, 5.0)
	}
	authority, err := gateway.TrackTurnWorker(ctx, r, w, turnAgentHolder{tc})
	if err != nil {
		return nil, err
	}
	w.executorTask = spawnGatewayTask(ctx, "turn-worker", func(taskCtx context.Context) (any, error) {
		return r.runInExecutorWithContext(taskCtx, runSyncWithTimeoutLifecycle)
	})
	if authority != nil {
		w.executorTask.addDoneCallback(func(*gatewayFuture) { gateway.PrunePhysicalWorkers(authority) })
	}
	return w, nil
}

// Activity summary of the agent, empty when it has none
func agentActivitySummary(a *agent.AIAgent) (summary map[string]any) {
	if a == nil {
		return map[string]any{}
	}
	defer func() {
		if rec := recover(); rec != nil {
			summary = map[string]any{}
		}
	}()
	return a.GetActivitySummary()
}

// One shot warning sent before the inactivity timeout escalates
func (r *GatewayRunner) runAgentInactivityWarning(ctx context.Context, w *runAgentWorker, source *gateway.SessionSource, statusThreadMetadata map[string]any) {
	adapter, _ := r.deliveryAdapterFor(ctx, source)
	if adapter == nil {
		return
	}
	minutes := max(int(*w.agentWarning/60), 1)
	remaining := max(int((*w.agentTimeout-*w.agentWarning)/60), 1)
	text := i18n.T(ctx, "gateway.progress.stuck_warning", "", map[string]any{"minutes": minutes, "remaining": remaining})
	if _, err := adapter.Base().EmitWarning(ctx, source.ChatID, text, "", interimMetadata(statusThreadMetadata), source.Platform); err != nil {
		runLog().Debug(fmt.Sprintf("Inactivity warning send error: %s", err))
	}
}

// Failed run of an inactivity timeout with the activity diagnostic, the still running agent interrupted
func (r *GatewayRunner) runAgentTimeoutResult(ctx context.Context, w *runAgentWorker, tc *TurnContext) (map[string]any, error) {
	timedOut := tc.currentAgent()
	activity := agentActivitySummary(timedOut)
	lastDesc := resultGet(activity, "last_activity_desc", "unknown")
	secsAgo, _ := numberOf(resultGet(activity, "seconds_since_activity", 0))
	curTool := activity["current_tool"]
	iterN := resultGet(activity, "api_call_count", 0)
	iterMax := resultGet(activity, "max_iterations", 0)
	toolText := "none"
	if s, ok := curTool.(string); ok && s != "" {
		toolText = s
	}
	runLog().Error(fmt.Sprintf("Agent idle for %.0fs (timeout %.0fs) in session %s | last_activity=%v | iteration=%v/%v | tool=%s",
		secsAgo, *w.agentTimeout, tc.sessionKey, lastDesc, iterN, iterMax, toolText))
	if timedOut != nil {
		agent.RequestHardInterrupt(timedOut, interruptReasonTimeout, interruptToolReasonTimeout)
	}
	timeoutMins := max(int(*w.agentTimeout/60), 1)
	progress, err := agent.FormatIterationProgress(ctx, iterN, iterMax)
	if err != nil {
		return nil, err
	}
	seconds := fmt.Sprintf("%.0f", secsAgo)
	lines := []string{i18n.T(ctx, "gateway.progress.timeout_header", "", map[string]any{"minutes": timeoutMins})}
	if s, ok := curTool.(string); ok && s != "" {
		lines = append(lines, i18n.T(ctx, "gateway.progress.timeout_stuck_tool", "", map[string]any{"tool": s, "seconds": seconds, "progress": progress}))
	} else {
		lines = append(lines, i18n.T(ctx, "gateway.progress.timeout_last_activity", "", map[string]any{"activity": lastDesc, "seconds": seconds, "progress": progress}))
	}
	lines = append(lines, i18n.T(ctx, "gateway.progress.timeout_hint", "", nil))
	var messages any = []any{}
	if held := tc.result(); held != nil {
		messages = resultGet(held, "messages", []any{})
	}
	var turnTools any = []any{}
	if len(tc.toolsHolder) > 0 {
		turnTools = tc.toolsHolder
	}
	failure := pyjson.ObjectOf("final_response", strings.Join(lines, "\n"), "messages", messages, "api_calls", iterN,
		"tools", turnTools, "history_offset", 0, "failed", true)
	recorded, _ := pyjson.Plain(gateway.RecordTerminalFailure(ctx, failure)).(map[string]any)
	return recorded, nil
}

// Polls the executor task with the inactivity timeout and backup interrupt check, a failed run on timeout
func (r *GatewayRunner) runAgentAwaitTurnWorker(ctx context.Context, w *runAgentWorker, tc *TurnContext, interruptDetected *pythread.Event,
	interruptMonitor *gatewayTask) (map[string]any, error) {
	warningFired := false
	for {
		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		result, err := w.executorTask.wait(waitCtx)
		cancel()
		if w.executorTask.isDone() {
			if err != nil {
				return nil, err
			}
			out, _ := result.(map[string]any)
			return out, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if w.agentTimeout != nil {
			if w.timeoutFired.IsSet() {
				break
			}
			reported, _ := numberOf(resultGet(agentActivitySummary(tc.currentAgent()), "seconds_since_activity", 0.0))
			idleSecs := tc.awakeIdle.Measure(reported)
			if !warningFired && w.agentWarning != nil && idleSecs >= *w.agentWarning {
				warningFired = true
				r.runAgentInactivityWarning(ctx, w, tc.source, tc.statusThreadMetadata)
			}
			if idleSecs >= *w.agentTimeout {
				go abandonTimedOutGatewayTurn(context.WithoutCancel(ctx), tc, w)
				break
			}
		}
		r.runAgentBackupInterruptCheck(ctx, tc, interruptDetected, interruptMonitor)
	}
	return r.runAgentTimeoutResult(ctx, w, tc)
}

// Polls the adapter for a pending interrupt every 200ms and signals the agent once
func (r *GatewayRunner) runAgentMonitorForInterrupt(ctx context.Context, tc *TurnContext, interruptDetected *pythread.Event) {
	if tc.sessionKey == "" {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
		fired := func() (fired bool) {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("monitor_for_interrupt error (will retry): %v", rec))
				}
			}()
			adapter, err := r.deliveryAdapterFor(ctx, tc.source)
			if err != nil {
				runLog().Debug(fmt.Sprintf("monitor_for_interrupt error (will retry): %s", err))
				return false
			}
			if adapter == nil || !adapter.Base().HasPendingInterrupt(tc.sessionKey) {
				return false
			}
			a := tc.currentAgent()
			if a == nil {
				return false
			}
			r.runAgentFirePendingInterrupt(ctx, adapter, a, tc, interruptDetected, "Voice-interrupt",
				func() { runLog().Debug("Interrupt detected from adapter, signaling agent...") })
			return true
		}()
		if fired {
			return
		}
	}
}

// Catches an interrupt the monitor task missed or died before seeing
func (r *GatewayRunner) runAgentBackupInterruptCheck(ctx context.Context, tc *TurnContext, interruptDetected *pythread.Event, interruptMonitor *gatewayTask) {
	if interruptDetected.IsSet() || tc.sessionKey == "" {
		return
	}
	adapter, _ := r.deliveryAdapterFor(ctx, tc.source)
	a := tc.currentAgent()
	if adapter == nil || a == nil || !adapter.Base().HasPendingInterrupt(tc.sessionKey) {
		return
	}
	r.runAgentFirePendingInterrupt(ctx, adapter, a, tc, interruptDetected, "Voice-backup-interrupt", func() {
		monitorState := "running"
		if interruptMonitor.isDone() {
			monitorState = "done"
		}
		runLog().Info(fmt.Sprintf("Backup interrupt detected for session %s (monitor task state: %s)", tc.sessionKey, monitorState))
	})
}

// Whether the final reply itself reached the user through the stream consumer
func runAgentStreamConfirmedFinalDelivery(consumer *gateway.GatewayStreamConsumer, finalText string) bool {
	if consumer == nil {
		return false
	}
	if consumer.FinalResponseSent() {
		if matches := consumer.DeliveredFinalMatches(finalText); matches != nil && !*matches {
			return false
		}
		return true
	}
	return consumer.HasDurablyDeliveredText(finalText)
}
