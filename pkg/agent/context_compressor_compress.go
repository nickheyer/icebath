package agent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

const compressionNote = "[Note: Some earlier conversation turns have been compacted into a handoff summary to preserve context space. The current session state may still reflect earlier work, so build on that summary and state rather than re-doing work. Your persistent memory (MEMORY.md, USER.md) remains fully authoritative regardless of compaction.]"

// Bounded head region whose handoff summaries reveal restart decay
func (c *ContextCompressor) restartHandoffProbeBounds(messages []map[string]any) (int, int) {
	if len(messages) == 0 || c.ProtectFirstN <= 0 {
		return 0, 0
	}
	first := 0
	if messages[0]["role"] == "system" {
		first = 1
	}
	return first, min(len(messages), first+c.ProtectFirstN+restartHandoffProbeExtraMessages)
}

// Head rows to protect beyond the system prompt, decayed to zero once the session was compressed
func (c *ContextCompressor) effectiveProtectFirstN(messages []map[string]any) int {
	if c.ContextEngineBase.CompressionCount >= 1 || (c.previousSummary != nil && *c.previousSummary != "") {
		return 0
	}
	if len(messages) > 0 && c.ProtectFirstN > 0 {
		lo, hi := c.restartHandoffProbeBounds(messages)
		for _, m := range messages[lo:hi] {
			if m != nil && state.IsCompactionSummaryMessage(m) {
				return 0
			}
		}
	}
	return c.ProtectFirstN
}

// System prompt plus the decaying first rows
func (c *ContextCompressor) protectHeadSize(messages []map[string]any) int {
	head := 0
	if len(messages) > 0 && messages[0]["role"] == "system" {
		head = 1
	}
	return head + c.effectiveProtectFirstN(messages)
}

// Pulls a compress end boundary back so a tool group is never split
func (c *ContextCompressor) alignBoundaryBackward(messages []map[string]any, idx int) int {
	if idx <= 0 || idx >= len(messages) {
		return idx
	}
	check := -1
	for i := idx - 1; i >= 0; i-- {
		if messages[i]["role"] != "tool" {
			check = i
			break
		}
	}
	if check >= 0 && messages[check]["role"] == "assistant" && pyTruthy(messages[check]["tool_calls"]) {
		return check
	}
	return idx
}

// Newest first indices of real user turns at or after headEnd
func realUserIndicesDesc(messages []map[string]any, headEnd int) []int {
	var out []int
	for i := len(messages) - 1; i >= headEnd; i-- {
		if state.IsRealUserTurn(messages[i]) {
			out = append(out, i)
		}
	}
	return out
}

// Last text bearing non summary assistant reply at or after headEnd, else the last non summary assistant
func (c *ContextCompressor) findLastAssistantMessageIdx(messages []map[string]any, headEnd int) int {
	lastAny := -1
	for i := len(messages) - 1; i >= headEnd; i-- {
		msg := messages[i]
		if msg["role"] != "assistant" || state.IsCompactionSummaryMessage(msg) {
			continue
		}
		if lastAny < 0 {
			lastAny = i
		}
		switch content := msg["content"].(type) {
		case string:
			if pystr.Strip(content) != "" {
				return i
			}
		case []any:
			for _, p := range content {
				part, ok := p.(map[string]any)
				if !ok {
					continue
				}
				t := part["text"]
				if !pyTruthy(t) {
					t = part["content"]
				}
				if s, isStr := t.(string); isStr && pystr.Strip(s) != "" {
					return i
				}
			}
		}
	}
	return lastAny
}

// Keeps the newest assistant reply in the protected tail without splitting its tool group
func (c *ContextCompressor) ensureLastAssistantMessageInTail(messages []map[string]any, cutIdx, headEnd int) int {
	last := c.findLastAssistantMessageIdx(messages, headEnd)
	if last < 0 || last >= cutIdx {
		return cutIdx
	}
	newCut := c.alignBoundaryBackward(messages, last)
	if !c.quietMode {
		slog.Debug(fmt.Sprintf("Anchoring tail cut to last assistant message at index %d (was %d, aligned to %d) to keep the previously-visible reply out of the compaction summary (#29824)", last, cutIdx, newCut))
	}
	return max(newCut, headEnd+1)
}

// Guarantees the newest user message stays in the protected tail, pushing past its pair when clamped
func (c *ContextCompressor) ensureLastUserMessageInTail(messages []map[string]any, cutIdx, headEnd int) int {
	last := c.FindLastUserMessageIdx(messages, headEnd)
	if last < 0 || last >= cutIdx {
		return cutIdx
	}
	if !c.quietMode {
		slog.Debug(fmt.Sprintf("Anchoring tail cut to last user message at index %d (was %d) to prevent active-task loss after compression", last, cutIdx))
	}
	adjusted := max(last, headEnd+1)
	if adjusted > last {
		pairEnd := c.findTurnPairEnd(messages, last)
		if !c.quietMode {
			slog.Debug(fmt.Sprintf("Causal Coupling: cut would split turn-pair at user %d; pushing cut forward to pair_end %d so the completed pair is summarised together (#22523)", last, pairEnd))
		}
		return max(pairEnd, headEnd+1)
	}
	return adjusted
}

// Whether a handoff carries the explicit active request replay after its last end marker
func hasMergedInflightReplay(message map[string]any) bool {
	if message == nil || !state.IsCompactionSummaryMessage(message) {
		return false
	}
	text := state.ContentTextForContains(message["content"])
	idx := strings.LastIndex(text, state.SummaryEndMarker)
	if idx < 0 {
		return false
	}
	rest := pystr.Lstrip(text[idx+len(state.SummaryEndMarker):])
	return strings.HasPrefix(rest, inflightTaskReplayHeader) && pystr.Strip(strings.TrimPrefix(rest, inflightTaskReplayHeader)) != ""
}

// User turn still awaiting completion across the whole transcript, nil when the run ended on a reply
func findInflightUserTask(messages []map[string]any) map[string]any {
	last := -1
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if (state.IsActionableUserTurn(msg) && isRealUserMessage(msg)) || hasMergedInflightReplay(msg) {
			last = i
			break
		}
	}
	if last < 0 {
		return nil
	}
	for i := len(messages) - 1; i > last; i-- {
		msg := messages[i]
		if msg == nil || msg["role"] != "assistant" || pyTruthy(msg["tool_calls"]) {
			break
		}
		if pystr.Strip(state.ContentTextForContains(msg["content"])) != "" {
			return nil
		}
	}
	return messages[last]
}

// Restates an unfinished user task after the handoff so the summary prefix points at it again
func (c *ContextCompressor) reappendInflightUserTask(compressed []map[string]any, inflight map[string]any) []map[string]any {
	if inflight == nil || len(compressed) == 0 {
		return compressed
	}
	carrierIdx := -1
	for i := len(compressed) - 1; i >= 0; i-- {
		if c.IsContextSummaryMessage(compressed[i]) {
			carrierIdx = i
			break
		}
	}
	if carrierIdx < 0 {
		return compressed
	}
	for _, msg := range compressed[carrierIdx+1:] {
		if state.IsRealUserTurn(msg) {
			return compressed
		}
	}
	carrier := compressed[carrierIdx]
	carrierText := state.ContentTextForContains(carrier["content"])
	_, after, found := strings.Cut(carrierText, state.SummaryEndMarker)
	if !found || pystr.Strip(after) != "" {
		return compressed
	}
	taskText := restatedRequestText(inflight["content"])
	if taskText == "" {
		return compressed
	}
	if !c.quietMode {
		slog.Info("Re-appending the in-flight user task after the compaction handoff so it stays actionable (#100818)")
	}
	lastVisible, _ := lastTemplateVisibleRole(compressed)
	var replay map[string]any
	if hasMergedInflightReplay(inflight) {
		replay = map[string]any{"role": "user", "content": taskText}
	} else {
		replay = freshCompactionMessageCopy(inflight)
	}
	delete(replay, compactionTailMarker)
	delete(replay, "timestamp")
	if _, isStr := replay["content"].(string); isStr {
		replay["content"] = inflightTaskReplayHeader + "\n" + taskText
	} else {
		replay["content"] = appendTextToContent(replay["content"], inflightTaskReplayHeader+"\n", true)
	}
	state.DropStaleAPIContent(replay)
	state.MarkInflightRestatement(replay)
	if replayUID, ok := state.MessageUIDOrNone(replay); ok {
		before := len(compressed)
		kept := slices.Clone(compressed)
		filtered := compressed[:0:0]
		for _, msg := range compressed {
			if rowPointer(msg) != rowPointer(carrier) && msg["role"] == "user" && msg[state.MessageUID] == replayUID {
				continue
			}
			filtered = append(filtered, msg)
		}
		compressed = filtered
		first := slices.IndexFunc(compressed, func(m map[string]any) bool { return m["role"] != "system" })
		if len(compressed) != before && compressed[first]["role"] != "user" {
			if _, visible := templateVisibleRole(carrier); !visible {
				compressed = kept
			} else {
				at := slices.IndexFunc(compressed, func(m map[string]any) bool { return rowPointer(m) == rowPointer(carrier) })
				compressed = slices.Delete(compressed, at, at+1)
				compressed = slices.Insert(compressed, first, carrier)
			}
		}
		if len(compressed) != before {
			role, visible := templateVisibleRole(carrier)
			at := slices.IndexFunc(compressed, func(m map[string]any) bool { return rowPointer(m) == rowPointer(carrier) })
			prior, _ := lastTemplateVisibleRole(compressed[:at])
			if visible && role == "assistant" && prior != "user" {
				carrier["role"] = "user"
				lastVisible, _ = lastTemplateVisibleRole(compressed)
			}
		}
	}
	if lastVisible == "user" {
		carrier["content"] = appendTextToContent(carrier["content"], "\n\n"+inflightTaskReplayHeader+"\n"+taskText, false)
		state.DropStaleAPIContent(carrier)
		state.RecordAbsorbedMessage(carrier, inflight, false)
		return compressed
	}
	return append(compressed, replay)
}

// Keeps the last N real user messages in the tail, one delegating to the single message anchor
func (c *ContextCompressor) ensureLastNUserMessagesInTail(messages []map[string]any, cutIdx, headEnd, n int) int {
	if n <= 1 {
		return c.ensureLastUserMessageInTail(messages, cutIdx, headEnd)
	}
	users := realUserIndicesDesc(messages, headEnd)
	if len(users) == 0 || users[min(n, len(users))-1] >= cutIdx {
		return cutIdx
	}
	return max(users[min(n, len(users))-1], headEnd+1)
}

// Index after the user, assistant and tools pair at userIdx, userIdx plus one when no reply follows
func (c *ContextCompressor) findTurnPairEnd(messages []map[string]any, userIdx int) int {
	idx := userIdx + 1
	if idx >= len(messages) || messages[idx]["role"] != "assistant" {
		return idx
	}
	return c.AlignBoundaryForward(messages, idx+1)
}

// Whether the route replays stale thinking every turn, false when the check panics
func (c *ContextCompressor) staleThinkingOnWire() (onWire bool) {
	defer func() {
		if recover() != nil {
			onWire = false
		}
	}()
	return staleThinkingReachesWire(c.apiMode, c.provider, c.model, c.baseURL)
}

// Per row tail budget on a preserved thinking native Anthropic route, nil on every other route
func (c *ContextCompressor) nativeAnthropicBudget() func(map[string]any) int {
	if c.apiMode != "anthropic_messages" || !provider.NativeAnthropicPreservesPriorThinking(c.baseURL, c.model) {
		return nil
	}
	var rejected map[string]bool
	if c.rejectedThinking != nil {
		rejected = c.rejectedThinking.SessionRejectedThinking(c.sessionID)
	} else {
		store, _ := c.sessionDB.(provider.RejectedThinkingStore)
		rejected = provider.RejectedThinkingFingerprints(store, c.sessionID)
	}
	return func(msg map[string]any) int {
		requestCopy := shallowRow(msg)
		if len(rejected) > 0 {
			provider.StripRejectedThinking(requestCopy, rejected)
		}
		projectedAny, readable := provider.NativeAnthropicAccountingProjection([]map[string]any{requestCopy})
		var projected map[string]any
		switch rows := projectedAny.(type) {
		case []map[string]any:
			projected = rows[0]
		case []any:
			projected, _ = rows[0].(map[string]any)
		}
		total := estimateMsgBudgetTokens(c.ctx, projected, true)
		for _, text := range readable {
			total += provider.EstimateTokens(text)
		}
		return total
	}
}

func shallowRow(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Tail cut used by the compression window, swapped by tests like the hermes instance patch
var compressorFindTailCutByTokens = (*ContextCompressor).findTailCutByTokens

// Tail start index from a backward token walk, keeping anchors and never splitting tool groups
func (c *ContextCompressor) findTailCutByTokens(messages []map[string]any, headEnd int, tokenBudget *int) int {
	return c.findTailCut(messages, headEnd, tokenBudget, true)
}

func (c *ContextCompressor) findTailCut(messages []map[string]any, headEnd int, tokenBudget *int, allowSplitTurn bool) int {
	budget := c.TailTokenBudget()
	if tokenBudget != nil {
		budget = *tokenBudget
	}
	n := len(messages)
	availableTail := max(0, n-headEnd-1)
	minTailFloor := max(3, min(c.ProtectLastN, maxTailMessageFloor))
	compressibleTailCap := max(3, availableTail-2)
	minTail := 0
	if availableTail > 1 {
		minTail = min(minTailFloor, compressibleTailCap, availableTail)
	}
	softCeiling := c.tailSoftCeiling(budget)
	walkFloor := minTail
	if softCeiling >= minTail*estimateMsgBudgetTokens(c.ctx, map[string]any{}, true) {
		walkFloor = 0
	}
	price := TailCutPricer(c, messages, headEnd)
	cutIdx, accumulated := c.walkTailBudget(messages, headEnd, softCeiling, walkFloor, false, price)
	if cutIdx <= headEnd && accumulated > 0 && accumulated <= softCeiling {
		cutIdx, _ = c.walkTailBudget(messages, headEnd, budget, minTail, true, price)
	}
	fallbackCut := n - minTail
	cutIdx = min(cutIdx, n-max(walkFloor, 1))
	if cutIdx <= headEnd {
		cutIdx = max(fallbackCut, headEnd+1)
	}
	cutIdx = c.alignBoundaryBackward(messages, cutIdx)
	lastUser := c.FindLastUserMessageIdx(messages, headEnd)
	userAnchored := c.ensureLastUserMessageInTail(messages, cutIdx, headEnd)
	splitOversized := false
	if allowSplitTurn && userAnchored < cutIdx && isTextOnlyContent(messages[lastUser]["content"]) &&
		estimateMsgBudgetTokens(c.ctx, messages[lastUser], true) <= softCeiling && anyToolCalls(messages, lastUser, cutIdx) {
		if walked, _ := c.walkTailBudget(messages, userAnchored, softCeiling, 0, false, nil); walked > userAnchored {
			splitOversized = true
			if !c.quietMode {
				slog.Debug(fmt.Sprintf("Active turn exceeds protected-tail soft ceiling; keeping tool-group-aligned mid-turn cut at index %d instead of anchoring user message %d (#80449)", cutIdx, lastUser))
			}
		}
	}
	if !splitOversized {
		cutIdx = userAnchored
		asstAnchored := c.ensureLastAssistantMessageInTail(messages, cutIdx, headEnd)
		overCeiling := false
		if asstAnchored < cutIdx && allowSplitTurn && anyToolCalls(messages, asstAnchored, cutIdx) {
			rowPrice, total := c.TailRowPricer(messages), 0
			for i := asstAnchored; i < cutIdx; i++ {
				total += rowPrice(i)
			}
			overCeiling = total > softCeiling
		}
		if overCeiling {
			if !c.quietMode {
				slog.Debug(fmt.Sprintf("Assistant reply anchor would retain an over-ceiling region; keeping tool-group-aligned cut at index %d instead of anchoring to %d (#131412)", cutIdx, asstAnchored))
			}
		} else {
			cutIdx = asstAnchored
		}
	}
	if c.minTailUserMessages > 1 {
		cutIdx = c.ensureLastNUserMessagesInTail(messages, cutIdx, headEnd, c.minTailUserMessages)
	}
	return min(n, c.AlignBoundaryForward(messages, max(cutIdx, TailCutProgressFloor(c, messages, headEnd, cutIdx))))
}

func anyToolCalls(messages []map[string]any, lo, hi int) bool {
	for i := max(lo, 0); i < hi && i < len(messages); i++ {
		if pyTruthy(messages[i]["tool_calls"]) {
			return true
		}
	}
	return false
}

// Whether a compressible middle exists between the protected head and tail
func (c *ContextCompressor) hasContentToCompress(messages []map[string]any) bool {
	start := c.AlignBoundaryForward(messages, c.protectHeadSize(messages))
	return start < compressorFindTailCutByTokens(c, messages, start, nil)
}

// Rehydrates the previous summary and provenance from in transcript handoffs and unwraps them from the window
func (c *ContextCompressor) scanWindowHandoffs(messages []map[string]any, start, end int, turns []map[string]any) HandoffScan {
	scan := HandoffScan{TurnsToSummarize: turns, SummaryIndices: map[int]bool{}, TailStart: end, PreviousSummaryBefore: c.previousSummary, HasUserTurnBefore: c.summaryHasUserTurn}
	searchStart := 0
	if len(messages) > 0 && messages[0]["role"] == "system" {
		searchStart = 1
	}
	hits := findContextSummaries(messages, searchStart, len(messages))
	realUser := transcriptHasRealUserTurn(messages)
	if len(hits) == 0 {
		if c.previousSummary != nil && *c.previousSummary != "" {
			c.previousSummary = nil
		}
		c.summaryHasUserTurn = boolPtr(realUser)
		return scan
	}
	last := hits[len(hits)-1]
	if c.previousSummary == nil || *c.previousSummary == "" {
		var bodies []string
		for _, h := range hits {
			if h.body != "" {
				bodies = append(bodies, h.body)
			}
		}
		if joined := strings.Join(bodies, "\n\n"); joined != "" {
			c.previousSummary = &joined
		}
	}
	provenance, isBool := messages[last.idx][state.CompressedSummaryHasUserTurnKey].(bool)
	switch {
	case realUser:
		c.summaryHasUserTurn = boolPtr(true)
	case isBool:
		c.summaryHasUserTurn = boolPtr(provenance)
	case c.summaryHasUserTurn == nil:
		c.summaryHasUserTurn = boolPtr(!(last.body != "" && strings.Contains(last.body, noUserTaskSentinel)))
	}
	for _, h := range hits {
		scan.SummaryIndices[h.idx] = true
	}
	windowRow := func(idx int, msg map[string]any) map[string]any {
		if !scan.SummaryIndices[idx] {
			return msg
		}
		return state.StripContextSummaryHandoffMessage(freshCompactionMessageCopy(msg))
	}
	var window []map[string]any
	for idx := start; idx < last.idx; idx++ {
		if row := windowRow(idx, messages[idx]); row != nil {
			window = append(window, row)
		}
	}
	if row := windowRow(last.idx, messages[last.idx]); row != nil {
		window = append(window, row)
	}
	scan.TurnsToSummarize = append(window, pySlice(messages, last.idx+1, end)...)
	if last.idx >= end {
		scan.TailStart = last.idx + 1
	}
	return scan
}

// Resets the per attempt outcome flags and opens the attempt telemetry
func (c *ContextCompressor) beginCompressAttempt(currentTokens *int, force bool) *pyjson.Object {
	c.LastSummaryDroppedCount = 0
	c.LastSummaryFallbackUsed = false
	c.lastFeasibilitySkip = false
	c.lastSummaryError = nil
	c.LastAuxModelFailureError, c.LastAuxModelFailureModel = "", ""
	c.lastCompressAborted, c.lastCompressRefusedWouldGrow = false, false
	c.lastSummaryOverloadDegraded, c.lastCompressionMadeProgress = false, false
	c.beginCompressionTelemetry(currentTokens)
	telemetry := c.Active
	telemetry.Set("chunk_count", 0)
	if force {
		c.clearCompressionFailureCooldown()
		c.structuralNoOpBackoffUntil = 0
	}
	return telemetry
}

// Opens content free attempt telemetry from the seed and the main runtime
func (c *ContextCompressor) beginCompressionTelemetry(currentTokens *int) {
	var current any
	if currentTokens != nil {
		current = *currentTokens
	}
	c.Begin(TelemetryAttempt{CurrentTokens: current, Provider: c.provider, Model: c.model, ContextLength: c.contextLength(), ThresholdTokens: c.thresholdTokensValue()})
}

func (c *ContextCompressor) structuralNoOpResult(telemetry *pyjson.Object, failureClass, reason string, nMessages int) {
	telemetry.Set("failure_class", failureClass)
	c.lastCompressionSavingsPct = 0
	c.recordStructuralNoOp(reason, nMessages)
}

// Lifts the structural backoff once the transcript has grown past the no-op length
func (c *ContextCompressor) LiftStructuralBackoffIfGrown(nMessages int) {
	if c.structuralNoOpBackoffUntil != 0 && nMessages > c.structuralNoOpLen {
		c.structuralNoOpBackoffUntil = 0
	}
}

// Drops blank user echoes after the newest user event when an assistant follows
func (c *ContextCompressor) dropBlankEchoes(messages []map[string]any) []map[string]any {
	blank := blankEchoIndicesAfter(messages, c.FindLastUserMessageIdx(messages, 0))
	if len(blank) == 0 {
		return messages
	}
	out := make([]map[string]any, 0, len(messages)-len(blank))
	for i, m := range messages {
		if !blank[i] {
			out = append(out, m)
		}
	}
	return out
}

func (c *ContextCompressor) logCompressionStart(displayTokens, start, end, nTurns, tailMsgs int) {
	slog.Info(fmt.Sprintf("Context compression triggered (%d tokens >= %d threshold)", displayTokens, c.thresholdTokensValue()))
	slog.Info(fmt.Sprintf("Model context limit: %d tokens (%.0f%% = %d)", c.contextLength(), c.ThresholdPercent*100, c.thresholdTokensValue()))
	slog.Info(fmt.Sprintf("Summarizing turns %d-%d (%d turns), protecting %d head + %d tail messages", start+1, end, nTurns, start, tailMsgs))
}

// Aborts with messages unchanged on a terminal failure or when configured to, true when aborted
func (c *ContextCompressor) abortOnSummaryFailureAttempt(ctx context.Context, telemetry *pyjson.Object, nSkipped int, previousBefore *string) bool {
	var terminal *terminalSummaryFailure
	for i := range terminalSummaryFailures {
		if *c.terminalFailureFlag(terminalSummaryFailures[i].Flag) {
			terminal = &terminalSummaryFailures[i]
			break
		}
	}
	if terminal == nil && !c.abortOnSummaryFailure {
		return false
	}
	c.LastSummaryDroppedCount = 0
	c.LastSummaryFallbackUsed = false
	c.lastCompressAborted = true
	failureClass, message := "summary_generation_aborted", "Summary generation failed — aborting compression (compression.abort_on_summary_failure=true). %d message(s) preserved unchanged. Conversation is frozen until the next /compress or /new."
	if terminal != nil {
		failureClass, message = terminal.Class, terminal.Message
	}
	telemetry.Set("failure_class", failureClass)
	if callerAttemptIsCurrent(ctx, c) {
		c.previousSummary = previousBefore
	}
	if !c.quietMode {
		slog.Warn(fmt.Sprintf(message, nSkipped))
	}
	return true
}

// Fresh head rows with the compression note on the system prompt and handoffs unwrapped
func (c *ContextCompressor) assembleHead(messages []map[string]any, start int) []map[string]any {
	var out []map[string]any
	for i := 0; i < start; i++ {
		msg := freshCompactionMessageCopy(messages[i])
		if i == 0 && msg["role"] == "system" {
			existing := msg["content"]
			if !strings.Contains(state.ContentTextForContains(existing), compressionNote) {
				sep := ""
				if s, ok := existing.(string); ok && s != "" {
					sep = "\n\n"
				}
				msg["content"] = appendTextToContent(existing, sep+compressionNote, false)
			}
		}
		if stripped := state.StripContextSummaryHandoffMessage(msg); stripped != nil {
			out = append(out, stripped)
		}
	}
	return out
}

// Deterministic fallback summary for the window, recorded as the new iterative base
func (c *ContextCompressor) fallbackSummaryForWindow(telemetry *pyjson.Object, turns []map[string]any, nDropped int, feasibilitySkip bool) string {
	if !c.quietMode && feasibilitySkip {
		slog.Info("Pre-LLM summary skip — inserting deterministic fallback context summary")
	} else if !c.quietMode {
		slog.Warn("Summary generation failed — inserting deterministic fallback context summary")
	}
	c.LastSummaryDroppedCount = nDropped
	c.LastSummaryFallbackUsed = true
	telemetry.Set("fallback_used", true)
	switch existing, _ := telemetry.Get("failure_class"); {
	case c.lastSummaryOverloadDegraded:
		telemetry.Set("failure_class", "summary_overload_degraded")
	case pyTruthy(existing):
	case feasibilitySkip:
		telemetry.Set("failure_class", "feasibility_skip")
	default:
		telemetry.Set("failure_class", "summary_generation_failed")
	}
	reason := ""
	if !feasibilitySkip && c.lastSummaryError != nil {
		reason = *c.lastSummaryError
	}
	summary := c.buildStaticFallbackSummary(turns, reason)
	body := stripSummaryPrefix(summary)
	c.previousSummary = &body
	return summary
}

// Fresh tail rows after the window, skipping handoffs already folded into the summary
func assembleTail(messages []map[string]any, end, tailStart int, summaryIndices map[int]bool) []map[string]any {
	var out []map[string]any
	for i := max(end, tailStart); i < len(messages); i++ {
		if summaryIndices[i] && i >= tailStart {
			continue
		}
		if stripped := state.StripContextSummaryHandoffMessage(freshCompactionMessageCopy(messages[i])); stripped != nil {
			out = append(out, stripped)
		}
	}
	return out
}

// Summary role, tail merge, forced user lead and first visible tail index keeping template alternation
func summaryPlacement(compressed, tail []map[string]any, start int) (string, bool, bool, int) {
	lastHead, lastHeadKnown := "user", true
	if len(compressed) > 0 {
		lastHead, lastHeadKnown = lastTemplateVisibleRole(compressed)
	}
	firstIdx, firstRole := -1, ""
	for i, m := range tail {
		if role, ok := templateVisibleRole(m); ok {
			firstIdx, firstRole = i, role
			break
		}
	}
	hasUserText := false
	for _, m := range append(slices.Clone(compressed), tail...) {
		if m["role"] == "user" && pystr.Strip(state.ContentTextForContains(m["content"])) != "" {
			hasUserText = true
			break
		}
	}
	forceUser := start == 0 || (lastHeadKnown && lastHead == "system") || !hasUserText
	role := "assistant"
	if !lastHeadKnown || lastHead == "assistant" || lastHead == "tool" || forceUser {
		role = "user"
	}
	merge := false
	if firstIdx >= 0 && role == firstRole {
		flipped := "user"
		if role == "user" {
			flipped = "assistant"
		}
		if lastHeadKnown && flipped != lastHead && !forceUser {
			role = flipped
		} else {
			merge = len(tail) > 0
		}
	}
	return role, merge, forceUser, firstIdx
}

// Merges the summary into a tail row as prior context or as a leading user handoff
func (c *ContextCompressor) mergeSummaryIntoTailRow(msg map[string]any, summary, role string, forceUser bool) {
	old, has := msg["content"]
	if !has {
		old = ""
	}
	if forceUser && role == "user" {
		msg["content"] = appendTextToContent(old, summary+"\n\n"+state.SummaryEndMarker+"\n\n", true)
	} else {
		suffix := "\n\n" + state.MergedSummaryDelimiter + "\n\n" + summary + "\n\n" + state.SummaryEndMarker
		msg["content"] = appendTextToContent(appendTextToContent(old, suffix, false), state.MergedPriorContextHeader+"\n", true)
	}
	msg[state.CompressedSummaryMetadataKey] = true
	msg[state.CompressedSummaryHasUserTurnKey] = c.summaryHasUserTurn != nil && *c.summaryHasUserTurn
	state.DropStaleAPIContent(msg)
}

// Sanitizes pairs, restates the inflight task, strips stale media and markers, and records the effect
func (c *ContextCompressor) finalizeCompressed(ctx context.Context, compressed, messages []map[string]any, nMessages int, sparePendingImages bool, prunedCount int) []map[string]any {
	compressed = c.sanitizeToolPairs(compressed)
	sparedStart, sparedEnd := 0, 0
	if sparePendingImages {
		sparedStart, sparedEnd = pendingToolRound(compressed)
	}
	compressed = c.reappendInflightUserTask(compressed, findInflightUserTask(messages))
	c.ContextEngineBase.CompressionCount++
	compressed = stripHistoricalMedia(compressed, sparedStart, sparedEnd)
	pre := estimateRowsRough(ctx, messages)
	saved := pre - estimateRowsRough(ctx, compressed)
	savingsPct := 0.0
	if pre > 0 {
		savingsPct = float64(saved) / float64(pre) * 100
	}
	c.lastCompressionSavingsPct = savingsPct
	if !c.quietMode {
		slog.Info(fmt.Sprintf("Compressed: %d -> %d messages (~%d tokens saved, %.0f%%)", nMessages, len(compressed), saved, savingsPct))
		slog.Info(fmt.Sprintf("Compression #%d complete", c.ContextEngineBase.CompressionCount))
	}
	stripPersistenceMarkers(compressed)
	prunedReplay := pruneStaleReasoningReplay(compressed)
	if prunedReplay > 0 && !c.quietMode {
		slog.Info(fmt.Sprintf("Pruned stale replay items from %d assistant message(s) during compaction", prunedReplay))
	}
	c.RecordEffect(ctx, nMessages, compressed, pre, prunedCount, prunedReplay)
	c.lastCompressionMadeProgress = true
	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Debug(fmt.Sprintf("post-compression memory trim failed: %v", r))
			}
		}()
		memory.TrimMemory(ctx, c.env, memory.TrimOptions{Reason: "post-compression", AllocatorOnly: true})
	}()
	c.resetMicroCompactCursorState()
	c.resetProactivePruneRearm()
	return compressed
}

func (c *ContextCompressor) compress(ctx context.Context, messages []map[string]any, currentTokens *int, focusTopic *string, force bool, memoryContext string) ([]map[string]any, error) {
	return c.CompressBypassingCooldown(ctx, messages, currentTokens, focusTopic, force, memoryContext, false)
}

// Prunes old tool bodies, protects head and tail, summarizes the middle and repairs tool pairs
func (c *ContextCompressor) CompressBypassingCooldown(ctx context.Context, messages []map[string]any, currentTokens *int, focusTopic *string, force bool, memoryContext string, bypassCooldown bool) ([]map[string]any, error) {
	if err := raiseIfStaleAttempt(ctx, c); err != nil {
		return nil, err
	}
	telemetry := c.beginCompressAttempt(currentTokens, force)
	telemetry.Set("has_focus_topic", focusTopic != nil && *focusTopic != "")
	nMessages := len(messages)
	minForCompress := c.protectHeadSize(messages) + 3 + 1
	if nMessages <= minForCompress {
		c.structuralNoOpResult(telemetry, "insufficient_messages", fmt.Sprintf("only %d messages (need > %d)", nMessages, minForCompress), nMessages)
		return messages, nil
	}
	displayTokens := c.LastPromptTokens
	if currentTokens != nil && *currentTokens != 0 {
		displayTokens = *currentTokens
	} else if displayTokens == 0 {
		displayTokens = estimateRowsRough(ctx, messages)
	}
	ps, pe := c.sparedPendingToolRound(messages)
	sparePendingImages := pe > ps
	canonical := c.dropBlankEchoes(messages)
	messages, prunedCount, start, end := c.planCompactionWindow(messages)
	if prunedCount > 0 && !c.quietMode {
		slog.Info(fmt.Sprintf("Pre-compression: pruned %d old tool result(s)", prunedCount))
	}
	nMessages = len(messages)
	if start >= end {
		c.RecordRegions(ctx, pySlice(messages, 0, start), nil, pySlice(messages, end, len(messages)))
		c.structuralNoOpResult(telemetry, "no_compressible_window", fmt.Sprintf("compress_start (%d) >= compress_end (%d) - transcript fits within tail budget", start, end), nMessages)
		return canonical, nil
	}
	prepared := c.takePreparedSummary(ctx, messages, start, force || (focusTopic != nil && *focusTopic != "") || bypassCooldown, pystr.Strip(memoryContext) != "")
	if prepared != nil {
		end = prepared.CompressEnd
	}
	turns := pySlice(messages, start, end)
	if c.tailMode == "lean" {
		messages = c.demoteStaleTailTools(messages, end)
	}
	scan := c.scanWindowHandoffs(messages, start, end, turns)
	turns = scan.TurnsToSummarize
	c.RecordRegions(ctx, pySlice(messages, 0, start), turns, pySlice(messages, end, len(messages)))
	if len(turns) > 0 {
		telemetry.Set("chunk_count", 1)
	} else {
		telemetry.Set("chunk_count", 0)
	}
	if len(turns) == 0 {
		c.structuralNoOpResult(telemetry, "empty_post_handoff_window", fmt.Sprintf("window %d-%d holds only already-summarized handoffs", start, end), nMessages)
		return canonical, nil
	}
	if !c.quietMode {
		c.logCompressionStart(displayTokens, start, end, len(turns), nMessages-scan.TailStart)
	}
	if err := raiseIfStaleAttempt(ctx, c); err != nil {
		return nil, err
	}
	feasibilitySkip := !(force || prepared != nil) && (c.fallbackStreakSkip(telemetry) || c.feasibilitySkip(telemetry, turns, start, end))
	summary := ""
	if prepared != nil {
		summary = c.adoptPreparedSummary(ctx, prepared)
	} else if !feasibilitySkip {
		got, err := SummarizeWindow(ctx, c, messages, turns, scan, focusTopic, memoryContext, bypassCooldown)
		if err != nil {
			return nil, err
		}
		if got != nil {
			summary = *got
		}
		if summary == "" && c.abortOnSummaryFailureAttempt(ctx, telemetry, end-start, scan.PreviousSummaryBefore) {
			return canonical, nil
		}
	}
	if summary == "" {
		summary = c.fallbackSummaryForWindow(telemetry, turns, end-start, feasibilitySkip)
	}
	compressed := c.assembleCompressed(messages, start, end, scan, summary)
	return c.finalizeCompressed(ctx, compressed, canonical, nMessages, sparePendingImages, prunedCount), nil
}

// Head, summary and tail from the pruned copy with the tail rows marked
func (c *ContextCompressor) assembleCompressed(messages []map[string]any, start, end int, scan HandoffScan, summary string) []map[string]any {
	compressed := c.assembleHead(messages, start)
	tail := assembleTail(messages, end, scan.TailStart, scan.SummaryIndices)
	summary, compressed, tail = WithSkillSafetyRule(summary, compressed, tail)
	role, merge, forceUser, firstIdx := summaryPlacement(compressed, tail, start)
	hasUser := c.summaryHasUserTurn != nil && *c.summaryHasUserTurn
	if !merge {
		compressed = append(compressed, map[string]any{"role": role, "content": summary + "\n\n" + state.SummaryEndMarker,
			state.CompressedSummaryMetadataKey: true, state.CompressedSummaryHasUserTurnKey: hasUser})
	}
	target := 0
	if forceUser && firstIdx >= 0 {
		target = firstIdx
	}
	for i, msg := range tail {
		if msg != nil {
			msg[compactionTailMarker] = true
		}
		if merge && i == target {
			c.mergeSummaryIntoTailRow(msg, summary, role, forceUser)
		}
		compressed = append(compressed, msg)
	}
	return compressed
}
