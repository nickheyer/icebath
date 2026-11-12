package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Persists an interrupted agent's in flight transcript before teardown, a recovery snapshot when the flush fails
func flushAgentTranscriptAtShutdown(ctx context.Context, a *agent.AIAgent) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("Shutdown transcript flush failed: %v", rec))
		}
	}()
	if a == nil || len(a.SessionMessages()) == 0 {
		return
	}
	func() {
		defer func() { _ = recover() }()
		a.DropTrailingSessionScaffolding()
	}()
	messages := agent.SessionMessageDicts(a.SessionMessages())
	if _, err := a.FlushMessagesToSessionDB(messages); err != nil {
		runLog().Warn(fmt.Sprintf("Shutdown transcript flush failed (%s); preserving %d in-memory message(s) to recovery snapshot",
			pyrepr.ExceptionMessage(err), len(messages)))
		history := make([]any, len(messages))
		for i, m := range messages {
			history[i] = m
		}
		sessionID := a.SessionID()
		gateway.FlushAgentHistoryToFile(ctx, &sessionID, history)
	}
}

// Flushes, finalizes, delivers plugin session end text and cleans up every agent the drain snapshot held
func (r *GatewayRunner) finalizeShutdownAgents(ctx context.Context, activeAgents map[string]*agent.AIAgent) {
	for _, sessionKey := range sortedAgentKeys(activeAgents) {
		a := activeAgents[sessionKey]
		flushAgentTranscriptAtShutdown(ctx, a)
		var sessionID any
		if a != nil {
			sessionID = a.SessionID()
		}
		messages := r.finalizeSessionOffLoop(ctx, sessionID, "gateway", "shutdown", sessionKey, nil)
		if err := r.deliverSessionEndMessages(ctx, messages, nil, sessionKey); err != nil {
			runLog().Debug(fmt.Sprintf("Session-end plugin message delivery failed for %s", sessionKey), "exc_info", err)
		}
		r.cleanupAgentResourcesOffLoop(ctx, a, "shutdown finalize", sessionKey)
	}
}

// Task or future whose completion the long running notice checks
type doneChecker interface {
	isDone() bool
}

// Whether the long running heartbeat still belongs to the live run of the session
func (r *GatewayRunner) shouldEmitLongRunningNotification(sessionKey string, a *agent.AIAgent, executorTask doneChecker) bool {
	if a == nil || (executorTask != nil && executorTask.isDone()) {
		return false
	}
	if r.draining.Load() || r.restartRequested {
		return false
	}
	if sessionKey != "" {
		if r.runningSlotAgent(sessionKey) != a {
			return false
		}
	}
	return true
}

// Restart failure counters file under the process home
func (r *GatewayRunner) stuckLoopCountsPath() string {
	return filepath.Join(home.Process(), stuckLoopFile)
}

// Parsed counter mapping, nil when the file is missing or unreadable
func readJSONCounts(path string) *pyjson.Object {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		return nil
	}
	counts, ok := data.(*pyjson.Object)
	if !ok {
		return nil
	}
	return counts
}

// Increments the persisted restart failure counter of every active session and drops the rest
func (r *GatewayRunner) incrementRestartFailureCounts(ctx context.Context, activeSessionKeys []string) {
	gateway.IncrementRestartFailureCounts(ctx, r.stuckLoopCountsPath(), activeSessionKeys)
}

// Suspends sessions active across too many restarts and clears the counters, returning how many it suspended
func (r *GatewayRunner) suspendStuckLoopSessions(ctx context.Context) int {
	path := r.stuckLoopCountsPath()
	if _, err := os.Stat(path); err != nil {
		return 0
	}
	counts := readJSONCounts(path)
	if counts == nil {
		return 0
	}
	suspended := 0
	for _, sessionKey := range counts.Keys {
		value := counts.Vals[sessionKey]
		if !countAtLeast(value, stuckLoopThreshold) {
			continue
		}
		func() {
			defer func() { _ = recover() }()
			entry := r.sessionStore.LoadedEntry(ctx, sessionKey)
			if entry != nil && !entry.Suspended {
				entry.Suspended = true
				suspended++
				runLog().Warn(fmt.Sprintf("Auto-suspended stuck session %s (active across %s consecutive restarts — likely a stuck loop)",
					sessionKey, pyrepr.Str(value)))
			}
		}()
	}
	if suspended > 0 {
		func() {
			defer func() { _ = recover() }()
			_ = r.sessionStore.Save(ctx)
		}()
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		runLog().Debug(fmt.Sprintf("Could not remove %s: %v", path, err))
	}
	return suspended
}

// Whether a decoded JSON counter compares at least threshold the way Python compares numbers
func countAtLeast(v any, threshold int) bool {
	switch n := v.(type) {
	case int64:
		return n >= int64(threshold)
	case float64:
		return n >= float64(threshold)
	case bool:
		if n {
			return 1 >= threshold
		}
		return 0 >= threshold
	case *big.Int:
		return n.Cmp(big.NewInt(int64(threshold))) >= 0
	}
	return false
}

// Clears a completed session's restart failure counter off the loop
func (r *GatewayRunner) clearRestartFailureCount(ctx context.Context, sessionKey string) {
	path := r.stuckLoopCountsPath()
	if _, err := os.Stat(path); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		gateway.ClearRestartFailureCount(context.WithoutCancel(ctx), path, sessionKey)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Sends plugin session finalize text to the chat owning the session as plain adapter sends
func (r *GatewayRunner) deliverSessionEndMessages(ctx context.Context, messages []string, source *gateway.SessionSource, sessionKey string) error {
	if len(messages) == 0 {
		return nil
	}
	if source == nil && sessionKey != "" {
		if target := r.shutdownNotificationTarget(ctx, sessionKey); target != nil {
			source = target.source
		}
	}
	var adapter gateway.PlatformAdapter
	if source != nil {
		var err error
		if adapter, err = r.deliveryAdapterFor(ctx, source); err != nil {
			return err
		}
	}
	if adapter == nil {
		runLog().Info(fmt.Sprintf("Session-end plugin message(s) dropped: no chat for session %s", sessionKey))
		return nil
	}
	metadata := r.threadMetadataForSource(source, nil)
	for _, message := range messages {
		sendNoticeLogged(ctx, adapter, source.ChatID, message, string(source.Platform),
			"Failed to send session-end plugin message to %s:%s: %s", "", metadata)
	}
	return nil
}

// Runs fn on the housekeeping pool inside the profile owning the session key, waiting at most timeout seconds
func (r *GatewayRunner) runBoundedInOwnerScope(ctx context.Context, sessionKey string, timeout float64, fn func(ctx context.Context)) error {
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout*float64(time.Second)))
	defer cancel()
	_, err := r.runHousekeepingInExecutor(waitCtx, func(workerCtx context.Context) (any, error) {
		var panicked any
		r.runReleaseInProfileScope(context.WithoutCancel(workerCtx), func(scoped context.Context) {
			defer func() { panicked = recover() }()
			fn(scoped)
		}, sessionKey)
		if panicked != nil {
			return nil, fmt.Errorf("%v", panicked)
		}
		return nil, nil
	})
	if err != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return errBoundedTimeout
	}
	return err
}

// Bounded off loop work ran past its timeout
var errBoundedTimeout = errors.New("bounded off loop work timed out")

// Plugin on_session_reset hooks off the loop under the profile owning the session key, bounded like finalize
func (r *GatewayRunner) ResetSessionOffLoop(ctx context.Context, sessionKey string, sessionID *string, reason, platform string, oldSessionID, newSessionID *string) error {
	kwargs := map[string]any{"session_id": optAny(sessionID), "reason": reason, "platform": platform,
		"old_session_id": optAny(oldSessionID), "new_session_id": optAny(newSessionID)}
	err := r.runBoundedInOwnerScope(ctx, sessionKey, finalizeTimeoutS, func(scoped context.Context) {
		agent.LifecycleInvokeHook(scoped, "on_session_reset", kwargs)
	})
	switch {
	case err == nil:
	case errors.Is(err, errBoundedTimeout):
		runLog().Warn(fmt.Sprintf("Session reset hooks (%s) exceeded %ss; proceeding without blocking the event loop.",
			pyrepr.Str(kwargs["old_session_id"]), pyrepr.Str(finalizeTimeoutS)))
	default:
		runLog().Debug(fmt.Sprintf("Session reset hooks (%s) failed", pyrepr.Str(kwargs["old_session_id"])), "exc_info", err)
	}
	return nil
}

// Session finalize hooks off the loop under the profile owning the session key, the plugins' messages returned
func (r *GatewayRunner) finalizeSessionOffLoop(ctx context.Context, sessionID any, platform, reason, sessionKey string, extra map[string]any) []string {
	var mu sync.Mutex
	messages := []string{}
	kwargs := map[string]any{"session_id": sessionID, "platform": platform, "reason": reason}
	for k, v := range extra {
		kwargs[k] = v
	}
	err := r.runBoundedInOwnerScope(ctx, sessionKey, finalizeTimeoutS, func(scoped context.Context) {
		texts := agent.SessionEndMessages(agent.FinalizeSession(scoped, kwargs))
		mu.Lock()
		messages = append(messages, texts...)
		mu.Unlock()
	})
	switch {
	case err == nil:
	case errors.Is(err, errBoundedTimeout):
		runLog().Warn(fmt.Sprintf("Session finalize hooks (%s, reason=%s) exceeded %ss; proceeding without blocking the event loop "+
			"(the worker thread is left to finish on its own).", pyrepr.Str(sessionID), reason, pyrepr.Str(finalizeTimeoutS)))
	default:
		runLog().Debug(fmt.Sprintf("Session finalize hooks (%s, reason=%s) failed", pyrepr.Str(sessionID), reason), "exc_info", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string{}, messages...)
}

// Session finalize hooks off the loop for the session boundary surface of pkg/gateway
func (r *GatewayRunner) FinalizeSessionOffLoop(ctx context.Context, sessionID, platform, reason, sessionKey string) ([]string, error) {
	return r.finalizeSessionOffLoop(ctx, sessionID, platform, reason, sessionKey, nil), nil
}
