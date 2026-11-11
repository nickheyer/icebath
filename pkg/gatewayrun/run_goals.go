package gatewayrun

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Configured goal turn budget from the top level goals block, twenty when unset or malformed
func (r *GatewayRunner) goalMaxTurnsFromConfig(ctx context.Context) int64 {
	goalsCfg, _ := r.readUserConfig(ctx)["goals"].(map[string]any)
	value := goalsCfg["max_turns"]
	if !pyrepr.Truthy(value) {
		return 20
	}
	n, err := pyint.Of(value)
	if err != nil || !n.IsInt64() {
		return 20
	}
	return n.Int64()
}

// Warms the goals session database cache off the loop, a failed warm up logged and never raised
func (r *GatewayRunner) warmGoalsSessionDB(ctx context.Context, label string) {
	if _, err := r.runInExecutorWithContext(ctx, func(c context.Context) (any, error) { return goals.SessionDB(c), nil }); err != nil {
		runLog().Warn(fmt.Sprintf("%s: session DB warm-up failed: %s", label, err))
	}
}

// Session entry for a goal or heartbeat manager, nil when the lookup fails or names no session
func (r *GatewayRunner) sessionEntryForManager(ctx context.Context, event *gateway.MessageEvent, label string) *gateway.SessionEntry {
	r.warmGoalsSessionDB(ctx, label)
	entry, err := r.sessionStore.GetOrCreateSession(ctx, event.Source, false, !event.Internal)
	if err != nil {
		runLog().Debug(fmt.Sprintf("%s: session lookup failed: %s", label, err))
		return nil
	}
	if entry == nil || entry.SessionID == "" {
		return nil
	}
	return entry
}

// Goal manager of the event's session with its entry, nils when the session does not resolve
func (r *GatewayRunner) getGoalManagerForEvent(ctx context.Context, event *gateway.MessageEvent) (*goals.GoalManager, *gateway.SessionEntry) {
	maxTurns := r.goalMaxTurnsFromConfig(ctx)
	entry := r.sessionEntryForManager(ctx, event, "goal manager")
	if entry == nil {
		return nil, nil
	}
	return goals.NewGoalManager(ctx, entry.SessionID, maxTurns), entry
}

// Heartbeat manager of the event's session with its entry, nils when the session does not resolve
func (r *GatewayRunner) getHeartbeatManagerForEvent(ctx context.Context, event *gateway.MessageEvent) (*agent.HeartbeatManager, *gateway.SessionEntry) {
	entry := r.sessionEntryForManager(ctx, event, "heartbeat manager")
	if entry == nil {
		return nil, nil
	}
	return agent.NewHeartbeatManager(ctx, entry.SessionID), entry
}

// Text event injecting a goal, heartbeat or loop prompt, never a reply to the message that registered it
func syntheticPromptEvent(source *gateway.SessionSource, text string, internal bool) *gateway.MessageEvent {
	if source != nil && source.MessageID != nil && *source.MessageID != "" {
		source = gateway.ReplaceSource(source, func(s *gateway.SessionSource) { s.MessageID = nil })
	}
	event := gateway.NewMessageEvent(text)
	event.Source, event.Internal, event.InheritChannelPin = source, internal, true
	return event
}

// Delivery adapter of a goal notice, logged when the platform has none
func (r *GatewayRunner) goalNoticeAdapter(ctx context.Context, source *gateway.SessionSource) (gateway.PlatformAdapter, error) {
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return nil, err
	}
	if adapter == nil {
		runLog().Debug(fmt.Sprintf("goal continuation: no adapter for %s", source.Platform))
	}
	return adapter, nil
}

// Sends a goal judge status line to the originating chat, a status update event for a local session
func (r *GatewayRunner) sendGoalStatusNotice(ctx context.Context, source *gateway.SessionSource, message string) error {
	if source.Platform == gateway.PlatformLocal {
		authority, err := gateway.ActiveAuthority(ctx, r)
		if err != nil {
			return err
		}
		if authority != nil {
			if live, ok := authority.Live(source.ChatID); ok && live != nil && live.EventStream != nil {
				live.EventStream.Publish(source.ChatID, map[string]any{"kind": "goal", "text": message}, "status.update", nil)
			}
		}
		return nil
	}
	adapter, err := r.goalNoticeAdapter(ctx, source)
	if err != nil || adapter == nil {
		return err
	}
	result, err := adapter.Send(ctx, source.ChatID, message, "", r.threadMetadataForSource(source, nil))
	if err != nil {
		return err
	}
	if !result.Success {
		reason := "unknown error"
		if pyrepr.Truthy(result.Error) {
			reason = pyrepr.Str(result.Error)
		}
		runLog().Warn(fmt.Sprintf("goal continuation: status send failed: %s", reason))
	}
	return nil
}

// Sends a goal status line after the main response is delivered, directly when no post delivery hook takes it
func (r *GatewayRunner) deferGoalStatusNoticeAfterDelivery(ctx context.Context, source *gateway.SessionSource, message string) error {
	adapter, err := r.goalNoticeAdapter(ctx, source)
	if err != nil || adapter == nil {
		return err
	}
	deliver := func(ctx context.Context) error {
		if err := r.sendGoalStatusNotice(ctx, source, message); err != nil {
			runLog().Warn(fmt.Sprintf("goal continuation: status send failed: %s", err))
		}
		return nil
	}
	sessionKey, keyErr := r.sessionKeyForSource(ctx, source)
	if keyErr == nil && sessionKey != "" {
		var generation *int
		if active := adapter.Base().ActiveSessionEvent(sessionKey); active != nil {
			if g, ok := active.RunGeneration().(int); ok {
				generation = &g
			}
		}
		adapter.Base().RegisterPostDeliveryCallback(sessionKey, deliver, generation)
		return nil
	}
	return deliver(ctx)
}

// Manager the factory builds for the entry's session after warming the database, nil without a session id
func (r *GatewayRunner) postTurnManager(ctx context.Context, entry *gateway.SessionEntry, label string, factory func(ctx context.Context, sid string) any) any {
	if entry == nil || entry.SessionID == "" {
		return nil
	}
	r.warmGoalsSessionDB(ctx, label)
	return factory(ctx, entry.SessionID)
}

// Runs the goal judge after a turn and enqueues a continuation through the adapter FIFO while the goal stays active
func (r *GatewayRunner) postTurnGoalContinuation(ctx context.Context, entry *gateway.SessionEntry, source *gateway.SessionSource, finalResponse string) error {
	maxTurns := r.goalMaxTurnsFromConfig(ctx)
	mgr, _ := r.postTurnManager(ctx, entry, "goal continuation", func(ctx context.Context, sid string) any {
		return goals.NewGoalManager(ctx, sid, maxTurns)
	}).(*goals.GoalManager)
	if mgr == nil || !mgr.IsActive() {
		return nil
	}
	backgroundProcs := goals.GatherBackgroundProcesses(ctx, "", entry.SessionID)
	activeDelegations := goals.CountActiveDelegations(entry.SessionID)
	out, err := r.runInExecutorWithContext(ctx, func(c context.Context) (any, error) {
		return mgr.EvaluateAfterTurn(finalResponse, backgroundProcs, activeDelegations)
	})
	if err != nil {
		return err
	}
	decision := out.(goals.GoalDecision)
	if decision.Message != "" && source != nil {
		if err := r.deferGoalStatusNoticeAfterDelivery(ctx, source, decision.Message); err != nil {
			return err
		}
	}
	prompt := ""
	if decision.ContinuationPrompt != nil {
		prompt = *decision.ContinuationPrompt
	}
	if !decision.ShouldContinue || prompt == "" || source == nil {
		return nil
	}
	if source.Platform == gateway.PlatformLocal {
		adapter, err := r.deliveryAdapterFor(ctx, source)
		if err == nil {
			_, err = gateway.AdmitRouteWake(ctx, r, adapter, source, prompt, "goal-continuation")
		}
		if err != nil {
			runLog().Debug("goal continuation: local admission failed", "error", err)
		}
		return nil
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		runLog().Debug(fmt.Sprintf("goal continuation: enqueue failed: %s", err))
		return nil
	}
	quickKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		runLog().Debug(fmt.Sprintf("goal continuation: enqueue failed: %s", err))
		return nil
	}
	if adapter != nil && quickKey != "" {
		r.enqueueFIFO(quickKey, syntheticPromptEvent(source, prompt, false), adapter)
	}
	return nil
}

// Goal and loop bookkeeping after an agent turn returns
func (r *GatewayRunner) runPostTurnHooks(ctx context.Context, agentResult any, source *gateway.SessionSource, isInternal bool, event *gateway.MessageEvent) error {
	authority, err := gateway.ActiveAuthority(ctx, r)
	if err != nil {
		return err
	}
	if gateway.ViewerDriven(authority, source.ChatID, "viewer_automation") {
		return nil
	}
	finalText := finalTextForPostTurnHooks(agentResult, event)
	entry, err := r.sessionStore.GetOrCreateSession(ctx, source, false, !isInternal)
	if err != nil {
		runLog().Debug(fmt.Sprintf("post-turn session resolution failed: %s", err))
		return nil
	}
	failed := turnFailed(ctx)
	type postTurnHook struct {
		label string
		run   func(ctx context.Context, entry *gateway.SessionEntry, source *gateway.SessionSource, finalResponse string) error
	}
	hooks := []postTurnHook{{"loop completion", r.postTurnLoopCompletion}}
	if pystr.Strip(finalText) != "" && !failed {
		hooks = append([]postTurnHook{{"goal continuation", r.postTurnGoalContinuation}}, hooks...)
	}
	finalResponse := finalText
	if failed {
		finalResponse = ""
	}
	for _, hook := range hooks {
		if err := hook.run(ctx, entry, source, finalResponse); err != nil {
			runLog().Debug(fmt.Sprintf("%s hook failed: %s", hook.label, err))
		}
	}
	return r.publishLocalControl(ctx, source, true)
}

// Repaints a local session's goal and loop cards after their state moved
func (r *GatewayRunner) publishLocalControl(ctx context.Context, source *gateway.SessionSource, onlyIfPresent bool) error {
	if source.Platform != gateway.PlatformLocal {
		return nil
	}
	authority, err := gateway.ActiveAuthority(ctx, r)
	if err != nil {
		return err
	}
	controlPublishAsync(ctx, authority, source.ChatID, onlyIfPresent, false)
	return nil
}

// Whether the owner recorded this turn's result as failed
func turnFailed(ctx context.Context) bool {
	captured := gateway.ExecutionResultOf(ctx)
	if captured == nil {
		return false
	}
	result, ok := captured.Get("terminal_result")
	if !ok {
		result, _ = captured.Get("result")
	}
	switch typed := result.(type) {
	case *pyjson.Object:
		return pyrepr.Truthy(typed.GetOr("failed", nil))
	case map[string]any:
		return pyrepr.Truthy(typed["failed"])
	}
	return false
}

// Text a goal or loop judges after a turn, the streamed reply when the result is empty
func finalTextForPostTurnHooks(agentResult any, event *gateway.MessageEvent) string {
	text := ""
	switch typed := agentResult.(type) {
	case map[string]any:
		if pyrepr.Truthy(typed["final_response"]) {
			text = pyrepr.Str(typed["final_response"])
		}
	case string:
		text = typed
	}
	if pystr.Strip(text) != "" {
		return text
	}
	if event != nil {
		if streamed := event.StreamedFinalResponse(); streamed != nil && pystr.Strip(*streamed) != "" {
			return *streamed
		}
	}
	return text
}

// Completes an in flight loop tick after a turn and schedules the next one
func (r *GatewayRunner) postTurnLoopCompletion(ctx context.Context, entry *gateway.SessionEntry, source *gateway.SessionSource, finalResponse string) error {
	mgr, _ := r.postTurnManager(ctx, entry, "loop completion", func(ctx context.Context, sid string) any {
		return agent.NewLoopManager(ctx, sid)
	}).(*agent.LoopManager)
	if mgr == nil {
		return nil
	}
	state := mgr.State()
	if state == nil || !state.AwaitingResponse {
		return nil
	}
	out, err := r.runInExecutorWithContext(ctx, func(c context.Context) (any, error) { return mgr.CompleteTick(c, finalResponse), nil })
	if err != nil {
		return err
	}
	if msg := out.(agent.LoopDecision).Message; msg != "" && source != nil {
		return r.deferGoalStatusNoticeAfterDelivery(ctx, source, msg)
	}
	return nil
}

// Text of a route field, empty when absent or None
func routeText(route *pyjson.Object, key string) string {
	if route == nil {
		return ""
	}
	value, ok := route.Get(key)
	if !ok || value == nil {
		return ""
	}
	return pyrepr.Str(value)
}

// Injects one due loop wakeup into its session, applying every deferral rule
func (r *GatewayRunner) loopWakeupFireOne(ctx context.Context, sid string, state *agent.SessionLoopState, now float64, warnedNoRoute map[string]bool, profile *string) error {
	if state.AwaitingResponse || now < state.NextDueAt {
		return nil
	}
	route := state.Route
	platformName, chatID := routeText(route, "platform"), routeText(route, "chat_id")
	if platformName == "" || chatID == "" {
		return nil
	}
	if routed := routeText(route, "profile"); routed != "" {
		profile = &routed
	}
	var adapter gateway.PlatformAdapter
	if adapters := r.adaptersForProfile(ctx, profile); adapters != nil {
		for p, a := range adapters.All() {
			if string(p) == platformName {
				adapter = a
				break
			}
		}
	}
	if adapter == nil {
		if !warnedNoRoute[sid] {
			warnedNoRoute[sid] = true
			runLog().Debug(fmt.Sprintf("loop wakeup: no adapter for platform %s (session %s, profile %s)", pyrepr.Repr(platformName), sid, derefOrNoneText(profile)))
		}
		return nil
	}
	evt := map[string]any{"session_key": "", "platform": platformName, "chat_id": chatID}
	for _, key := range []string{"chat_type", "thread_id", "user_id", "user_name"} {
		evt[key] = ""
		if route != nil {
			if value, ok := route.Get(key); ok {
				evt[key] = value
			}
		}
	}
	source, err := r.buildProcessEventSource(ctx, evt)
	if err != nil || source == nil {
		return err
	}
	if profile != nil && *profile != "" && (source.Profile == nil || *source.Profile == "") {
		profileName := *profile
		source.Profile = &profileName
	}
	if sessionKey, err := r.sessionKeyForSource(ctx, source); err == nil && sessionKey != "" && r.isSessionRunning(sessionKey) {
		return nil
	}
	if agent.GoalBlocksLoopTick(ctx, sid) {
		return nil
	}
	mgr := agent.NewLoopManager(ctx, sid)
	if !mgr.IsDueAt(now) {
		return nil
	}
	if tools.CheckPaused(ctx, "loop", runLog()) {
		return nil
	}
	out, err := r.runInExecutorWithContext(ctx, func(c context.Context) (any, error) {
		wakeup, ok := mgr.FireTick(c)
		if !ok {
			return "", nil
		}
		return wakeup, nil
	})
	if err != nil {
		return err
	}
	wakeup, _ := out.(string)
	if wakeup == "" {
		return nil
	}
	if err := r.loopWakeupInject(ctx, mgr, adapter, source, wakeup, platformName); err != nil {
		runLog().Warn(fmt.Sprintf("loop wakeup injection failed for %s: %s", sid, err))
		mgr.AbandonTick(ctx)
	}
	return r.publishLocalControl(ctx, source, false)
}

// Commits the wakeup to the authority FIFO or runs it as an internal turn, completing a slash tick at once
func (r *GatewayRunner) loopWakeupInject(ctx context.Context, mgr *agent.LoopManager, adapter gateway.PlatformAdapter, source *gateway.SessionSource, wakeup, platformName string) error {
	ticks := "?"
	if state := mgr.State(); state != nil {
		ticks = fmt.Sprint(state.TicksFired)
	}
	runLog().Info(fmt.Sprintf("loop wakeup #%s — injecting for %s chat=%s thread=%s", ticks, platformName, source.ChatID, derefOrNoneText(source.ThreadID)))
	slash := strings.HasPrefix(pystr.Lstrip(wakeup), "/")
	admitted := false
	if !slash {
		var err error
		if admitted, err = gateway.AdmitRouteWake(ctx, r, adapter, source, wakeup, "loop-tick"); err != nil {
			return err
		}
	}
	if slash || !admitted {
		if err := adapter.HandleMessage(ctx, syntheticPromptEvent(source, wakeup, true)); err != nil {
			return err
		}
	}
	if slash {
		if _, err := r.runInExecutorWithContext(ctx, func(c context.Context) (any, error) { return mgr.CompleteTick(c, ""), nil }); err != nil {
			return err
		}
	}
	return nil
}

// Fires due loop wakeups for idle sessions of every served profile store until the runner stops
func (r *GatewayRunner) loopWakeupWatcher(ctx context.Context, interval float64) error {
	if err := sleepCtx(ctx, 5); err != nil {
		return err
	}
	warnedNoRoute := map[string]bool{}
	scanOneStore := func(ctx context.Context, profileName *string) error {
		r.warmGoalsSessionDB(ctx, "loop wakeup")
		out, err := r.runInExecutorWithContext(ctx, func(c context.Context) (any, error) { return agent.ListActiveLoops(c), nil })
		if err != nil {
			return err
		}
		now := float64(time.Now().UnixNano()) / 1e9
		for _, loop := range out.([]agent.ActiveLoop) {
			if err := r.loopWakeupFireOne(ctx, loop.SessionID, loop.State, now, warnedNoRoute, profileName); err != nil {
				return err
			}
		}
		return nil
	}
	tick := func() error {
		scopes, err := r.resolveHandoffWatchScopes(ctx)
		if err != nil {
			return err
		}
		for _, scope := range scopes {
			var home *routedHome
			if scope.home != "" {
				active, err := r.runInExecutorWithContext(ctx, func(c context.Context) (any, error) { return profileHasActiveLoop(c, scope.home), nil })
				if err != nil {
					return err
				}
				if active != true {
					continue
				}
				home = &routedHome{path: scope.home}
			}
			scoped := scopeOrNull(ctx, func(c context.Context, h string) context.Context { return gateway.ProfileRuntimeScope(c, h, true) }, home)
			if err := scanOneStore(scoped, scope.name); err != nil {
				return err
			}
		}
		return nil
	}
	for r.running {
		if err := tick(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			runLog().Debug(fmt.Sprintf("loop wakeup watcher error: %s", err))
		}
		if err := sleepCtx(ctx, interval); err != nil {
			return err
		}
	}
	return nil
}
