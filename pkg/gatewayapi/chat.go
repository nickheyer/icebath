package gatewayapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds a peer turn waits on a live Bot Chat owner's receipt
const liveWaitSeconds = 300

// Validated session chat request with its runtime selection and run options
type sessionChatContext struct {
	gatewaySessionKey string
	sessionID         string
	body              *pyjson.Object
	userMessage       any
	runtimeRequest    *runtimeRequest
	lockActive        bool
	run               runAgentOptions
}

// Shared prelude of the session chat routes, header and body validation then runtime selection
func (a *APIServerAdapter) prepareSessionChat(ctx context.Context, request *gateway.WebRequest) (*sessionChatContext, *gateway.WebResponse, error) {
	gatewaySessionKey, keyErr := a.parseSessionKeyHeader(request)
	if keyErr != nil {
		return nil, keyErr, nil
	}
	sessionID := request.MatchInfo["session_id"]
	session, errResp, err := a.getExistingSessionOr404(ctx, sessionID)
	if err != nil || errResp != nil {
		return nil, errResp, err
	}
	body, errResp := readJSONBody(request)
	if errResp != nil {
		return nil, errResp, nil
	}
	userMessage, errResp := sessionChatUserMessage(body, "message")
	if errResp != nil {
		return nil, errResp, nil
	}
	turnAuthor, err := requestTurnAuthor(body)
	if err != nil {
		return nil, errorResponse(err.Error(), http.StatusBadRequest, errOpts{Code: "invalid_author"}), nil
	}
	systemPrompt := pyrepr.Or(body.GetOr("system_message", nil), body.GetOr("instructions", nil))
	promptText := ""
	if systemPrompt != nil {
		s, ok := systemPrompt.(string)
		if !ok {
			return nil, errorResponse("system_message must be a string", http.StatusBadRequest, errOpts{Code: "invalid_system_message"}), nil
		}
		promptText = s
	}
	r := a.effectiveSessionRuntimeRequest(session, body)
	if lockErr := a.runtimeLockError(r); lockErr != nil {
		return nil, lockErr, nil
	}
	if !a.persistSessionRuntimeLock(ctx, sessionID, r) {
		return nil, errorResponse("Could not persist the requested session model lock", http.StatusInternalServerError,
			errOpts{Code: "model_lock_persistence_failed"}), nil
	}
	lockActive := r.requireModelLock
	run := runAgentOptions{userMessage: userMessage, ephemeralSystemPrompt: promptText, sessionID: sessionID,
		gatewaySessionKey: gatewaySessionKey, requestedRuntime: r.requested(), routeSource: r.routeSource,
		confirmedRuntimeLock: lockActive, turnAuthor: turnAuthor, sessionHistoryDelivery: "1"}
	if run.routeSource == "" {
		run.routeSource = "global"
	}
	if lockActive {
		run.route = r.route
		run.requestedModel, run.requestedProvider = r.requestedModel, r.requestedProvider
		if r.modelOptions != nil && r.modelOptions.Len() > 0 {
			run.modelOptions = r.modelOptions
		}
	} else {
		storedModel := a.storedSessionModel(session)
		var storedRoute map[string]string
		if storedModel != "" {
			storedRoute = a.resolveRoute(storedModel)
		}
		run.route = storedRoute
		if run.route == nil {
			run.route = a.resolveRoute(body.GetOr("model", nil))
		}
		if storedModel != "" && storedRoute == nil {
			run.sessionModel = storedModel
		}
		overrides := requestAgentOverrides(body, a.modelName, true)
		run.requestedModel, _ = overrides["requested_model"].(string)
		run.requestedProvider, _ = overrides["requested_provider"].(string)
		if options, ok := overrides["model_options"]; ok {
			run.modelOptions = options
		}
		if conflict := a.requestRouteConflictError(ctx, sessionID, gatewaySessionKey, run.requestedModel, run.requestedProvider, run.route); conflict != "" {
			return nil, errorResponse(conflict, http.StatusBadRequest, errOpts{}), nil
		}
	}
	return &sessionChatContext{gatewaySessionKey: gatewaySessionKey, sessionID: sessionID, body: body, userMessage: userMessage,
		runtimeRequest: r, lockActive: lockActive, run: run}, nil, nil
}

// Session id and declared key response headers
func sessionHeaders(sessionID, gatewaySessionKey string) map[string]string {
	headers := map[string]string{"X-Hermes-Session-Id": sessionID}
	if gatewaySessionKey != "" {
		headers["X-Hermes-Session-Key"] = gatewaySessionKey
	}
	return headers
}

// Sanitized runtime metadata of a finished session chat turn
func (a *APIServerAdapter) effectiveTurnRuntime(r *runtimeRequest, result, usage map[string]any) *pyjson.Object {
	runtime := resultRuntime(result, usage)
	source := r.routeSource
	if source == "" {
		source = "global"
	}
	return sanitizeRuntimeMetadata(runtime, r.requested(), source, modelLockState(r, runtime))
}

// Runtime metadata of the result, falling back to the usage
func resultRuntime(result, usage map[string]any) *pyjson.Object {
	if rt, ok := result["runtime"].(*pyjson.Object); ok && rt.Len() > 0 {
		return rt
	}
	if rt, ok := usage["runtime"].(*pyjson.Object); ok && rt.Len() > 0 {
		return rt
	}
	return pyjson.NewObject()
}

// Confirmed once a runtime ran under a lock, accepted before, empty without a lock
func modelLockState(r *runtimeRequest, runtime *pyjson.Object) string {
	if !r.requireModelLock {
		return ""
	}
	if runtime != nil && runtime.Len() > 0 {
		return "confirmed"
	}
	return "accepted"
}

// Live Bot Chat admission of a peer turn, the profile home and mailbox record, nil when this process runs it
func (a *APIServerAdapter) admitToLiveBotChat(ctx context.Context, sessionID string, message any, author *pyjson.Object) (string, map[string]any, error) {
	text, ok := message.(string)
	if !ok {
		return "", nil, nil
	}
	if _, has := a.sessionAuthority(); has {
		return admitPeerTurn(ctx, a.GatewayRunner, sessionID, text, author)
	}
	db := a.ensureSessionDB(ctx)
	if db == nil {
		return "", nil, nil
	}
	profileHome := filepath.Dir(db.Path())
	owner, err := gateway.FindCanonicalLiveOwner(ctx, profileHome)
	if err != nil {
		var typed interface{ PyTypeName() string }
		if errors.As(err, &typed) && typed.PyTypeName() == "ValueError" {
			return "", nil, nil
		}
		return "", nil, err
	}
	if owner == nil {
		return "", nil, nil
	}
	tip, err := db.GetCompressionTip(ctx, sessionID)
	if err != nil {
		return "", nil, err
	}
	if tip != pyrepr.Str(owner["session_id"]) {
		return "", nil, nil
	}
	var authorMap map[string]any
	if author != nil {
		authorMap = pyjson.Plain(author).(map[string]any)
	}
	record, err := gateway.DeliverToLiveOwnerAuthored(ctx, profileHome, owner, text, "", "", authorMap)
	if err != nil {
		return "", nil, err
	}
	if record == nil {
		return "", nil, nil
	}
	return profileHome, record, nil
}

// Hands a turn aimed at a live held canonical Bot Chat to its owner and answers with the receipt
func (a *APIServerAdapter) answerThroughLiveBotChat(ctx context.Context, c *sessionChatContext) (*gateway.WebResponse, error) {
	profileHome, record, err := a.admitToLiveBotChat(ctx, c.sessionID, c.userMessage, c.run.turnAuthor)
	if err != nil || record == nil {
		return nil, err
	}
	sessionID := pyrepr.Str(pyrepr.Or(record["session_id"], c.sessionID))
	record, err = a.awaitLiveBotChatReceipt(ctx, profileHome, record, nil)
	if err != nil {
		return nil, err
	}
	deliveryID := record["delivery_id"]
	headers := sessionHeaders(sessionID, c.gatewaySessionKey)
	status := pyrepr.Str(record["status"])
	switch status {
	case "settled":
		return jsonResponse(pyjson.ObjectOf("object", "hermes.session.chat.completion", "session_id", sessionID,
			"message", pyjson.ObjectOf("role", "assistant", "content", pyrepr.Or(record["reply"], "")),
			"usage", pyjson.NewObject(), "runtime", pyjson.NewObject(), "delivery_id", deliveryID), http.StatusOK, headers), nil
	case "queued", "claimed":
		return jsonResponse(pyjson.ObjectOf("object", "hermes.session.chat.queued", "session_id", sessionID,
			"status", status, "delivery_id", deliveryID), http.StatusAccepted, headers), nil
	}
	message := pyrepr.Str(pyrepr.Or(record["error"], "Bot Chat delivery "+status))
	return errorResponse(message, http.StatusBadGateway, errOpts{Code: pyrepr.Or(record["reason"], status), Headers: headers}), nil
}

// Waits on the live Bot Chat receipt until it settles or the local DM budget runs out, pinging keepalive meanwhile
func (a *APIServerAdapter) awaitLiveBotChatReceipt(ctx context.Context, profileHome string, record map[string]any, keepalive func() error) (map[string]any, error) {
	deadline := time.Now().Add(liveWaitSeconds * time.Second)
	for {
		status := pyrepr.Str(record["status"])
		if status != "queued" && status != "claimed" {
			return record, nil
		}
		remaining := time.Until(deadline).Seconds()
		if remaining <= 0 {
			return record, nil
		}
		budget := remaining
		if keepalive != nil {
			budget = min(remaining, chatCompletionsSSEKeepaliveSeconds)
		}
		next, err := awaitLiveDelivery(ctx, a, profileHome, record, budget)
		if err != nil {
			return nil, err
		}
		record = next
		status = pyrepr.Str(record["status"])
		if keepalive != nil && (status == "queued" || status == "claimed") {
			if err := keepalive(); err != nil {
				return nil, err
			}
		}
	}
}

// SSE form of the live Bot Chat handoff, the receipt as the run's single completion event
func (a *APIServerAdapter) streamThroughLiveBotChat(ctx context.Context, request *gateway.WebRequest, c *sessionChatContext) (*gateway.WebResponse, error) {
	profileHome, record, err := a.admitToLiveBotChat(ctx, c.sessionID, c.userMessage, c.run.turnAuthor)
	if err != nil || record == nil {
		return nil, err
	}
	sessionID := pyrepr.Str(pyrepr.Or(record["session_id"], c.sessionID))
	events := newSessionEventQueue(sessionID, "run_"+hexUUID())
	response, err := a.prepareSSEResponse(request, sessionID, c.gatewaySessionKey)
	if err != nil {
		return nil, err
	}
	write := func(name string, payload *pyjson.Object) error {
		ev := events.payload(name, payload)
		frame, err := gateway.APISSEFrame(ev.payload, ev.name, false, nil)
		if err != nil {
			return err
		}
		return response.Write(frame)
	}
	keepalive := func() error { return response.Write([]byte(": keepalive\n\n")) }
	err = func() error {
		if err := write("run.started", pyjson.ObjectOf("user_message", pyjson.ObjectOf("role", "user", "content", c.userMessage),
			"runtime", pyjson.NewObject())); err != nil {
			return err
		}
		record, err := a.awaitLiveBotChatReceipt(ctx, profileHome, record, keepalive)
		if err != nil {
			return err
		}
		deliveryID := record["delivery_id"]
		status := pyrepr.Str(record["status"])
		switch status {
		case "settled":
			messageID := "msg_" + hexUUID()
			if err := write("message.started", pyjson.ObjectOf("message", pyjson.ObjectOf("id", messageID, "role", "assistant"))); err != nil {
				return err
			}
			if err := write("assistant.completed", pyjson.ObjectOf("message_id", messageID, "content", pyrepr.Or(record["reply"], ""),
				"delivery_id", deliveryID, "runtime", pyjson.NewObject())); err != nil {
				return err
			}
			if err := write("run.completed", pyjson.ObjectOf("message_id", messageID, "delivery_id", deliveryID,
				"usage", pyjson.NewObject(), "runtime", pyjson.NewObject())); err != nil {
				return err
			}
		case "queued", "claimed":
			if err := write("run.queued", pyjson.ObjectOf("status", status, "delivery_id", deliveryID)); err != nil {
				return err
			}
		default:
			if err := write("error", pyjson.ObjectOf("message", pyrepr.Or(record["error"], "Bot Chat delivery "+status),
				"code", pyrepr.Or(record["reason"], status), "delivery_id", deliveryID)); err != nil {
				return err
			}
		}
		return write("done", pyjson.NewObject())
	}()
	if err != nil {
		if isClientGone(err) {
			apiLog().Info("Session SSE client disconnected while a live Bot Chat held the turn")
		} else {
			return nil, err
		}
	}
	return response.WebResponse, nil
}

// Hex form of a fresh random UUID
func hexUUID() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }

// Whether a write error means the SSE client went away
func isClientGone(err error) bool {
	var pyErr *gateway.PyError
	if errors.As(err, &pyErr) {
		switch pyErr.Kind {
		case "ConnectionResetError", "ConnectionAbortedError", "BrokenPipeError", "OSError":
			return true
		}
	}
	return errors.Is(err, context.Canceled)
}

// One synchronous agent turn with one policy gated re-run of a transient failure
func (a *APIServerAdapter) handleSessionChat(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	if limited := a.concurrencyLimitedResponse(ctx); limited != nil {
		return limited, nil
	}
	c, errResp, err := a.prepareSessionChat(ctx, request)
	if err != nil || errResp != nil {
		return errResp, err
	}
	if handedOff, err := a.answerThroughLiveBotChat(ctx, c); err != nil || handedOff != nil {
		return handedOff, err
	}
	run := c.run
	run.conversationHistory = a.conversationHistoryForSession(ctx, c.sessionID)
	result, usage, err := a.runAgent(ctx, run)
	if err != nil {
		return nil, err
	}
	if action, err := tools.ResultRetryAction(result); err != nil {
		return nil, err
	} else if action != tools.RetryNone {
		run.conversationHistory = a.conversationHistoryForSession(ctx, c.sessionID)
		run.resumeUnansweredTurn = true
		if result, usage, err = a.runAgent(ctx, run); err != nil {
			return nil, err
		}
	}
	effective := c.sessionID
	if sid, ok := result["session_id"].(string); ok && sid != "" {
		effective = sid
	}
	final, _ := result["final_response"].(string)
	finalResponse := gateway.APIResolveMediaToDataURLs(ctx, final)
	return jsonResponse(pyjson.ObjectOf("object", "hermes.session.chat.completion", "status", responseStatus(result),
		"session_id", effective, "message", pyjson.ObjectOf("role", "assistant", "content", finalResponse), "usage", usage,
		"runtime", a.effectiveTurnRuntime(c.runtimeRequest, result, usage)), http.StatusOK, sessionHeaders(effective, c.gatewaySessionKey)), nil
}

// SSE wrapper over one agent turn with typed delta, tool, commentary and approval events
func (a *APIServerAdapter) handleSessionChatStream(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	if limited := a.concurrencyLimitedResponse(ctx); limited != nil {
		return limited, nil
	}
	c, errResp, err := a.prepareSessionChat(ctx, request)
	if err != nil || errResp != nil {
		return errResp, err
	}
	if handedOff, err := a.streamThroughLiveBotChat(ctx, request, c); err != nil || handedOff != nil {
		return handedOff, err
	}
	gatewaySessionKey, sessionID := c.gatewaySessionKey, c.sessionID
	lock := ""
	if c.lockActive {
		lock = "accepted"
	}
	source := c.runtimeRequest.routeSource
	if source == "" {
		source = "global"
	}
	runtimeMeta := sanitizeRuntimeMetadata(nil, c.runtimeRequest.requested(), source, lock)
	messageID := "msg_" + hexUUID()
	runID := "run_" + hexUUID()
	events := newSessionEventQueue(sessionID, runID)
	a.mu.Lock()
	a.runOwners[runID] = a.runIdempotencyScope(ctx, request)
	a.mu.Unlock()
	a.setRunStatus(ctx, runID, "queued", "session_id", sessionID, "model", c.body.GetOr("model", a.modelName))
	delta := func(text string) {
		if text != "" {
			events.enqueue("assistant.delta", pyjson.ObjectOf("message_id", messageID, "delta", text))
		}
	}
	toolProgress := func(eventType, toolName, preview string, args map[string]any, extra map[string]any) {
		switch eventType {
		case "reasoning.available":
			name := toolName
			if name == "" {
				name = "_thinking"
			}
			events.enqueue("tool.progress", pyjson.ObjectOf("message_id", messageID, "tool_name", name, "delta", preview))
		case "tool.started", "tool.completed", "tool.failed":
			name := eventType
			if eventType == "tool.completed" && pyrepr.Truthy(extra["is_error"]) {
				name = "tool.failed"
			}
			var toolValue, previewValue, argsValue any
			if toolName != "" {
				toolValue = toolName
			}
			if preview != "" {
				previewValue = preview
			}
			if args != nil {
				argsValue = args
			}
			events.enqueue(name, pyjson.ObjectOf("message_id", messageID, "tool_name", toolValue, "preview", previewValue, "args", argsValue))
		}
	}
	commentary := func(text string, alreadyStreamed bool) {
		if strings.TrimSpace(text) != "" {
			events.enqueue("assistant.commentary", pyjson.ObjectOf("message_id", messageID, "text", text, "already_streamed", alreadyStreamed))
		}
	}
	approvalNotify := a.registerSessionStreamApproval(ctx, runID, events, messageID)
	userMessage, runtimeRequest := c.userMessage, c.runtimeRequest
	task := startAgentTask(context.WithoutCancel(ctx), func(taskCtx context.Context) {
		defer func() {
			a.mu.Lock()
			a.forgetActiveRunAgentLocked(runID)
			delete(a.runApprovalSessions, runID)
			a.mu.Unlock()
			a.releaseRunOwnerIfForgotten(runID)
			events.flush()
			events.enqueue("done", pyjson.NewObject())
			events.close()
		}()
		events.enqueue("run.started", pyjson.ObjectOf("user_message", pyjson.ObjectOf("role", "user", "content", userMessage),
			"runtime", runtimeMeta))
		a.setRunStatus(taskCtx, runID, "running", "last_event", "run.started")
		events.enqueue("message.started", pyjson.ObjectOf("message", pyjson.ObjectOf("id", messageID, "role", "assistant")))
		history := a.conversationHistoryForSession(taskCtx, sessionID)
		run := c.run
		run.conversationHistory = history
		run.streamDeltaCallback = delta
		run.toolProgressCallback = toolProgress
		run.interimAssistantCb = commentary
		run.activeRunID = runID
		run.approvalNotifyCallback = approvalNotify
		run.approvalSessionKey = runID
		result, usage, err := a.runAgent(taskCtx, run)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				a.setRunStatus(taskCtx, runID, "cancelled", "last_event", "run.cancelled")
				return
			}
			apiLog().Error("[api_server] session chat stream failed", "error", err)
			a.setRunStatus(taskCtx, runID, "failed", "error", redactAPIErrorText(err.Error(), -1), "last_event", "run.failed")
			events.enqueue("error", pyjson.ObjectOf("message", redactAPIErrorText(err.Error(), -1)))
			return
		}
		events.flush()
		final, _ := result["final_response"].(string)
		finalResponse := gateway.APIResolveMediaToDataURLs(taskCtx, final)
		effective := sessionID
		if sid, ok := result["session_id"]; ok {
			effective = pyrepr.Str(sid)
		}
		turnMessages := a.turnTranscriptMessages(history, userMessage, result)
		effectiveRuntime := a.effectiveTurnRuntime(runtimeRequest, result, usage)
		status, fields := terminalRunStatus(result)
		completed := pyjson.ObjectOf("session_id", effective, "message_id", messageID, "content", finalResponse)
		completed.Update(fields)
		completed.Set("runtime", effectiveRuntime)
		events.enqueue("assistant.completed", completed)
		terminal := pyjson.ObjectOf("session_id", effective, "message_id", messageID)
		terminal.Update(fields)
		terminal.Set("messages", turnMessages)
		terminal.Set("usage", usage)
		terminal.Set("runtime", effectiveRuntime)
		events.enqueue("run."+status, terminal)
		statusFields := []any{"session_id", effective, "output", finalResponse, "usage", usage, "last_event", "run." + status}
		for _, k := range fields.Keys {
			v, _ := fields.Get(k)
			statusFields = append(statusFields, k, v)
		}
		a.setRunStatus(taskCtx, runID, status, statusFields...)
	})
	a.trackBackgroundTask(task)
	response, err := a.prepareSSEResponse(request, sessionID, gatewaySessionKey)
	if err != nil {
		return nil, err
	}
	streamErr := func() error {
		for {
			ev, ok, err := events.next(ctx, time.Duration(chatCompletionsSSEKeepaliveSeconds*float64(time.Second)))
			if err != nil {
				return err
			}
			if !ok {
				if err := response.Write([]byte(": keepalive\n\n")); err != nil {
					return err
				}
				continue
			}
			if ev.end {
				return nil
			}
			frame, err := gateway.APISSEFrame(ev.payload, ev.name, false, nil)
			if err != nil {
				return err
			}
			if err := response.Write(frame); err != nil {
				return err
			}
		}
	}()
	if streamErr != nil {
		switch {
		case errors.Is(streamErr, context.Canceled):
			a.drainSessionStreamTaskOnDisconnect(runID, task, "SSE task cancelled")
			apiLog().Info(fmt.Sprintf("Session SSE task cancelled; drained live run %s", runID))
			return nil, streamErr
		case isClientGone(streamErr):
			a.drainSessionStreamTaskOnDisconnect(runID, task, "SSE client disconnected")
			apiLog().Info(fmt.Sprintf("Session SSE client disconnected; interrupted live run %s", runID))
		default:
			apiLog().Debug(fmt.Sprintf("[api_server] session SSE stream error: %v", streamErr))
		}
	}
	return response.WebResponse, nil
}

// Routes a session stream turn's approvals to its SSE queue and the run approval endpoint, keyed by run id
func (a *APIServerAdapter) registerSessionStreamApproval(ctx context.Context, runID string, events *sessionEventQueue, messageID string) tools.GatewayNotifyFunc {
	a.mu.Lock()
	a.runApprovalSessions[runID] = runID
	a.mu.Unlock()
	return func(approvalData map[string]any) error {
		data, _ := pyjson.AsObject(approvalData)
		event := approvalRequestEvent(ctx, runID, data, "message_id", messageID)
		a.setRunStatus(ctx, runID, "waiting_for_approval", "last_event", "approval.request", "approval", event)
		events.enqueue("approval.request", event)
		return nil
	}
}

// Keeps live run control until the executor backed turn exits, interrupting its agent first
func (a *APIServerAdapter) drainSessionStreamTaskOnDisconnect(runID string, task *agentTask, interruptMessage string) {
	a.mu.Lock()
	ag := a.activeRunAgents[runID]
	a.mu.Unlock()
	if ag == nil {
		if !task.Done() {
			task.Cancel()
			task.Wait()
		}
		return
	}
	func() {
		defer func() { _ = recover() }()
		ag.Interrupt(interruptMessage)
	}()
	if !task.Done() {
		task.Wait()
	}
}

// Backend acknowledgement of a browser model lock
func (a *APIServerAdapter) handleSessionModelLock(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	sessionID := request.MatchInfo["session_id"]
	_, errResp, err := a.getExistingSessionOr404(ctx, sessionID)
	if err != nil || errResp != nil {
		return errResp, err
	}
	body, errResp := readJSONBody(request)
	if errResp != nil {
		return errResp, nil
	}
	r := a.sessionRuntimeRequestFromBody(body)
	r.requireModelLock = true
	if lockErr := a.runtimeLockError(r); lockErr != nil {
		return lockErr, nil
	}
	if !a.persistSessionRuntimeLock(ctx, sessionID, r) {
		return errorResponse("Could not persist the requested session model lock", http.StatusInternalServerError,
			errOpts{Code: "model_lock_persistence_failed"}), nil
	}
	route := r.route
	if route == nil {
		route = map[string]string{}
	}
	providerName := route["provider"]
	if providerName == "" {
		providerName = r.requestedProvider
	}
	model := route["model"]
	if model == "" {
		model = r.requestedModel
	}
	source := r.routeSource
	if source == "" {
		source = "raw_request"
	}
	runtime := sanitizeRuntimeMetadata(pyjson.ObjectOf("provider", providerName, "model", model, "route_source", source),
		r.requested(), source, "accepted")
	return jsonResponse(pyjson.ObjectOf("object", "hermes.session.model_lock", "session_id", sessionID, "runtime", runtime), http.StatusOK, nil), nil
}
