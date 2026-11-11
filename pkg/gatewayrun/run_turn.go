package gatewayrun

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var contextOverflowErrorPhrases = []string{
	"context length", "context size", "context window",
	"maximum context", "token limit", "too many tokens",
	"reduce the length", "exceeds the limit",
	"request entity too large", "prompt is too long",
	"payload too large", "input is too long",
}

// Reply when the model returned only a silence marker for a message that needed an answer
func unexpectedSilenceReply(ctx context.Context) string {
	return i18n.T(ctx, "gateway.errors.unexpected_silence", "", nil)
}

// True when a failed turn is a context overflow, the one verdict transcript persistence and the reply share
func isContextOverflowFailureResult(agentResult map[string]any, historyLen int) bool {
	if !pyrepr.Truthy(agentResult["failed"]) {
		return false
	}
	if pyrepr.Truthy(agentResult["compression_exhausted"]) {
		return true
	}
	errText := ""
	if v := agentResult["error"]; pyrepr.Truthy(v) {
		errText = strings.ToLower(pyrepr.Str(v))
	}
	for _, phrase := range contextOverflowErrorPhrases {
		if strings.Contains(errText, phrase) {
			return true
		}
	}
	return strings.Contains(errText, "400") && historyLen > 50
}

var hygieneSetupRoles = map[string]bool{"system": true, "session_meta": true}

// Setup head plus the newest tail within the limit never starting on a tool result, unchanged when under
func boundModelInputWithoutHygiene(history []map[string]any, limit int) []map[string]any {
	if len(history) <= limit {
		return history
	}
	headEnd := 0
	for headEnd < len(history) && history[headEnd] != nil && hygieneSetupRoles[roleOf(history[headEnd])] {
		headEnd++
	}
	headEnd = min(headEnd, limit-1)
	tailStart := len(history) - (limit - headEnd)
	for tailStart < len(history) && history[tailStart] != nil && roleOf(history[tailStart]) == "tool" {
		tailStart++
	}
	out := make([]map[string]any, 0, headEnd+len(history)-tailStart)
	out = append(out, history[:headEnd]...)
	return append(out, history[tailStart:]...)
}

// Role of a transcript row, empty when absent or not text
func roleOf(row map[string]any) string {
	role, _ := row["role"].(string)
	return role
}

// Why a hygiene compression left the session id unchanged with no in place commit, read from the attempt signals
func hygieneNoCommitReason(a *agent.AIAgent) string {
	if !a.LastCompressionAttemptRecorded() {
		return "compression did not run"
	}
	lockSkip := a.CompressionSkippedDueToLock()
	if b, ok := lockSkip.(bool); ok && b {
		return "attempt skipped: compression lease held by another process"
	}
	if _, ok := lockSkip.(string); ok {
		return "attempt skipped: compression lease held by another process"
	}
	if blocked := a.CompressionBlockedTransient(); pyrepr.Truthy(blocked) {
		return "attempt blocked: " + pyrepr.Str(blocked)
	}
	if a.LastCompressionAttemptInPlace() == nil {
		detail := "aborted before commit"
		if a.LastCompressionTimedOut() {
			detail = "summary timed out"
		}
		if warning := a.LastCompressionSummaryWarning(); warning != nil && *warning != "" {
			return "attempt " + detail + ": " + *warning
		}
		return "attempt " + detail
	}
	if a.SessionDB() == nil {
		return "no session_db on the hygiene agent"
	}
	return "in-place commit did not complete"
}
