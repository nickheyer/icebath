package agent

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// User facing text for a manual compress skipped by the compression lock, naming a confirmed holder
func DescribeCompressionLockSkip(lockSignal any) string {
	if holder, ok := lockSignal.(string); ok && strings.TrimSpace(holder) != "" {
		return "⏳ Compression already in progress for this session (holder: " + holder + "). Please wait for it to finish."
	}
	return "⏳ Compression skipped: could not acquire this session's compression lock. Another compression may " +
		"still be running, or the lock check failed — try again shortly."
}

// ManualCompressionState is the compressor outcome a manual compression summary reads, nil reading as no flags
type ManualCompressionState struct {
	LastCompressAborted          bool
	LastCompressRefusedWouldGrow bool
	LastSummaryFallbackUsed      bool
	LastSummaryError             string
	LastSummaryDroppedCount      *int
}

// ManualCompressionFeedback is the headline, token line and optional note shown after a manual compression
type ManualCompressionFeedback struct {
	Noop             bool
	Aborted          bool
	RefusedWouldGrow bool
	FallbackUsed     bool
	Headline         string
	TokenLine        string
	Note             *string
}

// Consistent user facing feedback for manual compression with a forced redaction of the failure reason
func SummarizeManualCompression(beforeMessages, afterMessages []map[string]any, beforeTokens, afterTokens int, state *ManualCompressionState) ManualCompressionFeedback {
	if state == nil {
		state = &ManualCompressionState{}
	}
	beforeCount, afterCount := len(beforeMessages), len(afterMessages)
	before, after := make([]any, beforeCount), make([]any, afterCount)
	for i, m := range beforeMessages {
		before[i] = m
	}
	for i, m := range afterMessages {
		after[i] = m
	}
	noop := anyutil.Equal(after, before)
	aborted, refused, fallback := state.LastCompressAborted, state.LastCompressRefusedWouldGrow, state.LastSummaryFallbackUsed
	failureReason := state.LastSummaryError
	if strings.TrimSpace(failureReason) == "" {
		failureReason = ""
	}
	var note *string
	setNote := func(s string) { note = &s }
	var headline string
	switch {
	case refused:
		headline = fmt.Sprintf("Compression refused (summary would grow the conversation): %d messages preserved", beforeCount)
		setNote("The generated summary was larger than what it would replace; no messages were removed.")
	case aborted:
		headline = fmt.Sprintf("Compression aborted: %d messages preserved", beforeCount)
		setNote("Summary generation failed; no messages were removed.")
	case fallback:
		headline = fmt.Sprintf("Compressed with fallback: %d → %d messages", beforeCount, afterCount)
		dropped := max(beforeCount-afterCount, 0)
		if state.LastSummaryDroppedCount != nil {
			dropped = *state.LastSummaryDroppedCount
		}
		setNote(fmt.Sprintf("Summary generation failed; Icebath used limited fallback context and removed %d message(s).", dropped))
	case noop:
		headline = fmt.Sprintf("No changes from compression: %d messages", beforeCount)
	default:
		headline = fmt.Sprintf("Compressed: %d → %d messages", beforeCount, afterCount)
		if afterCount < beforeCount && afterTokens > beforeTokens {
			setNote("Note: fewer messages can still raise this estimate when " +
				"compression rewrites the transcript into denser summaries.")
		}
	}
	b, a := textutil.Thousands(int64(beforeTokens)), textutil.Thousands(int64(afterTokens))
	tokenLine := "Approx request size: ~" + b + " → ~" + a + " tokens"
	if noop && afterTokens == beforeTokens || refused {
		tokenLine = "Approx request size: ~" + b + " tokens (unchanged)"
	}
	if failureReason != "" && (aborted || fallback) {
		prior := ""
		if note != nil {
			prior = *note + " "
		}
		setNote(prior + "Reason: " + tools.RedactSensitiveTextWith(nil, strings.TrimSpace(failureReason), tools.RedactOptions{Force: true}))
	}
	return ManualCompressionFeedback{Noop: noop, Aborted: aborted, RefusedWouldGrow: refused, FallbackUsed: fallback,
		Headline: headline, TokenLine: tokenLine, Note: note}
}
