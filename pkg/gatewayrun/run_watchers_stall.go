package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Bound of one stall notice send so a wedged transport cannot stall the watcher pass
const stallNotifySendTimeoutSeconds = 15.0

// Configured stall timeout in seconds, zero disabling the watchdog
func (r *GatewayRunner) sessionStallTimeoutSeconds() float64 {
	return floatEnv("ICEBATH_SESSION_STALL_TIMEOUT", 300)
}

// Stall progress snapshot of a session's running agent, nil without a live agent
func (r *GatewayRunner) sessionActivityForStall(sessionKey string) map[string]any {
	running, ok := r.runningAgents().Get(sessionKey)
	if !ok || running == nil || running == agentPendingSentinel {
		return nil
	}
	return safeActivitySummary(running)
}

// Activity summary of an agent, nil when reading it panics
func safeActivitySummary(running *agent.AIAgent) (summary map[string]any) {
	defer func() {
		if rec := recover(); rec != nil {
			summary = nil
		}
	}()
	return running.GetActivitySummary()
}

// Pending inbound event of a stall candidate with the adapter holding it
type stallCandidate struct {
	adapter gateway.PlatformAdapter
	event   *gateway.MessageEvent
}

// Live adapters of the default and every profile map, each once
func (r *GatewayRunner) liveAdaptersOnce() []gateway.PlatformAdapter {
	seen := map[gateway.PlatformAdapter]bool{}
	var out []gateway.PlatformAdapter
	add := func(a gateway.PlatformAdapter) {
		if a != nil && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, a := range r.adapters.All() {
		add(a)
	}
	for _, m := range r.profileAdapters.All() {
		for _, a := range m.All() {
			add(a)
		}
	}
	return out
}

// Session key to pending adapter event from every live adapter's pending slot, then the overflow queues, first one winning
func (r *GatewayRunner) stallCandidates(ctx context.Context) (map[string]stallCandidate, error) {
	candidates := map[string]stallCandidate{}
	for _, adapter := range r.liveAdaptersOnce() {
		for key, event := range adapter.Base().PendingMessagesSnapshot() {
			if _, taken := candidates[key]; key != "" && !taken && event != nil {
				candidates[key] = stallCandidate{adapter: adapter, event: event}
			}
		}
	}
	for key, overflow := range r.queuedEvents().Items() {
		if _, taken := candidates[key]; key == "" || taken || len(overflow) == 0 {
			continue
		}
		if source := overflow[0].Source; source != nil {
			adapter, err := r.deliveryAdapterFor(ctx, source)
			if err != nil {
				return nil, err
			}
			if adapter != nil {
				candidates[key] = stallCandidate{adapter: adapter, event: overflow[0]}
			}
		}
	}
	return candidates, nil
}

// Wall clock seconds since the epoch
func wallNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Notifies once per stall episode for pending inbound sessions and returns the notices sent
func (r *GatewayRunner) checkSessionStalls(ctx context.Context, timeoutSeconds float64) (int, error) {
	if r.sessionStallNotified == nil {
		r.sessionStallNotified = map[string]bool{}
	}
	notified := r.sessionStallNotified
	sent, now := 0, wallNow()
	candidates, err := r.stallCandidates(ctx)
	if err != nil {
		return 0, err
	}
	for key, c := range candidates {
		activity := r.sessionActivityForStall(key)
		idle := gateway.ResolveSessionIdleSecondsFromActivity(activity, &now)
		if gateway.ShouldClearSessionStallNotification(timeoutSeconds, idle, true) {
			delete(notified, key)
		}
		if idle == nil || !gateway.ShouldEmitSessionStallNotification(timeoutSeconds, idle, true, notified[key]) {
			continue
		}
		if activity == nil {
			activity = map[string]any{}
		}
		ok, err := r.notifySessionStall(ctx, key, c.adapter, c.event, *idle, activity, timeoutSeconds, notified)
		if err != nil {
			return sent, err
		}
		if ok {
			sent++
		}
	}
	for key := range notified {
		if _, live := candidates[key]; !live {
			delete(notified, key)
		}
	}
	return sent, nil
}

// First truthy value of an activity summary among keys, unknown when none is
func activityText(activity map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := activity[k]; pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
	}
	return "unknown"
}

// Logs one stall episode and delivers its notice, true only when sent and latched
func (r *GatewayRunner) notifySessionStall(ctx context.Context, sessionKey string, adapter gateway.PlatformAdapter, pending *gateway.MessageEvent, idleSeconds float64, activity map[string]any, timeoutSeconds float64, notified map[string]bool) (bool, error) {
	runLog().Warn(fmt.Sprintf("Session stall detected: session=%s idle=%.0fs (timeout=%.0fs, ~%d min); pending "+
		"inbound present | last_activity=%s | provenance=%s (agent.session_stall_timeout)",
		sessionKey, idleSeconds, timeoutSeconds, max(1, int(idleSeconds/60)),
		activityText(activity, "last_activity_desc", "last_activity_description"),
		activityText(activity, "provenance", "last_activity_provenance")))
	var source *gateway.SessionSource
	if pending != nil {
		source = pending.Source
	}
	if source == nil || source.ChatID == "" {
		runLog().Warn(fmt.Sprintf("Session stall notify skipped (no chat_id): session=%s", sessionKey))
		notified[sessionKey] = true
		return false, nil
	}
	slotted, _ := adapter.Base().PeekPendingMessage(sessionKey)
	stillSlotted := slotted != nil
	queued, _ := r.queuedEvents().Get(sessionKey)
	stillPending := stillSlotted || len(queued) > 0
	freshNow := wallNow()
	freshIdle := gateway.ResolveSessionIdleSecondsFromActivity(r.sessionActivityForStall(sessionKey), &freshNow)
	if !stillPending || (freshIdle != nil && *freshIdle < timeoutSeconds) {
		var fresh any
		if freshIdle != nil {
			fresh = *freshIdle
		}
		runLog().Info(fmt.Sprintf("Session stall notify aborted (no longer stale): session=%s pending=%s fresh_idle=%s",
			sessionKey, pyrepr.Str(stillPending), pyrepr.Str(fresh)))
		delete(notified, sessionKey)
		return false, nil
	}
	err := func() error {
		metadata := r.threadMetadataForSource(source, nil)
		notice := gateway.FormatSessionStallNotification(idleSeconds)
		var result *gateway.SendResult
		profileHome, err := r.resolveProfileHomeForSource(ctx, source)
		if err != nil {
			return err
		}
		scoped := gateway.ProfileRuntimeScope(ctx, profileHome, true)
		presented, err := config.PresentNotification(scoped, func(c context.Context) error {
			sendCtx, cancel := context.WithTimeout(c, time.Duration(stallNotifySendTimeoutSeconds*float64(time.Second)))
			defer cancel()
			res, err := adapter.Send(sendCtx, source.ChatID, notice, "", metadata)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(sendCtx.Err(), context.DeadlineExceeded) {
					return context.DeadlineExceeded
				}
				return err
			}
			result = &res
			return nil
		}, string(source.Platform), true, nil)
		if err != nil {
			return err
		}
		if !presented {
			notified[sessionKey] = true
			return errStallSuppressed
		}
		if result != nil && !result.Success {
			return fmt.Errorf("%s", pyrepr.Str(result.Error))
		}
		return nil
	}()
	switch {
	case errors.Is(err, errStallSuppressed):
		return false, nil
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		runLog().Warn(fmt.Sprintf("Session stall notify send timed out after %.0fs for %s; will retry next tick",
			stallNotifySendTimeoutSeconds, sessionKey))
		return false, nil
	case err != nil:
		if isCancellation(err) || ctx.Err() != nil {
			return false, err
		}
		runLog().Warn(fmt.Sprintf("Session stall notify failed for %s: %s", sessionKey, err.Error()))
		return false, nil
	}
	notified[sessionKey] = true
	return true, nil
}

// Whether an error is the cancellation of the running task
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, errTaskCancelled)
}

// Marker of a stall notice the warning policy suppressed
var errStallSuppressed = errors.New("stall notice suppressed")

// Refreshes the model picker's remote catalogs every TTL window until the runner stops
func (r *GatewayRunner) modelCatalogRefreshWatcher(ctx context.Context) error {
	if err := sleepCtx(ctx, 30); err != nil {
		return err
	}
	for r.running.Load() {
		if _, err := r.runInExecutorWithContext(ctx, func(c context.Context) (any, error) {
			return provider.RefreshCatalogs(c, nil), nil
		}); err != nil {
			if isCancellation(err) || ctx.Err() != nil {
				return err
			}
			runLog().Debug(fmt.Sprintf("Model catalog refresh failed: %s", err.Error()))
		}
		interval := provider.CatalogRefreshInterval(ctx).Seconds()
		deadline := pythread.Monotonic() + interval
		for r.running.Load() && pythread.Monotonic() < deadline {
			if err := sleepCtx(ctx, min(30.0, max(0.0, deadline-pythread.Monotonic()))); err != nil {
				return err
			}
		}
	}
	return nil
}

// Pending inbound plus stale activity stall watchdog, notify only, until the runner stops
func (r *GatewayRunner) sessionStallWatcher(ctx context.Context, interval float64) error {
	if err := sleepCtx(ctx, min(30.0, max(1.0, interval))); err != nil {
		return err
	}
	for r.running.Load() {
		if timeout := r.sessionStallTimeoutSeconds(); timeout > 0 {
			if _, err := r.checkSessionStalls(ctx, timeout); err != nil {
				if isCancellation(err) || ctx.Err() != nil {
					return err
				}
				runLog().Debug(fmt.Sprintf("Session stall watcher error: %s", err.Error()))
			}
		}
		if err := interruptibleSleep(ctx, r, max(1, int(interval))); err != nil {
			return err
		}
	}
	return nil
}
