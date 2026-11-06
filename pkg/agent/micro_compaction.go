package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Single exchange summarizer and marker reader the passes call, swapped by tests as hermes patches the instance
var (
	compressorMicroSummarizeOne        = (*ContextCompressor).microSummarizeOne
	compressorRollingSummaryFromMarker = rollingSummaryFromMarker
)

// Whether the row is a compaction summary marker
func isSummaryMarkerEntry(entry map[string]any) bool {
	return entry != nil && pyrepr.Truthy(entry[state.CompressedSummaryMetadataKey])
}

// Whether the row is a summary marker provably absorbed into the rolling summary
func isMicroMarkerEntry(entry map[string]any) bool {
	return isSummaryMarkerEntry(entry) && pyrepr.Truthy(entry[state.MicroCompactMarkerKey])
}

// First index not yet absorbed, rehydrating the rolling summary from a surviving marker on resume
func (c *ContextCompressor) resolveCompactCursor(messages []map[string]any, headEnd, tailStart int) int {
	if headEnd < c.microCompactCursor && c.microCompactCursor < tailStart {
		return c.microCompactCursor
	}
	last := -1
	for i := headEnd; i < tailStart; i++ {
		if state.IsCompactionSummaryMessage(messages[i]) {
			last = i
		}
	}
	cursor := headEnd
	if last >= headEnd {
		cursor = last + 1
		recovered := ""
		if pystr.Strip(c.microCompactRollingSummary) == "" {
			recovered = compressorRollingSummaryFromMarker(messages[last]["content"])
		}
		if recovered != "" {
			c.microCompactRollingSummary = recovered
			messages[last][state.MicroCompactMarkerKey] = true
			slog.Info(fmt.Sprintf("Micro-compaction: recovered rolling summary from transcript (%d chars)", pystr.Len(recovered)))
		}
	}
	c.microCompactCursor = cursor
	return cursor
}

// Next complete assistant and tool turn from start whose boundary closes the turn, ok false when none
func (c *ContextCompressor) findOneExchange(messages []map[string]any, start, tailStart int) (int, int, bool) {
	limit := min(tailStart, len(messages))
	turnRow := func(idx int, roles ...string) bool {
		role, _ := messages[idx]["role"].(string)
		return messages[idx] != nil && slices.Contains(roles, role) && !state.IsCompactionSummaryMessage(messages[idx])
	}
	idx := start
	for idx < limit && !turnRow(idx, "assistant") {
		idx++
	}
	if idx >= limit {
		return 0, 0, false
	}
	exchangeStart := idx
	idx++
	for idx < limit && turnRow(idx, "assistant", "tool") {
		idx++
	}
	if idx >= len(messages) || messages[idx] == nil {
		return 0, 0, false
	}
	if role := messages[idx]["role"]; role == "assistant" || role == "tool" {
		return 0, 0, false
	}
	return exchangeStart, idx, true
}

// Prompt messages merging one exchange into the running summary
func buildMicroSummaryPrompt(existingSummary, exchangeText string) []map[string]any {
	summaryBlock := existingSummary
	if pystr.Strip(existingSummary) == "" {
		summaryBlock = "(No previous summary yet.)"
	}
	userPrompt := "You are a summarization agent creating a compact record of an " +
		"ongoing conversation.  You are given a running summary and the " +
		"next exchange from the conversation.  Merge the exchange's key " +
		"decisions, requirements, file paths, and open questions into the " +
		"summary.  Preserve the summary's structure.  Drop resolved details " +
		"that are no longer relevant.  Add new decisions, file paths, and " +
		"open questions.\n\n" +
		"NEVER include API keys, tokens, passwords, secrets, credentials, " +
		"or connection strings in the summary — replace any that appear " +
		"with [REDACTED].\n\n" +
		"## Current Running Summary\n" + summaryBlock + "\n\n" +
		"## Next Exchange to Merge\n" + exchangeText + "\n\n" +
		"Return ONLY the updated summary text, no preamble or explanation. " +
		"Do not include this instruction block in your output."
	return []map[string]any{
		{"role": "system", "content": "You are a conversation summarization assistant."},
		{"role": "user", "content": userPrompt},
	}
}

// First choice message content of an aux response, the error when the response has no choice
func microResponseContent(response any) (string, error) {
	m, _ := response.(map[string]any)
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return "", fmt.Errorf("micro-summarization response has no choices: %s", pyrepr.Repr(response))
	}
	first, _ := choices[0].(map[string]any)
	var content any = first["message"]
	if msg, ok := content.(map[string]any); ok {
		content = msg["content"]
	}
	if s, ok := content.(string); ok {
		return s, nil
	}
	if pyrepr.Truthy(content) {
		return pyrepr.Str(content), nil
	}
	return "", nil
}

// Rolling summary with one exchange merged in through the aux model, nil on any unusable answer
func (c *ContextCompressor) microSummarizeOne(exchangeText string) (*string, error) {
	maxTokens := c.maxSummaryTokensValue()
	if maxTokens == 0 {
		maxTokens = 1500
	}
	callKwargs := map[string]any{
		"task":        "compression",
		"messages":    buildMicroSummaryPrompt(c.microCompactRollingSummary, exchangeText),
		"max_tokens":  min(1500, maxTokens),
		"temperature": 0.1,
	}
	ApplySummaryRoute(c, callKwargs, nil)
	if c.model != "" {
		if _, ok := callKwargs["main_runtime"]; !ok {
			callKwargs["main_runtime"] = map[string]any{"model": c.model, "provider": c.provider, "base_url": c.baseURL, "api_key": c.apiKey, "api_mode": c.apiMode}
		}
	}
	response, err := compressorCallLLM(provider.AuxInterruptProtection(c.ctx, true, nil, nil), callKwargs)
	if err != nil {
		slog.Info("micro-summarization call failed: " + err.Error())
		return nil, nil
	}
	if responseFinishReason(response) == "length" {
		slog.Warn("micro-summarization output hit the token cap (finish_reason=length) — discarding partial summary")
		return nil, nil
	}
	content, err := microResponseContent(response)
	if err != nil {
		return nil, err
	}
	content = pystr.Strip(provider.StripThinkBlocks(pystr.Strip(content)))
	if content == "" {
		slog.Info("micro-summarization returned empty content")
		return nil, nil
	}
	if isRefusalResponse(response, content) {
		slog.Warn("micro-summarization returned refusal content — discarding unusable summary")
		return nil, nil
	}
	return &content, nil
}

// Whether the rolling summary is large enough to defrag
func (c *ContextCompressor) needsDefrag() bool {
	return provider.EstimateTokens(c.microCompactRollingSummary) >= c.microCompactDefragThresholdTokens
}

// Re-summarizes the rolling summary and rewrites the newest micro marker in place, true when it rewrote
func (c *ContextCompressor) defragRollingSummary(messages []map[string]any) (bool, error) {
	oldSummary := c.microCompactRollingSummary
	if pystr.Strip(oldSummary) == "" {
		return false, nil
	}
	c.microCompactRollingSummary = ""
	fresh, err := compressorMicroSummarizeOne(c, oldSummary)
	if err != nil {
		return false, err
	}
	if fresh == nil {
		c.microCompactRollingSummary = oldSummary
		return false, nil
	}
	c.microCompactRollingSummary = *fresh
	for i := len(messages) - 1; i >= 0; i-- {
		if isMicroMarkerEntry(messages[i]) {
			messages[i]["content"] = renderMicroMarkerContent(*fresh)
			delete(messages[i], dbPersistedMarker)
			c.flushScanCursorInvalidated = true
			break
		}
	}
	slog.Info(fmt.Sprintf("Micro-compaction defrag: rolling summary re-summarized (%d -> %d chars)", pystr.Len(oldSummary), pystr.Len(*fresh)))
	return true, nil
}

func (c *ContextCompressor) resetMicroFailureTracking() {
	c.microCompactConsecutiveFailures = 0
	c.microCompactLastFailureCursor = -1
}

// Optional integer fields of one micro compaction telemetry line
type microTelemetry struct {
	outcome                       string
	messagesBefore, messagesAfter int
	tokensBefore, tokensAfter     *int
	exchangeTokens, durationMS    *int
	turnSessionID                 string
}

// One round of rolling micro compaction, the transcript returned unchanged on every skip or failure
func (c *ContextCompressor) microCompact(messages []map[string]any, turnSessionID string) ([]map[string]any, error) {
	if !c.microCompactEnabled {
		return messages, nil
	}
	everyN := max(1, c.microCompactEveryNTurns)
	if everyN > 1 {
		c.microCompactTurnsSincePass++
		if c.microCompactTurnsSincePass < everyN {
			return messages, nil
		}
		c.microCompactTurnsSincePass = 0
	}
	nMessages := len(messages)
	if nMessages < 4 {
		return messages, nil
	}
	exchangeStart, exchangeEnd, found := c.nextExchange(messages)
	if !found {
		return messages, nil
	}
	startedAt := compressorMonotonic()
	tokensBefore := estimateRowsRough(c.ctx, messages)
	held := append([]map[string]any(nil), messages...)
	skip, startWatermark := c.microStartWatermark(held)
	telemetry := func(outcome string, result []map[string]any, tokensAfter int, exchangeTokens *int) {
		duration := int((compressorMonotonic() - startedAt) * 1000)
		c.emitMicroCompactionTelemetry(microTelemetry{outcome: outcome, messagesBefore: nMessages, messagesAfter: len(result),
			tokensBefore: &tokensBefore, tokensAfter: &tokensAfter, exchangeTokens: exchangeTokens, durationMS: &duration, turnSessionID: turnSessionID})
	}
	if skip != "" {
		telemetry(skip, messages, tokensBefore, nil)
		return messages, nil
	}
	preCursor, preSummary := c.microCompactCursor, c.microCompactRollingSummary
	if c.needsDefrag() {
		var marker, preMarker map[string]any
		for i := len(messages) - 1; i >= 0; i-- {
			if isMicroMarkerEntry(messages[i]) {
				marker, preMarker = messages[i], maps.Clone(messages[i])
				break
			}
		}
		defragged, err := c.defragRollingSummary(messages)
		if err != nil {
			return nil, err
		}
		if defragged && !c.syncMicroCompactToDB(messages, held, startWatermark) {
			c.microCompactRollingSummary = preSummary
			if marker != nil {
				clear(marker)
				maps.Copy(marker, preMarker)
			}
			telemetry("stale_generation", messages, tokensBefore, nil)
			return messages, nil
		}
		outcome := "defrag_failed"
		if defragged {
			c.resetMicroFailureTracking()
			outcome = "defrag"
		}
		telemetry(outcome, messages, estimateRowsRough(c.ctx, messages), nil)
		return messages, nil
	}
	cumulative := pystr.Strip(c.microCompactRollingSummary) != ""
	exchangeText := serializeForSummary(messages[exchangeStart:exchangeEnd])
	exchangeTokens := provider.EstimateTokens(exchangeText)
	updated, err := compressorMicroSummarizeOne(c, exchangeText)
	if err != nil {
		return nil, err
	}
	if updated == nil {
		outcome := c.recordMicroFailure(exchangeStart, exchangeEnd)
		telemetry(outcome, messages, tokensBefore, &exchangeTokens)
		return messages, nil
	}
	c.microCompactRollingSummary = *updated
	c.microCompactCursor = exchangeEnd
	c.resetMicroFailureTracking()
	result := c.spliceMicroCompactResult(messages, exchangeStart, exchangeEnd, cumulative)
	c.microCompactCursor = cursorAfterSplice(result, exchangeStart+1)
	if !c.syncMicroCompactToDB(result, held, startWatermark) {
		c.microCompactCursor, c.microCompactRollingSummary = preCursor, preSummary
		telemetry("stale_generation", messages, tokensBefore, nil)
		return messages, nil
	}
	telemetry("absorbed", result, estimateRowsRough(c.ctx, result), &exchangeTokens)
	return result, nil
}

// Counts a summarize failure at this cursor and skips the exchange after too many in a row
func (c *ContextCompressor) recordMicroFailure(exchangeStart, exchangeEnd int) string {
	if exchangeStart == c.microCompactLastFailureCursor {
		c.microCompactConsecutiveFailures++
	} else {
		c.microCompactConsecutiveFailures = 1
	}
	c.microCompactLastFailureCursor = exchangeStart
	if c.microCompactConsecutiveFailures < microCompactMaxConsecutiveFailures {
		return "summarize_failed"
	}
	slog.Info(fmt.Sprintf("Micro-compaction: skipping exchange at cursor %d after %d consecutive failures", exchangeStart, c.microCompactConsecutiveFailures))
	c.microCompactCursor = exchangeEnd
	c.resetMicroFailureTracking()
	return "exchange_skipped"
}

// Next unabsorbed exchange inside the compressible window, ok false when none
func (c *ContextCompressor) nextExchange(messages []map[string]any) (int, int, bool) {
	compressStart := c.AlignBoundaryForward(messages, c.protectHeadSize(messages))
	compressEnd := c.findTailCut(messages, compressStart, nil, false)
	if compressStart >= compressEnd {
		return 0, 0, false
	}
	cursor := c.resolveCompactCursor(messages, compressStart, compressEnd)
	if cursor >= compressEnd {
		return 0, 0, false
	}
	return c.findOneExchange(messages, cursor, compressEnd)
}

// Rolling summary text recovered from a summary marker body
func rollingSummaryFromMarker(content any) string {
	s, ok := content.(string)
	if !ok || pystr.Strip(s) == "" {
		return ""
	}
	body := s
	if idx := strings.LastIndex(s, state.HistoricalTaskHeading); idx != -1 {
		body = s[idx+len(state.HistoricalTaskHeading):]
	}
	if end := strings.Index(body, state.SummaryEndMarker); end != -1 {
		body = body[:end]
	}
	return pystr.Strip(body)
}

// Index just past the newest summary marker of the spliced list, fallback when none
func cursorAfterSplice(result []map[string]any, fallback int) int {
	for i := len(result) - 1; i >= 0; i-- {
		if isSummaryMarkerEntry(result[i]) {
			return i + 1
		}
	}
	return fallback
}

func optInt(p *int) any {
	if p == nil {
		return nil
	}
	return telemetrySafeInt(*p)
}

// Logs and publishes one content free JSON line for a micro compaction pass
func (c *ContextCompressor) emitMicroCompactionTelemetry(t microTelemetry) {
	var delta *int
	if t.tokensBefore != nil && t.tokensAfter != nil {
		d := *t.tokensAfter - *t.tokensBefore
		delta = &d
		c.microCompactTokensSavedTotal -= d
	}
	c.microCompactPasses++
	threshold := c.thresholdTokens
	var occupancy any
	if threshold != nil && *threshold > 0 && t.tokensAfter != nil {
		occupancy = pyint.Round(float64(*t.tokensAfter)/float64(*threshold)*100, 1)
	}
	payload := pyjson.ObjectOf(
		"event", "micro_compaction", "session_id", c.sessionID, "outcome", t.outcome,
		"messages_before", t.messagesBefore, "messages_after", t.messagesAfter,
		"tokens_before", optInt(t.tokensBefore), "tokens_after", optInt(t.tokensAfter),
		"tokens_delta", optInt(delta), "exchange_tokens", optInt(t.exchangeTokens),
		"rolling_summary_tokens", provider.EstimateTokens(c.microCompactRollingSummary),
		"cursor", telemetrySafeInt(c.microCompactCursor), "passes_total", c.microCompactPasses,
		"tokens_saved_total", c.microCompactTokensSavedTotal, "duration_ms", optInt(t.durationMS),
		"threshold_tokens", optInt(threshold), "context_limit", optInt(c.resolvedContextLength),
		"occupancy_pct", occupancy, "main_model", c.model, "aux_model", c.summaryModel,
	)
	line, err := pyjson.Dumps(payload, pyjson.SortKeys(true), pyjson.Separators(",", ":"))
	if err != nil {
		slog.Debug("failed to emit micro-compaction telemetry: " + err.Error())
		return
	}
	slog.Info("micro compaction telemetry: " + line)
	record := map[string]any{}
	for _, k := range payload.Keys {
		record[k], _ = payload.Get(k)
	}
	PublishMicroCompaction(c.ctx, record, t.turnSessionID)
}

// Skip outcome and start watermark read before the slow step, empty outcome when the pass may run
func (c *ContextCompressor) microStartWatermark(held []map[string]any) (string, *int64) {
	db, ok := c.sessionDB.(archiveWatermarkDB)
	if c.sessionID == "" || !ok {
		return "", nil
	}
	watermark, err := db.GetActiveMessageWatermark(c.ctx, c.sessionID)
	if err == nil {
		_, err = archiveWatermarkFor(c.ctx, c.sessionDB, c.sessionID, held, &watermark)
	}
	var stale *StaleHeldHistory
	switch {
	case errors.As(err, &stale):
		slog.Info("micro-compaction: skipping this pass, " + err.Error())
		return "stale_generation", nil
	case err != nil:
		slog.Info("micro-compaction: watermark read failed, skipping this pass: " + err.Error())
		return "watermark_unavailable", nil
	}
	return "", &watermark
}

// Persists the micro compacted set atomically, false only when the held history was a stale generation
func (c *ContextCompressor) syncMicroCompactToDB(compacted, held []map[string]any, startWatermark *int64) bool {
	if c.sessionDB == nil || c.sessionID == "" {
		return true
	}
	err := c.archiveMicroCompaction(compacted, held, startWatermark)
	var stale *StaleHeldHistory
	var rewritten *state.HeldRowRewrittenError
	switch {
	case errors.As(err, &stale), errors.As(err, &rewritten):
		slog.Info("Micro-compaction commit skipped, " + err.Error())
		return false
	case err != nil:
		slog.Info("Micro-compaction DB sync failed — resume will double-load compacted messages until the next batch compression")
	}
	return true
}

// Archive and compact commit of a micro pass under the held watermark and coverage
func (c *ContextCompressor) archiveMicroCompaction(compacted, held []map[string]any, startWatermark *int64) error {
	var carried []map[string]any
	for _, m := range compacted {
		if m != nil && pyrepr.Truthy(m[dbPersistedMarker]) {
			carried = append(carried, m)
		}
	}
	o := state.ArchiveCompactOptions{CarriedMessages: carried, HeldForCAS: held}
	if held != nil && startWatermark != nil {
		watermark, err := archiveWatermarkFor(c.ctx, c.sessionDB, c.sessionID, held, startWatermark)
		if err != nil {
			return err
		}
		o.Watermark = watermark
		covered, unresolved, ok, err := state.CoverageForCommit(c.sessionDB, c.sessionID, held, nil)
		if err != nil {
			return err
		}
		if ok {
			o.CoveredIDs, o.UnresolvedHeld = covered, unresolved
		}
	}
	store, ok := c.sessionDB.(archiveCompactor)
	if !ok {
		return fmt.Errorf("%T object has no attribute 'archive_and_compact'", c.sessionDB)
	}
	if _, err := store.ArchiveAndCompact(c.ctx, c.sessionID, compacted, o); err != nil {
		return err
	}
	StampDBPersistedMarkers(compacted)
	return nil
}

// Replaces the exchange span with an assistant summary marker, superseding older micro markers
func (c *ContextCompressor) spliceMicroCompactResult(messages []map[string]any, spliceStart, spliceEnd int, supersede bool) []map[string]any {
	summaryText := c.microCompactRollingSummary
	if pystr.Strip(summaryText) == "" {
		return messages
	}
	summaryMsg := map[string]any{
		"role": "assistant", "content": renderMicroMarkerContent(summaryText),
		state.CompressedSummaryMetadataKey: true, state.MicroCompactMarkerKey: true, state.CompressedSummaryHasUserTurnKey: false,
	}
	result := append(append(append([]map[string]any{}, messages[:spliceStart]...), summaryMsg), messages[spliceEnd:]...)
	if !supersede {
		return result
	}
	var markers []int
	for i, m := range result {
		if isMicroMarkerEntry(m) {
			markers = append(markers, i)
		}
	}
	if len(markers) <= 1 {
		return result
	}
	stale := map[int]bool{}
	for _, i := range markers[:len(markers)-1] {
		stale[i] = true
	}
	kept := make([]map[string]any, 0, len(result))
	for i, m := range result {
		if !stale[i] {
			kept = append(kept, m)
		}
	}
	return c.mergeAdjacentUserTurns(kept)
}

// Marker content wrapped around the rolling summary text
func renderMicroMarkerContent(summaryText string) string {
	return state.SummaryPrefix + "\n\n" + state.HistoricalTaskHeading + "\n" + pystr.Strip(summaryText) + "\n\n" + state.SummaryEndMarker
}

// Merges consecutive plain text real user turns a supersede left adjacent
func (c *ContextCompressor) mergeAdjacentUserTurns(result []map[string]any) []map[string]any {
	plainUser := func(m map[string]any) bool {
		if m == nil || m["role"] != "user" || isSummaryMarkerEntry(m) {
			return false
		}
		_, ok := m["content"].(string)
		return ok
	}
	var merged []map[string]any
	for _, msg := range result {
		var prev map[string]any
		if len(merged) > 0 {
			prev = merged[len(merged)-1]
		}
		if !plainUser(msg) || !plainUser(prev) {
			merged = append(merged, msg)
			continue
		}
		var parts []string
		for _, s := range []string{prev["content"].(string), msg["content"].(string)} {
			if s != "" {
				parts = append(parts, s)
			}
		}
		prev["content"] = strings.Join(parts, "\n\n")
		dropStaleAPIContent(prev)
		display := map[string]any{}
		if dm, ok := prev["display_metadata"].(map[string]any); ok {
			maps.Copy(display, dm)
		}
		display[modelOnlyDisplayMetadataKey] = true
		prev["display_metadata"] = display
		delete(prev, dbPersistedMarker)
		c.flushScanCursorInvalidated = true
		state.RecordAbsorbedMessage(prev, msg, false)
	}
	return merged
}
