package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Seconds an off loop agent cleanup may run before the caller stops waiting on it
const cleanupTimeoutS = 30.0

// Drains memory writes, ends the memory session and closes the tool resources of a temporary or cached agent
func (r *GatewayRunner) cleanupAgentResources(ctx context.Context, a *agent.AIAgent) {
	if a == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		if mm, ok := a.MemoryManager().(*memory.MemoryManager); ok && mm != nil {
			func() {
				defer func() { _ = recover() }()
				mm.ClaimTeardown(true)
			}()
			func() {
				defer func() { _ = recover() }()
				mm.FlushPending(10 * time.Second)
			}()
		}
		a.ShutdownMemoryProvider(ctx, agent.SessionMessageDicts(a.SessionMessages()))
	}()
	func() {
		defer func() { _ = recover() }()
		_ = a.Close()
	}()
	func() {
		defer func() { _ = recover() }()
		provider.CleanupStaleAsyncClients()
	}()
}

// Runs the agent cleanup on the housekeeping pool inside the owning profile, waiting at most the cleanup timeout
func (r *GatewayRunner) cleanupAgentResourcesOffLoop(ctx context.Context, a *agent.AIAgent, cleanupContext, sessionKey string) {
	if a == nil {
		return
	}
	if strings.HasPrefix(cleanupContext, "shutdown") || cleanupContext == "session expiry" {
		a.SetEndSessionOnClose(false)
	}
	ctxLabel := ""
	if cleanupContext != "" {
		ctxLabel = " (" + cleanupContext + ")"
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(cleanupTimeoutS*float64(time.Second)))
	defer cancel()
	_, err := r.runHousekeepingInExecutor(waitCtx, func(workerCtx context.Context) (any, error) {
		r.runReleaseInProfileScope(context.WithoutCancel(workerCtx), func(scoped context.Context) { r.cleanupAgentResources(scoped, a) }, sessionKey)
		return nil, nil
	})
	switch {
	case err == nil:
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		runLog().Warn(fmt.Sprintf("Agent resource cleanup%s exceeded %ss; proceeding without blocking the event loop (the worker thread is left to finish on its own). (#53175)", ctxLabel, pyrepr.Str(cleanupTimeoutS)))
	default:
		runLog().Warn(fmt.Sprintf("Agent resource cleanup%s failed: %s (#53175)", ctxLabel, pyrepr.ExceptionMessage(err)))
	}
}

// Cleans the agent only after its executor future finishes, since the worker may still use its clients
func (r *GatewayRunner) deferAgentCleanupUntilFutureDone(ctx context.Context, future *gatewayFuture, a *agent.AIAgent, cleanupContext string) {
	r.trackDeferredAgentWorker(future, a)
	r.taskSetsMu.Lock()
	if r.deferredAgentCleanupTasks == nil {
		r.deferredAgentCleanupTasks = map[*gatewayTask]struct{}{}
	}
	tasks := r.deferredAgentCleanupTasks
	r.taskSetsMu.Unlock()
	r.trackTaskIn(tasks, spawnGatewayTask(context.WithoutCancel(ctx), "deferred-agent-cleanup", func(taskCtx context.Context) (any, error) {
		_, err := future.wait(taskCtx)
		if errors.Is(err, errTaskCancelled) || errors.Is(err, context.Canceled) {
			return nil, nil
		}
		if err != nil {
			label := ""
			if cleanupContext != "" {
				label = " (" + cleanupContext + ")"
			}
			runLog().Debug(fmt.Sprintf("Deferred agent worker%s finished with an error: %s", label, pyrepr.ExceptionMessage(err)))
		}
		r.cleanupAgentResourcesOffLoop(taskCtx, a, cleanupContext, "")
		return nil, nil
	}))
}
