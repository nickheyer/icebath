package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// AggressiveUnsupported is the refusal every surface prints for /compress --aggressive
const AggressiveUnsupported = "--aggressive is not supported; use '/compress here [N]' to keep only recent exchanges, " +
	"or /undo to drop turns."

// MinCompressMessages is the shortest history a manual compression runs on
const MinCompressMessages = 4

// CompressRequest holds the parsed /compress arguments
type CompressRequest struct {
	Preview    bool
	Aggressive bool
	Partial    bool
	KeepLast   int
	FocusTopic *string
}

// CompressResult is the outcome of one manual compression, status preview, compressed, lock_skipped or nothing_to_do
type CompressResult struct {
	Status         string
	BeforeMessages []map[string]any
	AfterMessages  []map[string]any
	BeforeTokens   int
	AfterTokens    int
	Request        CompressRequest
	Lines          []string
	LockHolder     *string
	Summary        *ManualCompressionFeedback
}

// Removed is how many messages the compression took out of the history
func (r CompressResult) Removed() int { return len(r.BeforeMessages) - len(r.AfterMessages) }

// Parses /compress flags anywhere, then the boundary aware and focus positional forms
func ParseCompressArgs(rawArgs string) CompressRequest {
	rest, preview, aggressive := provider.ExtractCompressFlags(pystr.Strip(rawArgs))
	partial, keepLast, focus := provider.ParsePartialCompressArgs(rest)
	if focus != nil && *focus == "" {
		focus = nil
	}
	return CompressRequest{Preview: preview, Aggressive: aggressive, Partial: partial, KeepLast: keepLast, FocusTopic: focus}
}

// Transcript plus system prompt plus tool schema token estimate of a request built from messages
func EstimateManualRequestTokens(ctx context.Context, a *AIAgent, messages []map[string]any) int {
	if len(messages) == 0 {
		return 0
	}
	rows := make([]*state.Message, len(messages))
	for i, m := range messages {
		rows[i] = state.MessageFromDict(m)
	}
	return provider.EstimateRequestTokens(ctx, rows, a.cachedSystemPrompt, agentToolMaps(a), true)
}

// Compressor flags the manual feedback reads, zero for an engine that is not the built in compressor
func manualCompressionState(engine ContextEngine) *ManualCompressionState {
	c, ok := engine.(*ContextCompressor)
	if !ok || c == nil {
		return nil
	}
	st := &ManualCompressionState{LastCompressAborted: c.lastCompressAborted, LastCompressRefusedWouldGrow: c.lastCompressRefusedWouldGrow,
		LastSummaryFallbackUsed: c.LastSummaryFallbackUsed, LastSummaryError: c.LastSummaryError()}
	dropped := c.LastSummaryDroppedCount
	st.LastSummaryDroppedCount = &dropped
	return st
}

// CompressNowOptions carries the keyword arguments of one manual compression run
type CompressNowOptions struct {
	SystemMessage     *string
	TaskID            string
	SkipWithoutWindow bool
	SnapshotIsCurrent func() (bool, error)
}

// Runs one manual compression of history on the agent without mutating history, the caller installs the result
func CompressNow(ctx context.Context, a *AIAgent, history []map[string]any, request CompressRequest, o CompressNowOptions) (CompressResult, error) {
	if o.TaskID == "" {
		o.TaskID = "default"
	}
	before := append([]map[string]any(nil), history...)
	beforeTokens := EstimateManualRequestTokens(ctx, a, before)
	head, tail := before, []map[string]any(nil)
	if request.Partial {
		head, tail = provider.SplitHistoryForPartialCompress(before, request.KeepLast)
		if len(tail) == 0 {
			head = before
		}
	}
	if request.Preview {
		report := provider.SummarizeCompressPreview(before, request.Partial, request.KeepLast, request.FocusTopic, beforeTokens)
		lines, _ := report["lines"].([]string)
		return CompressResult{Status: "preview", BeforeMessages: before, AfterMessages: before, BeforeTokens: beforeTokens,
			AfterTokens: beforeTokens, Request: request, Lines: lines}, nil
	}
	if o.SkipWithoutWindow {
		if c, ok := a.contextCompressor.(*ContextCompressor); ok && c != nil && !c.HasContentToCompress(head) {
			return CompressResult{Status: "nothing_to_do", BeforeMessages: before, AfterMessages: before, BeforeTokens: beforeTokens,
				AfterTokens: beforeTokens, Request: request}, nil
		}
	}
	var tailRows []map[string]any
	for _, m := range tail {
		row := freshCompactionMessageCopy(m)
		if !truthyMarker(m[dbPersistedMarker]) {
			dropped, had := row["_row_id"]
			delete(row, "_row_id")
			if id, ok := positiveRowID(dropped); had && ok {
				var absorbed []any
				if existing, ok := row["_absorbed_row_ids"].([]any); ok {
					absorbed = append(absorbed, existing...)
				}
				found := false
				for _, v := range absorbed {
					if n, ok := positiveRowID(v); ok && n == id {
						found = true
					}
				}
				if !found {
					absorbed = append(absorbed, dropped)
				}
				row["_absorbed_row_ids"] = absorbed
			}
		}
		tailRows = append(tailRows, row)
	}
	approx := beforeTokens
	opts := compressContextOptions{ApproxTokens: &approx, FocusTopic: request.FocusTopic, Force: true, DeferContextEngineNotification: true,
		SnapshotIsCurrent: o.SnapshotIsCurrent}
	if o.TaskID != "default" {
		opts.TaskID = o.TaskID
	}
	if len(tailRows) > 0 {
		opts.VerbatimTail = tailRows
	}
	compressed, _, err := a.compressContext(ctx, head, o.SystemMessage, opts)
	if err != nil {
		finalizeContextEngineCompressionNotification(a, false)
		return CompressResult{}, err
	}
	switch signal := a.compressionSkippedDueToLock.(type) {
	case bool:
		if signal {
			a.compressionSkippedDueToLock = nil
			finalizeContextEngineCompressionNotification(a, false)
			return CompressResult{Status: "lock_skipped", BeforeMessages: before, AfterMessages: before, BeforeTokens: beforeTokens,
				AfterTokens: beforeTokens, Request: request}, nil
		}
	case string:
		a.compressionSkippedDueToLock = nil
		finalizeContextEngineCompressionNotification(a, false)
		holder := signal
		return CompressResult{Status: "lock_skipped", BeforeMessages: before, AfterMessages: before, BeforeTokens: beforeTokens,
			AfterTokens: beforeTokens, Request: request, LockHolder: &holder}, nil
	}
	if len(tail) > 0 {
		allStamped := true
		for _, row := range tailRows {
			if v, ok := row[dbPersistedMarker].(bool); !ok || !v {
				allStamped = false
			}
		}
		if !allStamped {
			compressed = provider.RejoinCompressedHeadAndTail(compressed, tail)
		}
	}
	afterTokens := EstimateManualRequestTokens(ctx, a, compressed)
	summary := SummarizeManualCompression(before, compressed, beforeTokens, afterTokens, manualCompressionState(a.contextCompressor))
	return CompressResult{Status: "compressed", BeforeMessages: before, AfterMessages: append([]map[string]any(nil), compressed...),
		BeforeTokens: beforeTokens, AfterTokens: afterTokens, Request: request, Summary: &summary}, nil
}

// Truthiness of a persisted marker value as Python reads it
func truthyMarker(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	}
	return true
}

// Positive integer row id, bools and non integers refused
func positiveRowID(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), x > 0
	case int64:
		return x, x > 0
	case int32:
		return int64(x), x > 0
	}
	return 0, false
}

// Surface neutral text lines of a manual compression result, each prefixed
func RenderCompressResult(result CompressResult, prefix string) []string {
	switch result.Status {
	case "preview":
		out := make([]string, len(result.Lines))
		for i, line := range result.Lines {
			out[i] = prefix + line
		}
		return out
	case "lock_skipped":
		var holder any = true
		if result.LockHolder != nil {
			holder = *result.LockHolder
		}
		return []string{prefix + DescribeCompressionLockSkip(holder)}
	case "nothing_to_do":
		return []string{prefix + "Nothing to compress yet."}
	}
	var out []string
	if result.Summary == nil {
		return out
	}
	for _, line := range []string{result.Summary.Headline, result.Summary.TokenLine} {
		if line != "" {
			out = append(out, prefix+line)
		}
	}
	if result.Summary.Note != nil && *result.Summary.Note != "" {
		out = append(out, prefix+*result.Summary.Note)
	}
	return out
}

// Settles the deferred context engine notification of a manual compression once the caller's history commits
func FinalizeManualCompressionNotification(a *AIAgent, committed bool) bool {
	return finalizeContextEngineCompressionNotification(a, committed)
}
