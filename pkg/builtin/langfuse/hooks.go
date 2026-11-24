package langfuse

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// String keyword of a hook call, empty when missing or None
func kwStr(kw map[string]any, key string) string {
	switch v := kw[key].(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return anyutil.String(v)
	}
}

// Keyword of a hook call with its default when missing
func kwOr(kw map[string]any, key string, def any) any {
	if v, ok := kw[key]; ok {
		return v
	}
	return def
}

// Turn scoped pre_llm_call keeping the opted in sender, opening a root only for a request shaped call
func onPreLLMCall(ctx context.Context, kw map[string]any) (any, error) {
	taskID, sessionID, turnID, apiRequestID := kwStr(kw, "task_id"), kwStr(kw, "session_id"), kwStr(kw, "turn_id"), kwStr(kw, "api_request_id")
	senderID := kw["sender_id"]
	sender := ""
	if anyutil.IsTrue(senderID) && endUserAttribution(ctx) {
		sender = strings.TrimSpace(anyutil.String(senderID))
	}
	messages := kw["messages"]
	if _, isList := listItems(messages); !isList {
		if sender != "" && turnID != "" {
			withState(func() { rememberLocked(senderByTurn, turnID, sender) })
		}
		return nil, nil
	}
	client, key := clientAndKey(ctx, taskID, sessionID, turnID, apiRequestID)
	if client == nil {
		return nil, nil
	}
	withState(func() {
		user := sender
		if user == "" {
			user = turnUserLocked(turnID, sessionID)
		}
		getOrStartStateLocked(ctx, client, key, rootArgs{taskID: taskID, sessionID: sessionID, platform: kwStr(kw, "platform"),
			provider: kwStr(kw, "provider"), model: kwStr(kw, "model"), apiMode: kwStr(kw, "api_mode"), messages: messages,
			turnID: turnID, apiRequestID: apiRequestID, userID: user})
	})
	return nil, nil
}

// Records each MoA advisor as its own generation priced at its own model, once per distinct fan out
func emitMoAReferenceGenerations(state *traceState, references any) {
	refs, ok := listItems(references)
	if !ok || len(refs) == 0 {
		return
	}
	rows := []any{}
	for _, r := range refs {
		if !isDict(r) {
			continue
		}
		usage := dget(r, "usage")
		var outputTokens any
		if anyutil.IsTrue(usage) {
			if !isDict(usage) {
				panic(fmt.Sprintf("a mixture of agents reference usage is %s, expected a mapping", errchain.Kind(usage)))
			}
			outputTokens = dget(usage, "output_tokens")
		}
		rows = append(rows, []any{dget(r, "label"), dget(r, "model"), outputTokens})
	}
	fingerprint, err := jsonutil.Encode(rows, jsonutil.SortKeys(), jsonutil.Convert(func(v any) (any, error) { return anyutil.String(v), nil }))
	if err != nil {
		panic(err)
	}
	seen := false
	withState(func() {
		seen = state.moaEmitted[fingerprint]
		state.moaEmitted[fingerprint] = true
	})
	if seen {
		return
	}
	for _, ref := range refs {
		if !isDict(ref) {
			continue
		}
		usage := dget(ref, "usage")
		if !anyutil.IsTrue(usage) {
			usage = jsonutil.NewObject()
		}
		usageDetails := jsonutil.NewObject()
		if isDict(usage) {
			buckets := usageBuckets(func(attr string) any { return dget(usage, attr) })
			for _, k := range buckets.Keys() {
				if n := buckets.Value(k).(int); n != 0 {
					usageDetails.Set(k, n)
				}
			}
		}
		costDetails := jsonutil.NewObject()
		if cost := dget(ref, "cost_usd"); isNumber(cost) || isBool(cost) {
			f, _ := toFloat(cost)
			costDetails.Set("total", f)
		}
		label := dget(ref, "label")
		if !anyutil.IsTrue(label) {
			label = "advisor"
		}
		metadata := jsonutil.ObjectOf("moa_role", "reference", "label", label)
		for _, k := range []string{"provider", "cost_status", "cost_source", "temperature"} {
			if v := dget(ref, k); v != nil {
				metadata.Set(k, v)
			}
		}
		observation := startChildObservation(state, "MoA advisor: "+anyutil.String(label), "generation", nil, metadata, dget(ref, "model"), nil)
		endObservation(observation, endArgs{output: captureContent(dget(ref, "output"), false, nil), usage: usageDetails,
			cost: costDetails, metadata: metadata})
	}
}

// Reports a Python bool
func isBool(v any) bool {
	_, ok := v.(bool)
	return ok
}

// Per request pre_api_request, opening or reusing the turn root and starting this call's generation
func onPreLLMRequest(ctx context.Context, kw map[string]any) (any, error) {
	taskID, sessionID, turnID, apiRequestID := kwStr(kw, "task_id"), kwStr(kw, "session_id"), kwStr(kw, "turn_id"), kwStr(kw, "api_request_id")
	client, key := clientAndKey(ctx, taskID, sessionID, turnID, apiRequestID)
	if client == nil {
		return nil, nil
	}
	model := kwStr(kw, "model")
	if request := kw["request"]; isDict(request) {
		if body := dget(request, "body"); isDict(body) {
			if m, ok := dget(body, "model").(string); ok && m != "" {
				model = m
			}
		}
	}
	platform, providerName, apiMode := kwStr(kw, "platform"), kwStr(kw, "provider"), kwStr(kw, "api_mode")
	apiCallCount := kwOr(kw, "api_call_count", 0)
	inputMessages := coerceRequestMessages(kw["request_messages"], kw["messages"], kw["conversation_history"], kw["user_message"])
	langfuseInput := messagesForLangfuseInput(inputMessages, kw["system_prompt"])
	systemChars := 0
	if len(langfuseInput) > 0 && strEq(dget(langfuseInput[0], "role"), "system") {
		content := dget(langfuseInput[0], "content")
		if !anyutil.IsTrue(content) {
			content = ""
		}
		systemChars = utf8.RuneCountInString(anyutil.String(content))
	}
	reqKey := requestKey(apiCallCount)
	safeBaseURL := tools.RedactSensitiveTextWith(nil, kwStr(kw, "base_url"), tools.RedactOptions{Force: true, URLCredentials: true})
	withState(func() {
		state := getOrStartStateLocked(ctx, client, key, rootArgs{taskID: taskID, sessionID: sessionID, platform: platform,
			provider: providerName, model: model, apiMode: apiMode, messages: inputMessages, turnID: turnID, apiRequestID: apiRequestID,
			userID: turnUserLocked(turnID, sessionID)})
		if previous, ok := state.generations.pop(reqKey); ok {
			endObservation(previous, endArgs{level: "WARNING", statusMessage: "Retried: no response was recorded for this attempt."})
		}
		genMetadata := jsonutil.ObjectOf("provider", providerName, "platform", platform, "api_mode", apiMode, "base_url", safeBaseURL,
			"message_count", kwOr(kw, "message_count", 0), "approx_input_tokens", kwOr(kw, "approx_input_tokens", 0))
		if systemChars != 0 {
			genMetadata.Set("system_prompt_chars", systemChars)
		}
		state.generations.set(reqKey, startChildObservation(state, "LLM call "+anyutil.String(apiCallCount), "generation", langfuseInput, genMetadata,
			model, jsonutil.ObjectOf("api_mode", apiMode, "provider", providerName)))
		state.generationModels.set(reqKey, model)
	})
	return nil, nil
}

// Per request post_api_request and turn post_llm_call, closing the generation and the turn on a final answer
func onPostLLMCall(ctx context.Context, kw map[string]any) (any, error) {
	taskID, sessionID, turnID, apiRequestID := kwStr(kw, "task_id"), kwStr(kw, "session_id"), kwStr(kw, "turn_id"), kwStr(kw, "api_request_id")
	client, key := clientAndKey(ctx, taskID, sessionID, turnID, apiRequestID)
	if client == nil {
		return nil, nil
	}
	servedModel := ""
	if m, ok := kw["response_model"].(string); ok && m != "" {
		servedModel = m
	}
	model, providerName, baseURL, apiMode := kwStr(kw, "model"), kwStr(kw, "provider"), kwStr(kw, "base_url"), kwStr(kw, "api_mode")
	apiCallCount := kwOr(kw, "api_call_count", 0)
	state, generation, startModel := popGeneration(key, apiCallCount)
	if state == nil || generation == nil {
		return nil, nil
	}
	requestedModel := startModel
	if requestedModel == "" {
		requestedModel = model
	}
	if refs := kw["moa_references"]; anyutil.IsTrue(refs) {
		emitMoAReferenceGenerations(state, refs)
	}
	assistantMessage, assistantResponse := kw["assistant_message"], kw["assistant_response"]
	contentChars := kwOr(kw, "assistant_content_chars", 0)
	toolCallCount := kwOr(kw, "assistant_tool_call_count", 0)
	var output *jsonutil.Object
	switch {
	case assistantMessage != nil:
		output = serializeAssistantMessage(assistantMessage)
	case assistantResponse != nil:
		output = jsonutil.ObjectOf("content", captureContent(assistantResponse, false, nil), "reasoning", nil, "tool_calls", []any{})
	default:
		var content any
		if anyutil.IsTrue(contentChars) {
			content = fmt.Sprintf("[%s chars]", anyutil.String(contentChars))
		}
		calls := []any{}
		for i := range max(toInt(toolCallCount), 0) {
			calls = append(calls, jsonutil.ObjectOf("id", fmt.Sprintf("tc_%d", i)))
		}
		output = jsonutil.ObjectOf("content", content, "reasoning", nil, "tool_calls", calls)
	}
	outputCalls, _ := listItems(dget(output, "tool_calls"))
	if len(outputCalls) > 0 {
		withState(func() { state.turnToolCalls = append(state.turnToolCalls, outputCalls...) })
	}
	costModel := model
	if servedModel != "" {
		costModel = servedModel
	}
	response, usage := kw["response"], kw["usage"]
	var usageDetails, costDetails *jsonutil.Object
	switch {
	case objectAttr(response, "usage") != nil:
		usageDetails, costDetails = usageAndCost(ctx, response, providerName, costModel, baseURL, apiMode, nil)
	case isDict(usage) && anyutil.IsTrue(usage):
		usageDetails, costDetails = usageAndCost(ctx, nil, providerName, costModel, baseURL, "", usage)
	default:
		usageDetails, costDetails = jsonutil.NewObject(), jsonutil.NewObject()
	}
	var callCount any = len(outputCalls)
	if len(outputCalls) == 0 {
		callCount = toolCallCount
	}
	genMetadata := jsonutil.ObjectOf("tool_call_count", callCount)
	genMetadata.Merge(durationMeta(kw["api_duration"]))
	if reason := kw["finish_reason"]; anyutil.IsTrue(reason) {
		genMetadata.Set("finish_reason", reason)
	}
	if servedModel != "" && requestedModel != "" && servedModel != requestedModel {
		genMetadata.Set("requested_model", requestedModel)
	}
	var servedAttr any
	if servedModel != "" {
		servedAttr = servedModel
	}
	endObservation(generation, endArgs{output: output, usage: usageDetails, cost: costDetails, metadata: genMetadata, model: servedAttr})
	hasTools := toInt(toolCallCount) > 0
	if anyutil.IsTrue(assistantMessage) {
		hasTools = anyutil.IsTrue(getattr(assistantMessage, "tool_calls"))
	}
	if !hasTools && anyutil.IsTrue(dget(output, "content")) {
		finishTrace(ctx, key, output)
	}
	return nil, nil
}

// pre_tool_call, starting a tool observation keyed by call id or queued by tool name
func onPreToolCall(ctx context.Context, kw map[string]any) (any, error) {
	client, key := clientAndKey(ctx, kwStr(kw, "task_id"), kwStr(kw, "session_id"), kwStr(kw, "turn_id"), kwStr(kw, "api_request_id"))
	if client == nil {
		return nil, nil
	}
	toolName, toolCallID := kwStr(kw, "tool_name"), kwStr(kw, "tool_call_id")
	withState(func() {
		state, ok := traceStates.get(key)
		if !ok {
			return
		}
		o := startChildObservation(state, "Tool: "+toolName, "tool", captureContent(kw["args"], false, nil),
			jsonutil.ObjectOf("tool_name", kwOr(kw, "tool_name", ""), "tool_call_id", kwOr(kw, "tool_call_id", "")), nil, nil)
		if toolCallID != "" {
			state.tools.set(toolCallID, o)
		} else {
			queue, _ := state.pendingTools.get(toolName)
			state.pendingTools.set(toolName, append(queue, o))
		}
	})
	return nil, nil
}

// post_tool_call, closing the matching tool observation with its result and level and backfilling the turn's tool call
func onPostToolCall(ctx context.Context, kw map[string]any) (any, error) {
	key := traceKey(kwStr(kw, "task_id"), kwStr(kw, "session_id"), kwStr(kw, "turn_id"), kwStr(kw, "api_request_id"))
	toolName, toolCallID := kwStr(kw, "tool_name"), kwStr(kw, "tool_call_id")
	var o *observation
	withState(func() {
		state, ok := traceStates.get(key)
		if !ok {
			return
		}
		if toolCallID != "" {
			o, _ = state.tools.pop(toolCallID)
		}
		if o == nil {
			if queue, ok := state.pendingTools.get(toolName); ok && len(queue) > 0 {
				o = queue[0]
				if rest := queue[1:]; len(rest) > 0 {
					state.pendingTools.set(toolName, rest)
				} else {
					state.pendingTools.pop(toolName)
				}
			}
		}
	})
	if o == nil {
		return nil, nil
	}
	args := kw["args"]
	safeResult := captureContent(kw["result"], false, &toolResult{name: toolName, args: args})
	if toolCallID != "" {
		withState(func() {
			state, ok := traceStates.get(key)
			if !ok {
				return
			}
			for i := len(state.turnToolCalls) - 1; i >= 0; i-- {
				tc := state.turnToolCalls[i]
				if !anyutil.Equal(dget(tc, "id"), toolCallID) {
					continue
				}
				for _, target := range []any{tc, dget(tc, "function")} {
					setDictKey(target, "output", safeResult)
				}
				break
			}
		})
	}
	status := kwStr(kw, "status")
	level := map[string]string{"error": "ERROR", "blocked": "WARNING", "cancelled": "WARNING"}[status]
	statusMessage := ""
	if level != "" {
		reason := kw["error_type"]
		if !anyutil.IsTrue(reason) {
			reason = kwOr(kw, "status", "")
		}
		statusMessage = textutil.First(anyutil.String(reason), 200)
	}
	endObservation(o, endArgs{output: safeResult, metadata: jsonutil.ObjectOf("tool_name", kwOr(kw, "tool_name", ""), "args", captureContent(args, true, nil)),
		level: level, statusMessage: statusMessage})
	return nil, nil
}

// Sets a key on a dict value in place
func setDictKey(target any, key string, val any) {
	switch t := target.(type) {
	case *jsonutil.Object:
		if t != nil {
			t.Set(key, val)
		}
	case map[string]any:
		t[key] = val
	}
}

// api_request_error, closing the failed generation as an error and ending the turn when the failure is final
func onAPIRequestError(ctx context.Context, kw map[string]any) (any, error) {
	client, key := clientAndKey(ctx, kwStr(kw, "task_id"), kwStr(kw, "session_id"), kwStr(kw, "turn_id"), kwStr(kw, "api_request_id"))
	if client == nil {
		return nil, nil
	}
	state, generation, _ := popGeneration(key, kwOr(kw, "api_call_count", 0))
	if state == nil {
		return nil, nil
	}
	errInfo := kw["error"]
	if !isDict(errInfo) {
		errInfo = jsonutil.NewObject()
	}
	errorType, errorMessage := "", ""
	if v := dget(errInfo, "type"); anyutil.IsTrue(v) {
		errorType = anyutil.String(v)
	}
	if v := dget(errInfo, "message"); anyutil.IsTrue(v) {
		errorMessage = anyutil.String(v)
	}
	errorMetadata := jsonutil.ObjectOf("error", true, "error_type", errorType, "error_message", captureContent(errorMessage, false, nil))
	var reason any
	if r := kw["reason"]; anyutil.IsTrue(r) {
		reason = anyutil.String(r)
	}
	for _, kv := range []struct {
		key string
		val any
	}{{"status_code", kw["status_code"]}, {"retry_count", kw["retry_count"]}, {"max_retries", kw["max_retries"]},
		{"retryable", kw["retryable"]}, {"reason", reason}} {
		if kv.val != nil {
			errorMetadata.Set(kv.key, kv.val)
		}
	}
	errorMetadata.Merge(durationMeta(kw["api_duration"]))
	if generation != nil {
		failsafe("error-level update", func() {
			label := errorType
			if label == "" {
				label = "api_request_error"
			}
			generation.update(obsFields{level: "ERROR", statusMessage: textutil.First(label, 200)})
		})
		endObservation(generation, endArgs{metadata: errorMetadata})
	}
	if retryable, ok := kw["retryable"].(bool); ok && !retryable {
		finishTrace(ctx, key, jsonutil.ObjectOf("error", errorMetadata))
	} else {
		withState(func() { state.lastUpdated = time.Now() })
	}
	return nil, nil
}

// Session end boundary closing the session's open traces, flushing and shutting the client down only at process exit
func onSessionFinalize(ctx context.Context, kw map[string]any) (any, error) {
	slot := settledSlot(ctx)
	if slot == nil || slot.client == nil {
		return nil, nil
	}
	client := slot.client
	sessionID := kwStr(kw, "session_id")
	fragments := []string{"session:" + sessionID, "task:" + sessionID}
	var keys []string
	withState(func() {
		for _, k := range traceStates.keys {
			if sessionID == "" || k == sessionID || strings.Contains(k, fragments[0]) || strings.Contains(k, fragments[1]) {
				keys = append(keys, k)
			}
		}
	})
	for _, k := range keys {
		finishTrace(ctx, k, nil)
	}
	if kwStr(kw, "reason") != "shutdown" {
		return nil, nil
	}
	failsafe("finalize flush", client.flush)
	failsafe("langfuse shutdown", client.shutdown)
	return nil, nil
}

// subagent_start, one span per delegated child under the parent turn carrying the spawning user
func onSubagentStart(ctx context.Context, kw map[string]any) (any, error) {
	client := getLangfuse(ctx)
	childSessionID := kw["child_session_id"]
	if client == nil || !anyutil.IsTrue(childSessionID) {
		return nil, nil
	}
	withState(func() {
		state := stateForTurn(kwStr(kw, "parent_turn_id"))
		if state == nil {
			return
		}
		metadata := jsonutil.ObjectOf("child_session_id", childSessionID, "child_subagent_id", kw["child_subagent_id"], "child_role", kwOr(kw, "child_role", ""))
		if parent := kw["parent_subagent_id"]; anyutil.IsTrue(parent) {
			metadata.Set("parent_subagent_id", parent)
		}
		child := anyutil.String(childSessionID)
		if user, _ := state.propagated.Get("user_id"); anyutil.IsTrue(user) {
			rememberLocked(userByChildSession, child, anyutil.String(user))
		}
		role := kwStr(kw, "child_role")
		if role == "" {
			role = "delegate"
		}
		state.subagents.set(child, startChildObservation(state, "Subagent: "+role, "span", captureContent(kw["child_goal"], false, nil), metadata, nil, nil))
	})
	return nil, nil
}

// subagent_stop, ending the child's span with its summary, status, duration and tool calls
func onSubagentStop(ctx context.Context, kw map[string]any) (any, error) {
	childSessionID := kw["child_session_id"]
	if !anyutil.IsTrue(childSessionID) {
		return nil, nil
	}
	child := anyutil.String(childSessionID)
	var o *observation
	withState(func() {
		userByChildSession.pop(child)
		state := stateForTurn(kwStr(kw, "parent_turn_id"))
		if state == nil {
			return
		}
		o, _ = state.subagents.pop(child)
	})
	if o == nil {
		return nil, nil
	}
	metadata := jsonutil.ObjectOf("child_role", kwOr(kw, "child_role", ""))
	for _, k := range []string{"child_status", "duration_ms"} {
		if v := kw[k]; anyutil.IsTrue(v) {
			metadata.Set(strings.TrimPrefix(k, "child_"), v)
		}
	}
	if history := kw["tool_call_history"]; isPyList(history) {
		elems, _ := listItems(history)
		metadata.Set("tool_call_count", len(elems))
		metadata.Set("tool_calls", captureContent(history, false, nil))
	}
	endObservation(o, endArgs{output: captureContent(kw["child_summary"], false, nil), metadata: metadata})
	return nil, nil
}

// Reports a Python list value
func isPyList(v any) bool {
	_, ok := listItems(v)
	return ok
}
