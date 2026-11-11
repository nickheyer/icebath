package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Tracks the canonical route and starts the restart recoverable poller
func (r *GatewayRunner) registerHeartbeatWatch(ctx context.Context, quickKey string, source *gateway.SessionSource, sessionID string) {
	r.heartbeatMu.Lock()
	if r.heartbeatWatch == nil {
		r.heartbeatWatch = map[string]heartbeatWatchEntry{}
	}
	r.heartbeatWatch[quickKey] = heartbeatWatchEntry{source: source, sessionID: sessionID}
	r.heartbeatMu.Unlock()
	r.startHeartbeatPoller(ctx)
}

// Stops watching the route's heartbeat
func (r *GatewayRunner) unregisterHeartbeatWatch(quickKey string) {
	r.heartbeatMu.Lock()
	defer r.heartbeatMu.Unlock()
	delete(r.heartbeatWatch, quickKey)
}

// Snapshot of the watched routes in registration order of the map
func (r *GatewayRunner) heartbeatWatchItems() map[string]heartbeatWatchEntry {
	r.heartbeatMu.Lock()
	defer r.heartbeatMu.Unlock()
	items := make(map[string]heartbeatWatchEntry, len(r.heartbeatWatch))
	for key, entry := range r.heartbeatWatch {
		items[key] = entry
	}
	return items
}

// Wakes each idle watched session once, leaving busy sessions' ticks unclaimed
func (r *GatewayRunner) heartbeatPollOnce(ctx context.Context) {
	for quickKey, entry := range r.heartbeatWatchItems() {
		scoped, err := r.profileScopeForSource(ctx, entry.source)
		if err == nil {
			err = r.heartbeatPollWatch(scoped, quickKey, entry.source, entry.sessionID)
		}
		if err != nil {
			runLog().Debug(fmt.Sprintf("heartbeat poll for %s failed: %s", quickKey, err))
		}
	}
}

// Fires one watched heartbeat when its session is idle and a prompt is due
func (r *GatewayRunner) heartbeatPollWatch(ctx context.Context, quickKey string, source *gateway.SessionSource, sessionID string) error {
	r.warmGoalsSessionDB(ctx, "heartbeat poll")
	if r.sessionStore != nil {
		current, _ := r.sessionStore.PeekSessionID(quickKey)
		if current == "" {
			r.unregisterHeartbeatWatch(quickKey)
			return nil
		}
		sessionID = current
		r.heartbeatMu.Lock()
		if r.heartbeatWatch != nil {
			r.heartbeatWatch[quickKey] = heartbeatWatchEntry{source: source, sessionID: sessionID}
		}
		r.heartbeatMu.Unlock()
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return err
	}
	local, isLocal := adapter.(*gateway.LocalSessionAdapter)
	if adapter == nil || !(adapter.Base().MessageHandler() != nil || isLocal) {
		return nil
	}
	if isLocal && gateway.ViewerDriven(local.Authority, source.ChatID, "viewer_heartbeat") {
		return nil
	}
	if r.sessionAuthority != nil {
		return admitHeartbeat(ctx, r, adapter, source, sessionID, quickKey)
	}
	if r.isSessionRunning(quickKey) || adapter.Base().HasActiveSession(quickKey) || r.queueDepth(quickKey, adapter) > 0 {
		return nil
	}
	if tools.CheckPaused(ctx, "heartbeat", runLog()) {
		return nil
	}
	mgr := agent.NewHeartbeatManager(ctx, sessionID)
	if !mgr.HasHeartbeat() {
		r.unregisterHeartbeatWatch(quickKey)
		return nil
	}
	prompt, due := mgr.DuePrompt(nil)
	if !due || prompt == "" {
		return nil
	}
	event := syntheticPromptEvent(source, prompt, false)
	event.Metadata["gateway_session_key"] = quickKey
	event.SetHeartbeatSessionID(&sessionID)
	defer func() {
		if done := adapter.Base().SessionTaskDone(quickKey); done != nil {
			go func() {
				<-done
				settleHeartbeatAttempt(event, mgr)
			}()
		} else if !adapter.Base().HasActiveSession(quickKey) {
			mgr.AbandonFire()
		}
	}()
	return adapter.HandleMessage(ctx, event)
}

// Starts the single gateway wide heartbeat poll task once
func (r *GatewayRunner) startHeartbeatPoller(ctx context.Context) {
	r.heartbeatMu.Lock()
	defer r.heartbeatMu.Unlock()
	if existing := r.heartbeatPollTask; existing != nil && !existing.isDone() {
		return
	}
	task := spawnGatewayTask(context.WithoutCancel(ctx), "heartbeat-poller", func(taskCtx context.Context) (any, error) {
		for {
			if err := sleepCtx(taskCtx, agent.HeartbeatPollSeconds); err != nil {
				return nil, err
			}
			if err := restoreHeartbeatWatches(taskCtx, r); err != nil {
				return nil, err
			}
			if len(r.heartbeatWatchItems()) > 0 {
				r.heartbeatPollOnce(taskCtx)
			}
		}
	})
	r.heartbeatPollTask = task
	r.taskSetsMu.Lock()
	if r.supervisedWatchers == nil {
		r.supervisedWatchers = map[*gatewayTask]struct{}{}
	}
	r.supervisedWatchers[task] = struct{}{}
	r.taskSetsMu.Unlock()
	r.retainBackgroundTask(task)
}
