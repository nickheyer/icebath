package gatewayapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// State of an admitted run's background task, the request scope captured because the task outlives it
type runLaunch struct {
	owner                  *APIServerAdapter
	runID                  string
	queue                  *runStream
	sessionID              string
	gatewaySessionKey      string
	declaredSelected       bool
	userMessage            any
	conversationHistory    []any
	sessionHistoryDelivery bool
	agentOptions           agentOptions
	requestProfile         string
	turnAuthor             *pyjson.Object
	admission              any
	walkedFrom             string
}

// Approval session key of a run, isolated per run
func (r *runLaunch) approvalSessionKey() string { return r.runID }

// Queues an event only while this run still owns its live transport
func (r *runLaunch) putEvent(event *pyjson.Object) {
	r.owner.mu.Lock()
	current := r.owner.runStreams[r.runID]
	r.owner.mu.Unlock()
	if current == r.queue {
		r.queue.putNowait(event)
	}
}

// Route rows of the structured runs API
func (a *APIServerAdapter) runHTTPRoutes() []httpRoute {
	return []httpRoute{
		{"POST", "/v1/runs", a.admitAPIAgentRequest(a.handleRuns)}, {"GET", "/v1/runs/{run_id}", a.handleGetRun},
		{"GET", "/v1/runs/{run_id}/events", a.handleRunEvents},
		{"POST", "/v1/runs/{run_id}/approval", a.handleRunApproval},
		{"POST", "/v1/runs/{run_id}/clarify", a.handleRunClarify},
		{"POST", "/v1/runs/{run_id}/steer", a.handleSteerRun},
		{"POST", "/v1/runs/{run_id}/resolve-unknown", a.handleResolveUnknownRun},
		{"POST", "/v1/runs/{run_id}/stop", a.handleStopRun},
	}
}

// Whether an Idempotency-Key is one to 255 visible ASCII characters or absent
func validIdempotencyKey(key string) bool {
	if len(key) > 255 {
		return false
	}
	for _, ch := range key {
		if ch < 33 || ch > 126 {
			return false
		}
	}
	return true
}

// Starts an agent run and returns its id at once, a keyed retry replaying the admitted run
func (a *APIServerAdapter) handleRuns(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	gatewaySessionKey, keyErr := a.parseSessionKeyHeader(request)
	if keyErr != nil {
		return keyErr, nil
	}
	raw, err := requestJSON(request)
	if err != nil {
		return jsonError("Invalid JSON", "", http.StatusBadRequest), nil
	}
	normalized, roomErr, err := a.normalizeRoomDispatch(ctx, request, raw)
	if err != nil {
		return nil, err
	}
	if roomErr != nil {
		return roomErr, nil
	}
	body, _ := normalized.(*pyjson.Object)
	if body == nil {
		body = pyjson.NewObject()
	}
	roomDispatch, _ := body.GetOr("hosted_room_dispatch", nil).(*pyjson.Object)
	roomPolicy, _ := body.GetOr("_room_execution_policy", nil).(*pyjson.Object)
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if !validIdempotencyKey(idempotencyKey) {
		return jsonError("Idempotency-Key must be 1-255 visible ASCII characters", "invalid_idempotency_key", http.StatusBadRequest), nil
	}
	scope, fingerprint, err := a.runIdempotencyIdentity(ctx, request, body, gatewaySessionKey, idempotencyKey)
	if err != nil {
		return nil, err
	}
	rawInput := body.GetOr("input", nil)
	userMessage, inputErr := runUserMessage(rawInput)
	if inputErr != nil {
		return inputErr, nil
	}
	turnAuthor, err := requestTurnAuthor(body)
	if err != nil {
		return jsonError(err.Error(), "invalid_author", http.StatusBadRequest), nil
	}
	history, instructions, storedSessionID, historyErr, walkedFrom, err := a.resolveChainedHistory(ctx, body, rawInput)
	if err != nil {
		return nil, err
	}
	if historyErr != nil {
		return historyErr, nil
	}
	previousResponseID := body.GetOr("previous_response_id", nil)
	sessionID := textOf(firstTruthy(body.GetOr("session_id", nil), storedSessionID))
	route := a.resolveRoute(body.GetOr("model", nil))
	overrides := requestAgentOverrides(body, a.modelName, true)
	requestedModel, _ := overrides["requested_model"].(string)
	requestedProvider, _ := overrides["requested_provider"].(string)
	if conflict := a.requestRouteConflictError(ctx, sessionID, gatewaySessionKey, requestedModel, requestedProvider, route); conflict != "" {
		return jsonError(conflict, "", http.StatusBadRequest), nil
	}
	if idempotencyKey != "" {
		outcome, record, err := a.runIdempotencyStore.Lookup(ctx, scope, idempotencyKey, fingerprint, roomRetentionUntil(ctx))
		if err != nil {
			return nil, err
		}
		if outcome == "conflict" || (outcome == "reused" && record != nil) {
			return a.replayOrConflict(ctx, request, outcome, record, gatewaySessionKey)
		}
	}
	if limited := a.concurrencyLimitedResponse(ctx); limited != nil {
		return limited, nil
	}
	runID := "run_" + hexUUID()
	ownerScope := a.runIdempotencyScope(ctx, request)
	a.mu.Lock()
	a.runOwners[runID] = ownerScope
	a.mu.Unlock()
	declaredSelected := sessionID == "" && gatewaySessionKey != ""
	selected := sessionID
	if declaredSelected {
		selected = a.declaredConversationSession(ctx, gatewaySessionKey)
	}
	if selected != "" {
		selected = a.resolveLiveSessionID(ctx, selected)
	}
	sessionID = selected
	if sessionID == "" {
		sessionID = runID
	}
	sessionHistoryDelivery := !pyrepr.Truthy(previousResponseID) && len(history) == 0
	if len(history) == 0 && selected != "" && !pyrepr.Truthy(previousResponseID) {
		for _, m := range a.conversationHistoryForSession(ctx, selected) {
			obj, _ := pyjson.AsObject(m)
			history = append(history, obj)
		}
	}
	stream := newRunStream()
	createdAt := apiNow()
	a.mu.Lock()
	a.runStreams[runID] = stream
	a.runStreamsCreated[runID] = createdAt
	a.runApprovalSessions[runID] = runID
	a.mu.Unlock()
	initial := a.setRunStatus(ctx, runID, "queued", "created_at", createdAt, "session_id", sessionID, "model", body.GetOr("model", a.modelName))
	if idempotencyKey != "" {
		outcome, record, err := a.runIdempotencyStore.Reserve(ctx, scope, idempotencyKey, fingerprint, runID, initial.Copy(),
			int64(a.runOwnerPID), a.runOwnerStarted, roomRetentionUntil(ctx))
		if err != nil {
			return nil, err
		}
		if outcome != "created" {
			a.mu.Lock()
			a.forgetRunLocked(runID, func(id string) { delete(a.runStreams, id) }, func(id string) { delete(a.runStreamsCreated, id) },
				func(id string) { delete(a.runApprovalSessions, id) }, func(id string) { delete(a.runStatuses, id) },
				func(id string) { delete(a.runOwners, id) })
			a.mu.Unlock()
			return a.replayOrConflict(ctx, request, outcome, record, gatewaySessionKey)
		}
		a.mu.Lock()
		a.runIdempotencyIDs[runID] = true
		a.mu.Unlock()
	}
	opts := agentOptions{ephemeralSystemPrompt: textOf(instructions), sessionID: sessionID, gatewaySessionKey: gatewaySessionKey,
		route: route, requestedModel: requestedModel, requestedProvider: requestedProvider, modelOptions: overrides["model_options"],
		roomDispatch: roomDispatch, roomExecutionPolicy: roomPolicy, hasRoomDispatch: roomDispatch != nil, hasRoomExecutionPolicy: roomPolicy != nil}
	launch := &runLaunch{owner: a, runID: runID, queue: stream, sessionID: sessionID, gatewaySessionKey: gatewaySessionKey,
		declaredSelected: declaredSelected, userMessage: userMessage, conversationHistory: history,
		sessionHistoryDelivery: sessionHistoryDelivery, agentOptions: opts, requestProfile: requestProfile(ctx), turnAuthor: turnAuthor}
	if sessionID == storedSessionID {
		launch.walkedFrom = walkedFrom
	}
	var ownerHome string
	var admitted map[string]any
	if selected != "" {
		if ownerHome, admitted, err = a.admitToLiveBotChat(ctx, sessionID, userMessage, turnAuthor); err != nil {
			return nil, err
		}
	}
	a.retargetToDeliveredChat(ctx, launch, admitted)
	if _, hasAuthority := a.sessionAuthority(); admitted == nil && hasAuthority {
		if refused, err := a.admitRunToAuthority(ctx, launch, sessionHistoryDelivery, declaredSelected, scope, idempotencyKey); err != nil || refused != nil {
			return refused, err
		}
	}
	a.activateAdmittedRequest(ctx)
	runCtx := context.WithoutCancel(ctx)
	var task *agentTask
	if admitted != nil {
		task = startAgentTask(runCtx, func(taskCtx context.Context) { a.executeRunViaLiveOwner(taskCtx, launch, ownerHome, admitted) })
	} else {
		task = startAgentTask(runCtx, func(taskCtx context.Context) { a.executeRun(taskCtx, launch) })
	}
	a.mu.Lock()
	a.activeRunTasks[runID] = task
	a.mu.Unlock()
	a.trackBackgroundTask(task)
	return acceptedResponse(runID, "started", gatewaySessionKey, false), nil
}

// Points the run record at the local chat an owner minted when it moved an API held Bot Chat aside
func (a *APIServerAdapter) retargetToDeliveredChat(ctx context.Context, launch *runLaunch, admitted map[string]any) {
	if admitted == nil {
		return
	}
	delivered := textOf(admitted["session_id"])
	if delivered != "" && delivered != launch.sessionID {
		launch.sessionID = delivered
		a.setRunStatus(ctx, launch.runID, "queued", "session_id", delivered)
	}
}

// Terminal usage payload from the agent's session counters
func runUsage(ag *agent.AIAgent) *pyjson.Object {
	values := map[string]int{"session_prompt_tokens": ag.SessionPromptTokens(), "session_completion_tokens": ag.SessionCompletionTokens(),
		"session_total_tokens": ag.SessionTotalTokens(), "session_cache_read_tokens": ag.SessionCacheReadTokens(),
		"session_cache_write_tokens": ag.SessionCacheWriteTokens()}
	out := pyjson.NewObject()
	for _, f := range usageFields {
		out.Set(f[0], int64(values[f[1]]))
	}
	return out
}

// Provider and model that actually served the turn
func servedRuntime(ag *agent.AIAgent) *pyjson.Object {
	return pyjson.ObjectOf("provider", ag.Provider(), "model", ag.Model())
}

// Worker body of one run, the result, usage and served runtime
func (a *APIServerAdapter) runAgentSync(ctx context.Context, run *runLaunch, ag *agent.AIAgent, env *tools.Env, approvalNotify tools.GatewayNotifyFunc) (map[string]any, *pyjson.Object, *pyjson.Object, error) {
	sessionID := run.sessionID
	taskID := sessionID
	if taskID == "" {
		taskID = run.runID
	}
	var resets []func()
	defer func() {
		clearTurnProcessOwnership(ag)
		a.memorySessions.Checkin(ctx, ag)
		if run.declaredSelected {
			sid := ag.SessionID()
			if sid == "" {
				sid = sessionID
			}
			a.bindDeclaredConversation(ctx, sid, run.gatewaySessionKey)
		}
		func() {
			defer func() {
				for _, reset := range resets {
					func() {
						defer func() { _ = recover() }()
						reset()
					}()
				}
			}()
			tools.UnregisterGatewayNotify(run.approvalSessionKey())
		}()
	}()
	tools.SetCurrentSessionKey(env, run.approvalSessionKey())
	resets = append(resets, func() { tools.ResetCurrentSessionKey(env) })
	delivery := ""
	if run.sessionHistoryDelivery {
		delivery = "1"
	}
	bindAPIServerSession(env, sessionID, run.approvalSessionKey(), sessionID, run.requestProfile, delivery)
	resets = append(resets, func() { tools.ClearSessionVars(env) })
	if run.agentOptions.hasRoomDispatch {
		policy, err := gateway.RoomExecutionPolicyFromMapping(run.agentOptions.roomExecutionPolicy)
		if err != nil {
			return nil, nil, nil, err
		}
		resets = append(resets, gateway.BindRoomExecutionPolicyEnv(env, policy))
	}
	tools.RegisterGatewayNotify(run.approvalSessionKey(), approvalNotify)
	publishTurnProcessOwnership(ag, taskID)
	opts := agent.RunConversationOptions{ConversationHistory: historyMaps(run.conversationHistory), TaskID: taskID}
	if run.turnAuthor != nil {
		opts.TurnAuthor = pyjson.Plain(run.turnAuthor).(map[string]any)
	}
	result, err := ag.RunConversationContent(ctx, run.userMessage, opts)
	if err != nil {
		return nil, nil, nil, err
	}
	return result, runUsage(ag), servedRuntime(ag), nil
}

// Tools environment of a run under its profile scope
func runEnv(ctx context.Context) *tools.Env {
	return &tools.Env{Home: home.Dir(ctx), Config: config.Tree(gateway.LoadGatewayConfigMap(ctx, ""))}
}
