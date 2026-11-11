package gateway

import (
	"context"
	"reflect"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Stop a restart or shutdown latched before the generation's managed worker existed, with its cause
type stopCause struct {
	generation int64
	reason     string
}

// Steer texts accepted before the generation adopted its agent
type latchedSteers struct {
	generation int64
	texts      []string
}

// Runner whose running slots hold a turn's own agent or the pending sentinel of a claim
type runningAgentsRunner interface {
	RunningAgent(key string) (agent any, pending bool)
}

// Agent that clears a stop delivered for an earlier generation
type interruptClearer interface {
	ClearInterrupt()
}

type hardInterruptWithReason interface {
	HardInterrupt(message, toolReason string)
}

type hardInterruptMessageOnly interface {
	HardInterrupt(message string)
}

type interruptWithReason interface {
	Interrupt(message, toolReason string)
}

type interruptMessageOnly interface {
	Interrupt(message string)
}

// Legacy interrupt taking no message, a managed worker's cooperative stop
type interruptNoArgs interface {
	Interrupt() error
}

// Hard stop of any agent value, falling back to the legacy interrupt and false when neither exists
func requestHardInterrupt(agent any) bool {
	if agent == nil {
		return false
	}
	if value := reflect.ValueOf(agent); value.Kind() == reflect.Pointer && value.IsNil() {
		return false
	}
	switch target := agent.(type) {
	case hardInterruptWithReason:
		target.HardInterrupt("", "")
	case hardInterruptMessageOnly:
		target.HardInterrupt("")
	case interruptWithReason:
		target.Interrupt("", "")
	case interruptMessageOnly:
		target.Interrupt("")
	case interruptNoArgs:
		_ = target.Interrupt()
	default:
		return false
	}
	return true
}

// Transport a principal's connection publishes events through, nil when it registered none
func (a *SessionAuthority) transportOf(actor Principal) tuigateway.Transport {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Events[actor.TransportID]
}

// Joins a session's event stream and snapshots it, with its transcript unless the viewer pages it itself
func (a *SessionAuthority) Attach(ctx context.Context, actor Principal, ref SessionRef, history bool, replayFrom *ReplayCursor) (SubscriptionSnapshot, error) {
	if err := a.Authorize(ctx, actor, ref, "session:read"); err != nil {
		return SubscriptionSnapshot{}, err
	}
	var prepared *preparedHistory
	if history {
		var err error
		if prepared, err = prepareHistory(ctx, a, ref); err != nil {
			return SubscriptionSnapshot{}, err
		}
	}
	if err := keepFromDiscard(ctx, a, ref.SessionID); err != nil {
		return SubscriptionSnapshot{}, err
	}
	if err := a.Authorize(ctx, actor, ref, "session:read"); err != nil {
		return SubscriptionSnapshot{}, err
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return SubscriptionSnapshot{}, &pyKeyError{ref.SessionID}
	}
	live.EventStream.Lock.Lock()
	snapshot, err := a.joinStream(ctx, actor, ref, live, history, replayFrom, prepared)
	live.EventStream.Lock.Unlock()
	if err != nil {
		return SubscriptionSnapshot{}, err
	}
	if err := cancelPersistedClose(ctx, a, ref.SessionID); err != nil {
		return SubscriptionSnapshot{}, err
	}
	if err := reaskOnAttach(ctx, a, ref, live); err != nil {
		return SubscriptionSnapshot{}, err
	}
	return snapshot, nil
}

// Adds the viewer to the stream under its lock and builds the snapshot it resumes from
func (a *SessionAuthority) joinStream(ctx context.Context, actor Principal, ref SessionRef, live *LiveSession, history bool, replayFrom *ReplayCursor, prepared *preparedHistory) (SubscriptionSnapshot, error) {
	handle, err := a.handle(ctx, ref)
	if err != nil {
		return SubscriptionSnapshot{}, err
	}
	a.mu.Lock()
	var held []string
	for key, member := range live.Subscribers {
		if samePrincipal(member, actor) {
			held = append(held, key)
		}
	}
	sort.Strings(held)
	subscription := uuid4HexString()
	if len(held) > 0 && held[0] != "" {
		subscription = held[0]
	}
	if live.Subscribers == nil {
		live.Subscribers = map[string]Principal{}
	}
	live.Subscribers[subscription] = actor
	delete(a.pendingCloses, ref.SessionID)
	a.mu.Unlock()
	cancelOrphanClose(a, ref.SessionID)
	allowForSession(a.ProfileID, ref.SessionID)
	if transport := a.transportOf(actor); transport != nil {
		if replayFrom != nil && !live.EventStream.Fanout.Contains(transport) {
			live.EventStream.CatchUp(transport, replayFrom.Epoch, int(replayFrom.Sequence))
		}
		live.EventStream.Fanout.Attach(transport)
		live.EventStream.OnOverflow = func(dropped tuigateway.Transport) { a.retireOverflowed(ref.SessionID, dropped) }
	}
	var activeGeneration *int64
	if handle.ExecutionState == "running" {
		generation := handle.ExecutionGeneration
		activeGeneration = &generation
	}
	prompts := pendingControlsSnapshot(live, ref.SessionID, activeGeneration)
	epoch, sequence := live.EventStream.Watermark()
	snapshot := SubscriptionSnapshot{SubscriptionID: subscription, Handle: handle, ReplayEpoch: epoch, LastSequence: int64(sequence),
		History: []map[string]any{}, Pending: []PendingAdmission{}, Prompts: prompts}
	if history {
		if snapshot.History, err = unlessDeleted(ctx, a, ref, func() ([]map[string]any, error) { return localHistory(ctx, a, ref, prepared) }); err != nil {
			return SubscriptionSnapshot{}, err
		}
	} else {
		target, err := unlessDeleted(ctx, a, ref, func() (string, error) { return transcriptTarget(ctx, a, ref) })
		if err != nil {
			return SubscriptionSnapshot{}, err
		}
		snapshot.TranscriptTarget = &target
	}
	rows, err := pendingRows(ctx, a, ref)
	if err != nil {
		return SubscriptionSnapshot{}, err
	}
	for _, row := range rows {
		receipt, err := a.pendingReceipt(ctx, row)
		if err != nil {
			return SubscriptionSnapshot{}, err
		}
		snapshot.Pending = append(snapshot.Pending, receipt)
	}
	return snapshot, nil
}

// Ends the subscriptions of a peer whose backlog the fanout dropped
func (a *SessionAuthority) retireOverflowed(sessionID string, transport tuigateway.Transport) {
	a.mu.Lock()
	defer a.mu.Unlock()
	live := a.Sessions[sessionID]
	if live == nil {
		return
	}
	for subscription, member := range live.Subscribers {
		if a.Events[member.TransportID] == transport {
			delete(live.Subscribers, subscription)
		}
	}
}

// Ends a viewer's subscription, the last viewer leaving closing or handing back its session
func (a *SessionAuthority) Detach(ctx context.Context, actor Principal, subscriptionID string, endReason string, orphaned bool) error {
	a.mu.Lock()
	var sessionID string
	var live *LiveSession
	for sid, candidate := range a.Sessions {
		if _, ok := candidate.Subscribers[subscriptionID]; ok {
			sessionID, live = sid, candidate
			break
		}
	}
	if live == nil {
		a.mu.Unlock()
		return runtimeRefusal("not_found")
	}
	if !samePrincipal(live.Subscribers[subscriptionID], actor) {
		a.mu.Unlock()
		return runtimeRefusal("permission_denied")
	}
	delete(live.Subscribers, subscriptionID)
	last := len(live.Subscribers) == 0
	if last {
		live.Detaching++
	}
	a.mu.Unlock()
	if transport := a.transportOf(actor); transport != nil {
		live.EventStream.Fanout.Detach(transport)
	}
	if !last {
		return nil
	}
	releaseEditorMCP(context.WithoutCancel(ctx), a, []string{sessionID})
	a.mu.Lock()
	live.Detaching--
	a.mu.Unlock()
	if endReason != "" {
		return closeViewerSession(ctx, a, sessionID, endReason)
	}
	if _, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
		ended, err := endIdleACPSession(taskCtx, a, sessionID)
		return ended, err
	}, func(taskCtx context.Context, ended any) (any, error) {
		reopened, err := reopenIfReattached(taskCtx, a, sessionID, ended.(bool))
		return reopened, err
	}, false, nil); err != nil {
		return err
	}
	if orphaned {
		return armDetachedOrphanClose(ctx, a, sessionID)
	}
	return nil
}

// Operator acknowledgement that an unknown turn will not finish, resuming the paused FIFO behind it
func (a *SessionAuthority) ResolveUnknown(ctx context.Context, actor Principal, ref SessionRef, admissionID string, generation int64) (AdmissionReceipt, error) {
	if err := a.Authorize(ctx, actor, ref, "session:control"); err != nil {
		return AdmissionReceipt{}, err
	}
	if _, err := a.Receipt(ctx, actor, ref, admissionID); err != nil {
		return AdmissionReceipt{}, err
	}
	return a.ResolveUnknownAsOwner(ctx, ref, admissionID, generation, nil)
}

// Resolves an unknown turn past authorization, committing the result this owner captured when it has one
func (a *SessionAuthority) ResolveUnknownAsOwner(ctx context.Context, ref SessionRef, admissionID string, generation int64, notice any) (AdmissionReceipt, error) {
	a.mu.Lock()
	captured := a.PendingResults[admissionID]
	a.mu.Unlock()
	before, err := a.DB.GetSessionAdmission(ctx, admissionID)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	var prepared *state.CapturedResult
	if captured != nil && before != nil && rowString(before, "status") == AdmissionUnknown {
		row, err := a.DB.GetSessionAdmission(ctx, admissionID)
		if err != nil {
			return AdmissionReceipt{}, err
		}
		if prepared, err = prepareResult(row, capturedResponse(captured), "completed", captured); err != nil {
			return AdmissionReceipt{}, err
		}
	}
	result, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
		return a.DB.ResolveUnknownSessionInput(taskCtx, a.Epoch, admissionID, generation,
			func(writeCtx context.Context, conn state.SQLiteConn, lost *pyjson.Object) error {
				closing := notice
				if owner := rowInt(lost, "owner_epoch"); prepared != nil && owner != nil && *owner == a.Epoch {
					closing = nil
				}
				_, err := closeDiscardedTurn(writeCtx, a.DB, conn, lost, closing, nil, nil)
				return err
			}, prepared)
	}, func(taskCtx context.Context, written any) (any, error) {
		return a.resolvedUnknown(taskCtx, ref, admissionID, prepared, captured, written.(*pyjson.Object))
	}, false, nil)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	return result.(AdmissionReceipt), nil
}

// Final response a captured execution result carries, empty when it carries none
func capturedResponse(captured *pyjson.Object) string {
	result, _ := captured.Get("result")
	obj, _ := result.(*pyjson.Object)
	if obj == nil {
		return ""
	}
	text, _ := obj.Get("final_response")
	if !pyTruthy(text) {
		return ""
	}
	response, _ := text.(string)
	return response
}

// Publishes and delivers a resolved unknown turn's committed answer, then lets its successor run
func (a *SessionAuthority) resolvedUnknown(ctx context.Context, ref SessionRef, admissionID string, prepared *state.CapturedResult, captured, row *pyjson.Object) (AdmissionReceipt, error) {
	a.mu.Lock()
	delete(a.PendingResults, admissionID)
	a.mu.Unlock()
	owner := rowInt(row, "owner_epoch")
	owed := prepared != nil && owner != nil && *owner == a.Epoch
	if owed {
		live, ok := a.live(ref.SessionID)
		if !ok {
			return AdmissionReceipt{}, &pyKeyError{ref.SessionID}
		}
		live.EventStream.Lock.Lock()
		var err error
		if stamped, _ := live.EventStream.Execution.Get("admission_id"); stamped != admissionID {
			err = publishCompletion(ctx, a, live, ref, row, row, capturedResponse(captured), captured)
		}
		live.EventStream.Lock.Unlock()
		if err != nil {
			return AdmissionReceipt{}, err
		}
	}
	if err := releaseAdmissionMedia(ctx, a.DB, admissionID); err != nil {
		return AdmissionReceipt{}, err
	}
	if err := deliverResolved(ctx, a, admissionID, owed); err != nil {
		return AdmissionReceipt{}, err
	}
	if err := a.publishPending(ctx, ref); err != nil {
		return AdmissionReceipt{}, err
	}
	if err := scheduleSession(ctx, a, ref); err != nil {
		return AdmissionReceipt{}, err
	}
	return a.receiptOf(row), nil
}

// Stops the running turn of a generation, latching the stop for the agent the turn has yet to adopt
func (a *SessionAuthority) Interrupt(ctx context.Context, actor Principal, ref SessionRef, generation int64) (SessionHandle, error) {
	if err := a.Authorize(ctx, actor, ref, "session:control"); err != nil {
		return SessionHandle{}, err
	}
	handle, err := a.handle(ctx, ref)
	if err != nil {
		return SessionHandle{}, err
	}
	if handle.ExecutionGeneration != generation {
		return SessionHandle{}, runtimeRefusal("stale_generation")
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return SessionHandle{}, &pyKeyError{ref.SessionID}
	}
	var agent any
	if handle.ExecutionState == "running" {
		live.EventStream.Lock.Lock()
		agent, err = a.stopTarget(ref, live, generation)
		live.EventStream.Lock.Unlock()
		if err != nil {
			return SessionHandle{}, err
		}
	}
	if agent != nil {
		requestHardInterrupt(agent)
	}
	return a.handle(ctx, ref)
}

// Agent a stop of the generation reaches under the stream lock, the stop latched when the turn adopted none yet
func (a *SessionAuthority) stopTarget(ref SessionRef, live *LiveSession, generation int64) (any, error) {
	a.mu.Lock()
	adopted, hasAdopted := a.adopted[ref.SessionID]
	executing, isExecuting := a.executing[ref.SessionID]
	a.mu.Unlock()
	agent := adopted.agent
	if !hasAdopted || adopted.generation != generation {
		stamped, _ := live.EventStream.Execution.Get("execution_generation")
		stampedGeneration := rowInt(pyjson.ObjectOf("g", stamped), "g")
		returned := stampedGeneration != nil && *stampedGeneration == generation && !(isExecuting && executing == generation)
		agent = nil
		if !returned {
			var err error
			if agent, err = a.agent(ref); err != nil {
				return nil, err
			}
			own, err := a.runsOwnAgent(ref)
			if err != nil {
				return nil, err
			}
			if !own {
				a.mu.Lock()
				if a.pendingStops == nil {
					a.pendingStops = map[string]int64{}
				}
				a.pendingStops[ref.SessionID] = generation
				a.mu.Unlock()
			}
		}
	}
	if agent != nil {
		a.mu.Lock()
		if a.deliveredStops == nil {
			a.deliveredStops = map[string]int64{}
		}
		a.deliveredStops[ref.SessionID] = generation
		a.mu.Unlock()
	}
	return agent, nil
}

// True when the route's running slot holds a real agent and not the claim's pending sentinel
func (a *SessionAuthority) runsOwnAgent(ref SessionRef) (bool, error) {
	live, ok := a.live(ref.SessionID)
	if !ok {
		return false, &pyKeyError{ref.SessionID}
	}
	running, ok := a.Runner.(runningAgentsRunner)
	if !ok {
		return false, nil
	}
	agent, pending := running.RunningAgent(runningKey(a.Runner, live.Route, ref.SessionID))
	return agent != nil && !pending, nil
}

// Agent a steer for the running generation reaches, the text latched when the turn adopted none yet
func (a *SessionAuthority) SteerAgent(ctx context.Context, ref SessionRef, generation int64, text *string) (any, bool, error) {
	a.mu.Lock()
	adopted, hasAdopted := a.adopted[ref.SessionID]
	a.mu.Unlock()
	agent := adopted.agent
	if !hasAdopted || adopted.generation != generation {
		agent = nil
		own, err := a.runsOwnAgent(ref)
		if err != nil {
			return nil, false, err
		}
		if own {
			if agent, err = a.agent(ref); err != nil {
				return nil, false, err
			}
		}
	}
	if agent != nil || text == nil {
		return agent, false, nil
	}
	if adopts, err := adoptsOwnerAgent(ctx, a, ref); err != nil || !adopts {
		return agent, false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	latched := a.pendingSteers[ref.SessionID]
	texts := []string{*text}
	if latched.generation == generation && latched.texts != nil {
		texts = append(append([]string(nil), latched.texts...), *text)
	}
	if a.pendingSteers == nil {
		a.pendingSteers = map[string]latchedSteers{}
	}
	a.pendingSteers[ref.SessionID] = latchedSteers{generation, texts}
	return nil, true, nil
}

// Installs the turn's agent for the running claim, firing a stop and the steers latched for that generation
func (a *SessionAuthority) AdoptAgent(sessionID string, generation int64, agent any) {
	if live, ok := a.live(sessionID); ok {
		live.EventStream.Lock.Lock()
		defer live.EventStream.Lock.Unlock()
	}
	a.mu.Lock()
	if a.adopted == nil {
		a.adopted = map[string]adoptedAgent{}
	}
	a.adopted[sessionID] = adoptedAgent{generation, agent}
	delivered, wasDelivered := a.deliveredStops[sessionID]
	delete(a.deliveredStops, sessionID)
	stop, hasStop := a.pendingStops[sessionID]
	fire := hasStop && stop == generation
	if fire {
		delete(a.pendingStops, sessionID)
	}
	latched, hasSteers := a.pendingSteers[sessionID]
	delete(a.pendingSteers, sessionID)
	a.mu.Unlock()
	if clearer, ok := agent.(interruptClearer); ok && wasDelivered && delivered != generation {
		clearer.ClearInterrupt()
	}
	if fire {
		requestHardInterrupt(agent)
	}
	if hasSteers && latched.generation == generation {
		if receiver, ok := agent.(steerReceiver); ok {
			for _, text := range latched.texts {
				receiver.Steer(text)
			}
		}
	}
}

// Refuses a prompt or response of a generation that is no longer the running one
func (a *SessionAuthority) checkApprovalGeneration(ctx context.Context, sessionID string, generation int64) error {
	handle, err := a.handle(ctx, SessionRef{a.ProfileID, sessionID})
	if err != nil {
		return err
	}
	if handle.ExecutionGeneration != generation || handle.ExecutionState != "running" {
		return runtimeRefusal("stale_generation")
	}
	return nil
}

// Publishes a worker callback's event only while its exact claim still executes
func (a *SessionAuthority) PublishExecution(sessionID string, generation int64, eventType string, payload any) (bool, error) {
	live, ok := a.live(sessionID)
	if !ok {
		return false, &pyKeyError{sessionID}
	}
	live.EventStream.Lock.Lock()
	defer live.EventStream.Lock.Unlock()
	a.mu.Lock()
	executing, isExecuting := a.executing[sessionID]
	a.mu.Unlock()
	if !isExecuting || executing != generation {
		return false, nil
	}
	delivered := live.EventStream.Publish(sessionID, payload, eventType, nil)
	observed, err := publishAPIEvent(a, sessionID, eventType, payload)
	if err != nil {
		return false, err
	}
	return delivered || observed, nil
}

// Registers an approval prompt of the running generation with the session's pending controls
func (a *SessionAuthority) RegisterApproval(ctx context.Context, sessionID string, generation int64, route string, data map[string]any, timeoutS *float64) error {
	live, ok := a.live(sessionID)
	if !ok {
		return &pyKeyError{sessionID}
	}
	live.EventStream.Lock.Lock()
	defer live.EventStream.Lock.Unlock()
	if err := a.checkApprovalGeneration(ctx, sessionID, generation); err != nil {
		return err
	}
	return pendingControlsRegister(ctx, live, sessionID, route, generation, data, timeoutS)
}

// Registers a clarify prompt of the running generation with the session's pending controls
func (a *SessionAuthority) RegisterClarify(ctx context.Context, sessionID string, generation int64, entry *tools.ClarifyEntry, timeoutS *float64) error {
	live, ok := a.live(sessionID)
	if !ok {
		return &pyKeyError{sessionID}
	}
	live.EventStream.Lock.Lock()
	defer live.EventStream.Lock.Unlock()
	if err := a.checkApprovalGeneration(ctx, sessionID, generation); err != nil {
		return err
	}
	return pendingControlsRegisterClarify(ctx, live, sessionID, generation, entry, timeoutS)
}

// Answers an approval or clarify prompt of the running generation for a subscribed viewer
func (a *SessionAuthority) Respond(ctx context.Context, actor Principal, ref SessionRef, generation any, promptID any, response any, kind string) (any, error) {
	capability, ok := map[string]string{"approval": "session:approve", "clarify": "session:respond"}[kind]
	if !ok {
		return nil, runtimeRefusal("invalid_params")
	}
	if err := a.Authorize(ctx, actor, ref, capability); err != nil {
		return nil, err
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, &pyKeyError{ref.SessionID}
	}
	live.EventStream.Lock.Lock()
	defer live.EventStream.Lock.Unlock()
	a.mu.Lock()
	subscribed := false
	for _, member := range live.Subscribers {
		if samePrincipal(member, actor) {
			subscribed = true
		}
	}
	a.mu.Unlock()
	if !subscribed {
		return nil, runtimeRefusal("permission_denied")
	}
	var exact int64
	switch n := generation.(type) {
	case int:
		exact = int64(n)
	case int64:
		exact = n
	default:
		return nil, runtimeRefusal("stale_generation")
	}
	if err := a.checkApprovalGeneration(ctx, ref.SessionID, exact); err != nil {
		return nil, err
	}
	id, isText := promptID.(string)
	if !isText || id == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	return pendingControlsRespond(live, ref.SessionID, exact, id, response, kind)
}
