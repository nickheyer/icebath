package gatewayapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Prepared responses turn, the admission or plain run and the history it builds on
type responsesContext struct {
	sessionID           string
	userMessage         any
	conversationHistory []any
	instructions        any
	run                 runAgentOptions
	runAgent            func(ctx context.Context, run runAgentOptions) (map[string]any, map[string]any, error)
	terminalReplay      bool
}

// OpenAI Responses API turn, streaming or idempotent non streaming with stored snapshots
func (a *APIServerAdapter) handleResponses(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	if limited := a.concurrencyLimitedResponse(ctx); limited != nil {
		return limited, nil
	}
	gatewaySessionKey, keyErr := a.parseSessionKeyHeader(request)
	if keyErr != nil {
		return keyErr, nil
	}
	raw, err := requestJSON(request)
	if err != nil {
		return invalidRequest("Invalid JSON in request body"), nil
	}
	body, _ := raw.(*jsonutil.Object)
	if body == nil {
		return nil, &gateway.PyError{Kind: "AttributeError", Msg: "expected an object, got " + anyutil.Kind(raw)}
	}
	if body.ValueOr("input", nil) == nil {
		return errorResponse("Missing 'input' field", http.StatusBadRequest, errOpts{}), nil
	}
	previousResponseID := body.ValueOr("previous_response_id", nil)
	conversation := body.ValueOr("conversation", nil)
	store := gateway.APICoerceRequestBool(body.ValueOr("store", nil), true)
	if anyutil.IsTrue(conversation) && anyutil.IsTrue(previousResponseID) {
		return errorResponse("Cannot use both 'conversation' and 'previous_response_id'", http.StatusBadRequest, errOpts{}), nil
	}
	stream := gateway.APICoerceRequestBool(body.ValueOr("stream", nil), false)
	durableKey, durableFP, scope, idempotencyKey, replayed, err := a.responsesDurableReplay(ctx, request, body, stream)
	if err != nil {
		return nil, err
	}
	if replayed != nil {
		return replayed, nil
	}
	rc, errResp, err := prepareResponsesContext(ctx, a, request, body, gatewaySessionKey, durableKey, durableFP, scope, idempotencyKey)
	if err != nil {
		var runtimeErr *state.RuntimeStoreError
		if errors.As(err, &runtimeErr) {
			return storeErrorResponse(err), nil
		}
		return nil, err
	}
	if errResp != nil {
		return errResp, nil
	}
	conversationName := ""
	if anyutil.IsTrue(conversation) {
		conversationName = anyutil.String(conversation)
	}
	responseStore, err := a.currentResponseStore(ctx)
	if err != nil {
		return nil, err
	}
	responseID, createdAt, err := responseIdentity(ctx, responseStore, durableKey)
	if err != nil {
		return nil, err
	}
	responseModel := body.ValueOr("model", a.modelName)
	if durableKey != "" {
		if responseModel, err = responseStore.RequestModel(ctx, durableKey, responseModel); err != nil {
			return nil, err
		}
	}
	if stream {
		q := newStreamQueue()
		run := rc.run
		run.toolProgressCallback = func(eventType, name, preview string, args map[string]any, extra map[string]any) {}
		run.toolStartCallback = func(toolCallID, functionName string, functionArgs map[string]any) {
			var args any = functionArgs
			if functionArgs == nil {
				args = jsonutil.NewObject()
			}
			q.put(streamTagged{"__tool_started__", jsonutil.ObjectOf("tool_call_id", toolCallID, "name", functionName, "arguments", args)})
		}
		run.toolCompleteCallback = func(toolCallID, functionName string, functionArgs map[string]any, result string) {
			var args any = functionArgs
			if functionArgs == nil {
				args = jsonutil.NewObject()
			}
			q.put(streamTagged{"__tool_completed__", jsonutil.ObjectOf("tool_call_id", toolCallID, "name", functionName, "arguments", args,
				"result", result)})
		}
		run.interimAssistantCb = func(text string, alreadyStreamed bool) {
			if !alreadyStreamed && strings.TrimSpace(text) != "" {
				q.put(streamTagged{"__commentary__", jsonutil.ObjectOf("text", text)})
			}
		}
		task, ref, outcome := a.spawnStreamAgentWith(context.WithoutCancel(ctx), q, nil, run, rc.runAgent)
		return a.writeSSEResponses(ctx, request, responseID, responseModel, createdAt, q, task, ref, outcome, rc.conversationHistory,
			rc.userMessage, rc.instructions, conversationName, store, rc.sessionID, gatewaySessionKey, durableKey, durableFP, rc.terminalReplay)
	}
	compute := func() ([2]map[string]any, error) {
		result, usage, err := rc.runAgent(ctx, rc.run)
		return [2]map[string]any{result, usage}, err
	}
	outcome, errResp := a.runIdempotent(ctx, request, body, compute, "responses", responsesFingerprintKeysFor(body), "responses")
	if errResp != nil {
		return errResp, nil
	}
	if durableKey != "" {
		replay, err := responseStore.Get(ctx, durableKey)
		if err != nil {
			return nil, err
		}
		if obj, _ := replay.(*jsonutil.Object); settled(obj) {
			stored, _ := obj.ValueOr("headers", nil).(*jsonutil.Object)
			return jsonResponse(obj.ValueOr("response", nil), http.StatusOK, storedHeaders(stored)), nil
		}
	}
	result, usage := outcome[0], outcome[1]
	final, _ := result["final_response"].(string)
	finalResponse := gateway.APIResolveMediaToDataURLs(ctx, final)
	if finalResponse == "" {
		finalResponse = redactAPIErrorText(anyutil.Or(result["error"], "(No response generated)"), -1)
	}
	fullHistory := buildResponseConversationHistory(rc.conversationHistory, rc.userMessage, result, finalResponse, a.historyToolOutputMaxChars)
	effectiveSessionID := rc.sessionID
	if sid, ok := result["session_id"].(string); ok && sid != "" {
		effectiveSessionID = sid
	}
	start := responseMessagesTurnStartIndex(rc.conversationHistory, rc.userMessage, result)
	identity := ""
	if durableKey != "" {
		identity = responseID
	}
	output, err := extractOutputItems(result, start, identity)
	if err != nil {
		return nil, err
	}
	responseStore, err = a.currentResponseStore(ctx)
	if err != nil {
		return nil, err
	}
	if durableKey != "" {
		if output, err = responseStore.OrderOutput(ctx, durableKey, output); err != nil {
			return nil, err
		}
	}
	status := responseStatus(result)
	data := jsonutil.ObjectOf("id", responseID, "object", "response", "status", status, "created_at", createdAt, "model", responseModel,
		"output", objectsToAny(output), "usage", responsesUsagePayload(usage))
	if status == "failed" {
		data.Set("error", jsonutil.ObjectOf("code", "agent_error", "message", redactAPIErrorText(firstTruthy(result["error"], "The admitted turn failed."), -1)))
	}
	var snapshot *responseSnapshot
	if store {
		snapshot = &responseSnapshot{id: responseID, data: jsonutil.ObjectOf("response", data, "conversation_history", fullHistory,
			"instructions", rc.instructions, "session_id", effectiveSessionID)}
	}
	headers := map[string]string{"X-Icebath-Session-Id": effectiveSessionID}
	if gatewaySessionKey != "" {
		headers["X-Icebath-Session-Key"] = gatewaySessionKey
	}
	var replayRecord *jsonutil.Object
	if durableKey != "" {
		headerObj := jsonutil.NewObject()
		for _, k := range []string{"X-Icebath-Session-Id", "X-Icebath-Session-Key"} {
			if v, ok := headers[k]; ok {
				headerObj.Set(k, v)
			}
		}
		replayRecord = jsonutil.ObjectOf("fingerprint", durableFP, "response", data, "headers", headerObj)
	}
	if snapshot != nil || replayRecord != nil {
		if err := storeNonstreamResponse(ctx, responseStore, snapshot, durableKey, replayRecord, conversationName, rc.terminalReplay); err != nil {
			return nil, err
		}
	}
	return jsonResponse(data, http.StatusOK, headers), nil
}

// Stored response by id
func (a *APIServerAdapter) handleGetResponse(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	if authErr := a.checkAuth(ctx, request); authErr != nil {
		return authErr, nil
	}
	responseID := request.MatchInfo["response_id"]
	store, err := a.currentResponseStore(ctx)
	if err != nil {
		return nil, err
	}
	stored, err := store.Get(ctx, responseID)
	if err != nil {
		return nil, err
	}
	obj, _ := stored.(*jsonutil.Object)
	if obj == nil {
		return errorResponse("Response not found: "+responseID, http.StatusNotFound, errOpts{}), nil
	}
	return jsonResponse(obj.ValueOr("response", nil), http.StatusOK, nil), nil
}

// Deletes a stored response
func (a *APIServerAdapter) handleDeleteResponse(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	if authErr := a.checkAuth(ctx, request); authErr != nil {
		return authErr, nil
	}
	responseID := request.MatchInfo["response_id"]
	store, err := a.currentResponseStore(ctx)
	if err != nil {
		return nil, err
	}
	deleted, err := store.Delete(ctx, responseID)
	if err != nil {
		return nil, err
	}
	if !deleted {
		return errorResponse("Response not found: "+responseID, http.StatusNotFound, errOpts{}), nil
	}
	return jsonResponse(jsonutil.ObjectOf("id", responseID, "object", "response", "deleted", true), http.StatusOK, nil), nil
}

// Stored responses transcript without duplicated history, a compressed one stored as is
func buildResponseConversationHistory(history []any, userMessage any, result map[string]any, finalResponse any, toolOutputMaxChars int) []any {
	prior := append([]any(nil), history...)
	var currentUser any = jsonutil.ObjectOf("role", "user", "content", userMessage)
	if row, _ := jsonutil.AsMap(result["_turn_user_row"]); row != nil {
		obj, _ := jsonutil.AsObject(row)
		currentUser = obj.Clone()
	}
	var out []any
	if agentMessages, ok := result["messages"].([]any); ok && len(agentMessages) > 0 {
		turnStart := responseMessagesTurnStartIndex(history, userMessage, result)
		if turnStart != 0 || anyutil.IsTrue(result["_compressed"]) {
			out = append([]any(nil), agentMessages...)
		} else {
			out = append(append(prior, currentUser), agentMessages...)
		}
	} else {
		out = append(prior, currentUser, jsonutil.ObjectOf("role", "assistant", "content", finalResponse))
	}
	return capHistoryToolOutputs(out, toolOutputMaxChars)
}

// Index where this turn starts in a transcript shaped result
func responseMessagesTurnStartIndex(history []any, userMessage any, result map[string]any) int {
	var resultAny any
	if result != nil {
		resultAny = result
	}
	return gateway.APIResponseTurnStartIndex(history, userMessage, resultAny)
}

// This turn's assistant and tool messages in client safe shape, hidden handoffs left out
func (a *APIServerAdapter) turnTranscriptMessages(history []map[string]any, userMessage any, result map[string]any) []any {
	agentMessages, ok := result["messages"].([]any)
	if !ok || len(agentMessages) == 0 {
		return []any{}
	}
	historyAny := make([]any, len(history))
	for i, h := range history {
		historyAny[i] = h
	}
	start := responseMessagesTurnStartIndex(historyAny, userMessage, result)
	out := []any{}
	for _, raw := range agentMessages[min(start, len(agentMessages)):] {
		m, _ := jsonutil.AsMap(raw)
		if m == nil || (m["role"] != "assistant" && m["role"] != "tool") {
			continue
		}
		obj, _ := jsonutil.AsObject(m)
		projected := messageResponse(obj)
		if kind, _ := projected.Get("display_kind"); kind != "hidden" {
			out = append(out, projected)
		}
	}
	return out
}

// Output items of a result from the turn start, ids made durable when a response id is given
func extractOutputItems(result map[string]any, startIndex int, responseID string) ([]*jsonutil.Object, error) {
	items := []*jsonutil.Object{}
	messages, _ := result["messages"].([]any)
	if startIndex > 0 && startIndex <= len(messages) {
		messages = messages[startIndex:]
	}
	for _, raw := range messages {
		msg, _ := jsonutil.AsMap(raw)
		if msg == nil {
			return nil, &gateway.PyError{Kind: "AttributeError", Msg: "expected an object, got " + anyutil.Kind(raw)}
		}
		role := msg["role"]
		if role == "assistant" {
			if reasoning, ok := msg["reasoning"].(string); ok && strings.TrimSpace(reasoning) != "" {
				items = append(items, reasoningItem(reasoning))
			}
		}
		if calls, _ := msg["tool_calls"].([]any); role == "assistant" && len(calls) > 0 {
			for _, rawCall := range calls {
				tc, _ := jsonutil.AsMap(rawCall)
				fn, _ := jsonutil.AsMap(tc["function"])
				name, arguments := any(""), any("")
				if fn != nil {
					if v, ok := fn["name"]; ok {
						name = v
					}
					if v, ok := fn["arguments"]; ok {
						arguments = v
					}
				}
				callID := any("")
				if v, ok := tc["id"]; ok {
					callID = v
				}
				items = append(items, jsonutil.ObjectOf("id", "fc_"+hexUUID()[:24], "type", "function_call", "status", "completed",
					"name", name, "arguments", arguments, "call_id", callID))
			}
		} else if role == "tool" {
			callID, content := any(""), any("")
			if v, ok := msg["tool_call_id"]; ok {
				callID = v
			}
			if v, ok := msg["content"]; ok {
				content = v
			}
			items = append(items, jsonutil.ObjectOf("id", "fco_"+hexUUID()[:24], "type", "function_call_output", "status", "completed",
				"call_id", callID, "output", content))
		}
	}
	final := result["final_response"]
	if !anyutil.IsTrue(final) {
		final = redactAPIErrorText(anyutil.Or(result["error"], "(No response generated)"), -1)
	}
	items = append(items, messageItem(final))
	if responseID != "" {
		return gateway.APIIdentifyOutput(items, responseID)
	}
	return items, nil
}
