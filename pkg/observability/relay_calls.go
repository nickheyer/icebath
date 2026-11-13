package observability

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Opens one privacy safe Relay tool lifecycle under its task
func (r *Runtime) startToolCall(ctx context.Context, env *tools.Env, event map[string]any) error {
	taskID := eventStr(event, "task_id")
	session, task, err := r.taskPair(ctx, env, event, true, true)
	if err != nil || session == nil || task == nil || eventStr(event, "tool_call_id") == "" {
		return err
	}
	if IsConnectorBatch(event) {
		return nil
	}
	identity := toolCallIdentity(event)
	session.mu.Lock()
	defer session.mu.Unlock()
	if !r.admits(session, task, event, false) {
		return nil
	}
	key := toolCallKey{taskID, identity}
	if task.completedToolCallIDs[identity] || session.toolCalls.Has(key) {
		return nil
	}
	task.toolCallIDs[identity] = true
	call, err := r.openToolCall(ctx, task, event)
	if err != nil {
		return err
	}
	session.toolCalls.Set(key, call)
	return nil
}

// Records one bounded approval result without approval text or commands
func (r *Runtime) recordApproval(ctx context.Context, event map[string]any) error {
	session, task := r.approvalTask(ctx, event)
	if session == nil || task == nil {
		return nil
	}
	outcome := ToolApprovalOutcome(event)
	attribution := "unattributed"
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closing || !eventMatchesTaskTurn(task, event) {
		return nil
	}
	if eventStr(event, "tool_call_id") != "" {
		identity := toolCallIdentity(event)
		call, _ := session.toolCalls.Get(toolCallKey{task.taskID, identity})
		if call == nil {
			if key, ok := sole(compatibleToolCallKeys(session, task.taskID, identity)); ok {
				call, _ = session.toolCalls.Get(key)
			}
		}
		if call != nil {
			call.approvalOutcome = outcome
			attribution = "tool_call"
		}
	}
	return r.mark(ctx, session, task, ToolApprovalMark, map[string]any{"attribution": attribution, "outcome": outcome})
}

// Closes and counts one unique privacy safe tool lifecycle
func (r *Runtime) recordToolCall(ctx context.Context, event map[string]any) error {
	taskID := eventStr(event, "task_id")
	session, task, err := r.taskPair(ctx, nil, event, false, true)
	if err != nil || session == nil || task == nil {
		return err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if !r.admits(session, task, event, false) {
		return nil
	}
	var call *toolCall
	if eventStr(event, "tool_call_id") != "" {
		observed := toolCallIdentity(event)
		if r.settleConnectorBatch(ctx, session, task, observed, event) || task.completedToolCallIDs[observed] {
			return nil
		}
		identity := observed
		if c, ok := session.toolCalls.Get(toolCallKey{taskID, identity}); ok {
			call = c
			session.toolCalls.Delete(toolCallKey{taskID, identity})
		} else {
			for done := range task.completedToolCallIDs {
				if identitiesCompatible(done, observed) {
					return nil
				}
			}
			matching := compatibleToolCallKeys(session, taskID, observed)
			if len(matching) > 1 {
				return nil
			}
			if len(matching) == 1 {
				identity = matching[0].identity
				call, _ = session.toolCalls.Get(matching[0])
				session.toolCalls.Delete(matching[0])
			}
		}
		task.completedToolCallIDs[identity] = true
		task.completedToolCallIDs[observed] = true
		task.toolCallIDs[identity] = true
	} else {
		task.unidentifiedToolCalls++
	}
	if call == nil {
		if call, err = r.openToolCall(ctx, task, event); err != nil {
			return err
		}
	}
	if task.cost.UserTurn {
		session.efficiency.Tools.Used[ToolsetMetricName(event["toolset"])] = true
	}
	r.finishToolCall(ctx, task, call, event)
	if chain := session.connectors.Observe(event); chain != nil {
		return r.mark(ctx, session, task, ConnectorChainMark, relayData(chain))
	}
	return nil
}

// Ends a connector batch wrapper's still open entries with its outcome, true when the wrapper was settled
func (r *Runtime) settleConnectorBatch(ctx context.Context, session *metricsSession, task *taskRun, wrapper ToolCallIdentity, event map[string]any) bool {
	if !IsConnectorBatch(event) {
		return false
	}
	var started []toolCallKey
	for _, key := range session.toolCalls.Keys() {
		if key.taskID == task.taskID && IsBatchEntry(key.identity, wrapper) {
			started = append(started, key)
		}
	}
	if len(started) == 0 && !BatchEntryRan(task.completed(), wrapper) {
		return false
	}
	for _, key := range started {
		task.completedToolCallIDs[key.identity] = true
		call, _ := session.toolCalls.Get(key)
		session.toolCalls.Delete(key)
		r.finishToolCall(ctx, task, call, event)
	}
	task.completedToolCallIDs[wrapper] = true
	return true
}

// Emits one allowlisted skill fact without its local identity
func (r *Runtime) recordSkillLifecycle(ctx context.Context, event map[string]any) error {
	mark, fields, ok := SkillLifecycleMark, map[string]string(nil), false
	if pystr.Lower(pystr.Strip(eventStr(event, "action"))) == "loaded" {
		mark = SkillLoadMark
		fields, ok = SkillLoadFields(event)
	} else {
		fields, ok = SkillLifecycleFields(event)
	}
	if !ok {
		return nil
	}
	sessionID, taskID := eventStr(event, "session_id"), eventStr(event, "task_id")
	session, task, err := r.taskPair(ctx, nil, event, false, sessionID == "")
	if err != nil {
		return err
	}
	if session == nil {
		if sessionID == "" || taskID == "" {
			kwargs := map[string]any{"data": relayData(fields), "metadata": r.eventMetadata()}
			_, err := r.withScopeStack(ctx, func(ctx context.Context) (any, error) { return r.scopeEvent(ctx, mark, kwargs) })
			return err
		}
		return nil
	}
	if task == nil {
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if current, _ := session.tasks.Get(task.taskID); !session.closing && current == task && eventMatchesTaskTurn(task, event) {
		return r.mark(ctx, session, task, mark, relayData(fields))
	}
	return nil
}

// Closes one task scope exactly once with bounded terminal fields
func (r *Runtime) finishTask(ctx context.Context, event map[string]any) error {
	session := r.anySession(event)
	if session == nil {
		return nil
	}
	session.mu.Lock()
	finished := !session.closing && r.finishTaskLocked(ctx, session, eventStr(event, "task_id"), event)
	retired := finished && session.retiring && session.tasks.Len() == 0
	session.mu.Unlock()
	if retired {
		return r.closeSession(ctx, map[string]any{"session_id": session.sessionID})
	}
	if finished {
		r.scheduleFlushAndExport(ctx, "Icebath shared-metrics task flush failed")
	}
	return nil
}

// Aborts a session's open work, emits its summary and schedules the export
func (r *Runtime) closeSession(ctx context.Context, event map[string]any) error {
	session := r.session(event)
	if session == nil {
		r.closeUnseenSegment(ctx, eventStr(event, "session_id"))
		return nil
	}
	if !r.abortSession(ctx, session, mergeEvent(event, abortedFlags, map[string]any{"completed": false, "interrupted": false})) {
		return nil
	}
	r.emitSessionSummary(ctx, session)
	r.scheduleFlushAndExport(ctx, fmt.Sprintf("Icebath shared-metrics session %s flush failed", session.sessionID))
	r.sessionsMu.Lock()
	forget(r.sessions, session.sessionID, session)
	r.sessionsMu.Unlock()
	return nil
}

// Exit flush under this profile's scope, a revoked profile dropping its data unexported
func (r *Runtime) Shutdown() {
	ctx := r.ownerScope(r.baseCtx)
	if !relayEnabled(ctx) {
		if r.registered.Load() {
			r.Deactivate()
		}
		return
	}
	r.closeAndExport(ctx)
}

// Closes every session and unseen segment, then flushes, exports and releases the subscriber
func (r *Runtime) closeAndExport(ctx context.Context) {
	r.sessionsMu.Lock()
	r.active = false
	ids := r.sessions.Keys()
	r.sessionsMu.Unlock()
	for _, id := range ids {
		relaySafe(func() error { return r.closeSession(ctx, map[string]any{"session_id": id}) })
	}
	r.lineageMu.Lock()
	unseen := r.lineageOf.Keys()
	r.lineageMu.Unlock()
	for _, id := range unseen {
		relaySafe(func() error { r.closeUnseenSegment(ctx, id); return nil })
	}
	if !r.registered.Load() {
		return
	}
	r.flushAndExport(ctx, "Icebath shared-metrics shutdown flush failed")
	r.deregister()
	r.release()
}

// Deregisters the subscriber and releases managed execution
func (r *Runtime) deregister() {
	relaySafe(func() error {
		if r.relay.Subscribers == nil || r.relay.Subscribers.Deregister == nil {
			return tools.RelayAttributeError("NemoRelaySubscribers", "deregister")
		}
		return r.relay.Subscribers.Deregister(r.subscriberName)
	})
	r.host.ReleaseManagedExecution(r.subscriberName)
	r.registered.Store(false)
}

// Deactivate stops collection without exporting locally aggregated metrics
func (r *Runtime) Deactivate() {
	r.sessionsMu.Lock()
	r.active = false
	r.sessionsMu.Unlock()
	r.Subscriber.Deactivate()
	if r.registered.Load() {
		r.deregister()
	}
	r.sessionsMu.Lock()
	sessions := r.sessions.Values()
	r.sessionsMu.Unlock()
	for _, session := range sessions {
		r.abortSession(r.baseCtx, session, mergeEvent(map[string]any{"session_id": session.sessionID}, abortedFlags))
	}
	r.sessionsMu.Lock()
	r.sessions = pydictClear(r.sessions)
	r.sessionsMu.Unlock()
	r.taskSessionsMu.Lock()
	r.taskSessions, r.turnSessions = pydictClear(r.taskSessions), pydictClear(r.turnSessions)
	r.taskSessionsMu.Unlock()
	r.lineageMu.Lock()
	r.lineages, r.lineageOf = pydictClear(r.lineages), pydictClear(r.lineageOf)
	r.lineageMu.Unlock()
	r.release()
}

// Lets an in flight send finish briefly, then drops the exit hook
func (r *Runtime) release() {
	r.joinSendThread(2 * time.Second)
	if r.unregisterExit != nil {
		r.unregisterExit()
	}
}

// Waits a bounded time for the send pass, pending packages going out next run
func (r *Runtime) joinSendThread(timeout time.Duration) {
	r.sendMu.Lock()
	done := r.sendThread
	r.sendMu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// Session registered under the event's session id
func (r *Runtime) session(event map[string]any) *metricsSession {
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	s, _ := r.sessions.Get(eventStr(event, "session_id"))
	return s
}

// Owner session by task or turn correlation, else by session id
func (r *Runtime) anySession(event map[string]any) *metricsSession {
	if s := r.taskSession(event, true); s != nil {
		return s
	}
	return r.session(event)
}

// Session and task of a task scoped hook, start opening a missing task
func (r *Runtime) taskPair(ctx context.Context, env *tools.Env, event map[string]any, start, allowTaskIDFallback bool) (*metricsSession, *taskRun, error) {
	session := r.taskSession(event, allowTaskIDFallback)
	var task *taskRun
	if session != nil {
		session.mu.Lock()
		task, _ = session.tasks.Get(eventStr(event, "task_id"))
		session.mu.Unlock()
	}
	if task == nil && start {
		_, started, err := r.startTask(ctx, env, event)
		if err != nil {
			return nil, nil, err
		}
		task, session = started, nil
		if started != nil {
			session = r.taskSession(event, false)
		}
	}
	return session, task, nil
}

// Schedules the process wide Relay flush on one coalescing worker per runtime
func (r *Runtime) scheduleFlushAndExport(ctx context.Context, failureMessage string) {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	r.flushPending = true
	if r.flushThread != nil {
		return
	}
	done := make(chan struct{})
	r.flushThread = done
	go r.runFlushWorker(context.WithoutCancel(ctx), failureMessage, done)
}

// Runs flush passes until no trigger landed during the last one
func (r *Runtime) runFlushWorker(ctx context.Context, failureMessage string, done chan struct{}) {
	defer close(done)
	for {
		r.flushMu.Lock()
		if !r.flushPending {
			r.flushThread = nil
			r.flushMu.Unlock()
			return
		}
		r.flushPending = false
		r.flushMu.Unlock()
		r.flushAndExport(ctx, failureMessage)
	}
}

// Flushes the subscriber then exports under this profile's scope, a failed flush skipping the export
func (r *Runtime) flushAndExport(ctx context.Context, failureMessage string) {
	if err := r.subscribersFlush(); err != nil {
		slog.Warn(failureMessage, "error", err)
		return
	}
	r.export(r.ownerScope(ctx))
}

// Marks the session closing and system aborts its open tasks, false when already closing
func (r *Runtime) abortSession(ctx context.Context, session *metricsSession, base map[string]any) bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closing {
		return false
	}
	session.closing = true
	for _, taskID := range session.tasks.Keys() {
		r.finishTaskLocked(ctx, session, taskID, mergeEvent(base, map[string]any{"task_id": taskID}))
	}
	r.endPendingModelCalls(ctx, session, base)
	return true
}

// Owner session by session and turn, then session and task, then a unique task id
func (r *Runtime) taskSession(event map[string]any, allowTaskIDFallback bool) *metricsSession {
	sessionID, taskID := eventStr(event, "session_id"), eventStr(event, "task_id")
	if taskID == "" {
		return nil
	}
	r.taskSessionsMu.Lock()
	defer r.taskSessionsMu.Unlock()
	var owner *metricsSession
	if pair, ok := sessionPair(event, "turn_id"); ok {
		owner, _ = r.turnSessions.Get(pair)
	}
	if owner == nil && sessionID != "" {
		owner, _ = r.taskSessions.Get(taskKey{sessionID, taskID})
	}
	if owner != nil || !allowTaskIDFallback {
		return owner
	}
	var candidates []*metricsSession
	for key, s := range r.taskSessions.All() {
		if key.taskID == taskID {
			candidates = append(candidates, s)
		}
	}
	owner, _ = sole(candidates)
	return owner
}

// Remembers the event's turn on the task and indexes the session by it
func (r *Runtime) rememberTurn(session *metricsSession, task *taskRun, event map[string]any) {
	turnID := eventStr(event, "turn_id")
	if turnID == "" {
		return
	}
	if !task.hasTurn(turnID) {
		task.turnIDs = append(task.turnIDs, turnID)
	}
	r.taskSessionsMu.Lock()
	r.turnSessions.Set(taskKey{session.sessionID, turnID}, session)
	r.taskSessionsMu.Unlock()
}

// Provider local tool call identity without exporting its ids
func toolCallIdentity(event map[string]any) ToolCallIdentity {
	return ToolCallIdentity{eventStr(event, "api_request_id"), eventStr(event, "turn_id"), eventStr(event, "tool_call_id")}
}

// Rejects delayed hooks from a prior run that reused the task id
func eventMatchesTaskTurn(task *taskRun, event map[string]any) bool {
	turnID := eventStr(event, "turn_id")
	if turnID == "" {
		return true
	}
	return !task.retiredTurnIDs[turnID] && (len(task.turnIDs) == 0 || task.hasTurn(turnID))
}

// Whether the event may act on the task, current also requiring the session's live run, the caller holding its lock
func (r *Runtime) admits(session *metricsSession, task *taskRun, event map[string]any, current bool) bool {
	if session.closing || !eventMatchesTaskTurn(task, event) {
		return false
	}
	if live, _ := session.tasks.Get(task.taskID); current && live != task {
		return false
	}
	r.rememberTurn(session, task, event)
	return true
}

// Resolves approval correlation without guessing across ambiguous turns
func (r *Runtime) approvalTask(ctx context.Context, event map[string]any) (*metricsSession, *taskRun) {
	if active := tools.ActiveRelayTurn(ctx, nil); active != nil && active.Lease != nil {
		session, task, _ := r.taskPair(ctx, nil, mergeEvent(event, map[string]any{"session_id": active.Lease.SessionID, "task_id": active.TaskID}), false, false)
		if task != nil {
			return session, task
		}
	}
	if session, task, _ := r.taskPair(ctx, nil, event, false, false); task != nil {
		return session, task
	}
	turnID := eventStr(event, "turn_id")
	var session *metricsSession
	if turnID != "" {
		r.taskSessionsMu.Lock()
		var candidates []*metricsSession
		for key, candidate := range r.turnSessions.All() {
			if key.taskID != turnID {
				continue
			}
			r.sessionsMu.Lock()
			owner, _ := r.sessions.Get(key.sessionID)
			r.sessionsMu.Unlock()
			if owner == candidate {
				candidates = append(candidates, candidate)
			}
		}
		r.taskSessionsMu.Unlock()
		session, _ = sole(candidates)
	}
	if session == nil {
		return nil, nil
	}
	session.mu.Lock()
	var matches []*taskRun
	for _, t := range session.tasks.Values() {
		if t.hasTurn(turnID) {
			matches = append(matches, t)
		}
	}
	session.mu.Unlock()
	task, ok := sole(matches)
	if !ok {
		return nil, nil
	}
	return session, task
}

// Opens one Relay tool call under the task scope
func (r *Runtime) openToolCall(ctx context.Context, task *taskRun, event map[string]any) (*toolCall, error) {
	if r.relay.Tools == nil || r.relay.Tools.Call == nil {
		return nil, tools.RelayAttributeError("NemoRelayTools", "call")
	}
	kwargs := map[string]any{"handle": task.handle, "metadata": r.eventMetadata()}
	handle, err := r.runInTask(ctx, task, func(ctx context.Context) (any, error) {
		return r.relay.Tools.Call(ctx, ToolCallScope, map[string]any{}, kwargs)
	})
	if err != nil {
		return nil, err
	}
	return &toolCall{handle: handle, category: ToolCategory(event), toolName: ToolMetricName(event), startedNS: monotonicNS(), approvalOutcome: "not_required"}, nil
}

// Ends one tool call with its bounded terminal fields
func (r *Runtime) finishToolCall(ctx context.Context, task *taskRun, call *toolCall, event map[string]any) {
	approval, name := call.approvalOutcome, call.toolName
	fields := ToolTerminalFields(event, ToolTerminalOptions{
		Category: call.category, ApprovalOutcome: &approval, FallbackDurationMS: relayElapsedMS(call.startedNS), ToolName: &name,
	})
	relayGuarded("Icebath shared-metrics tool call close failed", func() error {
		if r.relay.Tools == nil || r.relay.Tools.CallEnd == nil {
			return tools.RelayAttributeError("NemoRelayTools", "call_end")
		}
		if r.relay.ToolExecutionResult == nil {
			return tools.RelayAttributeError("nemo_relay", "ToolExecutionResult")
		}
		result, err := r.relay.ToolExecutionResult(relayData(fields))
		if err != nil {
			return err
		}
		kwargs := map[string]any{"metadata": r.eventMetadata()}
		_, err = r.runInTask(ctx, task, func(ctx context.Context) (any, error) {
			return nil, r.relay.Tools.CallEnd(ctx, call.handle, result, kwargs)
		})
		return err
	})
}

// Ends the task's open tool calls with the status its terminal state implies
func (r *Runtime) endPendingToolCalls(ctx context.Context, session *metricsSession, task *taskRun, event map[string]any) {
	outcome, _, _ := TaskTerminalState(event)
	status := map[string]string{"cancelled": "cancelled", "timed_out": "timeout"}[outcome]
	if status == "" {
		status = "error"
	}
	for _, key := range session.toolCalls.Keys() {
		if key.taskID != task.taskID {
			continue
		}
		call, _ := session.toolCalls.Get(key)
		session.toolCalls.Delete(key)
		r.finishToolCall(ctx, task, call, mergeEvent(event, map[string]any{"status": status}))
	}
}

// Closes one model call with its route, outcome and last classified error
func (r *Runtime) finishModelCall(ctx context.Context, session *metricsSession, key modelCallKey, outcome string) {
	call, ok := session.modelCalls.Get(key)
	if !ok {
		return
	}
	session.modelCalls.Delete(key)
	errorClass := call.errorClass
	if outcome == "failed" && errorClass == "none" {
		errorClass = "unknown"
	}
	fields := ModelRouteFields(relayData(call.fields), "primary", outcome, errorClass, call.ttftBucket)
	task, _ := session.tasks.Get(call.taskID)
	relayGuarded("Icebath shared-metrics model call close failed", func() error {
		if r.relay.LLM == nil || r.relay.LLM.CallEnd == nil {
			return tools.RelayAttributeError("NemoRelayLLM", "call_end")
		}
		kwargs := map[string]any{"metadata": r.eventMetadata()}
		_, err := r.runScoped(ctx, session, task, func(ctx context.Context) (any, error) {
			return nil, r.relay.LLM.CallEnd(ctx, call.handle, relayData(fields), kwargs)
		})
		return err
	})
}

// Closes calls that never saw a successful response as failed or cancelled
func (r *Runtime) endPendingModelCalls(ctx context.Context, session *metricsSession, event map[string]any) {
	taskID := eventStr(event, "task_id")
	outcome := "failed"
	if state, _, _ := TaskTerminalState(event); state == "cancelled" {
		outcome = "cancelled"
	}
	var pending []modelCallKey
	for key, call := range session.modelCalls.All() {
		if taskID == "" || call.taskID == taskID {
			pending = append(pending, key)
		}
	}
	for _, key := range pending {
		r.finishModelCall(ctx, session, key, outcome)
	}
}

// Task and request key of an open call, a taskless event matching by request alone
func existingModelCallKey(session *metricsSession, event map[string]any) (modelCallKey, bool) {
	requestID := eventStr(event, "api_request_id")
	if requestID == "" {
		return modelCallKey{}, false
	}
	key := modelCallKey{eventStr(event, "task_id"), requestID}
	if session.modelCalls.Has(key) || key.taskID != "" {
		return key, session.modelCalls.Has(key)
	}
	var candidates []modelCallKey
	for _, candidate := range session.modelCalls.Keys() {
		if candidate.requestID == requestID {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}
	return modelCallKey{}, false
}

// Finishes one task with terminal fields and retires its turns, the caller holding the session lock
func (r *Runtime) finishTaskLocked(ctx context.Context, session *metricsSession, taskID string, event map[string]any) bool {
	task, _ := session.tasks.Get(taskID)
	if task == nil {
		return false
	}
	defer func() {
		session.tasks.Delete(taskID)
		session.retiredTurnIDs = append(session.retiredTurnIDs, task.turnIDs...)
		if n := len(session.retiredTurnIDs); n > retiredTurnsKept {
			session.retiredTurnIDs = session.retiredTurnIDs[n-retiredTurnsKept:]
		}
		r.taskSessionsMu.Lock()
		forget(r.taskSessions, taskKey{session.sessionID, taskID}, session)
		for _, turnID := range task.turnIDs {
			forget(r.turnSessions, taskKey{session.sessionID, turnID}, session)
		}
		r.taskSessionsMu.Unlock()
	}()
	r.endPendingToolCalls(ctx, session, task, event)
	r.endPendingModelCalls(ctx, session, mergeEvent(event, map[string]any{"task_id": taskID}))
	fields := TaskTerminalFields(ctx, mergeEvent(relayData(task.startFields), event), relayElapsedMS(task.startedNS), len(task.modelCallIDs), task.toolCallCount(), task.retryCount)
	if task.cost.UserTurn {
		countSessionTurn(session, task, fields["outcome"])
	}
	if !session.closing {
		r.observeModelTurn(ctx, session, task, fields)
	}
	output, metadata := relayData(fields), r.eventMetadata()
	popped, ok := relayGuardedValue("Icebath shared-metrics task close failed", func() (any, error) {
		return r.runInTask(ctx, task, func(ctx context.Context) (any, error) {
			return tools.PopRelayScopeIfTop(ctx, r.relay, task.handle, output, metadata)
		})
	})
	if ok && popped == false {
		slog.Debug(fmt.Sprintf("Left shared-metrics task scope %s under a concurrent turn's scope; session close drains it", taskID))
	}
	return true
}

// Folds one user turn into the session aggregation
func countSessionTurn(session *metricsSession, task *taskRun, outcome string) {
	if session.startFields == nil {
		session.startFields = maps.Clone(task.startFields)
		session.firstTurnNS = task.startedNS
	}
	session.turns++
	if outcome == "failed" {
		session.failedTurns++
	}
	session.apiCalls += len(task.modelCallIDs)
	session.toolCallTotal += task.toolCallCount()
	session.lastOutcome = outcome
	session.lastTurnNS = monotonicNS()
}
