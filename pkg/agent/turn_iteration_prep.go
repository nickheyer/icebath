package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"reflect"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const IterationBudgetWarningTemplate = "[SYSTEM NOTICE — iteration budget checkpoint] You have used %d of %d " +
	"iterations. Checkpoint durable progress now, then continue the task; do not stop " +
	"solely because of this warning."

const kanbanBudgetWarningSuffix = " While tools are still available, call kanban_complete only if all task " +
	"requirements are verified, or kanban_request_review if it is ready for " +
	"review; otherwise persist a kanban_comment handoff and " +
	"continue. A diff or commit alone is not completion evidence."

// True when the row at idx is this turn's user row verbatim or through its user originated view
func anchorsCurrentTurn(messages []*state.Message, idx int, userMessage any) bool {
	if idx < 0 || idx >= len(messages) {
		return false
	}
	msg := messages[idx]
	if msg == nil || msg.Role != "user" {
		return false
	}
	if reflect.DeepEqual(msg.Content, userMessage) {
		return true
	}
	view := UserOriginatedTurnView(state.MessageToDict(msg))
	return view != nil && reflect.DeepEqual(view["content"], userMessage)
}

// Appends text to a row's string or part list content, false for any other content shape
func appendNoticeToContent(msg *state.Message, notice string) bool {
	part := map[string]any{"type": "text", "text": notice}
	switch existing := msg.Content.(type) {
	case string:
		msg.Content = existing + "\n\n" + notice
	case nil:
		msg.Content = []any{part}
	case []any:
		msg.Content = append(append([]any{}, existing...), part)
	case []map[string]any:
		parts := make([]any, 0, len(existing)+1)
		for _, p := range existing {
			parts = append(parts, p)
		}
		msg.Content = append(parts, part)
	default:
		return false
	}
	return true
}

// Appends the opt in one shot iteration budget checkpoint to the newest unpersisted tool result
func maybeInjectIterationBudgetWarning(a *AIAgent, messages []*state.Message) bool {
	if a.interruptRequested {
		return false
	}
	ratio, haveRatio := a.budgetWarningRatio, a.budgetWarningRatio != 0
	kanbanWorker := os.Getenv("ICEBATH_KANBAN_TASK") != "" && tools.IsDispatcherOwnedWorkerContext(a.env) && a.validToolNames["kanban_complete"]
	if !haveRatio && kanbanWorker {
		ratio, haveRatio = 0.9, true
	}
	budget := a.iterationBudget
	if !haveRatio || budget == nil || budget.MaxTotal <= 1 || budget.MaxTotal >= math.MaxInt || a.iterationBudgetWarningInjected ||
		float64(budget.Used()) < math.Min(ratio*float64(budget.MaxTotal), float64(budget.MaxTotal-1)) {
		return false
	}
	notice := fmt.Sprintf(IterationBudgetWarningTemplate, budget.Used(), budget.MaxTotal)
	if kanbanWorker {
		notice += kanbanBudgetWarningSuffix
	}
	if len(messages) == 0 {
		return false
	}
	last := messages[len(messages)-1]
	if last == nil || last.Role != "tool" || pyTruthy(last.Extra[dbPersistedMarker]) {
		return false
	}
	if !appendNoticeToContent(last, notice) {
		return false
	}
	a.iterationBudgetWarningInjected = true
	return true
}

// Copies hidden interrupt placeholder rows the model echoes on replay into the current placeholder, never mutating seq
func neutraliseReplayEchoGhosts(seq []*state.Message) ([]*state.Message, int) {
	hazards := map[string]bool{interruptScaffoldMarker: true, LegacyInterruptedPlaceholder: true}
	neutralised := 0
	var out []*state.Message
	for i, m := range seq {
		if m != nil && m.DisplayKind == "hidden" && m.Role == "assistant" {
			content, isStr := m.Content.(string)
			if (isStr && hazards[pystr.Strip(content)]) || hazards[pystr.Strip(m.APIContent)] {
				if out == nil {
					out = append(make([]*state.Message, 0, len(seq)), seq[:i]...)
				}
				cp := *m
				cp.Content, cp.APIContent = "", InterruptedPlaceholder
				out = append(out, &cp)
				neutralised++
				continue
			}
		}
		if out != nil {
			out = append(out, m)
		}
	}
	if neutralised == 0 {
		return seq, 0
	}
	return out, neutralised
}

// Runs the agent step callback, logging whatever it panics with at debug level
func fireStepCallback(a *AIAgent, apiCallCount int, round []map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			conversationLoopLog().Debug(fmt.Sprintf("step_callback error (iteration %d): %v", apiCallCount, r))
		}
	}()
	switch cb := a.stepCallback.(type) {
	case func(int, []map[string]any):
		cb(apiCallCount, round)
	case func(int, []map[string]any) error:
		if err := cb(apiCallCount, round); err != nil {
			conversationLoopLog().Debug(fmt.Sprintf("step_callback error (iteration %d): %v", apiCallCount, err))
		}
	}
}

// Prepares the transcript for this iteration, steer drain, budget notices, argument repair, ghost rows and alternation
func prepareIteration(ctx context.Context, a *AIAgent, s *LoopState) error {
	if pyTruthy(a.nousWirePending) {
		applyPendingWireSwitch(ctx, a)
	}
	if a.stepCallback != nil {
		fireStepCallback(a, s.APICallCount, previousToolRound(s.Messages))
	}
	if a.skillNudgeInterval > 0 && a.validToolNames["skill_manage"] {
		a.itersSinceSkill++
	}
	a.adoptNousKeyBeforeExpiry(ctx)
	if steer := a.drainPendingSteer(); steer != "" {
		injectSteerAfterNewestToolResult(a, &s.Messages, steer)
	}
	if a.runBudgetSeconds != 0 {
		maybeInjectRunBudgetWrapup(a, s.Messages)
	}
	maybeInjectIterationBudgetWarning(a, s.Messages)

	log := conversationLoopLog()
	s.RequestLogger = log
	session := a.sessionID
	if session == "" {
		session = "-"
	}
	if a.sanitizeArgsCursor == nil {
		a.sanitizeArgsCursor = map[string]any{}
	}
	if repaired := sanitizeToolCallArguments(&s.Messages, log, a.sessionID, a.sanitizeArgsCursor); repaired > 0 {
		a.dbFlushScanPrefix = nil
		log.Info(fmt.Sprintf("Sanitized %d corrupted tool_call arguments before request (session=%s)", repaired, session))
	}
	var neutralised int
	s.Messages, neutralised = neutraliseReplayEchoGhosts(s.Messages)
	if neutralised > 0 {
		log.Info(fmt.Sprintf("Neutralised %d interrupt-placeholder row(s) before request (session=%s)", neutralised, session))
	}
	repairedSeq, err := repairMessageSequenceWithCursor(a, &s.Messages)
	if err != nil {
		return err
	}
	if repairedSeq > 0 {
		log.Info(fmt.Sprintf("Repaired %d message-alternation violations before request (session=%s)", repairedSeq, session))
		if s.UserMessage != nil {
			if idx := reanchor(a, s.Messages, s.UserMessage); idx != s.CurrentTurnUserIdx {
				log.Info(fmt.Sprintf("Re-anchored current_turn_user_idx %d -> %d after alternation repair (session=%s)", s.CurrentTurnUserIdx, idx, session))
				s.CurrentTurnUserIdx = idx
			}
		}
	}
	if s.UserMessage != nil && !anchorsCurrentTurn(s.Messages, s.CurrentTurnUserIdx, s.UserMessage) {
		idx := reanchor(a, s.Messages, s.UserMessage)
		log.Info(fmt.Sprintf("Re-anchored stale current_turn_user_idx %d -> %d (session=%s)", s.CurrentTurnUserIdx, idx, session))
		s.CurrentTurnUserIdx = idx
	}
	return nil
}

// Newest assistant tool call batch with each call's result for the agent step event
func previousToolRound(messages []*state.Message) []map[string]any {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m == nil || m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		results := map[string]any{}
		for _, tm := range messages[i+1:] {
			if tm == nil || tm.Role != "tool" {
				break
			}
			if tm.ToolCallID != "" {
				results[tm.ToolCallID] = tm.Content
			}
		}
		round := make([]map[string]any, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			var args any = tc.Function.Arguments
			round = append(round, map[string]any{"name": tc.Function.Name, "result": results[tc.ID], "arguments": args})
		}
		return round
	}
	return []map[string]any{}
}

// Inserts the steer row after the newest tool message or requeues the text when no tool row exists
func injectSteerAfterNewestToolResult(a *AIAgent, messages *[]*state.Message, steerText string) {
	for i := len(*messages) - 1; i >= 0; i-- {
		if m := (*messages)[i]; m != nil && m.Role == "tool" {
			row := state.MessageFromDict(SteerUserRow(steerText))
			*messages = append((*messages)[:i+1], append([]*state.Message{row}, (*messages)[i+1:]...)...)
			conversationLoopLog().Debug(fmt.Sprintf("Pre-API-call steer drain: appended user row after tool msg at index %d", i))
			return
		}
	}
	requeuePendingSteer(a, steerText)
}

var thinkingSpinnerTypes = []string{"brain", "sparkle", "pulse", "moon", "star"}

// Prints the verbose request summary or starts the quiet mode thinking indicator
func announceAPICall(ctx context.Context, a *AIAgent, s *LoopState) error {
	s.ThinkingSpinner = nil
	toolCount := len(a.tools)
	if !a.quietMode {
		a.vprint(fmt.Sprintf("\n%s🔄 Making API call #%d/%d...", a.logPrefix, s.APICallCount, a.maxIterations), false, false)
		a.vprint(fmt.Sprintf("%s   📊 Request size: %d messages, ~%s tokens (~%s chars)", a.logPrefix, len(s.APIMessages),
			pystr.GroupThousands(int64(s.ApproxTokens)), pystr.GroupThousands(int64(s.TotalChars))), false, false)
		a.vprint(fmt.Sprintf("%s   🔧 Available tools: %d", a.logPrefix, toolCount), false, false)
	} else {
		faces, verbs := thinkingFaces(), thinkingVerbs()
		face, verb := faces[rand.IntN(len(faces))], verbs[rand.IntN(len(verbs))]
		if a.thinkingCallback != nil {
			a.callThinking(face + " " + verb + "...")
		} else if !a.hasStreamConsumers() && a.shouldStartQuietSpinner() {
			kind := thinkingSpinnerTypes[rand.IntN(len(thinkingSpinnerTypes))]
			s.ThinkingSpinner = startKawaiiSpinner(face+" "+verb+"...", kind, a.printFn)
		}
	}
	if a.verboseLogging {
		last := "none"
		if n := len(s.Messages); n > 0 && s.Messages[n-1] != nil {
			last = s.Messages[n-1].Role
		}
		slog.Debug(fmt.Sprintf("API Request - Model: %s, Messages: %d, Tools: %d", a.model, len(s.Messages), toolCount))
		slog.Debug("Last message role: " + last)
		slog.Debug(fmt.Sprintf("Total message size: ~%s tokens", pystr.GroupThousands(int64(s.ApproxTokens))))
	}
	return nil
}

// Iteration entry, pending redirect, checkpoint reset, then the interrupt, review budget and iteration budget exits
func beginIteration(ctx context.Context, a *AIAgent, s *LoopState) (PhaseVerdict, error) {
	if redirect := a.drainPendingRedirect(); redirect != "" {
		applyActiveTurnRedirect(a, &s.Messages, redirect)
		if original, ok := s.OriginalUserMessage.(string); ok {
			s.OriginalUserMessage = original + "\n\n" + "User correction during the turn: " + redirect
		}
		if err := a.persistSession(&s.Messages, s.ConversationHistory); err != nil {
			return PhaseVerdict{}, err
		}
	}
	if a.checkpointMgr != nil {
		a.checkpointMgr.NewTurn()
	}
	if a.interruptRequested {
		s.Interrupted = true
		if issuer := interruptIssuer(a); issuer != "" {
			s.TurnExitReason = "interrupted_by_system(" + issuer + ")"
		} else {
			s.TurnExitReason = "interrupted_by_user"
		}
		if !a.quietMode {
			a.safePrint("\n⚡ Breaking out of tool loop due to interrupt...", false)
		}
		return PhaseVerdict{Action: "break"}, nil
	}
	if reviewInputBudgetExhausted(a) {
		s.TurnExitReason = "review_input_budget_exhausted"
		if !a.quietMode {
			a.safePrint(fmt.Sprintf("\n⏹️  Review input budget exhausted (%s tokens) — stopping the review tool loop before the next provider call.",
				pystr.GroupThousands(int64(a.sessionInputTokens))), true)
		}
		return PhaseVerdict{Action: "break"}, nil
	}
	s.APICallCount++
	a.apiCallCount = s.APICallCount
	a.touchActivity(fmt.Sprintf("starting API call #%d", s.APICallCount), "", false)
	if a.budgetGraceCall {
		a.budgetGraceCall = false
	} else if !a.iterationBudget.Consume() {
		s.TurnExitReason = "budget_exhausted"
		if !a.quietMode {
			a.safePrint(fmt.Sprintf("\n⚠️  Iteration budget exhausted (%d/%d iterations used)", a.iterationBudget.Used(), a.iterationBudget.MaxTotal), true)
		}
		return PhaseVerdict{Action: "break"}, nil
	}
	return PhaseVerdict{Action: "fallthrough"}, nil
}

// Consumes the retry state restart flags in priority order, refunding budget for restarts without an assistant item
func applyRetryRestarts(ctx context.Context, a *AIAgent, s *LoopState) (PhaseVerdict, error) {
	r := s.Retry
	if r == nil {
		r = &TurnRetryState{}
		s.Retry = r
	}
	if r.RestartWithRedirectedMessages {
		s.RestartCount++
		if s.RestartCount > s.MaxRetries {
			s.TurnExitReason = "redirect_restart_limit_exceeded"
			conversationLoopLog().Warn(fmt.Sprintf("Redirected-message restart limit (%d) exceeded; ending turn instead of refunding the iteration budget indefinitely.", s.MaxRetries))
			if unapplied := a.drainPendingRedirect(); unapplied != "" {
				a.Steer(unapplied)
			}
			return PhaseVerdict{Action: "break"}, nil
		}
		s.APICallCount--
		a.iterationBudget.Refund()
		r.RestartWithRedirectedMessages = false
		return PhaseVerdict{Action: "continue"}, nil
	}
	if s.Interrupted {
		s.TurnExitReason = interruptedDuringAPICallReason(a)
		return PhaseVerdict{Action: "break"}, nil
	}
	if r.RestartWithCompressedMessages {
		s.APICallCount--
		a.iterationBudget.Refund()
		s.RetryCount++
		r.RestartWithCompressedMessages = false
		if shouldSkipModelCallForReferenceHandoff(&s.Messages, s.UserMessage) {
			conversationLoopLog().Info("Skipping compressed-restart model call: reference-only handoff would be the sole active user turn (#80622)")
			if !pyTruthy(s.FinalResponse) {
				s.FinalResponse = handoffSkipFinalResponse
			}
			s.TurnExitReason = "compaction_handoff_not_actionable"
			return PhaseVerdict{Action: "break"}, nil
		}
		s.CurrentTurnUserIdx = reanchor(a, s.Messages, s.UserMessage)
		return PhaseVerdict{Action: "continue"}, nil
	}
	if r.RestartWithRebuiltMessages {
		s.RestartCount++
		if s.RestartCount > s.MaxRetries {
			s.TurnExitReason = "rebuilt_restart_limit_exceeded"
			conversationLoopLog().Warn(fmt.Sprintf("Rebuilt-message restart limit (%d) exceeded; ending turn instead of refunding the iteration budget indefinitely.", s.MaxRetries))
			return PhaseVerdict{Action: "break"}, nil
		}
		s.APICallCount--
		a.iterationBudget.Refund()
		r.RestartWithRebuiltMessages = false
		s.PreflightCompressionBlocked = false
		return PhaseVerdict{Action: "continue"}, nil
	}
	if r.RestartWithLengthContinuation {
		a.ephemeralMaxOutputTokens = a.boostedOutputCap(a.requestedOutputCapFromAPIKwargs(s.APIKwargs), s.LengthContinueRetries, 0)
		return PhaseVerdict{Action: "continue"}, nil
	}
	if s.Response == nil {
		s.TurnExitReason = "all_retries_exhausted_no_response"
		a.emitDiagnosticStatus("❌ The model provider didn't answer after all retries. Send /retry, or switch models with /model.")
		if err := a.persistSession(&s.Messages, s.ConversationHistory); err != nil {
			return PhaseVerdict{}, err
		}
		return PhaseVerdict{Action: "break"}, nil
	}
	s.RestartCount = 0
	return PhaseVerdict{Action: "fallthrough"}, nil
}
