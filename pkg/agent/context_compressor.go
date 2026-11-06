package agent

import (
	"context"
	"maps"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ContextCompressorOptions carries the constructor keywords with their hermes defaults from DefaultContextCompressorOptions
type ContextCompressorOptions struct {
	Model                          string
	ThresholdPercent               float64
	ProtectFirstN                  int
	ProtectLastN                   int
	SummaryTargetRatio             float64
	QuietMode                      bool
	SummaryModelOverride           string
	BaseURL                        string
	APIKey                         string
	ConfigContextLength            *int
	Provider                       string
	APIMode                        string
	AbortOnSummaryFailure          bool
	MaxTokens                      any
	ModelThresholds                map[string]float64
	ThresholdTokensCap             any
	ProactivePruneTokens           int
	ProactivePruneMinResultChars   int
	ProactivePruneMinReclaimTokens int
	MinTailUserMessages            int
	TailMode                       string
	CustomProviders                []any
	Env                            *tools.Env
}

// Keyword defaults of the hermes constructor for model
func DefaultContextCompressorOptions(model string) ContextCompressorOptions {
	return ContextCompressorOptions{Model: model, ThresholdPercent: 0.50, ProtectFirstN: 3, ProtectLastN: 20, SummaryTargetRatio: 0.20,
		ProactivePruneMinResultChars: 8000, ProactivePruneMinReclaimTokens: 4096, MinTailUserMessages: 1, TailMode: "lean"}
}

type reclaimBlockWarnKey struct {
	reason string
	rearm  int
}

// ContextCompressor is the default context engine that prunes tool results, protects head and tail and summarizes the middle
type ContextCompressor struct {
	ContextEngineBase
	CompressionTelemetry

	ctx context.Context
	env *tools.Env

	model, baseURL, apiKey, provider, apiMode string
	tailMode                                  string
	customProviders                           []any
	thresholdTokensCap                        *int
	auxContextCeiling                         *int

	proactivePruneTokens           int
	proactivePruneMinResultChars   int
	proactivePruneMinReclaimTokens int
	proactivePruneRearmTokens      int
	lastReclaimBlockWarn           *reclaimBlockWarnKey
	minTailUserMessages            int
	summaryTargetRatio             float64
	quietMode                      bool
	maxTokens                      *int
	abortOnSummaryFailure          bool

	microCompactEnabled               bool
	microCompactCursor                int
	microCompactRollingSummary        string
	microCompactConsecutiveFailures   int
	microCompactLastFailureCursor     int
	microCompactDefragThresholdTokens int
	flushScanCursorInvalidated        bool
	microCompactPasses                int
	microCompactTokensSavedTotal      int
	microCompactTurnsSincePass        int
	microCompactEveryNTurns           int

	configContextLength        *int
	configuredThresholdPercent float64
	resolvedContextLength      *int
	thresholdTokens            *int
	tailTokenBudget            *int
	maxSummaryTokens           *int
	logInitSummary             bool
	contextProbed              bool
	contextProbePersistable    bool

	lastRealPromptTokens              int
	lastCompressionRoughTokens        int
	awaitingRealUsageAfterCompression bool
	providerOmitsUsage                bool

	summaryModel              string
	summaryModelFallenBack    bool
	sessionDB                 any
	rejectedThinking          *provider.RejectedThinkingTracker
	sessionID                 string
	preparedCompaction        *PreparedCompaction
	compressionCancelledCheck func() bool
	attempt                   *compressorAttemptSlots

	previousSummary                     *string
	summaryHasUserTurn                  *bool
	lastSummaryError                    *string
	lastAuxResolvedModel                string
	consecutiveTimeoutFailures          int
	consecutiveTruncationFailures       int
	consecutiveOverloadAborts           int
	lastSummaryOverloadDegraded         bool
	lastFeasibilitySkip                 bool
	lastCompressionSavingsPct           float64
	ineffectiveCompressionCount         int
	antiThrashRecoveryDeadline          float64
	structuralNoOpBackoffUntil          float64
	structuralNoOpLen                   int
	prellmSkipCount                     int
	fallbackCompressionStreak           int
	fallbackProbeAt                     float64
	verifyCompactionClearedThreshold    bool
	lastCompressionMadeProgress         bool
	summaryFailureCooldownUntil         float64
	cooldownPersistFailed               bool
	lastCompressAborted                 bool
	lastCompressRefusedWouldGrow        bool
	lastCooldownRefreshWasAuthoritative *bool

	lastSummaryAuthFailure         bool
	lastSummaryNetworkFailure      bool
	lastSummaryTruncatedFailure    bool
	lastSummaryEmptyContentFailure bool
	lastSummaryOverloadFailure     bool
}

var (
	_ ContextEngine                   = (*ContextCompressor)(nil)
	_ ContextEngineInfo               = (*ContextCompressor)(nil)
	_ TailCutCompressor               = (*ContextCompressor)(nil)
	_ SummaryDispatchCompressor       = (*ContextCompressor)(nil)
	_ CooldownBypassSummarizer        = (*ContextCompressor)(nil)
	_ provider.CodexContextCompressor = (*ContextCompressor)(nil)
	_ engineSessionBinder             = (*ContextCompressor)(nil)
	_ PreparedCompactionCompressor    = (*ContextCompressor)(nil)
)

// Builds the compressor with deferred window resolution, ctx kept for the ctx free Compress of the engine interface
func NewContextCompressor(ctx context.Context, o ContextCompressorOptions) *ContextCompressor {
	c := &ContextCompressor{ContextEngineBase: NewContextEngineBase(), ctx: ctx, env: o.Env}
	c.model, c.baseURL, c.apiKey, c.provider, c.apiMode = o.Model, o.BaseURL, o.APIKey, o.Provider, o.APIMode
	c.tailMode = "lean"
	if o.TailMode == "legacy" || o.TailMode == "lean" {
		c.tailMode = o.TailMode
	}
	if len(o.CustomProviders) > 0 {
		c.customProviders = o.CustomProviders
	}
	c.ModelThresholds = o.ModelThresholds
	if c.ModelThresholds == nil {
		c.ModelThresholds = map[string]float64{}
	}
	pct := o.ThresholdPercent
	c.configThresholdPercent = &pct
	c.BaseThresholdPercent = resolveModelThreshold(o.Model, c.ModelThresholds, o.ThresholdPercent, o.Provider)
	c.ThresholdPercent = c.BaseThresholdPercent
	c.thresholdTokensCap = coerceMaxTokens(o.ThresholdTokensCap)
	c.ProtectFirstN, c.ProtectLastN = o.ProtectFirstN, o.ProtectLastN
	c.proactivePruneTokens = o.ProactivePruneTokens
	minResult := o.ProactivePruneMinResultChars
	if minResult == 0 {
		minResult = 8000
	}
	c.proactivePruneMinResultChars = max(pruneMinChars, minResult)
	c.proactivePruneMinReclaimTokens = max(0, o.ProactivePruneMinReclaimTokens)
	c.minTailUserMessages = o.MinTailUserMessages
	c.summaryTargetRatio = max(0.10, min(o.SummaryTargetRatio, 0.80))
	c.quietMode = o.QuietMode
	c.maxTokens = coerceMaxTokens(o.MaxTokens)
	c.abortOnSummaryFailure = o.AbortOnSummaryFailure
	c.resetMicroCompactCursorState()
	c.microCompactDefragThresholdTokens = 2000
	c.microCompactEveryNTurns = 1
	c.configContextLength = o.ConfigContextLength
	c.configuredThresholdPercent = c.ThresholdPercent
	c.logInitSummary = !o.QuietMode
	c.resetRealUsagePairing()
	c.summaryModel = o.SummaryModelOverride
	c.resetSessionCompactionState(ctx)
	c.clearTerminalSummaryFailures()
	return c
}

// Positive integer reservation or cap, nil for none, non numeric or non positive input
func coerceMaxTokens(value any) *int {
	if value == nil {
		return nil
	}
	n, err := pyint.Of(value)
	if err != nil || n.Sign() <= 0 || !n.IsInt64() {
		return nil
	}
	v := int(n.Int64())
	return &v
}

func (c *ContextCompressor) Name() string { return "compressor" }

// Resets every per session state for new or reset, micro compaction included
func (c *ContextCompressor) OnSessionReset() {
	c.ContextEngineBase.OnSessionReset()
	c.resetSessionCompactionState(c.ctx)
	c.resetMicroCompactCursorState()
	c.microCompactPasses, c.microCompactTokensSavedTotal, c.microCompactTurnsSincePass = 0, 0, 0
}

// Forgets the rolling micro summary with its cursor and failure bookkeeping
func (c *ContextCompressor) resetMicroCompactCursorState() {
	c.microCompactCursor = 0
	c.microCompactRollingSummary = ""
	c.microCompactConsecutiveFailures = 0
	c.microCompactLastFailureCursor = -1
}

// Forgets the real usage pairing read by the pending real usage check
func (c *ContextCompressor) resetRealUsagePairing() {
	c.lastRealPromptTokens, c.lastCompressionRoughTokens = 0, 0
	c.awaitingRealUsageAfterCompression, c.providerOmitsUsage = false, false
}

// Shared per session reset for new, reset and session end, discarding any prepared pass
func (c *ContextCompressor) resetSessionCompactionState(ctx context.Context) {
	if c.preparedCompaction != nil {
		c.preparedCompaction.Discard(ctx, "session boundary", "")
	}
	c.previousSummary, c.summaryHasUserTurn, c.lastSummaryError = nil, nil, nil
	c.LastAuxModelFailureError, c.LastAuxModelFailureModel = "", ""
	c.lastAuxResolvedModel = ""
	c.consecutiveTimeoutFailures, c.consecutiveTruncationFailures = 0, 0
	c.consecutiveOverloadAborts = 0
	c.lastSummaryOverloadDegraded = false
	c.LastSummaryDroppedCount = 0
	c.LastSummaryFallbackUsed, c.lastFeasibilitySkip = false, false
	c.lastCompressionSavingsPct = 100.0
	c.ineffectiveCompressionCount = 0
	c.antiThrashRecoveryDeadline, c.structuralNoOpBackoffUntil = 0, 0
	c.structuralNoOpLen = 0
	c.prellmSkipCount = 0
	c.fallbackCompressionStreak = 0
	c.fallbackProbeAt = 0
	c.verifyCompactionClearedThreshold = false
	c.lastCompressionMadeProgress = false
	c.summaryFailureCooldownUntil = 0
	c.cooldownPersistFailed = false
	c.lastCompressAborted, c.lastCompressRefusedWouldGrow = false, false
	c.contextProbed, c.contextProbePersistable = false, false
	c.resetRealUsagePairing()
	c.Last, c.Active = nil, nil
	c.Seed = nil
	c.resetProactivePruneRearm()
}

// Field behind one terminal summary failure flag name, nil for an unknown name
func (c *ContextCompressor) terminalFailureFlag(flag string) *bool {
	switch flag {
	case "_last_summary_auth_failure":
		return &c.lastSummaryAuthFailure
	case "_last_summary_network_failure":
		return &c.lastSummaryNetworkFailure
	case "_last_summary_truncated_failure":
		return &c.lastSummaryTruncatedFailure
	case "_last_summary_empty_content_failure":
		return &c.lastSummaryEmptyContentFailure
	case "_last_summary_overload_failure":
		return &c.lastSummaryOverloadFailure
	}
	return nil
}

// Clears every terminal summary failure flag
func (c *ContextCompressor) clearTerminalSummaryFailures() {
	for _, f := range terminalSummaryFailures {
		*c.terminalFailureFlag(f.Flag) = false
	}
}

// Fully rearms the proactive prune so a later lockout warns again
func (c *ContextCompressor) resetProactivePruneRearm() {
	c.proactivePruneRearmTokens = 0
	c.lastReclaimBlockWarn = nil
}

// Records provider usage, applies the real prompt verdict and consumes the post compaction flags
func (c *ContextCompressor) UpdateFromResponse(usage map[string]any) {
	c.LastPromptTokens = usageInt(usage, "prompt_tokens", 0)
	c.LastCompletionTokens = usageInt(usage, "completion_tokens", 0)
	c.LastTotalTokens = usageInt(usage, "total_tokens", c.LastPromptTokens+c.LastCompletionTokens)
	c.applyRealPromptVerdict()
	c.verifyCompactionClearedThreshold, c.awaitingRealUsageAfterCompression = false, false
}

// Integer usage field, def when absent or not integral
func usageInt(usage map[string]any, key string, def int) int {
	v, ok := usage[key]
	if !ok {
		return def
	}
	if n, err := pyint.Of(v); err == nil && n.IsInt64() {
		return int(n.Int64())
	}
	return def
}

// Installs a new window, keeping derived budgets when the same window is reassigned
func (c *ContextCompressor) SetContextLength(value int) {
	if c.resolvedContextLength != nil && *c.resolvedContextLength == value {
		return
	}
	c.resolvedContextLength = &value
	c.ContextEngineBase.ContextLength = value
	c.ThresholdPercent = effectiveThresholdPercent(value, c.BaseThresholdPercent)
	c.thresholdTokens, c.tailTokenBudget, c.maxSummaryTokens = nil, nil, nil
	c.emitInitSummaryOnce()
}

func (c *ContextCompressor) ShouldCompress(promptTokens *int) bool {
	return c.shouldCompress(promptTokens)
}

func (c *ContextCompressor) ShouldCompressInfo(promptTokens *int) (bool, *string) {
	return c.shouldCompressInfo(promptTokens)
}

func (c *ContextCompressor) Compress(messages []map[string]any, currentTokens *int, focusTopic *string, force bool, memoryContext string) ([]map[string]any, error) {
	return c.CompressContext(c.ctx, messages, currentTokens, focusTopic, force, memoryContext)
}

// Ctx taking compress the conversation_compression port calls under the summary route pin
func (c *ContextCompressor) CompressContext(ctx context.Context, messages []map[string]any, currentTokens *int, focusTopic *string, force bool, memoryContext string) ([]map[string]any, error) {
	return c.compress(ctx, messages, currentTokens, focusTopic, force, memoryContext)
}

func (c *ContextCompressor) ShouldDeferPreflightToRealUsage(roughTokens int) bool {
	return c.shouldDeferPreflightToRealUsage(roughTokens)
}

func (c *ContextCompressor) HasContentToCompress(messages []map[string]any) bool {
	return c.hasContentToCompress(messages)
}

func (c *ContextCompressor) OnSessionStart(sessionID string, kwargs map[string]any) {
	c.onSessionStart(sessionID, kwargs)
}

func (c *ContextCompressor) OnSessionEnd(sessionID string, messages []map[string]any) {
	c.onSessionEnd(sessionID, messages)
}

func (c *ContextCompressor) UpdateModel(model string, contextLength int, baseURL, apiKey, providerName, apiMode string) {
	c.updateModel(model, contextLength, baseURL, apiKey, providerName, apiMode)
}

func (c *ContextCompressor) CloneForAgent() ContextEngine { return c.cloneForAgent() }

func (c *ContextCompressor) BindSessionState(db *state.DB, sessionID string) error {
	c.bindSessionState(sessionStoreOf(db), sessionID)
	return nil
}

func (c *ContextCompressor) BoundSessionID() string { return c.sessionID }

func (c *ContextCompressor) SummaryModel() string { return c.summaryModel }

func (c *ContextCompressor) SummaryModelFallenBack() bool { return c.summaryModelFallenBack }

func (c *ContextCompressor) MainRoute() (providerName, model, baseURL, apiKey, apiMode string) {
	return c.provider, c.model, c.baseURL, c.apiKey, c.apiMode
}

func (c *ContextCompressor) SetLastSummaryError(text string) { c.lastSummaryError = &text }

func (c *ContextCompressor) LastSummaryError() string {
	if c.lastSummaryError == nil {
		return ""
	}
	return *c.lastSummaryError
}

func (c *ContextCompressor) ActiveCompressionTelemetry() *pyjson.Object { return c.Active }

func (c *ContextCompressor) RestoreSummaryState(previousSummary *string, hasUserTurn *bool) {
	c.previousSummary, c.summaryHasUserTurn = previousSummary, hasUserTurn
}

func (c *ContextCompressor) PreviousSummary() string {
	if c.previousSummary == nil {
		return ""
	}
	return *c.previousSummary
}

// Summary generator every compress path calls, swapped by tests as hermes patches _generate_summary
var compressorGenerateSummary = (*ContextCompressor).generateSummary

func (c *ContextCompressor) GenerateWindowSummary(ctx context.Context, turns []map[string]any, focusTopic *string, memoryContext string) (*string, error) {
	return compressorGenerateSummary(c, ctx, turns, focusTopic, memoryContext, false)
}

func (c *ContextCompressor) GenerateWindowSummaryBypassingCooldown(ctx context.Context, turns []map[string]any, focusTopic *string, memoryContext string, bypassCooldown bool) (*string, error) {
	return compressorGenerateSummary(c, ctx, turns, focusTopic, memoryContext, bypassCooldown)
}

func (c *ContextCompressor) GenerateSummary(ctx context.Context, turns []map[string]any, focusTopic *string) (*string, error) {
	return compressorGenerateSummary(c, ctx, turns, focusTopic, "", false)
}

// Per row tail price shared by every gate that sizes a tail region
func (c *ContextCompressor) TailRowPricer(messages []map[string]any) func(int) int {
	newestAsst := lastAssistantIndex(messages)
	chargeAllThinking := c.staleThinkingOnWire()
	if native := c.nativeAnthropicBudget(); native != nil {
		return func(i int) int { return native(messages[i]) }
	}
	return func(i int) int {
		return estimateMsgBudgetTokens(c.ctx, messages[i], chargeAllThinking || i == newestAsst)
	}
}

// Summary handoff message by metadata or content
func (c *ContextCompressor) IsContextSummaryMessage(message map[string]any) bool {
	return message != nil && state.IsCompactionSummaryMessage(message)
}

// Pushes a compress start boundary past any orphan tool results
func (c *ContextCompressor) AlignBoundaryForward(messages []map[string]any, idx int) int {
	for idx < len(messages) && messages[idx]["role"] == "tool" {
		idx++
	}
	return idx
}

// Latest actionable user turn at or after headEnd, minus one when there is none
func (c *ContextCompressor) FindLastUserMessageIdx(messages []map[string]any, headEnd int) int {
	for i := len(messages) - 1; i >= headEnd; i-- {
		if state.IsRealUserTurn(messages[i]) {
			return i
		}
	}
	return -1
}

// Compact focus hint from the most recent real user turns, nil when none carries text
func (c *ContextCompressor) DeriveAutoFocusTopic(messages []map[string]any) *string {
	candidates := []string{}
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg["role"] != "user" || state.IsSyntheticCompressionUserTurn(msg) || truthyField(msg, "display_kind") {
			continue
		}
		text := redactCompactionText(pystr.Strip(typedUserText(contentTextForContains(msg["content"]))))
		if text == "" {
			continue
		}
		text = strings.Join(pystr.SplitWhitespace(text, -1), " ")
		if pystr.Len(text) > autoFocusTurnMaxChars {
			text = pystr.Rstrip(pystr.Slice(text, 0, autoFocusTurnMaxChars-1)) + "…"
		}
		candidates = append(candidates, text)
		if len(candidates) >= autoFocusMaxTurns {
			break
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	lines := make([]string, 0, len(candidates))
	for i := len(candidates) - 1; i >= 0; i-- {
		lines = append(lines, "- "+candidates[i])
	}
	focus := "Recent user focus:\n" + strings.Join(lines, "\n")
	if pystr.Len(focus) > autoFocusMaxChars {
		focus = pystr.Rstrip(pystr.Slice(focus, 0, autoFocusMaxChars-1)) + "…"
	}
	return &focus
}

func truthyField(m map[string]any, key string) bool {
	v, ok := m[key]
	return ok && pyTruthy(v)
}

func (c *ContextCompressor) AwaitingRealUsageAfterCompression() bool {
	return c.awaitingRealUsageAfterCompression
}

func (c *ContextCompressor) SetAwaitingRealUsageAfterCompression(on bool) {
	c.awaitingRealUsageAfterCompression = on
}

func (c *ContextCompressor) CompressionCount() int { return c.ContextEngineBase.CompressionCount }

func (c *ContextCompressor) SetCompressionCount(n int) { c.ContextEngineBase.CompressionCount = n }

func (c *ContextCompressor) SetLastCompressionRoughTokens(n int) { c.lastCompressionRoughTokens = n }

func (c *ContextCompressor) SetLastUsage(promptTokens, completionTokens int) {
	c.LastPromptTokens, c.LastCompletionTokens = promptTokens, completionTokens
}

func (c *ContextCompressor) NoteUsageLessResponse() { c.noteUsageLessResponse() }

func (c *ContextCompressor) RecordCompletedCompaction(usedFallback bool) {
	c.recordCompletedCompaction(usedFallback, false)
}

func (c *ContextCompressor) SetVerifyCompactionClearedThreshold(on bool) {
	c.verifyCompactionClearedThreshold = on
}

func (c *ContextCompressor) PreparedCompactionState() *PreparedCompaction {
	return c.preparedCompaction
}

// Installs the prepared compaction state the agent build attaches
func (c *ContextCompressor) SetPreparedCompaction(p *PreparedCompaction) { c.preparedCompaction = p }

func (c *ContextCompressor) CompressorSessionID() string { return c.sessionID }

func (c *ContextCompressor) FallbackCompressionStreak() int { return c.fallbackCompressionStreak }

func (c *ContextCompressor) MicroCompactEnabled() bool { return c.microCompactEnabled }

func (c *ContextCompressor) SummaryFailureCooldownUntil() float64 {
	return c.summaryFailureCooldownUntil
}

func (c *ContextCompressor) SetSessionDB(db *state.DB) { c.sessionDB = sessionStoreOf(db) }

// Shares the agent rejected thinking fingerprints so the tail walk prices the replay the preflight prices
func (c *ContextCompressor) SetRejectedThinkingTracker(t *provider.RejectedThinkingTracker) {
	c.rejectedThinking = t
}

// Whether the last forced cooldown refresh read the durable row, nil before any refresh
func (c *ContextCompressor) LastCooldownRefreshWasAuthoritative() *bool {
	return c.lastCooldownRefreshWasAuthoritative
}

func (c *ContextCompressor) SetCompressionCancelledCheck(check func() bool) {
	c.compressionCancelledCheck = check
}

func (c *ContextCompressor) BeginCompressionTelemetry(currentTokens *int) {
	c.beginCompressionTelemetry(currentTokens)
}

// Shallow copy sharing durable backends, the prepared worker form of copy.copy
func (c *ContextCompressor) ShallowCopy() PreparedCompactionCompressor {
	dup := *c
	return &dup
}

// Value of one summary call attribute the prepared worker carries back, nil for an unset optional
func (c *ContextCompressor) SummaryCallAttr(name string) any {
	optional := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	switch name {
	case "summary_model":
		return c.summaryModel
	case "_last_aux_model_failure_error":
		return optional(c.LastAuxModelFailureError)
	case "_last_aux_model_failure_model":
		return optional(c.LastAuxModelFailureModel)
	case "_last_aux_resolved_model":
		return optional(c.lastAuxResolvedModel)
	}
	panic("ContextCompressor has no summary call attribute " + name)
}

// Sets one summary call attribute, nil clearing an optional one
func (c *ContextCompressor) SetSummaryCallAttr(name string, value any) {
	text, _ := value.(string)
	switch name {
	case "summary_model":
		c.summaryModel = text
	case "_last_aux_model_failure_error":
		c.LastAuxModelFailureError = text
	case "_last_aux_model_failure_model":
		c.LastAuxModelFailureModel = text
	case "_last_aux_resolved_model":
		c.lastAuxResolvedModel = text
	default:
		panic("ContextCompressor has no summary call attribute " + name)
	}
}

func (c *ContextCompressor) SummaryFailureFlag(flag string) bool {
	if p := c.terminalFailureFlag(flag); p != nil {
		return *p
	}
	return false
}

func (c *ContextCompressor) RecordSummarySuccess(summary string) { c.recordSummarySuccess(summary) }

func (c *ContextCompressor) PlanCompactionWindow(messages []map[string]any) (working []map[string]any, pruned, start, end int) {
	return c.planCompactionWindow(messages)
}

func (c *ContextCompressor) ScanWindowHandoffs(messages []map[string]any, start, end int, turns []map[string]any) HandoffScan {
	return c.scanWindowHandoffs(messages, start, end, turns)
}

func (c *ContextCompressor) PruneToolResultsOnly(messages []map[string]any, currentTokens *int) ([]map[string]any, int) {
	return c.pruneToolResultsOnly(messages, currentTokens)
}

// Independent engine copy with its own budget state, durable backends shared and no prepared worker
func (c *ContextCompressor) cloneForAgent() ContextEngine {
	clone := *c
	clone.ModelThresholds = maps.Clone(c.ModelThresholds)
	clone.customProviders = pycopy.Of(c.customProviders)
	for _, p := range []**int{&clone.thresholdTokensCap, &clone.auxContextCeiling, &clone.maxTokens, &clone.configContextLength, &clone.resolvedContextLength, &clone.thresholdTokens, &clone.tailTokenBudget, &clone.maxSummaryTokens} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	for _, p := range []**string{&clone.previousSummary, &clone.lastSummaryError} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	for _, p := range []**bool{&clone.summaryHasUserTurn, &clone.lastCooldownRefreshWasAuthoritative} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	if c.lastReclaimBlockWarn != nil {
		v := *c.lastReclaimBlockWarn
		clone.lastReclaimBlockWarn = &v
	}
	clone.attempt = c.attempt.copySlots()
	for _, p := range []**pyjson.Object{&clone.Seed, &clone.Active, &clone.Last} {
		if *p != nil {
			*p = pycopy.Of(*p)
		}
	}
	clone.preparedCompaction = nil
	return &clone
}
