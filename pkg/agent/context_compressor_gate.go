package agent

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Pairs the real prompt count with the rough estimate and judges the armed compaction verdict
func (c *ContextCompressor) applyRealPromptVerdict() {
	if c.LastPromptTokens <= 0 {
		return
	}
	c.lastRealPromptTokens = c.LastPromptTokens
	c.providerOmitsUsage = false
	if c.LastPromptTokens < c.thresholdTokensValue() {
		c.recordIneffectiveCompressionVerdict(0)
	}
	if !c.verifyCompactionClearedThreshold {
		return
	}
	if c.LastPromptTokens >= c.thresholdTokensValue() {
		c.recordIneffectiveCompressionVerdict(c.ineffectiveCompressionCount + 1)
		if !c.quietMode {
			slog.Warn(fmt.Sprintf("Compaction did not clear the threshold: %d real tokens still >= %d. The incompressible prompt (system prompt + tool schemas) may already exceed it, in which case shrinking messages cannot help. ineffective_compression_count=%d",
				c.LastPromptTokens, c.thresholdTokensValue(), c.ineffectiveCompressionCount))
		}
		return
	}
	c.recordIneffectiveCompressionVerdict(0)
}

// Display only seed of the prompt count from the zero state
func (c *ContextCompressor) MaybeSeedPreflightDisplayTokens(preflightTokens int) {
	if c.LastPromptTokens == 0 && preflightTokens > 0 {
		c.LastPromptTokens = preflightTokens
	}
}

// Display token count before a speculative preflight seed
func (c *ContextCompressor) SnapshotPreflightDisplayTokens() int { return c.LastPromptTokens }

// Restores a speculative display seed without touching compaction state
func (c *ContextCompressor) RollbackInterruptedPreflightDisplayTokens(snapshot int) {
	if c.awaitingRealUsageAfterCompression && c.LastPromptTokens == -1 {
		return
	}
	c.LastPromptTokens = snapshot
}

// Marks the provider as omitting usage so rough estimates decide until a real reading arrives
func (c *ContextCompressor) noteUsageLessResponse() { c.providerOmitsUsage = true }

// Waits for real usage after a native checkpoint replaced durable history
func (c *ContextCompressor) NoteNativeCompactionCheckpoint() {
	c.awaitingRealUsageAfterCompression = true
	c.lastCompressionRoughTokens = 0
}

// True when a rough estimate over the trigger must wait one request for real provider usage
func (c *ContextCompressor) shouldDeferPreflightToRealUsage(roughTokens int) bool {
	if roughTokens < c.thresholdTokensValue() {
		return false
	}
	if c.awaitingRealUsageAfterCompression {
		return true
	}
	if c.lastRealPromptTokens >= c.thresholdTokensValue() {
		return false
	}
	return !c.providerOmitsUsage
}

func (c *ContextCompressor) shouldCompress(promptTokens *int) bool {
	ok, _ := c.shouldCompressInfo(promptTokens)
	return ok
}

// Whether compression runs now, with a cooldown, backoff or ineffective reason when it is needed but blocked
func (c *ContextCompressor) shouldCompressInfo(promptTokens *int) (bool, *string) {
	tokens := c.LastPromptTokens
	if promptTokens != nil {
		tokens = *promptTokens
	}
	if tokens < c.thresholdTokensValue() {
		return false, nil
	}
	if c.automaticCompressionBlocked(false) {
		reason := "blocked"
		if r := c.compressionBlockReason(false); r != nil && *r != "" {
			reason = *r
		}
		return false, &reason
	}
	return true, nil
}

// Cooldown, structural backoff or ineffective block reason, nil when nothing blocks
func (c *ContextCompressor) compressionBlockReason(ignoreCooldown bool) *string {
	cooldownUntil := c.summaryFailureCooldownUntil
	if ignoreCooldown {
		cooldownUntil = 0
	}
	for _, gate := range []struct {
		label string
		until float64
	}{{"cooldown", cooldownUntil}, {"structural_backoff", c.structuralNoOpBackoffUntil}} {
		if remaining := gate.until - compressorMonotonic(); remaining > 0 {
			r := gate.label + ":" + fmt.Sprintf("%.0f", remaining)
			return &r
		}
	}
	if c.tripped() {
		r := "ineffective"
		return &r
	}
	return nil
}

// Only provider confirmed ineffective compactions trip the breaker
func (c *ContextCompressor) tripped() bool { return c.ineffectiveCompressionCount >= 2 }

// Re reads the durable cooldown and breaker state before a gate blocks
func (c *ContextCompressor) refreshDurableGuards() {
	for _, step := range []struct {
		label string
		fn    func()
	}{{"cooldown", func() { c.GetActiveCompressionFailureCooldown(true) }}, {"ineffective-count", c.loadIneffectiveCompressionCount}} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Debug(fmt.Sprintf("compression %s refresh failed: %v", step.label, r))
				}
			}()
			step.fn()
		}()
	}
}

// Whether automatic compaction is in cooldown, backoff or tripped after a durable refresh
func (c *ContextCompressor) automaticCompressionBlocked(ignoreCooldown bool) bool {
	if !c.automaticCompressionBlockedLocally(ignoreCooldown) {
		return false
	}
	c.refreshDurableGuards()
	return c.automaticCompressionBlockedLocally(ignoreCooldown)
}

// Evaluates the automatic compaction gate on in memory state, arming or spending the recovery probe
func (c *ContextCompressor) automaticCompressionBlockedLocally(ignoreCooldown bool) bool {
	for _, gate := range []struct {
		until float64
		skip  bool
		what  string
	}{{c.summaryFailureCooldownUntil, ignoreCooldown, "summary LLM in cooldown"}, {c.structuralNoOpBackoffUntil, false, "structural no-op backoff"}} {
		if remaining := gate.until - compressorMonotonic(); remaining > 0 && !gate.skip {
			if !c.quietMode {
				slog.Debug(fmt.Sprintf("Compression deferred — %s for %.0fs more", gate.what, remaining))
			}
			return true
		}
	}
	if c.tripped() {
		now := compressorWallNow()
		if c.antiThrashRecoveryDeadline <= 0 || c.antiThrashRecoveryDeadline-now > antiThrashRecoverySeconds {
			c.setAntiThrashRecoveryDeadline(now + antiThrashRecoverySeconds)
		} else if now >= c.antiThrashRecoveryDeadline {
			c.setAntiThrashRecoveryDeadline(0)
			if c.ineffectiveCompressionCount >= 2 {
				c.recordIneffectiveCompressionVerdict(1)
			}
			if !c.quietMode {
				slog.Info(fmt.Sprintf("Anti-thrashing recovery: %.0fs elapsed since the guard tripped — allowing one compaction probe (ineffective=%d fallback=%d).",
					antiThrashRecoverySeconds, c.ineffectiveCompressionCount, c.fallbackCompressionStreak))
			}
			return false
		}
		if !c.quietMode {
			slog.Warn(fmt.Sprintf("Compression skipped — repeated compaction attempts did not restore healthy context. ineffective=%d fallback=%d. Auto-compaction will retry once in %.0fs. Consider /new to start fresh, or /compress <topic> for focused compression.",
				c.ineffectiveCompressionCount, c.fallbackCompressionStreak, max(0.0, c.antiThrashRecoveryDeadline-now)))
		}
		return true
	}
	c.setAntiThrashRecoveryDeadline(0)
	return false
}

// Accumulates row tokens newest first until the ceiling, returning the first protected index and the total
func (c *ContextCompressor) walkTailBudget(messages []map[string]any, headEnd, ceiling, minTail int, cutAtBreak bool, price func(int) int) (int, int) {
	n := len(messages)
	if price == nil {
		price = c.TailRowPricer(messages)
	}
	accumulated, cut := 0, n
	for i := n - 1; i >= headEnd; i-- {
		msgTokens := price(i)
		if accumulated+msgTokens > ceiling && n-i >= minTail {
			if cutAtBreak {
				return i, accumulated
			}
			return cut, accumulated
		}
		accumulated += msgTokens
		cut = i
	}
	return cut, accumulated
}

// First index of the protected tail, the token budget beating the count floor when given
func (c *ContextCompressor) pruneBoundary(result []map[string]any, protectTailCount int, protectTailTokens *int) int {
	if protectTailTokens == nil || *protectTailTokens <= 0 {
		return len(result) - protectTailCount
	}
	minProtect := min(protectTailCount, len(result), maxTailMessageFloor)
	boundary, _ := c.walkTailBudget(result, 0, *protectTailTokens, minProtect, true, nil)
	return min(boundary, len(result)-minProtect)
}

// Keeps the newest copy of identical tool results and back references the older ones
func dedupeToolResults(result []map[string]any) int {
	pruned := 0
	seen := map[string]bool{}
	for i := len(result) - 1; i >= 0; i-- {
		msg := result[i]
		content, ok := msg["content"].(string)
		if msg["role"] != "tool" || !ok || utf8.RuneCountInString(content) < pruneMinChars {
			continue
		}
		sum := md5.Sum(pystr.EncodeUTF8Replace(content))
		h := hex.EncodeToString(sum[:])[:12]
		if seen[h] {
			result[i] = rewritten(msg, "[Duplicate tool output — same content as a more recent call]")
			pruned++
		}
		seen[h] = true
	}
	return pruned
}

// Replaces the tool result at idx with a one line summary, sparing protected skill bodies
func demoteToolResultAt(result []map[string]any, idx int, callIDToTool map[string]toolCallRef, minPruneChars int, protectedSkills map[string]bool) bool {
	msg := result[idx]
	if msg["role"] != "tool" {
		return false
	}
	content, has := msg["content"]
	if !has {
		content = ""
	}
	multimodal := false
	if d, ok := content.(map[string]any); ok && pyTruthy(d["_multimodal"]) {
		multimodal = true
	}
	if _, isList := content.([]any); isList || multimodal {
		stripped := stripImagesFromToolMsg(msg)
		if stripped != nil {
			result[idx] = stripped
		}
		return stripped != nil
	}
	text, ok := content.(string)
	if !ok || text == "" || text == prunedToolPlaceholder || strings.HasPrefix(text, "[Duplicate tool output") || strings.HasPrefix(text, "[screenshot removed") ||
		isSummaryLine(text) || utf8.RuneCountInString(text) <= minPruneChars {
		return false
	}
	ref, found := callIDToTool[compressorStrOrEmpty(msg["tool_call_id"])]
	if !found {
		ref = toolCallRef{"unknown", ""}
	}
	toolName := pyStrOf(ref.name)
	if len(protectedSkills) > 0 && toolName == "skill_view" {
		if obj := jsonDict(ref.args); obj != nil {
			if skill, ok := obj.Get("name"); ok {
				if s, isStr := skill.(string); isStr && protectedSkills[strings.ToLower(s)] {
					return false
				}
			}
		}
	}
	result[idx] = rewritten(msg, summarizeToolResult(toolName, ref.args, text))
	return true
}

// Soft overrun ceiling of the tail, at most the window share and never under the budget
func (c *ContextCompressor) tailSoftCeiling(tokenBudget int) int {
	ceiling := int(float64(tokenBudget) * 1.5)
	if window := c.contextLength(); window > 0 {
		ceiling = min(ceiling, int(float64(window)*tailMaxContextFraction))
	}
	return max(ceiling, tokenBudget)
}

// Demotes tool bodies inside the protected tail when it alone exceeds the soft budget
func (c *ContextCompressor) pressureDemoteTail(result []map[string]any, pruneBoundary, protectTailTokens int, callIDToTool map[string]toolCallRef, minPruneChars, sparedStart, sparedEnd int) int {
	softCeiling := c.tailSoftCeiling(protectTailTokens)
	demoteEnd := len(result) - min(pressureKeepRecentMessages, len(result))
	start := max(0, pruneBoundary)
	region := func() int {
		total := 0
		for i := start; i < len(result); i++ {
			total += estimateMsgBudgetTokens(c.ctx, result[i], true)
		}
		return total
	}
	spared := func(i int) bool { return i >= sparedStart && i < sparedEnd }
	demoted := 0
	shrinkAt := func(i int) {
		if spared(i) {
			return
		}
		if demoteToolResultAt(result, i, callIDToTool, minPruneChars, nil) {
			demoted++
		}
	}
	if demoteEnd <= pruneBoundary || region() <= softCeiling {
		return 0
	}
	for i := start; i < demoteEnd; i++ {
		shrinkAt(i)
		if region() <= softCeiling {
			break
		}
	}
	if region() > softCeiling {
		lastTool := lastIndexWithRole(result, "tool")
		for i := start; i < len(result); i++ {
			if i != lastTool {
				shrinkAt(i)
			}
		}
		if lastTool >= 0 && !spared(lastTool) && lastTool >= pruneBoundary && region() > softCeiling && demoteToolResultAt(result, lastTool, callIDToTool, minPruneChars, nil) {
			demoted++
		}
	}
	if demoted > 0 && !c.quietMode {
		slog.Info(fmt.Sprintf("Pre-compression pressure demotion: reclaimed protected-tail tool output (%d change(s); protected region now ~%s tokens, soft ceiling %s)",
			demoted, groupedInt(region()), groupedInt(softCeiling)))
	}
	return demoted
}

// Pending tool round with its owning assistant row when it fits the hard share of the input window
func (c *ContextCompressor) sparedPendingToolRound(messages []map[string]any) (int, int) {
	start, end := pendingToolRound(messages)
	if end > start && start > 0 && pyTruthy(messages[start-1]["tool_calls"]) {
		start--
	}
	hardShare := int(float64(effectiveInputWindow(c.contextLength(), c.maxTokens)) * tailMaxContextFraction)
	total := 0
	for i := start; i < end; i++ {
		total += estimateMsgBudgetTokens(c.ctx, messages[i], true)
	}
	if total <= hardShare {
		return start, end
	}
	return 0, 0
}

// Projects old tool result bodies to bounded summaries without rewriting tool call arguments
func (c *ContextCompressor) pruneOldToolResults(messages []map[string]any, protectTailCount int, protectTailTokens *int, minPruneChars int) ([]map[string]any, int) {
	if len(messages) == 0 {
		return messages, 0
	}
	result := make([]map[string]any, len(messages))
	for i, m := range messages {
		result[i] = maps.Clone(m)
	}
	callIDToTool := toolCallsByID(result)
	boundary := c.pruneBoundary(result, protectTailCount, protectTailTokens)
	sparedStart, sparedEnd := c.sparedPendingToolRound(result)
	if sparedEnd > sparedStart {
		boundary = min(boundary, sparedStart)
	}
	pruned := dedupeToolResults(result)
	protected := collectProtectedSkillNames(result, boundary)
	for i := 0; i < max(0, boundary); i++ {
		if demoteToolResultAt(result, i, callIDToTool, minPruneChars, protected) {
			pruned++
		}
	}
	pruned += retireStaleToolResultImages(result, maxKeepToolImages, sparedStart, sparedEnd)
	if protectTailTokens != nil && *protectTailTokens > 0 && len(result) > 0 {
		pruned += c.pressureDemoteTail(result, boundary, *protectTailTokens, callIDToTool, minPruneChars, sparedStart, sparedEnd)
	}
	return result, pruned
}
