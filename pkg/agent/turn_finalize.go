package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var verificationContinuationFlags = []string{"_verification_stop_synthetic", "_pre_verify_synthetic"}

var sentenceEnd = map[string]bool{".": true, "!": true, "?": true, "。": true, "！": true, "？": true, "`": true, ")": true}

const pendingToolResultReply = "No reply: the turn stopped while a tool result was still pending. Send `continue` to let the model summarize."

// True when an assistant row has no visible text, a blank final or a tool only row
func assistantRowMissingVisibleText(m *state.Message) bool {
	if m == nil || m.Role != "assistant" {
		return false
	}
	return pystr.Strip(state.FlattenMessageText(m.Content, "\n")) == ""
}

// KanbanBudgetExhaustedHook records the timed out kanban outcome, pkg/kanban RecordBudgetExhausted installed by the process wiring
var KanbanBudgetExhaustedHook func(ctx context.Context, kanbanTask string, apiCallCount, maxIterations int) error

// Records a timed out terminal outcome for a kanban worker whose iteration budget ran out
var recordKanbanBudgetExhausted = func(ctx context.Context, kanbanTask string, apiCallCount, maxIterations int) {
	if err := KanbanBudgetExhaustedHook(ctx, kanbanTask, apiCallCount, maxIterations); err != nil {
		conversationLoopLog().Warn(fmt.Sprintf("Failed to record budget-exhausted failure for task %s", kanbanTask), "error", err)
	}
}

// Removes the synthetic verification continuation nudges, keeping the attempted answer
func dropVerificationContinuationScaffolding(messages *[]*state.Message) {
	kept := make([]*state.Message, 0, len(*messages))
	for _, m := range *messages {
		if m != nil && anyFlagSet(m.Extra, verificationContinuationFlags) {
			continue
		}
		kept = append(kept, m)
	}
	*messages = kept
}

// True when any listed bookkeeping key holds a truthy value
func anyFlagSet(extra map[string]any, flags []string) bool {
	for _, f := range flags {
		if pyTruthy(extra[f]) {
			return true
		}
	}
	return false
}

// Rebinds the live transcript to dict rows, reusing the message each original row was read from
func rebindLiveRows(messages *[]*state.Message, before, after []map[string]any) {
	origin := make(map[uintptr]*state.Message, len(before))
	for i, row := range before {
		if row != nil && i < len(*messages) && (*messages)[i] != nil {
			origin[reflect.ValueOf(row).Pointer()] = (*messages)[i]
		}
	}
	out := make([]*state.Message, len(after))
	for i, row := range after {
		if row == nil {
			continue
		}
		fresh := state.MessageFromDict(row)
		if m, ok := origin[reflect.ValueOf(row).Pointer()]; ok {
			*m = *fresh
			fresh = m
		}
		out[i] = fresh
	}
	*messages = out
}

// Runs one post loop cleanup step, recording its failure instead of losing the response
func guardedCleanup(label string, fn func() error, errs *[]string) {
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				if isUserInterruptPanic(r) {
					panic(r)
				}
				err = fmt.Errorf("%v", r)
			}
		}()
		return fn()
	}()
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: %s", label, err))
		conversationLoopLog().Error(fmt.Sprintf("finalize_turn: _%s failed: %s", label, err))
	}
}

// Iteration summary request over the live transcript, replaced by tests
var handleMaxIterations = func(ctx context.Context, a *AIAgent, messages *[]*state.Message, apiCallCount int) (string, error) {
	before := messagesToRows(*messages)
	rows := append([]map[string]any(nil), before...)
	final, err := provider.HandleMaxIterations(ctx, a, &rows)
	rebindLiveRows(messages, before, rows)
	return final, err
}

// Budget exhaustion outcome of a finished loop
type budgetFallback struct {
	finalResponse any
	exitReason    any
	preserved     bool
	interrupted   bool
}

// Resolves the iteration budget fallback reply and records the kanban timeout for an owned worker
func resolveBudgetFallback(ctx context.Context, a *AIAgent, s *LoopState, final any, interrupted, failed bool, exitReason any) (budgetFallback, error) {
	apiCalls := s.APICallCount
	exhausted := apiCalls >= a.maxIterations || a.iterationBudget.Remaining() <= 0
	out := budgetFallback{finalResponse: final, exitReason: exitReason, interrupted: interrupted}
	if reason := pyrepr.Str(exitReason); final == nil && exhausted && !interrupted && !failed && (reason == "unknown" || reason == "budget_exhausted") {
		out.exitReason = fmt.Sprintf("max_iterations_reached(%d/%d)", apiCalls, a.maxIterations)
		if pyTruthy(s.PendingVerificationResponse) {
			out.finalResponse = s.PendingVerificationResponse
			if s.PendingVerificationResponsePreviewed {
				a.responseWasPreviewed = true
				if text, ok := out.finalResponse.(string); ok {
					a.reusedResponseText = &text
				}
			}
			out.preserved = true
		} else {
			a.emitDiagnosticStatus(fmt.Sprintf("⚠️ Iteration budget exhausted (%d/%d) — asking model to summarise", apiCalls, a.maxIterations))
			if !a.quietMode {
				a.safePrint(fmt.Sprintf("\n⚠️  Iteration budget exhausted (%d/%d) — requesting summary...", apiCalls, a.maxIterations), true)
			}
			start := apiCallWallNow()
			summary, err := handleMaxIterations(ctx, a, &s.Messages, apiCalls)
			var interrupt *provider.InterruptedError
			switch {
			case err == nil:
				out.finalResponse = summary
			case errors.As(err, &interrupt):
				out.interrupted = true
				out.exitReason = interruptedDuringAPICallReason(a)
				out.finalResponse = fmt.Sprintf("%s%.1fs elapsed).", interruptWaitingForModelPrefix, apiCallWallNow()-start)
			default:
				return out, err
			}
		}
	}
	if task := os.Getenv("ICEBATH_KANBAN_TASK"); exhausted && tools.IsDispatcherOwnedWorkerContext(a.env) && task != "" {
		recordKanbanBudgetExhausted(ctx, task, apiCalls, a.maxIterations)
	}
	return out, nil
}

// Rolls back the preflight seeded display count when an interrupt wins before any provider response
func rollbackInterruptedPreflightDisplay(a *AIAgent, interrupted bool) {
	snapshot := a.turnPreflightDisplaySnapshot
	if !interrupted || snapshot == nil || a.turnReceivedProviderResponse || a.contextCompressor == nil {
		return
	}
	if c, ok := a.contextCompressor.(interface{ RollbackInterruptedPreflightDisplayTokens(int) }); ok {
		c.RollbackInterruptedPreflightDisplayTokens(*snapshot)
	}
}

// Strips private retry scaffolding and the synthetic verification nudges from the transcript
func dropTranscriptScaffolding(a *AIAgent, messages *[]*state.Message) {
	a.dropTrailingEmptyResponseScaffolding(messages)
	dropVerificationContinuationScaffolding(messages)
}

// Recovers an empty terminal final from text the stream already delivered
func recoverFinalFromStream(a *AIAgent, final any, interrupted, failed bool) (any, bool) {
	if interrupted || failed {
		return final, false
	}
	streamed := pystr.Strip(a.currentStreamedAssistantText())
	visible := ""
	if pyTruthy(final) {
		visible = pystr.Strip(state.FlattenMessageText(final, "\n"))
	}
	if visible == "" && streamed != "" {
		return streamed, true
	}
	return final, false
}

// Closes an interrupted tool tail with the stopped assistant row
func closeLiveInterruptedToolSequence(messages *[]*state.Message, final any) {
	before := messagesToRows(*messages)
	rows := append([]map[string]any(nil), before...)
	if closeInterruptedToolSequence(&rows, final, true) {
		rebindLiveRows(messages, before, rows)
	}
}

// Shapes the transcript tail so a delivered final always has its assistant row before the durable snapshot
func closeTranscriptTail(a *AIAgent, messages *[]*state.Message, final any, interrupted, recovered bool) {
	if interrupted {
		closeLiveInterruptedToolSequence(messages, final)
	}
	if pyTruthy(final) && !interrupted {
		var tail *state.Message
		if n := len(*messages); n > 0 {
			tail = (*messages)[n-1]
		}
		switch {
		case tail == nil || tail.Role != "assistant":
			appendLiveMessage(messages, map[string]any{"role": "assistant", "content": final})
		case !reflect.DeepEqual(tail.Content, final) && assistantRowMissingVisibleText(tail) && (len(tail.ToolCalls) > 0 || recovered):
			row := state.MessageToDict(tail)
			row["content"] = final
			state.StampMessageTimestamp(row, nil)
			delete(row, dbPersistedMarker)
			*tail = *state.MessageFromDict(row)
			a.dbFlushScanPrefix = nil
		}
	}
	rows := messagesToRows(*messages)
	a.applyPersistUserMessageOverride(rows)
	rebindLiveRows(messages, rows, rows)
}

// Stores the rewritten reply on an unflushed tail row that still holds the raw model text
func storeRewriteOnTail(a *AIAgent, messages []*state.Message, raw, final any) {
	if len(messages) == 0 || messages[len(messages)-1] == nil {
		return
	}
	tail := messages[len(messages)-1]
	row := state.MessageToDict(tail)
	if row["role"] != "assistant" || !reflect.DeepEqual(row["content"], raw) || pyTruthy(row[dbPersistedMarker]) {
		return
	}
	if row["display_kind"] == "hidden" && a.holdStream {
		delete(row, "display_kind")
		if !reflect.DeepEqual(final, raw) {
			if _, ok := row["api_content"]; !ok {
				row["api_content"] = raw
			}
		}
	}
	row["content"] = final
	*tail = *state.MessageFromDict(row)
}

// Absorbs the oldest uncompacted exchange into the rolling summary before the turn persists
func microCompactAfterTurn(ctx context.Context, a *AIAgent, messages *[]*state.Message, final any, taskID string) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			conversationLoopLog().Info(fmt.Sprintf("Micro-compaction failed: %v", r))
		}
	}()
	c, ok := a.contextCompressor.(*ContextCompressor)
	if !ok || c == nil || !c.microCompactEnabled || !pyTruthy(final) || a.compressionCheckpointRequired || a.persistDisabled {
		return
	}
	before := len(*messages)
	rows := messagesToRows(*messages)
	compacted, err := c.microCompact(rows, a.inflightTurnSessionID)
	if err != nil {
		conversationLoopLog().Info(fmt.Sprintf("Micro-compaction failed: %v", err))
		return
	}
	if c.flushScanCursorInvalidated {
		c.flushScanCursorInvalidated = false
		a.dbFlushScanPrefix = nil
	}
	if len(compacted) > 0 {
		spliced := !sameRowList(compacted, rows)
		rebindLiveRows(messages, rows, compacted)
		if spliced {
			resetReadDedupCaches(ctx, taskID, a.sessionID)
		}
	}
	if before != len(*messages) {
		conversationLoopLog().Info(fmt.Sprintf("Micro-compaction: %d -> %d messages", before, len(*messages)))
	}
}

// Python len of a final response, code points for text
func pyLen(v any) int {
	switch x := v.(type) {
	case nil:
		return 0
	case string:
		return utf8.RuneCountInString(x)
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice || rv.Kind() == reflect.Map {
		return rv.Len()
	}
	return 0
}

// Logs why the turn ended, a warning when the transcript stops on a pending tool result
func logTurnExit(a *AIAgent, messages []*state.Message, final any, apiCallCount int, exitReason any, interrupted bool) {
	func() {
		defer func() {
			if r := recover(); r != nil {
				conversationLoopLog().Debug("turn-end activity stamp failed", "error", fmt.Sprint(r))
			}
		}()
		a.TouchActivity("turn end logged")
	}()
	lastRole, lastTool := "None", "None"
	if n := len(messages); n > 0 && messages[n-1] != nil {
		lastRole = messages[n-1].Role
	}
	if lastRole == "tool" {
		for i := len(messages) - 1; i >= 0; i-- {
			if m := messages[i]; m != nil && m.Role == "assistant" && len(m.ToolCalls) > 0 {
				if name := m.ToolCalls[len(m.ToolCalls)-1].Function.Name; name != "" {
					lastTool = name
				}
				break
			}
		}
	}
	toolTurns := 0
	for _, m := range messages {
		if m != nil && m.Role == "assistant" && len(m.ToolCalls) > 0 {
			toolTurns++
		}
	}
	used, maxTotal := 0, 0
	if a.iterationBudget != nil {
		used, maxTotal = a.iterationBudget.Used(), a.iterationBudget.MaxTotal
	}
	session := a.sessionID
	if session == "" {
		session = "none"
	}
	diag := fmt.Sprintf("Turn ended: reason=%s model=%s api_calls=%d/%d budget=%d/%d tool_turns=%d last_msg_role=%s response_len=%d session=%s",
		pyrepr.Str(exitReason), a.model, apiCallCount, a.maxIterations, used, maxTotal, toolTurns, lastRole, pyLen(final), session)
	if a.turnOrigin != "" {
		diag += " origin=" + a.turnOrigin
	}
	if lastRole == "tool" && !interrupted {
		conversationLoopLog().Warn("Turn ended with pending tool result (agent may appear stuck). " + diag + " last_tool=" + lastTool)
		return
	}
	conversationLoopLog().Info(diag)
}

// Appends the verifier advisory when failed file writes were never superseded by a successful write
func appendFileMutationFooter(ctx context.Context, a *AIAgent, final any) any {
	defer func() {
		if r := recover(); r != nil {
			conversationLoopLog().Debug(fmt.Sprintf("file-mutation verifier footer failed: %v", r))
		}
	}()
	failed := a.turnFailedFileMutations
	if failed == nil || failed.Len() == 0 || !a.fileMutationVerifierEnabled(ctx) {
		return final
	}
	failed = fileMutationsStillFailed(failed)
	footer := formatFileMutationFailureFooter(ctx, failed)
	text, ok := final.(string)
	if footer == "" {
		return final
	}
	if !ok {
		conversationLoopLog().Debug(fmt.Sprintf("file-mutation verifier footer failed: %T has no rstrip", final))
		return final
	}
	return pystr.Rstrip(text) + "\n\n" + footer
}

// Surfaces one explanation from the exit reason when the turn produced no usable reply
func explainAbnormalExit(ctx context.Context, a *AIAgent, final, exitReason any, preserved bool) any {
	defer func() {
		if r := recover(); r != nil {
			conversationLoopLog().Debug(fmt.Sprintf("turn-completion explainer failed: %v", r))
		}
	}()
	if !a.turnCompletionExplainerEnabled(ctx) {
		return final
	}
	stripped := ""
	if pyTruthy(final) {
		text, ok := final.(string)
		if !ok {
			conversationLoopLog().Debug(fmt.Sprintf("turn-completion explainer failed: %T has no strip", final))
			return final
		}
		stripped = pystr.Strip(text)
	}
	reason := pyrepr.Str(exitReason)
	emptyTerminal := stripped == "" || stripped == "(empty)"
	lastChar := ""
	if r, size := utf8.DecodeLastRuneInString(stripped); size > 0 {
		lastChar = string(r)
	}
	partialFragment := !emptyTerminal && !preserved && !strings.HasPrefix(reason, "text_response") &&
		exitReason != "fallback_prior_turn_content" && utf8.RuneCountInString(stripped) <= 24 && !sentenceEnd[lastChar]
	if !emptyTerminal && !partialFragment && reason != "partial_stream_recovery" {
		return final
	}
	cause, _ := a.lastPersistenceErrorCause.(string)
	dbPath := ""
	if a.sessionDB != nil {
		dbPath = a.sessionDB.Path()
	}
	explanation := formatTurnCompletionExplanation(ctx, exitReason, cause, dbPath, a.model)
	if explanation == "" {
		return final
	}
	if emptyTerminal {
		return explanation
	}
	return stripped + "\n\n" + explanation
}

// Reasoning from the current turn only, the newest non empty one since its user message
func lastTurnReasoning(messages []*state.Message) any {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m == nil {
			continue
		}
		if m.Role == "user" {
			return nil
		}
		if m.Role == "assistant" && m.Reasoning != "" {
			return m.Reasoning
		}
	}
	return nil
}

// Resolves the turn transform outcome then fires post_llm_call once for a session publishing agent
func applyOutputHooks(ctx context.Context, a *AIAgent, final string, platform, taskID, turnID string, originalUserMessage any, messages []*state.Message) (string, bool, any) {
	final, transformed, pre := applyLLMOutputTransform(ctx, a, final, turnID, &platform)
	if a.persistDisabled {
		return final, transformed, pre
	}
	rows := messagesToRows(messages)
	token, err := issueSideQuestionToken(ctx, a, rows, turnID)
	if err != nil {
		token = nil
		conversationLoopLog().Warn("side question lease failed", "error", err)
	}
	invokeHookSafely(ctx, "post_llm_call", map[string]any{
		"session_id": a.sessionID, "task_id": taskID, "turn_id": turnID, "user_message": originalUserMessage,
		"assistant_response": final, "conversation_history": rows, "model": a.model, "platform": platform,
		"side_question_token": token,
	})
	return final, transformed, pre
}

// Turn result pre transform text, withheld for a held turn so only the rewrite leaves the agent
func publicPreTransform(a *AIAgent, pre any) any {
	if a.holdStream {
		return nil
	}
	return pre
}

// Syncs external memory then spawns the background review unless a pre_llm_call hook vetoed the prompt
func mirrorTurnToMemory(a *AIAgent, originalUserMessage, final any, interrupted bool, messages []*state.Message, reviewMemory, reviewSkills, vetoed bool) {
	if vetoed {
		return
	}
	a.syncExternalMemoryForTurn(originalUserMessage, final, interrupted, messagesToRows(messages))
	if !pyTruthy(final) || interrupted || a.skipBackgroundReview || !(reviewMemory || reviewSkills) {
		return
	}
	func() {
		defer func() {
			if r := recover(); r != nil && isUserInterruptPanic(r) {
				panic(r)
			}
		}()
		a.spawnBackgroundReview(messagesToRows(messages), reviewMemory, reviewSkills)
	}()
}

// Prompt tokens a gateway session entry records, the real API reading rather than the preflight seed
func resultLastPromptTokens(a *AIAgent) int {
	if a.contextCompressor == nil {
		return 0
	}
	last := a.contextCompressor.Base().LastPromptTokens
	if last > 0 {
		if c, ok := a.contextCompressor.(*ContextCompressor); ok {
			return c.lastRealPromptTokens
		}
	}
	return last
}

// Requested service tier from the request overrides extra body, nil when unset
func requestedServiceTier(a *AIAgent) any {
	extra, _ := a.requestOverrides["extra_body"].(map[string]any)
	return extra["service_tier"]
}

// Runs the post loop finalization and returns the turn result
func finalizeTurn(ctx context.Context, a *AIAgent, s *LoopState) (map[string]any, error) {
	vetoed := s.TurnExitReason == PreLLMBlockExitReason
	fb, err := resolveBudgetFallback(ctx, a, s, s.FinalResponse, s.Interrupted, s.Failed, s.TurnExitReason)
	if err != nil {
		return nil, err
	}
	final, exitReason, preserved, interrupted, failed := fb.finalResponse, fb.exitReason, fb.preserved, fb.interrupted, s.Failed
	apiCalls, taskID, turnID := s.APICallCount, s.EffectiveTaskID, s.TurnID

	if n := len(s.Messages); !pyTruthy(final) && !interrupted && n > 0 && s.Messages[n-1] != nil && s.Messages[n-1].Role == "tool" &&
		pystr.Strip(a.currentStreamedAssistantText()) == "" {
		exitReason, failed, final = "pending_tool_result", true, ""
		func() {
			defer func() {
				if r := recover(); r != nil {
					final = ""
				}
			}()
			if a.turnCompletionExplainerEnabled(ctx) {
				final = formatTurnCompletionExplanation(ctx, "pending_tool_result", "", "", "")
			}
		}()
		if final == "" {
			final = pendingToolResultReply
		}
	}

	var exitFailure *ExitFailure
	if !interrupted {
		if ef, ok := ExitReasonFailure(exitReason); ok {
			exitFailure = &ef
		}
	}
	if exitFailure != nil && exitFailure.FailsTurn {
		failed = true
	}
	completed := final != nil && !failed && !interrupted &&
		(apiCalls < a.maxIterations || strings.HasPrefix(pyrepr.Str(exitReason), "text_response("))

	rollbackInterruptedPreflightDisplay(a, interrupted)
	observability.FinishTurn(ctx, a, exitReason, final, interrupted, failed)

	var cleanupErrors []string
	guardedCleanup("start_deferred_title_upgrade", func() error { startDeferredTitleUpgrade(a); return nil }, &cleanupErrors)
	guardedCleanup("save_trajectory", func() error {
		return a.saveTrajectory(messagesToRows(s.Messages), provider.SummarizeUserMessageForLog(s.UserMessage, " "), completed)
	}, &cleanupErrors)
	guardedCleanup("cleanup_task_resources", func() error { a.cleanupTaskResources(tools.WithEnvHome(ctx, a.env), taskID); return nil }, &cleanupErrors)
	guardedCleanup("persist_session", func() error {
		dropTranscriptScaffolding(a, &s.Messages)
		var recovered bool
		final, recovered = recoverFinalFromStream(a, final, interrupted, failed)
		if text, ok := final.(string); ok && text != "" && !interrupted {
			rewritten, _, _ := applyLLMOutputTransform(ctx, a, text, turnID, nil)
			final = rewritten
			storeRewriteOnTail(a, s.Messages, text, rewritten)
		}
		closeTranscriptTail(a, &s.Messages, final, interrupted, recovered)
		if !interrupted && !failed && !vetoed {
			microCompactAfterTurn(ctx, a, &s.Messages, final, taskID)
		}
		return a.persistSession(&s.Messages, s.ConversationHistory)
	}, &cleanupErrors)
	a.sessionMessages = s.Messages

	logTurnExit(a, s.Messages, final, apiCalls, exitReason, interrupted)
	if pyTruthy(final) && !interrupted {
		final = appendFileMutationFooter(ctx, a, final)
	}
	if !interrupted {
		final = explainAbnormalExit(ctx, a, final, exitReason, preserved)
	}

	platform := a.platform
	transformed := false
	var preTransform any
	if text, ok := final.(string); ok && text != "" && !interrupted {
		final, transformed, preTransform = applyOutputHooks(ctx, a, text, platform, taskID, turnID, s.OriginalUserMessage, s.Messages)
	}

	notifyContextEngineTurnComplete(a, messagesToRows(s.Messages), a.lastTurnUsage, map[string]any{
		"turn_id": turnID, "task_id": taskID, "api_call_count": apiCalls, "interrupted": interrupted,
		"failed": failed, "turn_exit_reason": exitReason,
	})

	if text, ok := final.(string); ok {
		final = state.SanitizeSurrogates(text)
	}

	reused := false
	if text, ok := final.(string); ok && text != "" && a.reusedResponseText != nil {
		reused = text == *a.reusedResponseText
	}
	result := map[string]any{
		"final_response": final, "last_reasoning": lastTurnReasoning(s.Messages), "messages": s.Messages,
		"api_calls": apiCalls, "completed": completed, "turn_exit_reason": exitReason, "failed": failed,
		"partial": false, "interrupted": interrupted, "response_transformed": transformed,
		"pre_transform_response": publicPreTransform(a, preTransform), "response_previewed": a.responseWasPreviewed,
		"response_reused": reused, "model": a.model,
	}
	for k, v := range resultModelFields(a) {
		result[k] = v
	}
	result["provider"], result["base_url"] = a.provider, a.baseURL
	result["input_tokens"], result["output_tokens"] = a.sessionInputTokens, a.sessionOutputTokens
	result["cache_read_tokens"], result["cache_write_tokens"] = a.sessionCacheReadTokens, a.sessionCacheWriteTokens
	result["reasoning_tokens"], result["prompt_tokens"] = a.sessionReasoningTokens, a.sessionPromptTokens
	result["completion_tokens"], result["total_tokens"] = a.sessionCompletionTokens, a.sessionTotalTokens
	result["last_prompt_tokens"] = resultLastPromptTokens(a)
	result["estimated_cost_usd"], result["cost_status"], result["cost_source"] = a.sessionEstimatedCostUSD, a.sessionCostStatus, a.sessionCostSource
	result["service_tier"], result["session_id"] = requestedServiceTier(a), a.sessionID
	if a.toolGuardrailHaltDecision != nil {
		result["guardrail"] = a.toolGuardrailHaltDecision.ToMetadata()
	}
	if failed && pyrepr.Str(exitReason) == "session_persistence_failed" {
		if pyTruthy(final) {
			result["error"] = final
		} else {
			result["error"] = "session storage could not be written — check the state database health (`icebath " +
				home.ProfileCLISelector(home.Dir(ctx)) + "doctor`), then send your message again"
		}
		cause, _ := a.lastPersistenceErrorCause.(string)
		if cause == "" {
			cause = "unknown"
		}
		result["failure_reason"] = "session_persistence_failed:" + cause
	} else if exitFailure != nil {
		if failed {
			if pyTruthy(final) {
				result["error"] = final
			} else {
				result["error"] = pyrepr.Str(exitReason)
			}
		}
		StampFailure(result, exitFailure.Reason, exitFailure.Retryable)
	}
	if len(cleanupErrors) > 0 {
		result["cleanup_errors"] = cleanupErrors
	}
	if steer := a.drainLeftoverSteer(); steer != "" {
		result["pending_steer"] = steer
	}
	a.responseWasPreviewed, a.reusedResponseText = false, nil
	if interrupted && a.interruptMessage != "" {
		result["interrupt_message"] = a.interruptMessage
	}
	a.clearInterrupt(false)
	a.streamCallback = nil
	endVoiceTurnRoute(ctx, a)

	reviewSkills := a.skillNudgeInterval > 0 && a.itersSinceSkill >= a.skillNudgeInterval && a.validToolNames["skill_manage"]
	if reviewSkills {
		a.itersSinceSkill = 0
	}
	mirrorTurnToMemory(a, s.OriginalUserMessage, final, interrupted, s.Messages, s.ShouldReviewMemory, reviewSkills, vetoed)
	if !vetoed {
		MaybePrepareCompaction(ctx, a, messagesToRows(s.Messages), "turn-end")
	}
	if !a.persistDisabled {
		invokeHookSafely(ctx, "on_session_end", map[string]any{
			"session_id": a.sessionID, "task_id": taskID, "turn_id": turnID, "completed": completed, "failed": failed,
			"interrupted": interrupted, "turn_exit_reason": exitReason, "model": a.model, "platform": platform,
		})
	}
	a.turnPreflightDisplaySnapshot = nil
	a.turnReceivedProviderResponse = false
	return result, nil
}
