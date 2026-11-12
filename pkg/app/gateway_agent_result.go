package app

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// User row the turn adds to history, the CLI's staged row when it staged one
func (a *GatewayAgent) userRow(userMessage any) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingCLIUserMessage != nil {
		return a.pendingCLIUserMessage
	}
	return map[string]any{"role": "user", "content": userMessage}
}

// Ends a turn the owner consumed as a typed voice stop, the history unchanged
func (a *GatewayAgent) voiceStopped(history []map[string]any) map[string]any {
	a.giveBackTurnRoute()
	a.emitPrint(i18n.T(a.ctx, "cli.voice.stop_phrase_typed", "", nil))
	out := gatewayResultDefaults()
	out["messages"] = history
	out["voice_stopped"] = true
	out["session_id"] = a.SessionID()
	return out
}

// Result of a turn the owner never saw, the CLI keeping its local state for the resend
func (a *GatewayAgent) unadmitted(history []map[string]any, userMessage any, errText string) map[string]any {
	a.giveBackTurnRoute()
	out := a.failed(history, userMessage, errText, "gateway_connection", false)
	out["messages"] = history
	out["admitted"] = false
	return out
}

// Failed turn result, a gateway_connection reason making the CLI show the error as written
func (a *GatewayAgent) failed(history []map[string]any, userMessage any, errText, failureReason string, retryable bool) map[string]any {
	out := gatewayResultDefaults()
	messages := append(append([]map[string]any{}, history...), a.userRow(userMessage))
	out["final_response"], out["messages"], out["failed"], out["completed"], out["error"], out["session_id"] = "", messages, true, false, errText, a.SessionID()
	if failureReason != "" {
		out["failure_reason"], out["failure_retryable"] = failureReason, retryable
	}
	return out
}

// Turn result built from the completion and the admission's receipt, the end of turn stats applied
func (a *GatewayAgent) result(ctx context.Context, history []map[string]any, userMessage any, complete map[string]any, turn *gatewayTurn) map[string]any {
	receipt, _ := complete["_receipt"].(map[string]any)
	if len(receipt) > 0 {
		complete = map[string]any{"outcome": receipt["outcome"]}
	} else if pyrepr.Truthy(turn.admission) {
		raw, err := a.connection.CallWith(ctx, "prompt.receipt", map[string]any{"session_id": turn.sessionID, "admission_id": turn.admission,
			"include_result": true}, gatewaycli.CallOptions{Abandon: a.lost(turn)})
		if err != nil {
			if !isGatewayClientError(err) {
				panic(err)
			}
			slog.Debug(fmt.Sprintf("prompt.receipt failed: %s", err.Error()))
		}
		receipt, _ = pyjson.Plain(raw).(map[string]any)
	}
	stored, _ := receipt["result"].(map[string]any)
	stored = copyMap(stored)
	usage, _ := receipt["usage"].(map[string]any)
	usage = copyMap(usage)
	status := complete["status"]
	if !pyrepr.Truthy(status) {
		status = complete["outcome"]
	}
	out := gatewayResultDefaults()
	for k, v := range stored {
		out[k] = v
	}
	if _, has := stored["final_response"]; !has {
		switch {
		case pyrepr.Truthy(complete["text"]):
			out["final_response"] = complete["text"]
		case pyrepr.Truthy(complete["content"]):
			out["final_response"] = complete["content"]
		default:
			out["final_response"] = ""
		}
	}
	if status == "interrupted" || status == "cancelled" || complete["outcome"] == "cancelled" {
		out["interrupted"], out["completed"] = true, false
	} else if (status == "error" || status == "failed") && len(stored) == 0 {
		out["failed"], out["completed"] = true, false
	}
	suffix, hasSuffix := stored["messages"].([]any)
	var messages []map[string]any
	if hasSuffix && pyrepr.Truthy(stored["_compressed"]) {
		messages = anyRows(suffix)
	} else {
		messages = append(append(append([]map[string]any{}, history...), a.userRow(userMessage)), anyRows(suffix)...)
	}
	if len(suffix) == 0 && pyrepr.Truthy(out["final_response"]) && !pyrepr.Truthy(out["failed"]) {
		messages = append(messages, map[string]any{"role": "assistant", "content": out["final_response"]})
	}
	out["messages"] = messages
	out["session_id"] = a.SessionID()
	if !a.owns(turn) {
		return out
	}
	calls, counted := pyIntValue(stored["api_calls"])
	a.mu.Lock()
	a.sessionMessages = messages
	if counted && !pyrepr.Truthy(out["interrupted"]) {
		a.turnAPICalls = &calls
	} else {
		a.turnAPICalls = nil
	}
	a.mu.Unlock()
	a.applyUsage(ctx, stored, usage, turn)
	return out
}

// Dict rows of a decoded list, non dict items dropped
func anyRows(list []any) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if row, isDict := item.(map[string]any); isDict {
			out = append(out, row)
		}
	}
	return out
}

// Completes after a replay gap what each consumer did not get of the reply, never twice
func (a *GatewayAgent) paintOwed(turn *gatewayTurn, out, complete map[string]any, streamCallback func(string)) {
	var reply any = out["final_response"]
	if pyrepr.Truthy(out["response_transformed"]) {
		if pre, isStr := out["pre_transform_response"].(string); isStr {
			reply = pre
		}
	}
	text, isStr := reply.(string)
	if pyrepr.Truthy(out["failed"]) || !isStr || pystr.Strip(text) == "" {
		return
	}
	reused := pyrepr.Truthy(out["response_previewed"]) || pyrepr.Truthy(out["response_reused"]) || pyrepr.Truthy(complete["response_reused"])
	seen := map[string]bool{pystr.Strip(turn.shownText()): true, pystr.Strip(turn.spokenText()): true}
	for k := range turn.seen {
		seen[k] = true
	}
	previewed := reused && seen[pystr.Strip(text)]
	out["response_previewed"] = previewed
	if previewed {
		return
	}
	if cb := a.opts.StreamDeltaCallback; cb != nil {
		if owed := replayGapOwedReply(turn.shownText(), text); owed != "" {
			cb(owed)
		}
	}
	if streamCallback != nil {
		if owed := replayGapOwedReply(turn.spokenText(), text); owed != "" {
			streamCallback(owed)
		}
	}
}

// Rebuilds the usage anchor over this viewer's transcript so the bar shows the gateway's context figure
func (a *GatewayAgent) anchorDisplay(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	messages, tokens := a.sessionMessages, a.contextTokens
	if tokens <= 0 || len(messages) == 0 || a.generation != nil {
		a.turnBaseUsageAnchor = nil
		return
	}
	lastUser := len(messages) - 1
	if ask := a.turnAskIndex; ask != nil && *ask >= 0 && *ask < len(messages) && messages[*ask]["role"] == "user" {
		lastUser = *ask
	} else {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i]["role"] == "user" {
				lastUser = i
				break
			}
		}
	}
	calls, turnBase := a.counters["session_api_calls"], a.turnCallsBase
	madeCall := calls > turnBase || (0 < calls && calls < turnBase)
	if a.turnAPICalls != nil {
		madeCall = *a.turnAPICalls > 0
	}
	base := lastUser
	if madeCall {
		base = lastUser + 1
	}
	if base <= 0 {
		a.turnBaseUsageAnchor = nil
		return
	}
	delta := messages[base:]
	if len(delta) > 0 && delta[0]["role"] == "assistant" {
		delta = delta[1:]
	}
	estimate := 0
	if len(delta) > 0 {
		estimate = provider.EstimateMessagesTokens(agent.RowsToMessages(delta), provider.CurrentImageTokenCost(ctx), false)
	}
	a.turnBaseUsageAnchor = provider.CaptureUsageAnchor(max(1, tokens-estimate), 0, agent.RowsToMessages(messages[:base]))
}

// Reads session.stats into the same figures session.usage sets, false when unavailable
func (a *GatewayAgent) refreshStats(turn *gatewayTurn) bool {
	o := gatewaycli.CallOptions{Timeout: 10 * time.Second}
	if turn != nil {
		o.Abandon = a.lost(turn)
	}
	raw, err := a.connection.CallWith(a.ctx, "session.stats", map[string]any{"session_id": a.SessionID()}, o)
	if err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		slog.Debug(fmt.Sprintf("session.stats unavailable: %s", err.Error()))
		return false
	}
	stats, _ := pyjson.Plain(raw).(map[string]any)
	if nested, isDict := stats["usage"].(map[string]any); isDict {
		stats = nested
	}
	if _, ok := pyIntValue(stats["api_calls"]); !ok {
		return false
	}
	if turn == nil || a.owns(turn) {
		a.state().ApplyUsage(a.ctx, stats)
	}
	return true
}

// Applies the session stats at turn end, falling back to the receipt's figures without the verb
func (a *GatewayAgent) applyUsage(ctx context.Context, stored, receiptUsage map[string]any, turn *gatewayTurn) {
	if a.refreshStats(turn) || !a.owns(turn) {
		return
	}
	usage := map[string]any{}
	for _, pair := range gatewayCounterFromUsage {
		if n, ok := pyIntValue(stored[pair[1]]); ok {
			usage[pair[1]] = n
		}
	}
	a.mu.Lock()
	counters := copyCounters(a.counters)
	a.mu.Unlock()
	for _, key := range []string{"input_tokens", "output_tokens"} {
		if _, has := usage[key]; !has {
			if n, ok := pyIntValue(receiptUsage[key]); ok {
				usage[key] = counters["session_"+key] + n
			}
		}
	}
	if _, has := usage["prompt_tokens"]; !has {
		usage["prompt_tokens"] = usage["input_tokens"]
	}
	if _, has := usage["completion_tokens"]; !has {
		usage["completion_tokens"] = usage["output_tokens"]
	}
	prompt, promptOK := pyIntValue(usage["prompt_tokens"])
	completion, completionOK := pyIntValue(usage["completion_tokens"])
	if _, has := usage["total_tokens"]; !has && promptOK && completionOK {
		usage["total_tokens"] = prompt + completion
	}
	if n, ok := pyIntValue(stored["api_calls"]); ok {
		usage["api_calls"] = counters["session_api_calls"] + n
	}
	for _, key := range []string{"last_prompt_tokens", "context_length", "model"} {
		if pyrepr.Truthy(receiptUsage[key]) || pyrepr.Truthy(stored[key]) {
			if pyrepr.Truthy(receiptUsage[key]) {
				usage[key] = receiptUsage[key]
			} else {
				usage[key] = stored[key]
			}
		}
	}
	a.applySessionUsage(ctx, usage)
}

func copyCounters(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Session counters, context occupancy and latency history for the status bar
func (a *GatewayAgent) StatusBarUsage(ctx context.Context) agent.StatusBarUsage {
	a.mu.Lock()
	defer a.mu.Unlock()
	output := make([]int, len(a.apiOutputHistory))
	for i, v := range a.apiOutputHistory {
		output[i] = int(v)
	}
	usage := agent.StatusBarUsage{Counters: copyCounters(a.counters), HasCompressor: true,
		LatencyHistory: append([]float64{}, a.apiLatencyHistory...), OutputHistory: output}
	compressor := a.compressor
	contextTokens := max(0, compressor.lastPromptTokens)
	real := compressor.lastRealPromptTokens
	usage.ContextEstimated = real != nil && compressor.lastPromptTokens > 0 && compressor.lastPromptTokens != *real
	if anchor := a.turnBaseUsageAnchor; anchor != nil {
		messages := agent.RowsToMessages(a.sessionMessages)
		if anchored, ok := provider.AnchoredContextTokens(ctx, messages, anchor, false); ok && anchored > 0 {
			contextTokens = anchored
			delta := messages[anchor.BaseCount:]
			if len(delta) > 0 && delta[0] != nil && delta[0].Role == "assistant" {
				delta = delta[1:]
			}
			usage.ContextEstimated = len(delta) > 0
		}
	}
	contextLength := max(0, compressor.contextLength)
	if contextLength != 0 {
		contextTokens = min(contextTokens, contextLength)
	}
	usage.ContextTokens, usage.ContextLength = contextTokens, contextLength
	var pin any
	if compressor.configContextLength != nil {
		pin = *compressor.configContextLength
	}
	usage.ContextPinned = agent.IsContextPinned(contextLength, pin)
	usage.Compressions = compressor.compressionCount
	if contextLength != 0 {
		pct := max(0, min(100, int(math.RoundToEven(float64(contextTokens)/float64(contextLength)*100))))
		usage.ContextPercent = &pct
	}
	return usage
}

// Context window the config pins, nil when model.context_length is unset
func (a *GatewayAgent) ConfigContextLength() *int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.configContextLength
}

// Installs the function status lines print through
func (a *GatewayAgent) SetPrintFn(fn func(string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.printFn = fn
}

// Installs the callback a mid turn control update calls
func (a *GatewayAgent) SetControlUpdateCallback(fn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.controlUpdateCallback = fn
}

// Marks the admissions this agent sends as finite, the one shot headless runs
func (a *GatewayAgent) SetFinite(finite bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.finite = finite
}

// Silences or restores status output
func (a *GatewayAgent) SetSuppressStatusOutput(suppress bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.suppressStatusOutput = suppress
}

// Stages the CLI's user row for the next turn, nil to clear it
func (a *GatewayAgent) SetPendingCLIUserMessage(row map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pendingCLIUserMessage = row
}

// Marks the next turn as a voice turn
func (a *GatewayAgent) SetVoiceTurnPending(pending bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.voiceTurnPending = pending
}

// Transcript rows of the last settled turn
func (a *GatewayAgent) SessionMessages() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionMessages
}

// Session effort word for a reasoning config, empty for no override
func effortWord(config map[string]any) string {
	if config == nil {
		return ""
	}
	if enabled, has := config["enabled"]; has && enabled == false {
		return "none"
	}
	effort, _ := config["effort"].(string)
	return effort
}

// Rate limit state rebuilt from its serialized dict, nil for anything else
func rateLimitStateOf(data any) *provider.RateLimitState {
	dict, isDict := data.(map[string]any)
	if !isDict {
		return nil
	}
	bucket := func(name string) provider.RateLimitBucket {
		raw, _ := dict[name].(map[string]any)
		var b provider.RateLimitBucket
		if n, ok := pyIntValue(raw["limit"]); ok {
			b.Limit = n
		}
		if n, ok := pyIntValue(raw["remaining"]); ok {
			b.Remaining = n
		}
		if f, ok := pyNumber(raw["reset_seconds"]); ok {
			b.ResetSeconds = f
		}
		if f, ok := pyNumber(raw["captured_at"]); ok && f != 0 {
			b.CapturedAt = time.Unix(0, int64(f*float64(time.Second)))
		}
		return b
	}
	state := &provider.RateLimitState{RequestsMin: bucket("requests_min"), RequestsHour: bucket("requests_hour"),
		TokensMin: bucket("tokens_min"), TokensHour: bucket("tokens_hour"), Provider: strOrEmpty(dict["provider"])}
	if f, ok := pyNumber(dict["captured_at"]); ok && f != 0 {
		state.CapturedAt = time.Unix(0, int64(f*float64(time.Second)))
	}
	return state
}

// Indented background review line of a review.summary frame, empty for an empty one
func reviewSummaryLine(payload any) string {
	dict, _ := payload.(map[string]any)
	text := pystr.Strip(strOrEmpty(dict["text"]))
	if text == "" {
		return ""
	}
	return "  " + text
}

// Handler of a mixture of agents frame, its index and count under the keys the CLI reads
func moaHandler(kind string) func(a *GatewayAgent, payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	return func(a *GatewayAgent, payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
		cb := a.opts.ToolProgressCallback
		if cb == nil {
			return
		}
		extra := map[string]any{}
		for k, v := range payload {
			switch k {
			case "label", "text", "name", "preview", "index", "count":
			default:
				extra[k] = v
			}
		}
		if payload["index"] != nil {
			extra["moa_index"] = payload["index"]
		}
		if payload["count"] != nil {
			extra["moa_count"] = payload["count"]
		}
		name := payload["label"]
		if !pyrepr.Truthy(name) {
			name = payload["name"]
		}
		preview := payload["text"]
		if !pyrepr.Truthy(preview) {
			preview = payload["preview"]
		}
		cb(kind, noneOrStr(name), noneOrStr(preview), nil, extra)
	}
}

// Clarify panel answer to a clarify server request's whole question set
func clarifyReply(callback func(questions []any) map[string]any, params map[string]any) map[string]any {
	var questions []any
	rawQuestions, _ := params["questions"].([]any)
	for _, raw := range rawQuestions {
		q, isDict := raw.(map[string]any)
		if !isDict || !pyrepr.Truthy(q["qid"]) {
			continue
		}
		choices, _ := q["choices"].([]any)
		questions = append(questions, map[string]any{"qid": q["qid"], "question": strOrEmpty(q["question"]),
			"choices": append([]any{}, choices...), "multi_select": pyrepr.Truthy(q["multi_select"])})
	}
	if callback == nil || len(questions) == 0 {
		return map[string]any{"outcome": "cancelled"}
	}
	reply := callback(questions)
	if reply == nil {
		return map[string]any{"outcome": "cancelled"}
	}
	answers := map[string]any{}
	rawAnswers, _ := reply["answers"].(map[string]any)
	for qid, value := range rawAnswers {
		switch value.(type) {
		case string, nil:
			answers[qid] = value
		default:
			dumped, err := pyjson.Dumps(value)
			if err != nil {
				panic(err)
			}
			answers[qid] = dumped
		}
	}
	out := map[string]any{"answers": answers}
	switch reply["outcome"] {
	case "submitted", "timed_out", "cancelled":
		out["outcome"] = reply["outcome"]
	}
	return out
}

// Manual compression on the owner as session.mutate compress, returning the gateway's compacted transcript
func (a *GatewayAgent) CompressContext(ctx context.Context, messages []map[string]any, systemMessage *string, focusTopic *string,
	verbatimTail []map[string]any) ([]map[string]any, *string, error) {
	payload := map[string]any{}
	if focusTopic != nil && *focusTopic != "" {
		payload["focus"] = *focusTopic
	}
	if len(verbatimTail) > 0 {
		users := 0
		for _, row := range verbatimTail {
			if row["role"] == "user" {
				users++
			}
		}
		payload["partial"], payload["keep_last"] = true, max(1, users)
	}
	a.emitPrint(agent.CompactionStatus)
	a.mu.Lock()
	a.compressionSummary = nil
	a.mu.Unlock()
	sessionID := a.SessionID()
	snapshot, err := a.connection.Call(ctx, "session.resume", map[string]any{"session_id": sessionID})
	if err != nil {
		return nil, nil, err
	}
	revision, generation, err := snapshotFence(snapshot)
	if err != nil {
		return nil, nil, err
	}
	raw, err := a.connection.CallWith(ctx, "session.mutate", map[string]any{"session_id": sessionID, "request_id": uuidHex(),
		"expected_revision": revision, "expected_generation": generation, "operation": "compress", "payload": payload},
		gatewaycli.CallOptions{Timeout: gateway.CompressRPCTimeout})
	if err != nil {
		return nil, nil, err
	}
	result, _ := pyjson.Plain(raw).(map[string]any)
	if summary, isDict := result["summary"].(map[string]any); isDict {
		a.mu.Lock()
		a.compressionSummary = summary
		a.mu.Unlock()
	}
	if result["status"] == "noop" {
		return append([]map[string]any{}, messages...), systemMessage, nil
	}
	for _, row := range verbatimTail {
		row[state.DBPersistedMarker] = true
	}
	after, err := a.connection.Call(ctx, "session.resume", map[string]any{"session_id": sessionID})
	if err != nil {
		return nil, nil, err
	}
	a.state().AdoptSnapshot(after)
	a.mu.Lock()
	a.compressor.compressionCount++
	a.compressor.lastPromptTokens = -1
	a.turnBaseUsageAnchor = nil
	a.mu.Unlock()
	afterDict, _ := pyjson.Plain(after).(map[string]any)
	rows, _ := afterDict["messages"].([]any)
	var out []map[string]any
	for _, row := range anyRows(rows) {
		if pyrepr.Truthy(row["archived"]) {
			continue
		}
		kept := copyMap(row)
		delete(kept, "row_id")
		out = append(out, kept)
	}
	return out, systemMessage, nil
}

// Owner's report of the last manual compression, nil when it sent none
func (a *GatewayAgent) CompressionSummary() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.compressionSummary
}

// Credits capture of the session's route, created once and following the session id
func (a *GatewayAgent) CreditsCapture() *provider.CreditsCapture {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.credits == nil {
		tree, err := config.LoadReadonly(a.ctx, home.Dir(a.ctx), os.LookupEnv)
		if err != nil {
			tree = nil
		}
		a.credits = provider.NewCreditsCapture(tree, a.provider, a.model, a.baseURL, a.sessionID)
		return a.credits
	}
	a.credits.SetRoute(a.provider, a.model, a.baseURL)
	a.credits.SessionID = a.sessionID
	return a.credits
}

// Binds one foreground notification config for the turn, restored by the returned func
func (a *GatewayAgent) NotificationPolicySnapshot(cfg map[string]any) func() {
	a.mu.Lock()
	saved := a.notificationConfig
	a.notificationConfig = cfg
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.notificationConfig = saved
		a.mu.Unlock()
	}
}

// Foreground notification config bound for the running turn, nil outside one
func (a *GatewayAgent) NotificationConfig() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.notificationConfig
}

// Whether the running notification turn mutes its reply on every surface
func (a *GatewayAgent) MuteNotificationReply() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.muteNotificationReply
}

// Freezes a muted turn's presentation callbacks and status output, restored by the returned func
func (a *GatewayAgent) NotificationTurn(ctx context.Context, muted bool, sessionID string) (context.Context, func()) {
	if !muted {
		return ctx, func() {}
	}
	a.mu.Lock()
	saved := a.opts
	suppressed, mutedReply := a.suppressStatusOutput, a.muteNotificationReply
	a.opts.StreamDeltaCallback, a.opts.ToolProgressCallback, a.opts.ToolCompleteCallback = nil, nil, nil
	a.opts.ToolGenCallback, a.opts.ReactionCallback = nil, nil
	a.suppressStatusOutput, a.muteNotificationReply = true, true
	a.mu.Unlock()
	reasoning := a.ReasoningCallback()
	a.SetReasoningCallback(nil)
	return agent.WithMutedSurface(ctx, sessionID), func() {
		a.mu.Lock()
		a.opts.StreamDeltaCallback, a.opts.ToolProgressCallback, a.opts.ToolCompleteCallback = saved.StreamDeltaCallback, saved.ToolProgressCallback, saved.ToolCompleteCallback
		a.opts.ToolGenCallback, a.opts.ReactionCallback = saved.ToolGenCallback, saved.ReactionCallback
		a.suppressStatusOutput, a.muteNotificationReply = suppressed, mutedReply
		a.mu.Unlock()
		a.SetReasoningCallback(reasoning)
	}
}

// Sets whether status output stays quiet
func (a *GatewayAgent) SetQuietMode(quiet bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.quietMode = quiet
}

// Drops every presentation callback that writes to stdout, the one shot quiet run printing only the reply
func (a *GatewayAgent) silencePresentation() {
	a.mu.Lock()
	a.opts.StreamDeltaCallback, a.opts.ToolGenCallback, a.opts.ToolProgressCallback, a.opts.ToolCompleteCallback = nil, nil, nil, nil
	a.mu.Unlock()
	a.SetReasoningCallback(nil)
}

// Routes reply deltas into cb, the stream json emitter's text sink
func (a *GatewayAgent) SetStreamDeltaCallback(cb any) {
	fn, _ := cb.(func(any))
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opts.StreamDeltaCallback = fn
}

// Routes tool progress into cb, the stream json emitter's tool sink
func (a *GatewayAgent) SetToolProgressCallback(cb any) {
	fn, _ := cb.(func(eventType, functionName, preview string, functionArgs map[string]any, extra map[string]any))
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opts.ToolProgressCallback = fn
}
