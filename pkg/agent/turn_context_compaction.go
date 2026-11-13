package agent

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Locals rebuilt by turn start compaction that the turn context reads back
type CompactionOutcome struct {
	Messages            []*state.Message
	ActiveSystemPrompt  string
	ConversationHistory []*state.Message
	CurrentTurnUserIdx  int
	Compressed, Blocked bool
}

// Keyword arguments of the turn start compaction
type turnStartCompactionRequest struct {
	Messages            []*state.Message
	SystemMessage       *string
	ActiveSystemPrompt  string
	ConversationHistory []*state.Message
	CurrentTurnUserIdx  int
	UserMessage         any
	EffectiveTaskID     string
}

// Request estimate every compaction pass measures, the seam the preflight tests replace
var compactionRequestTokens = preflightRequestTokens

func idleCompactionStatus(idleSeconds, tokens int) string {
	return fmt.Sprintf("💤 Resumed after %ds idle — compacting ~%s tokens before continuing.", idleSeconds, groupThousands(tokens))
}

func preflightCompressionStatus(tokens, threshold int) string {
	return fmt.Sprintf("📦 Preflight compression: ~%s tokens >= %s threshold. This may take a moment.", groupThousands(tokens), groupThousands(threshold))
}

func preAPICompressionStatus(tokens int) string {
	return fmt.Sprintf("📦 Pre-API compression: ~%s tokens near the context/output limit. Compacting before the next model call.", groupThousands(tokens))
}

// Re-arms the context overflow warning dedup once compression actually runs
func clearOverflowWarn(a *AIAgent) { a.clearContextOverflowWarn() }

// Gives the compacted request a fresh chance by clearing retry and empty response state
func resetRetryStateAfterCompaction(a *AIAgent) {
	a.emptyContentRetries, a.thinkingPrefillRetries = 0, 0
	a.lastContentWithTools, a.lastContentToolsAllHousekeeping, a.mutePostResponse = nil, false, false
}

// Why an over threshold request is blocked, the spent attempt budget named when the engine still wants to compress
func blockedCompressReason(compressor ContextEngine, tokens int, attemptsSpent *int) (reason *string) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			reason = nil
		}
	}()
	shouldNow, why := ShouldCompressInfo(compressor, &tokens)
	if attemptsSpent != nil && shouldNow && (why == nil || *why == "") {
		spent := fmt.Sprintf("attempts_exhausted:%d", *attemptsSpent)
		return &spent
	}
	return why
}

// Recalibrates the compressor to the bigger window a managed local runtime granted
func applyGrownWindow(a *AIAgent, compressor ContextEngine, grown int) {
	compressor.UpdateModel(a.model, grown, a.baseURL, a.apiKey, a.provider, a.apiMode)
	a.bufferStatus(fmt.Sprintf("📈 Context window grown to %dK (local model; conversation continues uncompressed)", grown/1024))
}

// Refunds the call count and iteration budget of a pass that never reached the provider
func refundAPICall(a *AIAgent, apiCallCount int) int {
	apiCallCount--
	a.apiCallCount.Store(int64(apiCallCount))
	if budget := a.iterationBudget.Load(); budget != nil {
		budget.Refund()
	}
	return apiCallCount
}

// Re-anchors this turn's user index after compaction so the stamp and persist override hit one row
func reanchor(a *AIAgent, messages []*state.Message, userMessage any) int {
	idx := reanchorCurrentTurnUserIdx(messages, userMessage)
	a.persistUserMessageIdx = &idx
	return idx
}

// Whether two row lists are the same list object, the Python is test
func sameRowSlice(x, y []map[string]any) bool {
	return x != nil && y != nil && len(x) == len(y) && cap(x) == cap(y) && unsafe.SliceData(x) == unsafe.SliceData(y)
}

// Whether two dicts are the same object, two nils counting as None is None
func sameMapObject(x, y map[string]any) bool {
	if x == nil || y == nil {
		return x == nil && y == nil
	}
	return reflect.ValueOf(x).UnsafePointer() == reflect.ValueOf(y).UnsafePointer()
}

// One compress_context call over the live messages, the input list kept when the pass skipped
type liveCompaction struct {
	messages []*state.Message
	rows     []map[string]any
	system   string
	skipped  bool
}

// Compresses the live messages with the caller's trigger label
func compactLive(ctx context.Context, a *AIAgent, messages []*state.Message, systemMessage *string, approxTokens int, taskID, trigger string) (liveCompaction, error) {
	rows := messagesToRows(messages)
	tokens, label := approxTokens, trigger
	out, system, err := a.compressContext(ctx, rows, systemMessage, compressContextOptions{ApproxTokens: &tokens, TaskID: taskID, Trigger: &label})
	if err != nil {
		return liveCompaction{messages: messages, rows: rows, skipped: true}, err
	}
	if sameRowSlice(out, rows) {
		return liveCompaction{messages: messages, rows: rows, system: system, skipped: true}, nil
	}
	return liveCompaction{messages: rowsToMessages(out), rows: out, system: system}, nil
}

// Flush baseline after a real compaction, sharing message identity with the compacted list
func historyAfterCompaction(a *AIAgent, lc liveCompaction, previous []*state.Message) []*state.Message {
	var prevRows []map[string]any
	if previous != nil {
		prevRows = messagesToRows(previous)
	}
	got := conversationHistoryAfterCompression(a, lc.rows, prevRows)
	switch {
	case got == nil:
		return nil
	case sameRowSlice(got, prevRows):
		return previous
	}
	return slices.Clone(lc.messages)
}

// Whether the compressor still waits on the first real usage after its last compaction
func awaitingRealUsageAfterCompression(compressor ContextEngine) bool {
	c, ok := compressor.(interface{ AwaitingRealUsageAfterCompression() bool })
	return ok && c.AwaitingRealUsageAfterCompression()
}

// Live compression failure cooldown of the engine, nil when the engine has none or none is active
func activeCompressionFailureCooldown(compressor ContextEngine) map[string]any {
	if c, ok := compressor.(interface {
		GetActiveCompressionFailureCooldown(bool) map[string]any
	}); ok {
		return c.GetActiveCompressionFailureCooldown(false)
	}
	return nil
}

// Python type name of an engine for the attribute errors a missing engine field raises
func engineAttributeError(compressor ContextEngine, attr string) error {
	name := reflect.TypeOf(compressor).String()
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return fmt.Errorf("'%s' object has no attribute '%s'", name, attr)
}

// Runs idle compaction then preflight compression or the uncompressed overflow re-arm at turn start
func runTurnStartCompaction(ctx context.Context, a *AIAgent, r turnStartCompactionRequest) (CompactionOutcome, error) {
	out := CompactionOutcome{
		Messages: r.Messages, ActiveSystemPrompt: r.ActiveSystemPrompt,
		ConversationHistory: r.ConversationHistory, CurrentTurnUserIdx: r.CurrentTurnUserIdx,
	}
	if err := idleCompaction(ctx, a, &out, r.SystemMessage, r.UserMessage, r.EffectiveTaskID); err != nil {
		return out, err
	}
	err := preflightCompression(ctx, a, &out, r.SystemMessage, r.UserMessage, r.EffectiveTaskID)
	return out, err
}

// Compacts a session resumed after the configured idle gap when it sits above the post compaction floor
func idleCompaction(ctx context.Context, a *AIAgent, out *CompactionOutcome, systemMessage *string, userMessage any, taskID string) error {
	messages := out.Messages
	idleAfter := int(a.compressionIdleCompactAfterSeconds)
	if !(a.compressionEnabled && idleAfter > 0 && len(messages) > 0) {
		return nil
	}
	idleGap := loopNow() - a.lastActivityTS
	if idleGap < float64(idleAfter) {
		return nil
	}
	compressor := a.contextCompressor
	if awaitingRealUsageAfterCompression(compressor) {
		return nil
	}
	idleTokens := compactionRequestTokens(ctx, a, messages, out.ActiveSystemPrompt)
	ratioer, ok := compressor.(interface{ SummaryTargetRatio() float64 })
	if !ok {
		return engineAttributeError(compressor, "summary_target_ratio")
	}
	idleFloor := int(float64(compressor.Base().ThresholdTokens) * ratioer.SummaryTargetRatio())
	cooldown := activeCompressionFailureCooldown(compressor)
	lastCompaction := 0
	if c, ok := compressor.(interface{ LastCompressionRoughTokens() int }); ok {
		lastCompaction = c.LastCompressionRoughTokens()
	}
	if !shouldIdleCompact(a.compressionEnabled, idleAfter, idleGap, idleTokens, idleFloor, len(cooldown) > 0, lastCompaction) {
		return nil
	}
	lastText := "n/a"
	if lastCompaction > 0 {
		lastText = groupThousands(lastCompaction)
	}
	turnContextLog().Info(fmt.Sprintf("Idle compaction: %ds idle >= %ds, ~%s tokens > %s floor (last compaction produced ~%s) (session %s)",
		int(idleGap), idleAfter, groupThousands(idleTokens), groupThousands(idleFloor), lastText, sessionOrNone(a)))
	status := AutomaticCompactionStatusMessage(compressor, "idle", idleCompactionStatus(int(idleGap), idleTokens),
		map[string]any{"approx_tokens": idleTokens, "idle_seconds": int(idleGap), "model": a.model})
	if status != nil {
		a.emitStatus(*status)
	}
	lc, err := compactLive(ctx, a, messages, systemMessage, idleTokens, taskID, "idle")
	if err != nil {
		return err
	}
	out.Messages, out.ActiveSystemPrompt = lc.messages, lc.system
	if !lc.skipped {
		out.ConversationHistory = historyAfterCompaction(a, lc, out.ConversationHistory)
		out.CurrentTurnUserIdx = reanchor(a, out.Messages, userMessage)
	}
	return nil
}

func sessionOrNone(a *AIAgent) string {
	if a.sessionID == "" {
		return "none"
	}
	return a.sessionID
}

// True when a codex app server thread compacts itself, Icebath compacting only in hermes mode
func codexNativeAutoCompaction(a *AIAgent) bool {
	mode := a.codexAppServerAutoCompaction
	if mode == "" {
		mode = "native"
	}
	mode = strings.ToLower(mode)
	return a.apiMode == "codex_app_server" && (mode == "native" || mode == "off")
}

// Preflight context compression at turn start, the cheap row and size gate guarding the full estimate
func preflightCompression(ctx context.Context, a *AIAgent, out *CompactionOutcome, systemMessage *string, userMessage any, taskID string) error {
	a.turnReceivedProviderResponse = false
	a.turnPreflightDisplaySnapshot = nil
	if !a.compressionEnabled {
		rearmUncompressedOverflowWarn(ctx, a, out.Messages, out.ActiveSystemPrompt)
		return nil
	}
	compressor := a.contextCompressor
	if l, ok := compressor.(interface{ LiftStructuralBackoffIfGrown(int) }); ok {
		l.LiftStructuralBackoffIfGrown(len(out.Messages))
	}
	base := compressor.Base()
	if reviewForkFirstRequestPending(a) || !shouldRunPreflightEstimate(ctx, out.Messages, base.ProtectFirstN, base.ProtectLastN, base.ThresholdTokens) {
		return nil
	}
	tokens := compactionRequestTokens(ctx, a, out.Messages, out.ActiveSystemPrompt)
	if s, ok := compressor.(interface{ SnapshotPreflightDisplayTokens() int }); ok {
		snapshot := s.SnapshotPreflightDisplayTokens()
		a.turnPreflightDisplaySnapshot = &snapshot
	}
	deferred := !a.requestPressureAnchored && compressor.ShouldDeferPreflightToRealUsage(tokens)
	codexNativeAuto := codexNativeAutoCompaction(a)
	if !deferred {
		if s, ok := compressor.(interface{ MaybeSeedPreflightDisplayTokens(int) }); ok {
			s.MaybeSeedPreflightDisplayTokens(tokens)
		}
	}
	cooldown := activeCompressionFailureCooldown(compressor)
	shouldNow := false
	var blockReason *string
	switch {
	case deferred:
		var lastReal int
		real, ok := compressor.(realPromptTokensEngine)
		if ok {
			lastReal, ok = real.LastRealPromptTokens()
		}
		if !ok {
			return engineAttributeError(compressor, "last_real_prompt_tokens")
		}
		turnContextLog().Info(fmt.Sprintf("Skipping preflight compression: rough estimate ~%s >= %s is not anchored on real usage (last real provider prompt %s); deferring to the next response",
			groupThousands(tokens), groupThousands(base.ThresholdTokens), groupThousands(lastReal)))
	case len(cooldown) > 0:
		remaining := 0.0
		if v, ok := cooldown["remaining_seconds"]; ok {
			remaining, _ = pyFloat(v)
		}
		turnContextLog().Info(fmt.Sprintf("Skipping preflight compression: same-session cooldown active (~%d seconds remaining, session %s)", int(remaining), sessionOrNone(a)))
		if tokens >= base.ThresholdTokens {
			reason := fmt.Sprintf("cooldown:%.0f", remaining)
			blockReason = &reason
		}
	case codexNativeAuto:
		turnContextLog().Info(fmt.Sprintf("Skipping Icebath preflight compression for codex app-server (mode=%s); Icebath will not start thread compaction here.", a.codexAppServerAutoCompaction))
	default:
		shouldNow = compressor.ShouldCompress(&tokens)
		if !shouldNow {
			blockReason = blockedCompressReason(compressor, tokens, nil)
		}
	}
	if shouldNow {
		if grown := growLocalWindowGuarded(a, compressor, tokens); grown != 0 {
			applyGrownWindow(a, compressor, grown)
			shouldNow = compressor.ShouldCompress(&tokens)
		}
	}
	switch {
	case shouldNow:
		if err := runPreflightPasses(ctx, a, out, compressor, tokens, systemMessage, taskID); err != nil {
			return err
		}
	case blockReason != nil && *blockReason != "":
		a.warnContextOverflowBlocked(*blockReason, tokens, base.ThresholdTokens)
	default:
		clearOverflowWarn(a)
		if !(len(cooldown) > 0 || deferred || codexNativeAuto) {
			if err := enginePreflightMaintenance(ctx, a, out, compressor, tokens, systemMessage, taskID); err != nil {
				return err
			}
		}
	}
	if out.Compressed {
		out.CurrentTurnUserIdx = reanchor(a, out.Messages, userMessage)
	}
	return nil
}

// The grown local window, zero when the runtime refused or the growth check raised
func growLocalWindowGuarded(a *AIAgent, compressor ContextEngine, tokens int) (grown int) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			grown = 0
		}
	}()
	if g, ok := maybeGrowLocalWindow(a, compressor, tokens); ok {
		return g
	}
	return 0
}

// Threshold triggered preflight passes bounded by compression.max_attempts, failing closed on an over window stall
func runPreflightPasses(ctx context.Context, a *AIAgent, out *CompactionOutcome, compressor ContextEngine, tokens int, systemMessage *string, taskID string) error {
	out.Compressed = true
	clearOverflowWarn(a)
	base := compressor.Base()
	turnContextLog().Info(fmt.Sprintf("Preflight compression: ~%s tokens >= %s threshold (model %s, ctx %s)",
		groupThousands(tokens), groupThousands(base.ThresholdTokens), a.model, groupThousands(base.ContextLength)))
	status := AutomaticCompactionStatusMessage(compressor, "preflight", preflightCompressionStatus(tokens, base.ThresholdTokens), map[string]any{
		"approx_tokens": tokens, "threshold_tokens": base.ThresholdTokens, "context_length": base.ContextLength, "model": a.model,
	})
	if status != nil {
		a.emitStatus(*status)
	}
	maxPasses := a.maxCompressionAttempts
	if maxPasses == 0 {
		maxPasses = 3
	}
	for range max(1, maxPasses) {
		input := out.Messages
		origLen, origTokens := len(input), tokens
		lc, err := compactLive(ctx, a, input, systemMessage, tokens, taskID, "turn_start_threshold")
		if err != nil {
			return err
		}
		out.Messages, out.ActiveSystemPrompt = lc.messages, lc.system
		if lc.skipped && compressionSkippedDueToLock(a) {
			turnContextLog().Info(fmt.Sprintf("Preflight compression deferred: compression lock held by another path (session %s)", sessionOrNone(a)))
			break
		}
		tokens = compactionRequestTokens(ctx, a, out.Messages, out.ActiveSystemPrompt)
		if !compressionMadeProgress(origLen, len(out.Messages), origTokens, tokens) {
			if err := failClosedAfterPreflightTimeout(a, tokens); err != nil {
				return err
			}
			if err := failClosedOnInsufficientProgress(a, tokens); err != nil {
				return err
			}
			out.Blocked = true
			break
		}
		out.ConversationHistory = historyAfterCompaction(a, lc, out.ConversationHistory)
		resetRetryStateAfterCompaction(a)
		if !compressor.ShouldCompress(&tokens) {
			break
		}
		if !compressionWarrantsAnotherPreflightPass(origTokens, tokens, base.ThresholdTokens) {
			out.Blocked = true
			turnContextLog().Warn(fmt.Sprintf("Preflight compression made insufficient progress: ~%s -> ~%s request tokens; skipping additional passes",
				groupThousands(origTokens), groupThousands(tokens)))
			if err := failClosedOnInsufficientProgress(a, tokens); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

// One engine driven compress pass below threshold when the engine asks for preflight maintenance
func enginePreflightMaintenance(ctx context.Context, a *AIAgent, out *CompactionOutcome, compressor ContextEngine, tokens int, systemMessage *string, taskID string) error {
	wants, ok := func() (wants, ok bool) {
		defer func() {
			if r := recover(); r != nil {
				if isUserInterruptPanic(r) {
					panic(r)
				}
				turnContextLog().Debug(fmt.Sprintf("should_compress_preflight raised %v; skipping engine-driven preflight maintenance", r))
				wants, ok = false, false
			}
		}()
		return compressor.ShouldCompressPreflight(messagesToRows(out.Messages)), true
	}()
	if !ok || !wants {
		return nil
	}
	turnContextLog().Info(fmt.Sprintf("Engine-driven preflight maintenance: %s requested compress() at ~%s tokens (below %s threshold)",
		compressor.Name(), groupThousands(tokens), groupThousands(compressor.Base().ThresholdTokens)))
	lc, err := compactLive(ctx, a, out.Messages, systemMessage, tokens, taskID, "engine_preflight")
	if err != nil {
		return err
	}
	out.Messages, out.ActiveSystemPrompt = lc.messages, lc.system
	if !lc.skipped {
		out.Compressed = true
		out.ConversationHistory = historyAfterCompaction(a, lc, nil)
		resetRetryStateAfterCompaction(a)
	}
	return nil
}

// Re-arms the uncompressed session overflow warning once the request is back under the window
func rearmUncompressedOverflowWarn(ctx context.Context, a *AIAgent, messages []*state.Message, activeSystemPrompt string) {
	if a.contextCompressor == nil {
		return
	}
	ctxLen := a.contextCompressor.Base().ContextLength
	if ctxLen <= 0 {
		return
	}
	rawChars := 0
	for _, m := range messages {
		if m == nil {
			continue
		}
		if text, ok := m.Content.(string); ok {
			rawChars += pystr.Len(text)
		} else if pyTruthy(m.Content) {
			rawChars = ctxLen + 1
			break
		}
	}
	if rawChars <= ctxLen {
		clearOverflowWarn(a)
		return
	}
	if compactionRequestTokens(ctx, a, messages, activeSystemPrompt) <= ctxLen {
		clearOverflowWarn(a)
	}
}
