package gatewayapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Unbounded agent stream queue, nil being the end of stream sentinel
type streamQueue struct {
	mu      sync.Mutex
	pending []any
	ready   chan struct{}
}

func newStreamQueue() *streamQueue { return &streamQueue{ready: make(chan struct{}, 1)} }

// Appends an item from any goroutine
func (q *streamQueue) put(item any) {
	q.mu.Lock()
	q.pending = append(q.pending, item)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// Next item without waiting, ok false when empty
func (q *streamQueue) tryGet() (any, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return nil, false
	}
	item := q.pending[0]
	q.pending = q.pending[1:]
	return item, true
}

// Next item waiting up to the timeout, ok false on timeout
func (q *streamQueue) get(ctx context.Context, timeout time.Duration) (any, bool, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if item, ok := q.tryGet(); ok {
			return item, true, nil
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-timer.C:
			return nil, false, nil
		case <-q.ready:
		}
	}
}

// Stream items until the end of stream, keepalives written while idle, the sentinel yielded once
func iterStreamItems(ctx context.Context, q *streamQueue, task *agentTask, response *gateway.WebStreamResponse, yield func(item any, eos bool) error) error {
	lastActivity := time.Now()
	for {
		item, ok, err := q.get(ctx, 500*time.Millisecond)
		if err != nil {
			return err
		}
		if !ok {
			if task.Done() {
				for {
					item, ok := q.tryGet()
					if !ok || item == nil {
						return nil
					}
					if err := yield(item, false); err != nil {
						return err
					}
					lastActivity = time.Now()
				}
			}
			if time.Since(lastActivity).Seconds() >= chatCompletionsSSEKeepaliveSeconds {
				if err := response.Write([]byte(": keepalive\n\n")); err != nil {
					return err
				}
				lastActivity = time.Now()
			}
			continue
		}
		if item == nil {
			return yield(nil, true)
		}
		if err := yield(item, false); err != nil {
			return err
		}
		lastActivity = time.Now()
	}
}

// Chat messages as the system prompt, the conversation messages and a 400 for malformed content
func parseChatMessages(messages []any) (*string, []*pyjson.Object, *gateway.WebResponse) {
	var systemPrompt *string
	conversation := []*pyjson.Object{}
	for idx, raw := range messages {
		msg, ok := raw.(*pyjson.Object)
		if !ok {
			panic(&gateway.PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))})
		}
		role := msg.GetOr("role", "")
		rawContent := msg.GetOr("content", "")
		switch role {
		case "system":
			content := gateway.APINormalizeChatContent(rawContent)
			if systemPrompt == nil {
				systemPrompt = &content
			} else {
				joined := *systemPrompt + "\n" + content
				systemPrompt = &joined
			}
		case "user", "assistant":
			content, err := gateway.APINormalizeMultimodalContent(rawContent)
			if err != nil {
				return nil, nil, multimodalValidationError(err, fmt.Sprintf("messages[%d].content", idx))
			}
			conversation = append(conversation, pyjson.ObjectOf("role", role, "content", content))
		}
	}
	return systemPrompt, conversation, nil
}

// Responses input as messages, a 400 for a malformed input
func parseResponsesInput(rawInput any) ([]*pyjson.Object, *gateway.WebResponse) {
	messages := []*pyjson.Object{}
	switch v := rawInput.(type) {
	case string:
		messages = append(messages, pyjson.ObjectOf("role", "user", "content", v))
	case []any:
		for idx, item := range v {
			switch it := item.(type) {
			case string:
				messages = append(messages, pyjson.ObjectOf("role", "user", "content", it))
			case *pyjson.Object:
				if isReasoningInputItem(it) {
					continue
				}
				content, err := gateway.APINormalizeMultimodalContent(it.GetOr("content", ""))
				if err != nil {
					return nil, multimodalValidationError(err, fmt.Sprintf("input[%d].content", idx))
				}
				messages = append(messages, pyjson.ObjectOf("role", it.GetOr("role", "user"), "content", content))
			}
		}
	default:
		return nil, errorResponse("'input' must be a string or array", http.StatusBadRequest, errOpts{})
	}
	return messages, nil
}

// Explicit responses conversation history, a 400 for a malformed entry
func parseConversationHistory(rawHistory any) ([]any, *gateway.WebResponse) {
	list, ok := rawHistory.([]any)
	if !ok {
		return nil, errorResponse("'conversation_history' must be an array of message objects", http.StatusBadRequest, errOpts{})
	}
	history := []any{}
	for i, entry := range list {
		if isReasoningInputItem(entry) {
			continue
		}
		obj, isObj := entry.(*pyjson.Object)
		hasRole, hasContent := false, false
		if isObj {
			_, hasRole = obj.Get("role")
			_, hasContent = obj.Get("content")
		}
		if !isObj || !hasRole || !hasContent {
			return nil, errorResponse(fmt.Sprintf("conversation_history[%d] must have 'role' and 'content' fields", i), http.StatusBadRequest, errOpts{})
		}
		raw, _ := obj.Get("content")
		content, err := gateway.APINormalizeMultimodalContent(raw)
		if err != nil {
			return nil, multimodalValidationError(err, fmt.Sprintf("conversation_history[%d].content", i))
		}
		role, _ := obj.Get("role")
		history = append(history, pyjson.ObjectOf("role", pyrepr.Str(role), "content", content))
	}
	return history, nil
}

// Model route alias and per request overrides, a 400 for an ambiguous mix
func (a *APIServerAdapter) selectRequestRoute(ctx context.Context, body *pyjson.Object, sessionID, gatewaySessionKey string, modelAlias any) (map[string]string, map[string]any, *gateway.WebResponse) {
	route := a.resolveRoute(modelAlias)
	overrides := requestAgentOverrides(body, a.modelName, a.directModelRequests)
	requestedModel, _ := overrides["requested_model"].(string)
	requestedProvider, _ := overrides["requested_provider"].(string)
	if conflict := a.requestRouteConflictError(ctx, sessionID, gatewaySessionKey, requestedModel, requestedProvider, route); conflict != "" {
		return route, overrides, errorResponse(conflict, http.StatusBadRequest, errOpts{})
	}
	return route, overrides, nil
}

// Exposes a streaming completion as a run for the approval endpoint, the done hook retiring it
func (a *APIServerAdapter) registerStreamApproval(ctx context.Context, request *gateway.WebRequest, completionID string, q *streamQueue, sessionID string) (tools.GatewayNotifyFunc, func(result map[string]any, err error)) {
	a.mu.Lock()
	a.runOwners[completionID] = a.runIdempotencyScope(ctx, request)
	a.mu.Unlock()
	a.setRunStatus(ctx, completionID, "running", "session_id", sessionID)
	a.mu.Lock()
	a.runApprovalSessions[completionID] = completionID
	a.mu.Unlock()
	notify := func(approvalData map[string]any) error {
		data, _ := pyjson.AsObject(approvalData)
		event := approvalRequestEvent(ctx, completionID, data, "session_id", sessionID)
		a.setRunStatus(ctx, completionID, "waiting_for_approval", "last_event", "approval.request", "approval", event)
		q.put(streamTagged{"__approval__", event})
		return nil
	}
	onDone := func(result map[string]any, err error) {
		a.mu.Lock()
		delete(a.runApprovalSessions, completionID)
		a.mu.Unlock()
		status := "completed"
		switch {
		case errors.Is(err, context.Canceled):
			status = "cancelled"
		case err != nil:
			status = "failed"
		case result != nil:
			status, _ = terminalRunStatus(result)
		}
		a.setRunStatus(ctx, completionID, status)
		a.releaseRunOwnerIfForgotten(completionID)
	}
	return notify, onDone
}

// Outcome of a spawned stream agent
type streamAgentOutcome struct {
	result map[string]any
	usage  map[string]any
	err    error
}

// Starts the agent turn for an SSE writer, the end of stream sentinel queued once it finishes
func (a *APIServerAdapter) spawnStreamAgent(ctx context.Context, q *streamQueue, onDone func(map[string]any, error), run runAgentOptions) (*agentTask, **agent.AIAgent, *streamAgentOutcome) {
	return a.spawnStreamAgentWith(ctx, q, onDone, run, a.runAgent)
}

// Starts a turn through the given runner for an SSE writer, the end of stream sentinel queued once it finishes
func (a *APIServerAdapter) spawnStreamAgentWith(ctx context.Context, q *streamQueue, onDone func(map[string]any, error), run runAgentOptions, runAgent func(context.Context, runAgentOptions) (map[string]any, map[string]any, error)) (*agentTask, **agent.AIAgent, *streamAgentOutcome) {
	run.streamDeltaCallback = func(delta string) { q.put(delta) }
	run.reasoningCallback = func(text string) {
		if text != "" {
			q.put(streamTagged{"__reasoning__", pyjson.ObjectOf("text", text)})
		}
	}
	run.statusCallback = func(kind string, message any) {
		var raw any = kind
		if message != nil {
			raw = message
		}
		text := strings.TrimSpace(redactAPIErrorText(raw, -1))
		if text != "" {
			q.put(streamTagged{"__status__", pyjson.ObjectOf("kind", kind, "text", text)})
		}
	}
	var ref *agent.AIAgent
	run.agentRef = &ref
	outcome := &streamAgentOutcome{}
	task := startAgentTask(ctx, func(taskCtx context.Context) {
		defer func() {
			defer q.put(nil)
			if onDone != nil {
				func() {
					defer func() {
						if rec := recover(); rec != nil {
							apiLog().Debug("[api_server] stream on_done hook failed", "error", rec)
						}
					}()
					onDone(outcome.result, outcome.err)
				}()
			}
		}()
		outcome.result, outcome.usage, outcome.err = runAgent(taskCtx, run)
	})
	return task, &ref, outcome
}

// OpenAI chat completions, streaming or idempotent non streaming
func (a *APIServerAdapter) handleChatCompletions(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	if limited := a.concurrencyLimitedResponse(ctx); limited != nil {
		return limited, nil
	}
	raw, err := requestJSON(request)
	if err != nil {
		return errorResponse("Invalid JSON in request body", http.StatusBadRequest, errOpts{}), nil
	}
	body, _ := raw.(*pyjson.Object)
	if body == nil {
		return nil, &gateway.PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))}
	}
	relayMetadata := requestRelayMetadata(body)
	messages, ok := body.GetOr("messages", nil).([]any)
	if !ok || len(messages) == 0 {
		return invalidRequest("Missing or invalid 'messages' field"), nil
	}
	stream := gateway.APICoerceRequestBool(body.GetOr("stream", nil), false)
	systemPrompt, conversation, msgErr := parseChatMessages(messages)
	if msgErr != nil {
		return msgErr, nil
	}
	var userMessage any = ""
	history := []any{}
	if len(conversation) > 0 {
		userMessage = conversation[len(conversation)-1].GetOr("content", "")
		for _, m := range conversation[:len(conversation)-1] {
			history = append(history, m)
		}
	}
	if !gateway.APIContentHasVisiblePayload(userMessage) {
		return invalidRequest("No user message found in messages"), nil
	}
	gatewaySessionKey, keyErr := a.parseSessionKeyHeader(request)
	if keyErr != nil {
		return keyErr, nil
	}
	providedSessionID := strings.TrimSpace(request.Header.Get("X-Hermes-Session-Id"))
	var sessionID string
	var historyRows []map[string]any
	useStored := false
	if providedSessionID != "" {
		if a.apiKey == "" {
			apiLog().Warn("Session continuation via X-Hermes-Session-Id rejected: no API key configured.  Set API_SERVER_KEY to enable session continuity.")
			return errorResponse("Session continuation requires API key authentication. Configure API_SERVER_KEY to enable this feature.",
				http.StatusForbidden, errOpts{}), nil
		}
		if sessionHeaderUnsafeRE.MatchString(providedSessionID) || gateway.IsPathUnsafe(providedSessionID) {
			return invalidRequest("Invalid session ID"), nil
		}
		if len([]rune(providedSessionID)) > maxSessionHeaderLen {
			return invalidRequest("Session ID too long"), nil
		}
		sessionID = providedSessionID
		if db := a.ensureSessionDB(ctx); db != nil {
			sessionID = a.resolveLiveSessionID(ctx, providedSessionID)
			rows, err := db.GetMessagesAsConversation(ctx, sessionID, state.ConversationOptions{})
			if err != nil {
				apiLog().Warn(fmt.Sprintf("Failed to load session history for %s: %v", sessionID, err))
				rows = []map[string]any{}
			}
			historyRows, useStored = rows, true
		}
	} else {
		firstUser := ""
		for _, m := range conversation {
			if r, _ := m.Get("role"); r == "user" {
				firstUser = pyrepr.Str(m.GetOr("content", ""))
				break
			}
		}
		prompt := ""
		if systemPrompt != nil {
			prompt = *systemPrompt
		}
		derived := deriveChatSessionID(ctx, prompt, firstUser, requestProfile(ctx))
		resolved, lookupErr, err := a.implicitChatSession(ctx, request, derived)
		if err != nil {
			return nil, err
		}
		if lookupErr != nil {
			return lookupErr, nil
		}
		sessionID = resolved
	}
	if useStored {
		history = make([]any, 0, len(historyRows))
		for _, r := range historyRows {
			obj, _ := pyjson.AsObject(r)
			history = append(history, obj)
		}
	}
	completionID := "chatcmpl-" + hexUUID()[:29]
	modelName := body.GetOr("model", a.modelName)
	created := int64(apiNow())
	route, overrides, selectionErr := a.selectRequestRoute(ctx, body, sessionID, gatewaySessionKey, modelName)
	if selectionErr != nil {
		return selectionErr, nil
	}
	promptText := ""
	if systemPrompt != nil {
		promptText = *systemPrompt
	}
	run := runAgentOptions{userMessage: userMessage, conversationHistory: historyMaps(history), ephemeralSystemPrompt: promptText,
		sessionID: sessionID, gatewaySessionKey: gatewaySessionKey, route: route, relayMetadata: relayMetadata}
	run.requestedModel, _ = overrides["requested_model"].(string)
	run.requestedProvider, _ = overrides["requested_provider"].(string)
	if options, ok := overrides["model_options"]; ok {
		run.modelOptions = options
	}
	if providedSessionID != "" {
		run.sessionHistoryDelivery = "1"
	}
	a.applyAuthorityChatKwargs(&run, request, providedSessionID)
	if providedSessionID != "" && body.GetOr("hermes_notification_category", nil) == "diagnostic" {
		run.notificationCategory = "diagnostic"
	}
	if stream {
		q := newStreamQueue()
		var startedMu sync.Mutex
		started := map[string]bool{}
		env := &tools.Env{}
		run.toolStartCallback = func(toolCallID, functionName string, functionArgs map[string]any) {
			if toolCallID == "" || strings.HasPrefix(functionName, "_") {
				return
			}
			startedMu.Lock()
			started[toolCallID] = true
			startedMu.Unlock()
			label := tools.BuildToolPreview(env, functionName, functionArgs, nil)
			if label == "" {
				label = functionName
			}
			q.put(streamTagged{"__tool_progress__", pyjson.ObjectOf("tool", functionName, "emoji", tools.GetToolEmoji(env, functionName, ""),
				"label", label, "toolCallId", toolCallID, "status", "running")})
		}
		run.toolCompleteCallback = func(toolCallID, functionName string, functionArgs map[string]any, result string) {
			startedMu.Lock()
			known := toolCallID != "" && started[toolCallID]
			delete(started, toolCallID)
			startedMu.Unlock()
			if !known {
				return
			}
			q.put(streamTagged{"__tool_progress__", pyjson.ObjectOf("tool", functionName, "toolCallId", toolCallID, "status", "completed")})
		}
		notify, endStreamRun := a.registerStreamApproval(ctx, request, completionID, q, sessionID)
		run.approvalNotifyCallback = notify
		run.approvalSessionKey = completionID
		task, ref, outcome := a.spawnStreamAgent(context.WithoutCancel(ctx), q, endStreamRun, run)
		echoed := providedSessionID
		if echoed == "" {
			echoed = sessionID
		}
		return a.writeSSEChatCompletion(ctx, request, completionID, modelName, created, q, task, ref, outcome, echoed, gatewaySessionKey)
	}
	compute := func() ([2]map[string]any, error) {
		result, usage, err := a.runAgent(ctx, run)
		return [2]map[string]any{result, usage}, err
	}
	outcome, errResp := a.runIdempotent(ctx, request, body, compute, "chat completions",
		[]string{"model", "provider", "model_options", "messages", "tools", "tool_choice", "stream", "hermes_notification_category"},
		"chat_completions")
	if errResp != nil {
		return errResp, nil
	}
	return a.chatCompletionResponse(ctx, outcome[0], outcome[1], history, userMessage, completionID, modelName, created, providedSessionID,
		sessionID, gatewaySessionKey), nil
}

// Non streaming chat completion response of a finished turn, a 502 envelope for a hard failure
func (a *APIServerAdapter) chatCompletionResponse(ctx context.Context, result, usage map[string]any, history []any, userMessage any, completionID string, modelName any, created int64, providedSessionID, sessionID, gatewaySessionKey string) *gateway.WebResponse {
	muted := result["_notification_presentation_suppressed"] == true
	final, _ := result["final_response"].(string)
	finalResponse := gateway.APIResolveMediaToDataURLs(ctx, final)
	completed, isPartial, isFailed, errMsg := resultFlags(result)
	if pyrepr.Truthy(errMsg) {
		errMsg = redactAPIErrorText(errMsg, -1)
	}
	exhausted := budgetExhausted(result)
	finish := finishReason(completed, isPartial, isFailed, errMsg, nil, exhausted)
	echoed := providedSessionID
	if echoed == "" {
		echoed = pyrepr.Str(firstTruthy(result["session_id"], sessionID))
		if _, has := result["session_id"]; has {
			echoed = pyrepr.Str(result["session_id"])
		}
	}
	headers := map[string]string{"X-Hermes-Session-Id": echoed}
	if gatewaySessionKey != "" {
		headers["X-Hermes-Session-Key"] = gatewaySessionKey
	}
	partialFlag := "false"
	if isPartial {
		partialFlag = "true"
	}
	if finalResponse == "" && (isFailed || isPartial) {
		message := "Agent run did not produce a response."
		if pyrepr.Truthy(errMsg) {
			message = pyrepr.Str(errMsg)
		}
		if muted {
			message = ""
		}
		errBody := openAIError(message, "server_error", nil, "agent_incomplete")
		inner, _ := errBody.Get("error")
		inner.(*pyjson.Object).Set("hermes", pyjson.ObjectOf("completed", completed, "partial", isPartial, "failed", isFailed))
		headers["X-Hermes-Completed"] = "false"
		headers["X-Hermes-Partial"] = partialFlag
		return jsonResponse(errBody, http.StatusBadGateway, headers)
	}
	content := finalResponse
	if muted {
		content = ""
	}
	message := pyjson.ObjectOf("role", "assistant", "content", content)
	data := pyjson.ObjectOf("id", completionID, "object", "chat.completion", "created", created, "model", modelName,
		"choices", []any{pyjson.ObjectOf("index", int64(0), "message", message, "finish_reason", finish)},
		"usage", chatUsagePayload(usage))
	if reasoning := turnReasoningText(history, userMessage, result); reasoning != "" && !muted {
		message.Set("reasoning_content", reasoning)
	}
	if isPartial || isFailed || !completed {
		var extrasErr any = errMsg
		if muted {
			extrasErr = ""
		}
		data.Set("hermes", hermesExtras(completed, isPartial, isFailed, extrasErr, finish, exhausted))
		headers["X-Hermes-Completed"] = "false"
		headers["X-Hermes-Partial"] = partialFlag
		if pyrepr.Truthy(errMsg) && !muted {
			headers["X-Hermes-Error"] = redactAPIErrorText(errMsg, 200)
		}
	}
	return jsonResponse(data, http.StatusOK, headers)
}

// History objects as the agent's message maps
func historyMaps(history []any) []map[string]any {
	out := make([]map[string]any, 0, len(history))
	for _, h := range history {
		if m, ok := pyjson.Plain(h).(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
