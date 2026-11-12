package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Dispatches one synthetic startup resume and waits for its agent turn, releasing an unclaimed pre claimed slot
func (r *GatewayRunner) runStartupResumeEvent(ctx context.Context, adapter gateway.PlatformAdapter, event *gateway.MessageEvent, sessionKey string) error {
	defer func() {
		r.releaseRunningSlot(ctx, sessionKey, nil, agentPendingSentinel)
	}()
	if err := adapter.Base().HandleMessage(ctx, event); err != nil {
		return err
	}
	if done := adapter.Base().SessionTaskDone(sessionKey); done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Queues an inbound message that arrived while startup auto resume runs
func (r *GatewayRunner) queueStartupRestoreEvent(event *gateway.MessageEvent) {
	if r.startupRestoreQueue == nil {
		r.startupRestoreQueue = []*gateway.MessageEvent{}
	}
	r.startupRestoreQueue = append(r.startupRestoreQueue, event)
	platform, chat := "unknown", "unknown"
	if source := event.Source; source != nil {
		chat = source.ChatID
		if source.Platform != "" {
			platform = string(source.Platform)
		}
	}
	runLog().Info(fmt.Sprintf("Queued inbound message during gateway startup restore: platform=%s chat=%s", platform, chat))
}

// Replays the inbound messages queued while startup auto resume ran, one failure never stopping the rest
func (r *GatewayRunner) drainStartupRestoreQueue(ctx context.Context) int {
	drained := 0
	for len(r.startupRestoreQueue) > 0 {
		event := r.startupRestoreQueue[0]
		r.startupRestoreQueue = r.startupRestoreQueue[1:]
		adapter, err := r.intakeAdapterFor(ctx, event.Source)
		if err == nil && adapter == nil {
			platform := "None"
			if event.Source != nil {
				platform = string(event.Source.Platform)
			}
			runLog().Warn(fmt.Sprintf("Dropping startup-restore queued message: adapter unavailable for %s", platform))
			continue
		}
		if err == nil {
			event.StartupRestoreReplay = true
			err = adapter.Base().HandleMessage(ctx, event)
		}
		if err != nil {
			logSuppressedExc(slog.LevelWarn, err, "Startup-restore queued replay failed; continuing drain")
			continue
		}
		drained++
	}
	return drained
}

// Waits boundedly for boot auto resume, then opens the inbound gate and drains the queued inbound
func (r *GatewayRunner) finishStartupRestore(ctx context.Context) {
	drained := 0
	func() {
		defer func() { r.startupRestoreInProgress = false }()
		r.startupRestoreTasksMu.Lock()
		tasks := append([]*gatewayTask(nil), r.startupRestoreTasks...)
		r.startupRestoreTasksMu.Unlock()
		if len(tasks) > 0 {
			done := r.waitBoundedOrRelease(ctx, tasks, startupRestoreDrainTimeoutSecs(),
				"Startup-restore gate released after %.0fs with %d boot auto-resume turn(s) "+
					"still running; draining inbound queue now (resume slots already claimed, so no "+
					"duplicate agents). Slow turn(s) continue in the background.",
				"background startup auto-resume task failed after gate release", slog.LevelDebug, false)
			report := lateFailureCallback("startup auto-resume task failed", slog.LevelDebug)
			for _, task := range done {
				report(task.gatewayFuture)
			}
		}
		r.startupRestoreTasksMu.Lock()
		r.startupRestoreTasks = []*gatewayTask{}
		r.startupRestoreTasksMu.Unlock()
		r.awaitStartupWarmup(ctx)
		drained = r.drainStartupRestoreQueue(ctx)
	}()
	if drained > 0 {
		runLog().Info(fmt.Sprintf("Drained %d inbound message(s) queued during startup restore", drained))
	}
}
