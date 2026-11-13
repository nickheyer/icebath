package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/observability"
)

// Resume reasons the boot auto continue picks up
var autoResumeReasons = map[string]bool{"restart_timeout": true, "shutdown_timeout": true, "restart_interrupted": true}

// Profile home resolver binding the runner to the context its lookups run in
type runnerHomeResolver struct {
	ctx context.Context
	r   *GatewayRunner
}

// Profile home a source's turn runs under
func (h runnerHomeResolver) ResolveProfileHomeForSource(source any) (string, error) {
	s, _ := source.(*gateway.SessionSource)
	return h.r.ResolveProfileHomeForSource(h.ctx, s)
}

// Epoch seconds of a stored gateway timestamp, nil when missing or unparseable
func coerceGatewayTimestamp(value any) *float64 {
	scaled := func(f float64) *float64 {
		if f > 10_000_000_000 {
			f /= 1000.0
		}
		return &f
	}
	switch v := value.(type) {
	case nil, bool:
		return nil
	case time.Time:
		f := float64(v.UnixNano()) / 1e9
		return &f
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return nil
		}
		if f, err := pyint.ParseFloat(text); err == nil {
			return scaled(f)
		}
		t, aware, err := pydatetime.ParseISOFormat(text)
		if err != nil {
			return nil
		}
		if !aware {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
		}
		f := float64(t.UnixNano()) / 1e9
		return &f
	}
	if f, err := pyint.Float(value); err == nil {
		return scaled(f)
	}
	return nil
}

// Whether an interruption marker is fresh enough to auto continue, an unknown timestamp counting as fresh
func isFreshGatewayInterruption(value any, now *float64, windowSecs float64) bool {
	if windowSecs <= 0 {
		return true
	}
	timestamp := coerceGatewayTimestamp(value)
	if timestamp == nil {
		return true
	}
	current := float64(time.Now().UnixNano()) / 1e9
	if now != nil {
		current = *now
	}
	return current-*timestamp <= windowSecs
}

// Resume pending entries, scoped to a platform when given, nil when enumeration failed or the restart loop breaker tripped
func (r *GatewayRunner) resumePendingCandidates(ctx context.Context, platform *gateway.Platform) []*gateway.SessionEntry {
	var candidates []*gateway.SessionEntry
	if err := func() (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		candidates = r.sessionStore.FilterLoadedEntries(ctx, func(entry *gateway.SessionEntry) bool {
			return entry.ResumePending && !entry.Suspended && entry.Origin != nil && entry.ResumeReason != nil &&
				autoResumeReasons[*entry.ResumeReason] && (platform == nil || entry.Origin.Platform == *platform)
		})
		return nil
	}(); err != nil {
		runLog().Warn(fmt.Sprintf("Failed to enumerate resume-pending sessions: %s", err))
		return nil
	}
	if len(candidates) > 0 {
		maxRestarts, window, maxGap := r.restartLoopGuardConfig(ctx)
		tripped, err := gateway.RestartLoopCheckAndRecord(ctx, maxRestarts, window, nil, maxGap)
		if err != nil {
			runLog().Debug(fmt.Sprintf("Restart-loop guard check skipped: %s", err))
		} else if tripped {
			return nil
		}
	}
	return candidates
}

// Whether the session owner is still authorized under the current allowlist
func (r *GatewayRunner) resumeOwnerAuthorized(ctx context.Context, sessionKey string, source *gateway.SessionSource) bool {
	authorized, err := r.isUserAuthorizedForSource(ctx, source, true)
	if err != nil {
		runLog().Warn(fmt.Sprintf("Skipping auto-resume for %s: authorization check failed: %s", sessionKey, err))
		return false
	}
	if authorized {
		return true
	}
	runLog().Warn(fmt.Sprintf("Skipping auto-resume for %s: session owner is no longer authorized under the current allowlist", sessionKey))
	return false
}

// Auto continues fresh restart interrupted sessions with a synthetic empty turn, returning how many were scheduled
func (r *GatewayRunner) scheduleResumePendingSessions(ctx context.Context, platform *gateway.Platform) (int, error) {
	window := gateway.AutoContinueFreshnessWindow()
	candidates := r.resumePendingCandidates(ctx, platform)
	if candidates == nil {
		return 0, nil
	}
	scheduled := 0
	for _, entry := range candidates {
		held := false
		for _, a := range gateway.AllAuthorities(r) {
			holds, err := gateway.HoldsAdmission(ctx, a, entry.SessionID)
			if err != nil {
				return scheduled, err
			}
			if holds {
				held = true
				break
			}
		}
		if held {
			continue
		}
		if !isFreshGatewayInterruption(entry.ResumeMarkerTimestamp(), nil, window) {
			continue
		}
		if r.isSessionRunning(entry.SessionKey) {
			continue
		}
		source, err := r.restoredSource(ctx, entry)
		if err != nil {
			return scheduled, err
		}
		adapter, err := r.deliveryAdapterFor(ctx, source)
		if err != nil {
			return scheduled, err
		}
		if adapter == nil {
			runLog().Debug(fmt.Sprintf("Skipping auto-resume for %s: adapter not ready for %s", entry.SessionKey, source.Platform))
			continue
		}
		if !r.resumeOwnerAuthorized(ctx, entry.SessionKey, source) {
			continue
		}
		r.withSessionState(entry.SessionKey, func(s *SessionState) {
			s.turn.agent = agentPendingSentinel
			s.turn.startedTS = float64(time.Now().UnixNano()) / 1e9
		})
		observability.NoteRunningChatOwner(runnerHomeResolver{ctx, r}, entry.SessionKey, source)
		r.persistActiveAgents(ctx)
		event := gateway.NewMessageEvent("")
		event.Source, event.Internal = source, true
		key := entry.SessionKey
		task := r.retainBackgroundTask(spawnGatewayTask(context.WithoutCancel(ctx), "", func(ctx context.Context) (any, error) {
			return nil, r.runStartupResumeEvent(ctx, adapter, event, key)
		}))
		r.startupRestoreTasksMu.Lock()
		if r.startupRestoreInProgress {
			r.startupRestoreTasks = append(r.startupRestoreTasks, task)
		}
		r.startupRestoreTasksMu.Unlock()
		scheduled++
	}
	if scheduled > 0 {
		runLog().Info(fmt.Sprintf("Scheduled auto-resume for %d restart-interrupted session(s)", scheduled))
	}
	return scheduled, nil
}

// Whether a restart, drain or shutdown began while starting up
func (r *GatewayRunner) startupShouldAbort() bool {
	return r.restartRequested || r.draining.Load() || r.shutdownEvent.IsSet()
}

// Cancels an adapter's background tasks then disconnects it
func (r *GatewayRunner) startupTeardownAdapter(ctx context.Context, adapter gateway.PlatformAdapter, platform gateway.Platform) {
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				logSuppressed(slog.LevelDebug, fmt.Errorf("%v", rec), "✗ %s background-task cancel error: %v", platform)
			}
		}()
		adapter.CancelBackgroundTasks(ctx)
	}()
	r.safeAdapterDisconnect(ctx, adapter, platform)
}

// Failed platform entry of a startup failure whose first retry comes in thirty seconds
func (r *GatewayRunner) startupRetryEntry(platform gateway.Platform, adapter gateway.PlatformAdapter, platformConfig *gateway.PlatformConfig, queued bool) map[string]any {
	return r.reconnectQueueEntry(platform, adapter, platformConfig, 1, 30, queued)
}

// Cleans up and leaves startup when a restart or shutdown began meanwhile, true when it did
func (r *GatewayRunner) abortStartupIfShutdownRequested(ctx context.Context, adapter gateway.PlatformAdapter, platform *gateway.Platform) (bool, error) {
	if !r.startupShouldAbort() {
		return false, nil
	}
	if adapter != nil && platform != nil {
		r.startupTeardownAdapter(ctx, adapter, *platform)
	}
	stopTask := r.currentStopTask()
	if stopTask != nil && stopTask != currentGatewayTask(ctx) {
		if _, err := stopTask.wait(ctx); err != nil {
			return true, err
		}
	} else if !r.shutdownEvent.IsSet() {
		if err := r.Stop(ctx, r.restartRequested, r.restartDetached, r.restartViaService); err != nil {
			return true, err
		}
	}
	return true, nil
}

// Runs the boot path sends without letting them pin the inbound restore gate, claiming the ledger rows inline first
func (r *GatewayRunner) awaitStartupBootSends(ctx context.Context, plannedRestartNotificationPending bool) error {
	claimed := r.claimPendingObligations(ctx)
	bootTask := spawnGatewayTask(context.WithoutCancel(ctx), "", func(ctx context.Context) (any, error) {
		r.sendRestartNotification(ctx)
		if plannedRestartNotificationPending {
			r.replayPendingPlannedRestartNotification(ctx)
		}
		r.redeliverClaimedObligations(ctx, claimed)
		return nil, nil
	})
	timeout := startupRestoreDrainTimeoutSecs()
	if timeout <= 0 {
		_, err := bootTask.wait(ctx)
		return err
	}
	r.waitBoundedOrRelease(ctx, []*gatewayTask{bootTask}, timeout,
		"Boot-path sends still running after %.0fs; releasing inbound gate so other platforms are not "+
			"frozen. Restart notification / obligation redelivery continue in the background.",
		"background boot-path send failed after gate release: see traceback", slog.LevelWarn, true)
	return nil
}

// Holds the startup restore gate open for an adapter recovery task while startup restore runs
func (r *GatewayRunner) JoinStartupRestore(ctx context.Context, name string, wait func(ctx context.Context) error) {
	r.startupRestoreTasksMu.Lock()
	defer r.startupRestoreTasksMu.Unlock()
	if !r.startupRestoreInProgress {
		return
	}
	task := spawnGatewayTask(context.WithoutCancel(ctx), name, func(ctx context.Context) (any, error) { return nil, wait(ctx) })
	r.startupRestoreTasks = append(r.startupRestoreTasks, task)
}

var _ gateway.StartupRestoreTaskJoiner = (*GatewayRunner)(nil)
