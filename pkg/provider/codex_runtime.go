package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// CodexSessionDB is the session store surface the runtime reads thread bindings from and queues accounting into
type CodexSessionDB interface {
	GetModelConfigValue(id, key string) (any, bool, error)
	PatchModelConfig(id string, patch map[string]any) error
	QueueTokenCounts(sessionID, source string, counts map[string]any) error
}

// CodexContextCompressor is the context compressor surface the runtime updates after usage and compaction
type CodexContextCompressor interface {
	AwaitingRealUsageAfterCompression() bool
	SetAwaitingRealUsageAfterCompression(on bool)
	UpdateFromResponse(usage map[string]any)
	SetContextLength(n int)
	CompressionCount() int
	SetCompressionCount(n int)
	SetLastCompressionRoughTokens(n int)
	SetLastUsage(promptTokens, completionTokens int)
}

// Optional compressor hooks Python reaches through getattr, absent when the compressor lacks them
type codexUsageLessNoter interface{ NoteUsageLessResponse() }
type codexCompactionRecorder interface{ RecordCompletedCompaction(usedFallback bool) }
type codexCompactionVerifier interface{ SetVerifyCompactionClearedThreshold(on bool) }

// CodexToolProgressExtra carries the keyword arguments of a tool.completed progress callback
type CodexToolProgressExtra struct {
	Duration *float64
	IsError  bool
	Result   *string
}

// CodexRuntimeAgent is every agent field and method the codex app-server runtime reads or writes, a nil func is an unset hook
type CodexRuntimeAgent interface {
	SessionID() string
	Provider() string
	RequestedProvider() string
	Model() string
	BaseURL() string
	APIKey() string
	Platform() string
	ReasoningConfig() map[string]any
	EphemeralSystemPrompt() string
	CachedSystemPrompt() string
	SessionCwd() string
	ValidToolNames() map[string]bool
	ShowCommentary() bool
	HoldStream() bool
	InterruptRequested() bool
	InterruptMessage() any
	ClearInterrupt()
	ItersSinceSkill() int
	SetItersSinceSkill(n int)
	SkillNudgeInterval() int
	CodexSession() *CodexAppServerSession
	SetCodexSession(s *CodexAppServerSession)
	CodexSessionPrompt() *string
	SetCodexSessionPrompt(p *string)
	CodexSessionModelProvider() *string
	SetCodexSessionModelProvider(p *string)
	CodexSessionDB() CodexSessionDB
	SessionDBCreated() bool
	EnsureDBSession() error
	SessionSource() string
	FlushMessagesToSessionDB(messages []map[string]any) state.FlushOutcome
	AddSessionAPICalls(n int)
	AddSessionUsage(key string, value int)
	AddSessionEstimatedCostUSD(usd float64)
	SetSessionCost(status, source string)
	SetLastCompactionInPlace(on bool)
	SetCurrentStreamedAssistantText(text string)
	CompressionCheckpointRequired() bool
	CodexContextCompressor() CodexContextCompressor
	SetUsageAnchor(anchor *UsageAnchor)
	EmitCompactionStatus()
	EmitDiagnosticStatus(text string)
	PublishProviderNativeCompaction()
	EventCallback() func(event string, data map[string]any)
	ToolProgressCallback() func(event, name string, preview any, args any, extra *CodexToolProgressExtra)
	ToolStartCallback() func(callID, name string, args any)
	ToolCompleteCallback() func(callID, name string, args any, result string)
	StreamDeltaHook(attr string) func(text string)
	InterimAssistantMessageHook() func(message map[string]any)
	TouchActivityHook() func(desc string)
	SyncExternalMemoryForTurnHook() func(originalUserMessage, finalResponse any, interrupted bool, messages []map[string]any)
	SpawnBackgroundReviewHook() func(messagesSnapshot []map[string]any, reviewMemory, reviewSkills bool)
	ToolEnv() *tools.Env
}

// Invokes an optional display or debug hook, a panic in it is logged and never ends the turn
func codexCallGuarded(fn func(), present bool, failMsg string) {
	if !present {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(failMsg, "panic", r)
		}
	}()
	fn()
}

// Serialized request bytes and the exception class chain of a request failure, eight links at most
func codexRequestFailureDetails(err error) (*int, string) {
	var bodyBytes *int
	var classes []string
	seen := map[error]bool{}
	current := err
	for current != nil && !codexSeenError(seen, current) && len(seen) < 8 {
		seen[current] = true
		classes = append(classes, pyrepr.TypeName(current))
		if bodyBytes == nil {
			var carrier failedRequestCarrier
			if errors.As(current, &carrier) {
				if req := carrier.FailedRequest(); req != nil && req.Content != nil {
					n := len(req.Content)
					bodyBytes = &n
				}
			}
		}
		current = codexNextError(current)
	}
	return bodyBytes, strings.Join(classes, " <- ")
}

func codexSeenError(seen map[error]bool, err error) (found bool) {
	defer func() {
		if recover() != nil {
			found = false
		}
	}()
	return seen[err]
}

// Cause of an error, the first of a joined error
func codexNextError(err error) error {
	switch e := err.(type) {
	case interface{ Unwrap() error }:
		return e.Unwrap()
	case interface{ Unwrap() []error }:
		if errs := e.Unwrap(); len(errs) > 0 {
			return errs[0]
		}
	}
	return nil
}

// Non negative int of a usage value, bools and unparsable text are zero and a NaN float panics like int() raises
func codexCoerceUsageInt(value any) int {
	switch v := value.(type) {
	case bool:
		return 0
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			panic(fmt.Sprintf("cannot convert float %s to integer", pyrepr.Repr(v)))
		}
		return max(int(v), 0)
	case string:
		if n, ok := codexToolsParseIntText(strings.TrimSpace(v)); ok {
			if i, isInt := pyrepr.Int(n); isInt && i.IsInt64() {
				return max(int(i.Int64()), 0)
			}
		}
		return 0
	}
	if n, ok := pyrepr.Int(value); ok {
		if !n.IsInt64() {
			if n.Sign() < 0 {
				return 0
			}
			return math.MaxInt
		}
		return max(int(n.Int64()), 0)
	}
	return 0
}

// Queues per call accounting for the session DB writer, counts built only once the DB and session exist
func codexQueueTokenCounts(agent CodexRuntimeAgent, failMsg string, failExtra []any, counts func() map[string]any) {
	db := agent.CodexSessionDB()
	if db == nil || agent.SessionID() == "" {
		return
	}
	fail := func(err any) {
		args := append([]any{agent.SessionID()}, failExtra...)
		slog.Debug(pyPercentFormat(failMsg, append(args, pyrepr.Str(err))...))
	}
	defer func() {
		if r := recover(); r != nil {
			fail(r)
		}
	}()
	if !agent.SessionDBCreated() {
		if err := agent.EnsureDBSession(); err != nil {
			fail(err.Error())
			return
		}
	}
	if err := db.QueueTokenCounts(agent.SessionID(), agent.SessionSource(), counts()); err != nil {
		fail(err.Error())
	}
}

// Minimal printf of a logging format with %s and %d placeholders
func pyPercentFormat(format string, args ...any) string {
	var b strings.Builder
	i := 0
	for j := 0; j < len(format); j++ {
		if format[j] == '%' && j+1 < len(format) && (format[j+1] == 's' || format[j+1] == 'd') && i < len(args) {
			b.WriteString(pyrepr.Str(args[i]))
			i++
			j++
			continue
		}
		b.WriteByte(format[j])
	}
	return b.String()
}

// Translates codex token usage into session accounting, a usage less turn still counts as one call
func recordCodexAppServerUsage(agent CodexRuntimeAgent, turn *CodexTurnResult, messages []map[string]any) map[string]any {
	agent.AddSessionAPICalls(1)
	compressor := agent.CodexContextCompressor()
	billing := func(extra map[string]any) map[string]any {
		out := map[string]any{"model": agent.Model(), "billing_provider": agent.Provider(), "billing_base_url": agent.BaseURL(), "api_call_count": 1}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	usage := turn.TokenUsageLast
	if usage == nil || usage.Len() == 0 {
		if compressor != nil && compressor.AwaitingRealUsageAfterCompression() {
			compressor.UpdateFromResponse(map[string]any{})
		}
		if noter, ok := compressor.(codexUsageLessNoter); ok && compressor != nil {
			noter.NoteUsageLessResponse()
		}
		codexQueueTokenCounts(agent, "Codex app-server api-call persistence failed (session=%s): %s", nil,
			func() map[string]any { return billing(map[string]any{"billing_mode": "subscription_included"}) })
		return map[string]any{}
	}
	get := func(k string) any { v, _ := usage.Get(k); return v }
	cacheRead := codexCoerceUsageInt(get("cachedInputTokens"))
	canonical := CanonicalUsage{
		InputTokens: max(0, codexCoerceUsageInt(get("inputTokens"))-cacheRead), OutputTokens: codexCoerceUsageInt(get("outputTokens")),
		CacheReadTokens: cacheRead, ReasoningTokens: codexCoerceUsageInt(get("reasoningOutputTokens")), RequestCount: 1,
		RawUsage: pyjson.Plain(usage).(map[string]any),
	}
	promptTokens := canonical.PromptTokens()
	totalTokens := codexCoerceUsageInt(get("totalTokens"))
	if totalTokens == 0 {
		totalTokens = canonical.TotalTokens()
	}
	tokenKeys := []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"}
	tokenCounts := map[string]int{"input_tokens": canonical.InputTokens, "output_tokens": canonical.OutputTokens,
		"cache_read_tokens": canonical.CacheReadTokens, "cache_write_tokens": canonical.CacheWriteTokens, "reasoning_tokens": canonical.ReasoningTokens}
	usageKeys := append([]string{"prompt_tokens", "completion_tokens", "total_tokens"}, tokenKeys...)
	usageDict := map[string]int{"prompt_tokens": promptTokens, "completion_tokens": canonical.OutputTokens, "total_tokens": totalTokens}
	for k, v := range tokenCounts {
		usageDict[k] = v
	}
	if compressor != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Debug("codex app-server usage update failed", "panic", r)
				}
			}()
			plain := map[string]any{}
			for k, v := range usageDict {
				plain[k] = v
			}
			compressor.UpdateFromResponse(plain)
			if turn.ModelContextWindow > 0 {
				compressor.SetContextLength(turn.ModelContextWindow)
			}
		}()
	}
	if messages != nil {
		rows := make([]*state.Message, len(messages))
		for i, m := range messages {
			rows[i] = state.MessageFromDict(m)
		}
		if anchor := CaptureUsageAnchor(promptTokens, canonical.OutputTokens, rows); anchor != nil {
			agent.SetUsageAnchor(anchor)
		}
	}
	for _, k := range usageKeys {
		agent.AddSessionUsage(k, usageDict[k])
	}
	cost := EstimateUsageCost(context.Background(), agent.Model(), canonical, agent.Provider(), agent.BaseURL(), agent.APIKey())
	var costUSD any
	if cost.AmountUSD != nil {
		f, _ := cost.AmountUSD.Float64()
		costUSD = f
		agent.AddSessionEstimatedCostUSD(f)
	}
	agent.SetSessionCost(cost.Status, cost.Source)
	var billingMode any
	if cost.Status == "included" {
		billingMode = "subscription_included"
	}
	codexQueueTokenCounts(agent, "Codex app-server token persistence failed (session=%s, tokens=%d): %s", []any{totalTokens}, func() map[string]any {
		extra := map[string]any{"estimated_cost_usd": costUSD, "cost_status": cost.Status, "cost_source": cost.Source, "billing_mode": billingMode}
		for k, v := range tokenCounts {
			extra[k] = v
		}
		return billing(extra)
	})
	out := map[string]any{}
	for k, v := range usageDict {
		out[k] = v
	}
	out["last_prompt_tokens"] = promptTokens
	out["estimated_cost_usd"], out["cost_status"], out["cost_source"] = costUSD, cost.Status, cost.Source
	return out
}

// Folds a codex turn's token usage into the agent, compressor and session row
func RecordCodexAppServerUsage(agent CodexRuntimeAgent, turn *CodexTurnResult, messages []map[string]any) map[string]any {
	return recordCodexAppServerUsage(agent, turn, messages)
}

// Records a codex native compaction boundary in session counters without rewriting local transcript rows
func recordCodexAppServerCompaction(agent CodexRuntimeAgent, turn *CodexTurnResult, approxTokens int, force bool) bool {
	if !force && !turn.Compacted {
		return false
	}
	sessionLabel := agent.SessionID()
	if sessionLabel == "" {
		sessionLabel = "none"
	}
	slog.Info(fmt.Sprintf("codex app-server compaction observed: session=%s thread=%s turn=%s force=%s", sessionLabel, turn.ThreadID, turn.TurnID, pyrepr.Str(force)))
	if !force {
		func() {
			defer func() { _ = recover() }()
			agent.EmitCompactionStatus()
		}()
	}
	compressor := agent.CodexContextCompressor()
	compressionCount := 0
	if compressor != nil {
		compressor.SetCompressionCount(compressor.CompressionCount() + 1)
		compressor.SetLastCompressionRoughTokens(approxTokens)
		if recorder, ok := compressor.(codexCompactionRecorder); ok {
			recorder.RecordCompletedCompaction(false)
		} else if verifier, ok := compressor.(codexCompactionVerifier); ok {
			verifier.SetVerifyCompactionClearedThreshold(true)
		}
		if turn.TokenUsageLast == nil || turn.TokenUsageLast.Len() == 0 {
			compressor.SetLastUsage(-1, 0)
			compressor.SetAwaitingRealUsageAfterCompression(true)
		}
		compressionCount = compressor.CompressionCount()
	}
	agent.SetUsageAnchor(nil)
	agent.SetLastCompactionInPlace(false)
	cb := agent.EventCallback()
	codexCallGuarded(func() {
		cb("session:compress", map[string]any{"platform": agent.Platform(), "session_id": agent.SessionID(), "old_session_id": "", "in_place": false,
			"compression_count": compressionCount, "runtime": "codex_app_server", "thread_id": turn.ThreadID, "turn_id": turn.TurnID})
	}, cb != nil, "event_callback error on codex session:compress")
	if !force {
		agent.PublishProviderNativeCompaction()
	}
	return true
}

// Records a server side or forced codex compaction and reports whether one happened
func RecordCodexAppServerCompaction(agent CodexRuntimeAgent, turn *CodexTurnResult, approxTokens int, force bool) bool {
	return recordCodexAppServerCompaction(agent, turn, approxTokens, force)
}

var codexMonotonicStart = time.Now()

// Builds the session on_event hook that fires the agent display callbacks and refreshes its activity clock
func makeCodexAppServerEventBridge(agent CodexRuntimeAgent) func(note any) {
	type startedTool struct {
		name    string
		args    any
		startAt float64
	}
	started := map[string]startedTool{}
	monotonic := func() float64 { return time.Since(codexMonotonicStart).Seconds() }
	fireToolStarted := func(item any) {
		itemID, name := cxIDText(cxOr(cxGet(item, "id"), "")), codexItemToToolName(item)
		args := codexItemToArgs(item)
		if itemID != "" {
			started[itemID] = startedTool{name, args, monotonic()}
		}
		progress := agent.ToolProgressCallback()
		codexCallGuarded(func() { progress("tool.started", name, codexItemToPreview(item), args, nil) }, progress != nil,
			fmt.Sprintf("tool_progress_callback raised on tool.started for %s", name))
		start := agent.ToolStartCallback()
		codexCallGuarded(func() { start(codexStableCallID(item, name), name, args) }, start != nil, fmt.Sprintf("tool_start_callback raised for %s", name))
	}
	fireToolCompleted := func(item any) {
		name := codexItemToToolName(item)
		var prior *startedTool
		if itemID := cxIDText(cxOr(cxGet(item, "id"), "")); itemID != "" {
			if p, ok := started[itemID]; ok {
				prior = &p
				delete(started, itemID)
			}
		}
		var duration *float64
		codexMS := cxGet(item, "durationMs")
		if _, isBool := codexMS.(bool); !isBool && codexMS != nil {
			if f, ok := codexNumber(codexMS); ok && f >= 0 {
				d := f / 1000.0
				duration = &d
			}
		}
		if duration == nil && prior != nil {
			d := monotonic() - prior.startAt
			duration = &d
		}
		result, isError := codexItemCompletionPayload(item)
		progress := agent.ToolProgressCallback()
		codexCallGuarded(func() {
			progress("tool.completed", name, nil, nil, &CodexToolProgressExtra{Duration: duration, IsError: isError, Result: &result})
		}, progress != nil, fmt.Sprintf("tool_progress_callback raised on tool.completed for %s", name))
		var args any
		if prior != nil {
			args = prior.args
		} else {
			args = codexItemToArgs(item)
		}
		complete := agent.ToolCompleteCallback()
		codexCallGuarded(func() { complete(codexStableCallID(item, name), name, args, result) }, complete != nil,
			fmt.Sprintf("tool_complete_callback raised for %s", name))
	}
	fireAgentMessageCompleted := func(item any) {
		text, isStr := cxOr(cxGet(item, "text"), "").(string)
		if isStr && strings.TrimSpace(text) != "" && agent.ShowCommentary() {
			hook := agent.InterimAssistantMessageHook()
			codexCallGuarded(func() { hook(map[string]any{"role": "assistant", "content": text}) }, hook != nil, "_emit_interim_assistant_message raised")
		}
		agent.SetCurrentStreamedAssistantText("")
	}
	onItem := func(params any, completed bool) {
		item := cxGet(params, "item")
		if !cxIsObject(item) {
			return
		}
		itemType, _ := cxOr(cxGet(item, "type"), "").(string)
		if codexToolItemTypes[itemType] {
			if completed {
				fireToolCompleted(item)
			} else {
				fireToolStarted(item)
			}
		} else if completed && itemType == "agentMessage" {
			fireAgentMessageCompleted(item)
		}
	}
	deltaAttrs := map[string]string{}
	for _, pair := range codexTextDeltaMethods {
		deltaAttrs[pair[0]] = "_fire_" + pair[1] + "_delta"
	}
	return func(note any) {
		if !cxIsObject(note) {
			return
		}
		method, _ := cxOr(cxGet(note, "method"), "").(string)
		params := cxGet(note, "params")
		if !cxIsObject(params) {
			params = map[string]any{}
		}
		item := cxGet(params, "item")
		isDelta := codexProgressDeltaMethods[method] && codexDeltaText(params) != ""
		itemType, _ := cxGet(item, "type").(string)
		isItem := (method == "item/started" || method == "item/completed") && cxIsObject(item) && codexProgressItemTypes[itemType]
		if isDelta || isItem {
			touch := agent.TouchActivityHook()
			codexCallGuarded(func() { touch("codex app-server: " + method) }, touch != nil, "_touch_activity raised")
		}
		if attr, ok := deltaAttrs[method]; ok {
			if text := codexDeltaText(params); text != "" {
				hook := agent.StreamDeltaHook(attr)
				codexCallGuarded(func() { hook(text) }, hook != nil, attr+" raised")
			}
			return
		}
		switch method {
		case "item/started":
			onItem(params, false)
		case "item/completed":
			onItem(params, true)
		}
	}
}

// Float of a decoded JSON int or float, false for anything else
func codexNumber(v any) (float64, bool) {
	if f, ok := v.(float64); ok {
		return f, true
	}
	if n, ok := pyrepr.Int(v); ok {
		f, _ := strconv.ParseFloat(n.String(), 64)
		return f, true
	}
	return 0, false
}

// Drops the session so the next turn respawns codex instead of reusing a dead client
func closeCodexSession(agent CodexRuntimeAgent) {
	if s := agent.CodexSession(); s != nil {
		func() {
			defer func() { _ = recover() }()
			s.Close()
		}()
	}
	agent.SetCodexSession(nil)
}

// Reports and clears a user interrupt so a hard stop cannot poison the next turn
func consumeUserInterrupt(agent CodexRuntimeAgent, active bool) (bool, any) {
	interrupted := active && agent.InterruptRequested()
	var message any
	if interrupted {
		message = agent.InterruptMessage()
		agent.ClearInterrupt()
	}
	return interrupted, message
}

// System prompt composition the standard loop sends, the cached prompt then the ephemeral one
func codexDeveloperInstructions(agent CodexRuntimeAgent) string {
	instructions := agent.CachedSystemPrompt()
	if eph := agent.EphemeralSystemPrompt(); eph != "" {
		instructions = strings.TrimSpace(instructions + "\n\n" + eph)
	}
	return instructions
}

const (
	codexThreadIDKey        = "codex_thread_id"
	codexThreadResumeNotice = "Codex thread could not be resumed; starting a new one."
)

// Codex thread id bound to the session row, empty when none is stored
func storedCodexThreadID(agent CodexRuntimeAgent) (string, error) {
	db, sessionID := agent.CodexSessionDB(), agent.SessionID()
	if db == nil || sessionID == "" {
		return "", nil
	}
	v, _, err := db.GetModelConfigValue(sessionID, codexThreadIDKey)
	if err != nil {
		return "", err
	}
	id, _ := v.(string)
	return id, nil
}

// Merges the thread binding into the session row, an empty id clears it and a failed write only logs
func storeCodexThreadID(agent CodexRuntimeAgent, threadID string) {
	db, sessionID := agent.CodexSessionDB(), agent.SessionID()
	if db == nil || sessionID == "" {
		return
	}
	var value any
	if threadID != "" {
		value = threadID
	}
	codexCallGuarded(func() {
		if err := db.PatchModelConfig(sessionID, map[string]any{codexThreadIDKey: value}); err != nil {
			panic(err)
		}
	}, true, "codex thread id could not be stored on the session row")
}

// Starts the thread, dropping a stored thread codex cannot resume and telling the user once
func startCodexThread(agent CodexRuntimeAgent) (string, error) {
	id, err := agent.CodexSession().EnsureStarted()
	var resumeErr *CodexThreadResumeError
	if err == nil || !errors.As(err, &resumeErr) {
		return id, err
	}
	slog.Warn(fmt.Sprintf("%s; starting a new codex thread (session=%s)", resumeErr.Message, agent.SessionID()))
	storeCodexThreadID(agent, "")
	agent.EmitDiagnosticStatus(codexThreadResumeNotice)
	return agent.CodexSession().EnsureStarted()
}

// Post turn bookkeeping mirroring the chat completions loop, returning the usage fields
func finishCodexTurn(agent CodexRuntimeAgent, turn *CodexTurnResult, messages []map[string]any, originalUserMessage any, shouldReviewMemory bool, finalResponse any) map[string]any {
	agent.SetItersSinceSkill(agent.ItersSinceSkill() + turn.ToolIterations)
	recordCodexAppServerCompaction(agent, turn, 0, false)
	usage := recordCodexAppServerUsage(agent, turn, messages)
	interval := agent.SkillNudgeInterval()
	shouldReviewSkills := 0 < interval && interval <= agent.ItersSinceSkill() && agent.ValidToolNames()["skill_manage"]
	if shouldReviewSkills {
		agent.SetItersSinceSkill(0)
	}
	if !turn.Interrupted && turn.Error == "" {
		sync := agent.SyncExternalMemoryForTurnHook()
		codexCallGuarded(func() { sync(originalUserMessage, finalResponse, false, messages) }, sync != nil, "external memory sync raised")
	}
	if turn.FinalText != "" && !turn.Interrupted && (shouldReviewMemory || shouldReviewSkills) {
		review := agent.SpawnBackgroundReviewHook()
		snapshot := append([]map[string]any(nil), messages...)
		codexCallGuarded(func() { review(snapshot, shouldReviewMemory, shouldReviewSkills) }, review != nil, "background review spawn raised")
	}
	return usage
}

// Result shape shared with the chat completions path, partial is the negation of completed
func codexTurnResultOf(interrupted bool, interruptMessage any, messages []map[string]any, apiCalls int, completed bool, errText any, finalResponse any, extra map[string]any) map[string]any {
	out := map[string]any{"final_response": finalResponse, "messages": messages, "api_calls": apiCalls, "completed": completed,
		"partial": !completed, "interrupted": interrupted, "error": errText}
	if cxTruthy(interruptMessage) {
		out["interrupt_message"] = interruptMessage
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// CodexTokenCountDelta maps one queued codex accounting map onto the session store delta, None values left nil
func CodexTokenCountDelta(source string, counts map[string]any) state.TokenCountDelta {
	str := func(key string) *string {
		if s, ok := counts[key].(string); ok {
			return &s
		}
		return nil
	}
	num := func(key string) int64 { return int64(codexCoerceUsageInt(counts[key])) }
	delta := state.TokenCountDelta{InputTokens: num("input_tokens"), OutputTokens: num("output_tokens"), CacheReadTokens: num("cache_read_tokens"),
		CacheWriteTokens: num("cache_write_tokens"), ReasoningTokens: num("reasoning_tokens"), Model: str("model"), CostStatus: str("cost_status"),
		CostSource: str("cost_source"), BillingProvider: str("billing_provider"), BillingBaseURL: str("billing_base_url"), BillingMode: str("billing_mode"),
		APICallCount: codexCoerceUsageInt(counts["api_call_count"]), Source: &source}
	if f, ok := counts["estimated_cost_usd"].(float64); ok {
		delta.EstimatedCostUSD = &f
	}
	return delta
}
