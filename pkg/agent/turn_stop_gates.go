package agent

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/plugins/verify"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Whether a nudge was appended and the turn loop re-enters with the pending verification fields
type stopGateVerdict struct {
	continueTurn                         bool
	finalResponse                        any
	pendingVerificationResponse          any
	pendingVerificationResponsePreviewed bool
}

const maxLSPTurnEndNotes = 3

// Verify on stop follow up for this turn's edits, empty when disabled or already proven
func verifyOnStopNudge(ctx context.Context, a *AIAgent) string {
	if !verifyOnStopEnabled(ctx, a.env, nil) {
		return ""
	}
	nudge, err := buildVerifyOnStopNudge(ctx, a.env, a.sessionID, sortedMutationPaths(a), a.verificationStopNudges, defaultVerifyOnStopMaxAttempts)
	if err != nil {
		conversationLoopLog().Debug("verification stop-loop check failed", "error", err)
		return ""
	}
	return nudge
}

// Changed paths of this turn in sorted order
func sortedMutationPaths(a *AIAgent) []string {
	edited := make([]string, 0, len(a.turnFileMutationPaths))
	for p := range a.turnFileMutationPaths {
		edited = append(edited, p)
	}
	sort.Strings(edited)
	return edited
}

// Continue message of a registered pre_verify hook after code edits, empty when no hook keeps the turn going
func preVerifyNudge(ctx context.Context, a *AIAgent, finalResponse string, attempt int) string {
	edited := sortedMutationPaths(a)
	if len(edited) == 0 || !lifecycleHasHook(ctx, "pre_verify") {
		return ""
	}
	limit, err := verify.MaxVerifyNudges(nil)
	if err != nil {
		conversationLoopLog().Debug("pre_verify hook check failed", "error", err)
		return ""
	}
	if attempt >= limit {
		return ""
	}
	if a.resolvedIsCoding == nil {
		coding := tools.IsCodingContext(a.env, a.platform, "")
		a.resolvedIsCoding = &coding
	}
	return plugins.GetPreVerifyContinueMessage(ctx, plugins.PreVerifyInput{
		SessionID: a.sessionID, Platform: a.platform, Model: a.model, Coding: *a.resolvedIsCoding,
		Attempt: attempt, FinalResponse: finalResponse, ChangedPaths: edited,
	})
}

// Kanban worker follow up when the session made no terminal board call
func kanbanStopNudge(ctx context.Context, a *AIAgent, s *LoopState) string {
	rows := make([]map[string]any, 0, len(s.Messages))
	for _, m := range s.Messages {
		if m != nil {
			rows = append(rows, state.MessageToDict(m))
		}
	}
	return buildKanbanStopNudge(ctx, rows, a.kanbanStopNudges, defaultKanbanStopMaxAttempts, "")
}

// Language server verdicts on this turn's edits that no tool result carried, waited for briefly
func lateLSPVerdicts(a *AIAgent) string {
	if a.lspTurnEndNotes >= maxLSPTurnEndNotes {
		return ""
	}
	notes := tools.LSPSettleForAgent(a.sessionID, a.currentTaskID, tools.LSPTurnEndWait)
	if len(notes) == 0 {
		return ""
	}
	texts := make([]string, len(notes))
	for i, n := range notes {
		texts[i] = n.Text
	}
	return "[System note: the language server reported problems in files you edited this turn, after " +
		"those tool results had returned. Fix them if they are real, then give your final answer.]\n\n" +
		strings.Join(texts, "\n\n")
}

// Emits and persists the attempted answer as interim, a hidden held answer stays unflushed
func appendInterimAnswer(a *AIAgent, finalMsg map[string]any, s *LoopState, flushFailMsg string) {
	a.emitInterimAssistantMessage(finalMsg)
	appendLiveMessage(&s.Messages, finalMsg)
	if finalMsg["display_kind"] == "hidden" {
		return
	}
	guardedFlush(func() { a.flushLiveMessages(s.Messages, s.ConversationHistory) }, func(exc *OuterLoopError) {
		conversationLoopLog().Debug(flushFailMsg, "exc_info", exc.Stack, "error", exc)
	})
}

// Runs late LSP verdicts, verify on stop, the pre_verify hook and the kanban stop guard in order
func applyStopGates(ctx context.Context, a *AIAgent, finalMsg map[string]any, finalResponse string, s *LoopState) stopGateVerdict {
	cont := func(nudge, flag string) stopGateVerdict {
		appendLiveMessage(&s.Messages, map[string]any{"role": "user", "content": nudge, flag: true})
		a.setSessionMessages(s.Messages)
		return stopGateVerdict{
			continueTurn: true, finalResponse: nil, pendingVerificationResponse: finalResponse,
			pendingVerificationResponsePreviewed: a.interimContentWasStreamed(finalResponse),
		}
	}

	if lspNote := lateLSPVerdicts(a); lspNote != "" {
		a.lspTurnEndNotes++
		finalMsg["finish_reason"] = "verification_required"
		appendInterimAnswer(a, finalMsg, s, "late-LSP interim flush failed")
		appendLiveMessage(&s.Messages, map[string]any{"role": "user", "content": lspNote})
		a.setSessionMessages(s.Messages)
		conversationLoopLog().Debug(fmt.Sprintf("late LSP verdicts delivered before the final answer (%d)", a.lspTurnEndNotes))
		return stopGateVerdict{
			continueTurn: true, finalResponse: nil, pendingVerificationResponse: finalResponse,
			pendingVerificationResponsePreviewed: a.interimContentWasStreamed(finalResponse),
		}
	}

	if nudge := verifyOnStopNudge(ctx, a); nudge != "" {
		a.verificationStopNudges++
		finalMsg["finish_reason"] = "verification_required"
		appendInterimAnswer(a, finalMsg, s, "verify-on-stop interim flush failed")
		verdict := cont(nudge, "_verification_stop_synthetic")
		conversationLoopLog().Debug(fmt.Sprintf("verification stop-loop nudge issued (attempt %d)", a.verificationStopNudges))
		return verdict
	}

	attempt := a.preVerifyNudges
	if nudge := preVerifyNudge(ctx, a, finalResponse, attempt); nudge != "" {
		a.preVerifyNudges = attempt + 1
		finalMsg["finish_reason"] = "verify_hook_continue"
		appendInterimAnswer(a, finalMsg, s, "pre_verify interim flush failed")
		verdict := cont(nudge, "_pre_verify_synthetic")
		conversationLoopLog().Debug(fmt.Sprintf("pre_verify nudge issued (attempt %d)", a.preVerifyNudges))
		return verdict
	}

	if nudge := kanbanStopNudge(ctx, a, s); nudge != "" {
		a.kanbanStopNudges++
		finalMsg["finish_reason"] = "kanban_terminal_required"
		finalMsg["_kanban_stop_synthetic"] = true
		appendLiveMessage(&s.Messages, finalMsg)
		verdict := cont(nudge, "_kanban_stop_synthetic")
		conversationLoopLog().Info(fmt.Sprintf("kanban stop-loop nudge issued (attempt %d) task=%s", a.kanbanStopNudges, os.Getenv("ICEBATH_KANBAN_TASK")))
		a.emitDiagnosticStatus("⚠️ Kanban worker tried to exit without a terminal board call " +
			"(kanban_complete/kanban_request_review/kanban_block) — nudging to finish")
		return verdict
	}
	return stopGateVerdict{
		continueTurn: false, finalResponse: finalResponse,
		pendingVerificationResponse:          s.PendingVerificationResponse,
		pendingVerificationResponsePreviewed: s.PendingVerificationResponsePreviewed,
	}
}
