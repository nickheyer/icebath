package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"maps"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Empty dict of the same type, the Python clear
func pydictClear[K comparable, V any](_ *pydict.Dict[K, V]) *pydict.Dict[K, V] {
	return pydict.New[K, V]()
}

// A turn the user saw end, its trailing failure state, engagement, interrupt friction and cost rows
func (r *Runtime) observeModelTurn(ctx context.Context, session *metricsSession, task *taskRun, fields map[string]string) {
	route := task.modelRoute
	if route == nil {
		route = session.modelState.LastRoute
	}
	session.modelState.ObserveTurn(fields["outcome"], route, session.lastTurnNS)
	if task.selectedRoute != nil && task.cost.UserTurn {
		r.withRouteRun(session.sessionID, session.routeRun, func(run *RouteRun) { run.Observe(task.selectedRoute) })
	}
	if task.modelRoute != nil && EngagedTurn(task.startFields, task.cost.UserTurn) {
		data := relayData(maps.Clone(task.modelRoute))
		relayGuarded("Icebath shared-metrics engagement mark failed", func() error {
			return r.mark(ctx, session, nil, EngagementTurnMark, data)
		})
	}
	if fields["end_reason"] == "user_cancelled" && route != nil && Attended(task.startFields) {
		data := relayData(FrictionFields("interrupt", route))
		relayGuarded("Icebath shared-metrics friction mark failed", func() error {
			return r.mark(ctx, session, nil, ModelFrictionMark, data)
		})
	}
	if task.cost.UserTurn && Attended(task.startFields) {
		rows := session.efficiency.FinishTurn(task.cost, fields, route, len(task.modelCallIDs), task.toolCallCount())
		r.emitRows(ctx, session, rows)
	}
}

// Emits rows under the session scope, or as process marks without a session
func (r *Runtime) emitRows(ctx context.Context, session *metricsSession, rows []MarkRow) {
	for _, row := range rows {
		data := relayData(row.Dims)
		if session == nil {
			relayGuarded("Icebath shared-metrics efficiency mark failed", func() error { return r.recordProcessMark(ctx, row.Mark, data) })
		} else {
			relayGuarded("Icebath shared-metrics efficiency mark failed", func() error { return r.mark(ctx, session, nil, row.Mark, data) })
		}
	}
}

// One finished primary call of a user turn, its tokens and an unannounced cold cache read
func (r *Runtime) observeTurnCall(ctx context.Context, session *metricsSession, task *taskRun, call *modelCall, event map[string]any) {
	if task == nil || !task.cost.UserTurn {
		return
	}
	route, usage := call.fields, event["usage"]
	task.cost.AddCall(route, usage)
	expected := r.consumeCold(coldKey(ctx, session.sessionID))
	if cause, ok := session.efficiency.ObserveCache(route, usage, call.startedNS, monotonicNS(), expected); ok {
		r.emitRows(ctx, session, []MarkRow{CacheBreakRow(cause, route)})
	}
}

// Conversation whose lineage root survives compression rotation, else the session
func coldKey(ctx context.Context, sessionID string) string {
	if ConversationContextHook != nil {
		if conversation := ConversationContextHook(ctx); conversation != "" {
			return conversation
		}
	}
	return sessionID
}

// Expects the next read of the key to be cold, false when a break was already announced
func (r *Runtime) announceCold(key string) bool {
	r.coldMu.Lock()
	defer r.coldMu.Unlock()
	if r.coldExpected.Has(key) {
		return false
	}
	r.coldExpected.Set(key, struct{}{})
	for r.coldExpected.Len() > coldExpectedKept {
		r.coldExpected.Delete(r.coldExpected.Keys()[0])
	}
	return true
}

// Takes an announced cold read for the key
func (r *Runtime) consumeCold(key string) bool {
	r.coldMu.Lock()
	defer r.coldMu.Unlock()
	return r.coldExpected.Delete(key)
}

// Counts a prefix invalidation the agent caused once, before the cold read it causes
func (r *Runtime) recordKnownCacheBreak(ctx context.Context, cause string, route map[string]string, sessionID string) {
	if r.announceCold(coldKey(ctx, sessionID)) {
		r.emitRows(ctx, nil, []MarkRow{CacheBreakRow(cause, route)})
	}
}

type sessionToolsKey struct {
	agentTools, toolsForAPI uintptr
	count                   int
}

// Records the tools one primary request sent, a changed array announcing a cache break
func (r *Runtime) recordSessionTools(ctx context.Context, sessionID string, agent MetricsAgent, toolsForAPI []map[string]any) error {
	var session *metricsSession
	if sessionID != "" {
		session = r.session(map[string]any{"session_id": sessionID})
	}
	if session == nil {
		return nil
	}
	var agentTools []map[string]any
	if agent != nil {
		agentTools = agent.ToolDefinitions()
	}
	key := sessionToolsKey{uintptr(unsafe.Pointer(unsafe.SliceData(agentTools))), uintptr(unsafe.Pointer(unsafe.SliceData(toolsForAPI))), len(toolsForAPI)}
	session.mu.Lock()
	skip := session.closing || session.efficiency.ToolsKey == any(key)
	session.mu.Unlock()
	if skip {
		return nil
	}
	names, toolsets, enabledCount, schemaTokens, err := ToolSnapshot(ctx, agent, toolsForAPI)
	if err != nil {
		return err
	}
	session.mu.Lock()
	if session.closing {
		session.mu.Unlock()
		return nil
	}
	session.efficiency.ToolsKey = key
	changed := session.efficiency.Tools.Observe(names, toolsets, enabledCount, schemaTokens)
	session.mu.Unlock()
	var provider, model any
	if agent != nil {
		provider, model = agent.Provider(), agent.Model()
	}
	route := ModelRoute(provider, model)
	if changed && r.announceCold(coldKey(ctx, sessionID)) {
		r.emitRows(ctx, session, []MarkRow{CacheBreakRow("toolset_change", route)})
	}
	return nil
}

// Links a compression rotated id to its conversation and closes the old segment once idle
func (r *Runtime) rotateSegment(ctx context.Context, oldID, newID string) {
	old := r.session(map[string]any{"session_id": oldID})
	r.lineageMu.Lock()
	key, ok := r.lineageOf.Get(oldID)
	if !ok {
		if old == nil {
			r.lineageMu.Unlock()
			return
		}
		key = oldID
		r.lineages.Set(key, &peakLineage{
			open: map[string]bool{oldID: true}, peak: NewModelSessionState(), tools: NewToolUsage(), tally: NewSessionTally(),
			run: old.routeRun, spent: old.efficiency.Spent, connectors: old.connectors,
		})
		r.lineageOf.Set(oldID, key)
	}
	lineage, _ := r.lineages.Get(key)
	lineage.open[newID] = true
	r.lineageOf.Set(newID, key)
	var overflow []string
	if keys := r.lineages.Keys(); len(keys) > maxLineages {
		overflow = keys[:len(keys)-maxLineages]
	}
	r.lineageMu.Unlock()
	if fresh := r.session(map[string]any{"session_id": newID}); fresh != nil {
		r.shareSpent(fresh)
	}
	if old != nil {
		old.mu.Lock()
		old.retiring = !old.closing
		idle := old.retiring && old.tasks.Len() == 0
		old.mu.Unlock()
		if idle {
			relaySafe(func() error { return r.closeSession(ctx, map[string]any{"session_id": oldID}) })
		}
	}
	for _, stale := range overflow {
		r.flushLineage(ctx, stale)
	}
}

// Spent turns of the conversation a segment belongs to, nil outside a lineage
func (r *Runtime) lineageSpent(sessionID string) *SpentTurns {
	r.lineageMu.Lock()
	defer r.lineageMu.Unlock()
	if key, ok := r.lineageOf.Get(sessionID); ok {
		if lineage, ok := r.lineages.Get(key); ok {
			return lineage.spent
		}
	}
	return nil
}

// Points a rotated to segment at the earlier segments' spent turns and connector chain
func (r *Runtime) shareSpent(session *metricsSession) {
	if spent := r.lineageSpent(session.sessionID); spent != nil {
		session.efficiency.Spent = spent
	}
	r.lineageMu.Lock()
	defer r.lineageMu.Unlock()
	if key, ok := r.lineageOf.Get(session.sessionID); ok {
		if lineage, ok := r.lineages.Get(key); ok && lineage.connectors != nil {
			session.connectors = lineage.connectors
		}
	}
}

// Applies an update to the conversation's model turn run, the lineage's when the segment has one
func (r *Runtime) withRouteRun(sessionID string, own *RouteRun, update func(run *RouteRun)) bool {
	r.lineageMu.Lock()
	defer r.lineageMu.Unlock()
	run := own
	if key, ok := r.lineageOf.Get(sessionID); ok {
		if lineage, ok := r.lineages.Get(key); ok {
			run = lineage.run
		}
	}
	if run == nil {
		return false
	}
	update(run)
	return true
}

// Segment peak, tally and tools, or the merged lineage ones once its last open segment closes
func (r *Runtime) closeSegment(sessionID string, state *ModelSessionState, tally *SessionTally, usage *ToolUsage) (*ModelSessionState, *SessionTally, *ToolUsage) {
	r.lineageMu.Lock()
	defer r.lineageMu.Unlock()
	key, ok := r.lineageOf.Get(sessionID)
	r.lineageOf.Delete(sessionID)
	var lineage *peakLineage
	if ok {
		lineage, _ = r.lineages.Get(key)
	}
	if lineage == nil {
		return state, tally, usage
	}
	delete(lineage.open, sessionID)
	if state != nil && tally != nil {
		absorbPeak(lineage.peak, state)
		lineage.tally.Absorb(tally)
	}
	lineage.tools.Absorb(usage)
	if len(lineage.open) > 0 {
		return nil, nil, nil
	}
	r.lineages.Delete(key)
	return lineage.peak, lineage.tally, lineage.tools
}

// Closes a rotated to segment that never served a turn here
func (r *Runtime) closeUnseenSegment(ctx context.Context, sessionID string) {
	if sessionID != "" {
		peak, tally, usage := r.closeSegment(sessionID, nil, nil, NewToolUsage())
		r.emitSummaryRows(ctx, nil, peak, tally, usage)
	}
}

// Reports what a never closed conversation holds and detaches its segments
func (r *Runtime) flushLineage(ctx context.Context, key string) {
	r.lineageMu.Lock()
	lineage, ok := r.lineages.Get(key)
	r.lineages.Delete(key)
	if ok {
		for segment := range lineage.open {
			r.lineageOf.Delete(segment)
		}
	}
	r.lineageMu.Unlock()
	if ok {
		r.emitSummaryRows(ctx, nil, lineage.peak, lineage.tally, lineage.tools)
	}
}

// Context peak, session and tool usage rows plus extras, skipping rows without data
func (r *Runtime) emitSummaryRows(ctx context.Context, session *metricsSession, peak *ModelSessionState, tally *SessionTally, usage *ToolUsage, extra ...MarkRow) {
	var rows []MarkRow
	if peak != nil {
		rows = append(rows, MarkRow{ContextPeakMark, peak.ContextPeakFields()})
	}
	if tally != nil {
		rows = append(rows, MarkRow{SessionMark, tally.Fields()})
	}
	if usage != nil {
		rows = append(rows, usage.Rows()...)
	}
	rows = append(rows, extra...)
	kept := rows[:0]
	for _, row := range rows {
		if row.Dims != nil {
			kept = append(kept, row)
		}
	}
	r.emitRows(ctx, session, kept)
}

// One session and context peak row per closed top level conversation, its tool usage and a quick abandon friction
func (r *Runtime) emitSessionSummary(ctx context.Context, session *metricsSession) {
	start := session.startFields
	counted := session.turns > 0 && start != nil && start["entrypoint"] != "delegated"
	var tally *SessionTally
	var state *ModelSessionState
	if counted {
		tally = &SessionTally{
			StartFields: start, Turns: session.turns, FailedTurns: session.failedTurns, LastOutcome: session.lastOutcome,
			FirstTurnNS: session.firstTurnNS, LastTurnNS: session.lastTurnNS, ModelCalls: session.apiCalls,
			ToolCalls: session.toolCallTotal, Replies: session.replies,
		}
		state = session.modelState
	}
	usage := NewToolUsage()
	if counted && Attended(start) {
		usage = session.efficiency.Tools
	}
	usage.Surface = "unknown"
	if surface, ok := start["execution_surface"]; ok {
		usage.Surface = surface
	}
	peak, merged, tools := r.closeSegment(session.sessionID, state, tally, usage)
	var abandoned map[string]string
	if counted && !session.retiring {
		abandoned = session.modelState.QuickAbandonRoute(monotonicNS())
	}
	var friction []MarkRow
	if abandoned != nil && Attended(start) {
		friction = append(friction, MarkRow{ModelFrictionMark, FrictionFields("quick_abandon", abandoned)})
	}
	r.emitSummaryRows(ctx, session, peak, merged, tools, friction...)
}

// Counts the turns the model being left served before a counted model switch
func (r *Runtime) recordSwitchAfter(ctx context.Context, sessionID string) error {
	session := r.session(map[string]any{"session_id": sessionID})
	var own *RouteRun
	if session != nil {
		session.mu.Lock()
		own = session.routeRun
	}
	var route map[string]string
	var turns int
	var taken bool
	r.withRouteRun(sessionID, own, func(run *RouteRun) { route, turns, taken = run.Take() })
	if session != nil {
		session.mu.Unlock()
	}
	if !taken {
		return nil
	}
	return r.recordProcessMark(ctx, ModelSwitchAfterMark, relayData(SwitchAfterFields(route, turns)))
}

// Friction against the session's last primary model, an undo or retry also counting the turns it threw away
func (r *Runtime) recordFriction(ctx context.Context, signal, sessionID string, fallbackRoute map[string]string, turns int) error {
	var session *metricsSession
	if sessionID != "" {
		session = r.session(map[string]any{"session_id": sessionID})
	}
	var route map[string]string
	var wasted []MarkRow
	if session != nil {
		session.mu.Lock()
		route = session.modelState.LastRoute
	}
	if WasteReasons.Has(signal) {
		efficiency := NewSessionEfficiency()
		if session != nil {
			efficiency = session.efficiency
		} else if spent := r.lineageSpent(sessionID); spent != nil {
			efficiency.Spent = spent
		}
		wasted = efficiency.DiscardTurns(signal, turns, orRoute(route, fallbackRoute))
	}
	if session != nil {
		session.mu.Unlock()
	}
	data := FrictionFields(signal, orRoute(route, fallbackRoute))
	if data == nil {
		return nil
	}
	if err := r.recordProcessMark(ctx, ModelFrictionMark, relayData(data)); err != nil {
		return err
	}
	r.emitRows(ctx, nil, wasted)
	return nil
}

// Process level fact with no owning task
func (r *Runtime) recordProcessMark(ctx context.Context, mark string, data map[string]any) error {
	kwargs := map[string]any{"data": data, "metadata": r.eventMetadata()}
	_, err := r.withScopeStack(ctx, func(ctx context.Context) (any, error) { return r.scopeEvent(ctx, mark, kwargs) })
	return err
}

// Emits marks under one commit ticket, flushes and returns how many settled without a store error
func (r *Runtime) recordProcessMarksSaved(ctx context.Context, marks []Mark) (int, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return 0, err
	}
	ticket := hex.EncodeToString(raw)
	metadata := r.eventMetadata()
	metadata[CommitTicketKey] = ticket
	for _, m := range marks {
		kwargs := map[string]any{"data": relayData(m.Data), "metadata": metadata}
		if _, err := r.withScopeStack(ctx, func(ctx context.Context) (any, error) { return r.scopeEvent(ctx, m.Name, kwargs) }); err != nil {
			return 0, err
		}
	}
	if err := r.subscribersFlush(); err != nil {
		return 0, err
	}
	return r.Subscriber.TakeSaved(ticket), nil
}

// Token mark of one auxiliary call
func (r *Runtime) recordAuxiliaryTokens(ctx context.Context, event map[string]any) error {
	model := event["response_model"]
	if !pyrepr.Truthy(model) {
		model = event["model"]
	}
	tokens := ModelTokenFields(event["usage"], model, event["provider"], "auxiliary", event["aux_task"])
	if tokens == nil {
		return nil
	}
	kwargs := map[string]any{"data": tokens, "metadata": r.eventMetadata()}
	_, err := r.withScopeStack(ctx, func(ctx context.Context) (any, error) { return r.scopeEvent(ctx, ModelTokensMark, kwargs) })
	return err
}

// Creates and exports a due package, then sends exported packages off the caller's path
func (r *Runtime) export(ctx context.Context) {
	if _, ok := relayGuardedValue("Icebath shared metrics operation failed", func() ([]string, error) {
		return r.Subscriber.Store.CreateAndExportPackageIfDue(ctx)
	}); ok {
		relaySafe(func() error { r.sendExportedPackages(ctx); return nil })
	}
}

// Records the consent edge, then starts at most one send pass when sending is consented
func (r *Runtime) sendExportedPackages(ctx context.Context) {
	resolved, err := resolvedSendConfig(ctx)
	if err != nil {
		slog.Debug("Unable to read shared-metrics send policy", "error", err)
		return
	}
	relayGuarded("Unable to record a shared-metrics consent transition", func() error {
		return reconcileStoreConsent(ctx, r.Subscriber.Store, resolved.Send)
	})
	if !resolved.Send {
		return
	}
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	if r.sendThread != nil {
		select {
		case <-r.sendThread:
		default:
			return
		}
	}
	done := make(chan struct{})
	r.sendThread = done
	passCtx := context.WithoutCancel(ctx)
	go func() {
		defer close(done)
		r.runSendPass(passCtx, resolved.Endpoint)
	}()
}

// Sends pending packages while the profile still consents to this endpoint
func (r *Runtime) runSendPass(ctx context.Context, endpoint string) {
	stillConsented := func() (bool, error) {
		resolved, err := resolvedSendConfig(ctx)
		if err != nil {
			return false, err
		}
		return resolved.Send && resolved.Endpoint == endpoint, nil
	}
	sender := newMetricsSender(r.Subscriber.Store, endpoint, WithConsentCheck(stillConsented))
	relayGuarded("Shared-metrics send pass failed", func() error { sender.SendPending(ctx); return nil })
}
