package engine

import (
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/contexts"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

const (
	// Newest tool results a salvage leaves whole, and the longest a summary stays
	salvageKeepTools  = 2
	salvageSummaryMax = 8_000
)

// Candidate shrunk toward the budget: old tool results cleared, overlong summaries cut keeping their skill rule, and the reasoning replay
// of replies before the person's newest message dropped; nil when there is nothing to shrink
func (c *compressor) Salvage(original, candidate []*sessionsv1.Message, budget int) []*sessionsv1.Message {
	if len(candidate) == 0 || len(original) == 0 || budget <= 0 {
		return nil
	}
	out := make([]*sessionsv1.Message, len(candidate))
	copy(out, candidate)
	var toolsAt []int
	for i, m := range out {
		if isTool(m) {
			toolsAt = append(toolsAt, i)
		}
	}
	keep := map[int]bool{}
	for _, i := range toolsAt[max(len(toolsAt)-salvageKeepTools, 0):] {
		keep[i] = true
	}
	lastUser := -1
	for i, m := range out {
		if isUser(m) {
			lastUser = i
		}
	}
	for i, m := range out {
		text := contexts.Text(m)
		if isTool(m) && !keep[i] && len([]rune(text)) > pruneMinChars {
			out[i] = contexts.Rewritten(m, prunedPlaceholder)
			continue
		}
		if len([]rune(text)) > salvageSummaryMax && summaryLike(m, text) {
			out[i] = contexts.Rewritten(m, elideKeepingRule(text, salvageSummaryMax)+"\n\n"+contexts.EndMarker)
			continue
		}
		if i < lastUser && isAssistant(m) && len(m.GetCarriers()) > 0 {
			stripped := contexts.Copy(m)
			stripped.Carriers = nil
			out[i] = stripped
		}
	}
	return out
}

// Whether a message reads as a summary of a compaction rather than one of the conversation
func summaryLike(m *sessionsv1.Message, text string) bool {
	if !strings.HasSuffix(strings.TrimRightFunc(text, unicode.IsSpace), contexts.EndMarker) || strings.HasPrefix(text, contexts.PriorHeader) || isTool(m) ||
		(isUser(m) || isAssistant(m)) && !m.GetSummary() {
		return false
	}
	head := string([]rune(text)[:min(len([]rune(text)), 280)])
	return m.GetSummary() || strings.Contains(head, "CONTEXT COMPACTION") || strings.Contains(head, "Conversation Summary")
}
