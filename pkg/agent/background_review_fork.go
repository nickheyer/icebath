package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	backgroundReviewOrigin            = "background_review"
	frozenToolSnapshotGeneration      = int64(2_147_483_647)
	reviewForkCacheTag                = "review"
	reviewUsageRecordingFailedMessage = "Background review usage recording failed (non-fatal): %v"
)

// Usage counter keys a review fork snapshot carries, api_calls last
var reviewUsageCounters = []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "api_calls"}

func backgroundReviewLog() *slog.Logger {
	return slog.Default().With("logger", "agent.background_review")
}

// Agent constructor the cache parity fork goes through, replaced by tests recording the keywords
var newCacheParityForkAgent = NewAIAgent

// Model, provider, base url, token counters and estimated cost of a review fork before teardown
func snapshotReviewUsage(reviewAgent *AIAgent) map[string]any {
	usage := map[string]any{"model": nil, "provider": nil, "base_url": nil, "estimated_cost_usd": nil}
	for _, key := range reviewUsageCounters {
		usage[key] = 0
	}
	if reviewAgent == nil {
		return usage
	}
	usage["model"], usage["provider"], usage["base_url"] = reviewAgent.model, reviewAgent.provider, reviewAgent.baseURL
	usage["input_tokens"], usage["output_tokens"] = reviewAgent.sessionInputTokens, reviewAgent.sessionOutputTokens
	usage["cache_read_tokens"], usage["cache_write_tokens"] = reviewAgent.sessionCacheReadTokens, reviewAgent.sessionCacheWriteTokens
	usage["reasoning_tokens"], usage["api_calls"] = reviewAgent.sessionReasoningTokens, reviewAgent.sessionAPICalls
	usage["estimated_cost_usd"] = reviewAgent.sessionEstimatedCostUSD
	return usage
}

// Integer of a usage value after or zero, an error for a value int cannot parse
func reviewUsageInt(v any) (int, error) {
	if !pyTruthy(v) {
		return 0, nil
	}
	n, ok := pyIntCoerce(v)
	if !ok {
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(v))
	}
	return n, nil
}

// Optional string of a usage value, nil for None or an empty attribute
func reviewUsageStr(v any) *string {
	if v == nil {
		return nil
	}
	s := pyrepr.Str(v)
	if s == "" {
		return nil
	}
	return &s
}

// Records a fork's usage against the parent session or the given session under task, never failing the caller
func recordReviewUsageToParent(ctx context.Context, parent *AIAgent, usage map[string]any, task, sessionID string) {
	defer func() {
		if r := recover(); r != nil {
			backgroundReviewLog().Debug(fmt.Sprintf(reviewUsageRecordingFailedMessage, r))
		}
	}()
	if task == "" {
		task = backgroundReviewOrigin
	}
	sessionDB := parent.sessionDB
	if sessionID == "" {
		sessionID = parent.sessionID
	}
	counts := map[string]int{}
	nonzero := false
	for _, key := range reviewUsageCounters {
		n, err := reviewUsageInt(usage[key])
		if err != nil {
			backgroundReviewLog().Debug(fmt.Sprintf(reviewUsageRecordingFailedMessage, err))
			return
		}
		counts[key] = n
		nonzero = nonzero || n != 0
	}
	if sessionDB == nil || sessionID == "" || !nonzero {
		return
	}
	var cost *float64
	switch c := usage["estimated_cost_usd"].(type) {
	case float64:
		cost = &c
	case int:
		f := float64(c)
		cost = &f
	}
	calls := counts["api_calls"]
	err := sessionDB.RecordAuxiliaryUsage(ctx, sessionID, task, state.AuxiliaryUsage{
		Model: reviewUsageStr(usage["model"]), BillingProvider: reviewUsageStr(usage["provider"]),
		BillingBaseURL: reviewUsageStr(usage["base_url"]), EstimatedCostUSD: cost, APICallCount: &calls,
		InputTokens: int64(counts["input_tokens"]), OutputTokens: int64(counts["output_tokens"]),
		CacheReadTokens: int64(counts["cache_read_tokens"]), CacheWriteTokens: int64(counts["cache_write_tokens"]),
		ReasoningTokens: int64(counts["reasoning_tokens"]),
	})
	if err != nil {
		backgroundReviewLog().Debug(fmt.Sprintf(reviewUsageRecordingFailedMessage, err))
	}
}

// Maps review action lines to none, skill, memory or skill+memory by their localized or English prefixes
func classifyReviewResult(ctx context.Context, actions []string) string {
	skillPrefixes := []string{"skill", pystr.Lower(i18n.T(ctx, "display.review.label.skill", "", nil))}
	memoryPrefixes := []string{"memory", "user profile", pystr.Lower(i18n.T(ctx, "display.review.label.memory", "", nil)),
		pystr.Lower(i18n.T(ctx, "display.review.label.user", "", nil))}
	hasPrefix := func(line string, prefixes []string) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(line, p) {
				return true
			}
		}
		return false
	}
	hasSkill, hasMemory := false, false
	for _, action := range actions {
		line := pystr.Lower(pystr.Lstrip(strings.TrimPrefix(pystr.Lstrip(action), "📝")))
		hasSkill = hasSkill || hasPrefix(line, skillPrefixes)
		hasMemory = hasMemory || hasPrefix(line, memoryPrefixes)
	}
	var kinds []string
	if hasSkill {
		kinds = append(kinds, "skill")
	}
	if hasMemory {
		kinds = append(kinds, "memory")
	}
	if len(kinds) == 0 {
		return "none"
	}
	return strings.Join(kinds, "+")
}

// Logs one completion line per fork with its calls, tokens and result kind
func logReviewCompletion(usage map[string]any, result string) {
	n := func(key string) int {
		v, _ := reviewUsageInt(usage[key])
		return v
	}
	backgroundReviewLog().Info(fmt.Sprintf("Background review complete: thread=bg-review calls=%d in=%d out=%d cache_read=%d result=%s",
		n("api_calls"), n("input_tokens"), n("output_tokens"), n("cache_read_tokens"), result))
}

// Keeps a same model fork's request bytes identical to the parent's reasoning, prompt, pins and prefill
func sameModelParityKwargs(a *AIAgent, o *AIAgentOptions) {
	o.ReasoningConfig, o.EphemeralSystemPrompt = a.reasoningConfig, a.ephemeralSystemPrompt
	if pyTruthy(a.providersAllowed) {
		o.ProvidersAllowed = a.providersAllowed
	}
	if pyTruthy(a.providersIgnored) {
		o.ProvidersIgnored = a.providersIgnored
	}
	if pyTruthy(a.providersOrder) {
		o.ProvidersOrder = a.providersOrder
	}
	if pyTruthy(a.providerSort) {
		o.ProviderSort = a.providerSort
	}
	if a.providerRequireParameters {
		o.ProviderRequireParameters = true
	}
	if pyTruthy(a.providerDataCollection) {
		o.ProviderDataCollection = a.providerDataCollection
	}
	if len(a.prefillMessages) > 0 {
		o.PrefillMessages = pycopy.Of(a.prefillMessages)
	}
}

// Tells the user once per parent that the configured review reasoning effort is ignored on the same model path
func warnIgnoredReasoningEffort(ctx context.Context, a *AIAgent, taskCfg map[string]any) {
	effort := pystr.Strip(pyStrOr(backgroundReviewTaskConfig(ctx, taskCfg)["reasoning_effort"], ""))
	if effort == "" || a.warnedBgReviewReasoningEffort {
		return
	}
	a.warnedBgReviewReasoningEffort = true
	message := i18n.T(ctx, "display.review.reasoning_effort_warning", "", map[string]any{"effort": effort})
	func() {
		defer func() { _ = recover() }()
		a.emitWarning(message)
	}()
	backgroundReviewLog().Warn(message)
}

// Context engine hook rebinding its session state, implemented by engines owning persistence
type sessionStateBinder interface {
	BindSessionState(db *state.DB, sessionID string) error
}

// Severs the fork compressor from the parent session and enables in place compaction only after the rebind succeeds
func detachForkCompression(reviewAgent *AIAgent) {
	detached := false
	if binder, ok := reviewAgent.contextCompressor.(sessionStateBinder); ok && reviewAgent.contextCompressor != nil {
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			return binder.BindSessionState(nil, "")
		}()
		if err == nil {
			detached = true
		} else {
			backgroundReviewLog().Warn("background-review compressor detachment failed; keeping compression DISABLED on this review fork "+
				"(fail-closed, issue #93057 / #38727)", "exc_info", err.Error())
		}
	}
	reviewAgent.compressionInPlace = true
	reviewAgent.compressionEnabled = detached
	if detached {
		reviewAgent.reviewDeferCompactionBeforeFirstResponse = true
	}
}

// Reasoning config a routed fork takes from the configured effort, nil for unset or an unknown level
func routedReasoningConfig(ctx context.Context, taskCfg map[string]any) map[string]any {
	effort, ok := backgroundReviewTaskConfig(ctx, taskCfg)["reasoning_effort"]
	if !ok || effort == nil || effort == "" {
		return nil
	}
	parsed := config.ParseReasoningEffort(effort)
	if parsed == nil {
		backgroundReviewLog().Warn(fmt.Sprintf("auxiliary.background_review.reasoning_effort %s is not a valid level (none, %s) — using "+
			"the routed provider's default", pyrepr.Repr(effort), strings.Join(config.ValidReasoningEfforts, ", ")))
	}
	return parsed
}

// Constructor keywords of the review fork, parent toolsets and a skipped external memory, parity fields on the same model
func forkInitKwargs(ctx context.Context, a *AIAgent, rt reviewRuntime, routed bool, maxIterations int, taskCfg map[string]any) AIAgentOptions {
	o := AIAgentOptions{
		Model: pyStrOr(rt.Model, a.model), MaxIterations: maxIterations, QuietMode: true, Platform: a.platform,
		Provider: pyStrOr(rt.Provider, a.provider), APIMode: rt.APIMode, BaseURL: rt.BaseURL, APIKey: rt.APIKey,
		CredentialPool: rt.CredentialPool, RequestOverrides: rt.RequestOverrides, ParentSessionID: a.sessionID,
		EnabledToolsets: a.enabledToolsets, DisabledToolsets: a.disabledToolsets, SkipMemory: true,
		MaxTokens: rt.MaxTokens,
	}
	if o.RequestOverrides == nil {
		o.RequestOverrides = map[string]any{}
	}
	if rt.Command != "" {
		o.ACPCommand, o.ACPArgs = rt.Command, rt.Args
		if o.ACPArgs == nil {
			o.ACPArgs = []string{}
		}
	}
	if !routed {
		sameModelParityKwargs(a, &o)
	} else if cfg := routedReasoningConfig(ctx, taskCfg); cfg != nil {
		o.ReasoningConfig = cfg
	}
	return o
}

// Advertises the parent's exact tools on the fork and freezes its snapshot generation
func inheritParentToolSurface(reviewAgent, a *AIAgent) error {
	tools := pycopy.Of(a.tools)
	if tools == nil {
		tools = []any{}
	}
	names := map[string]bool{}
	for _, tool := range tools {
		entry, _ := tool.(map[string]any)
		fn, ok := entry["function"].(map[string]any)
		if !ok {
			return fmt.Errorf("KeyError: 'function'")
		}
		name, ok := fn["name"]
		if !ok {
			return fmt.Errorf("KeyError: 'name'")
		}
		names[pyrepr.Str(name)] = true
	}
	reviewAgent.tools, reviewAgent.validToolNames = tools, names
	generation := frozenToolSnapshotGeneration
	reviewAgent.toolSnapshotGeneration = &generation
	return nil
}

// Detached fork with warm prompt cache parity, its runtime and whether it routes to a different model
func buildCacheParityFork(ctx context.Context, parent *AIAgent, taskCfg map[string]any, maxIterations int, writeOrigin string) (*AIAgent, reviewRuntime, bool, error) {
	if writeOrigin == "" {
		writeOrigin = backgroundReviewOrigin
	}
	rt := resolveReviewRuntime(ctx, parent, taskCfg)
	routed := rt.Routed
	if !routed && writeOrigin == backgroundReviewOrigin {
		warnIgnoredReasoningEffort(ctx, parent, taskCfg)
	}
	reviewAgent, err := newCacheParityForkAgent(ctx, forkInitKwargs(ctx, parent, rt, routed, maxIterations, taskCfg))
	if err != nil {
		return nil, rt, routed, err
	}
	isolateForkEnv(reviewAgent)
	reviewAgent.memoryWriteOrigin, reviewAgent.memoryWriteContext = writeOrigin, writeOrigin
	reviewAgent.turnOrigin = writeOrigin
	reviewAgent.memoryStore = parent.memoryStore
	reviewAgent.memoryEnabled = parent.memoryEnabled
	reviewAgent.userProfileEnabled = parent.userProfileEnabled
	reviewAgent.memoryNudgeInterval, reviewAgent.skillNudgeInterval = 0, 0
	reviewAgent.skipMCPRefresh, reviewAgent.persistDisabled, reviewAgent.suppressStatusOutput = true, true, true
	reviewAgent.endSessionOnClose = false
	reviewAgent.sessionDB = nil
	reviewAgent.sessionID = parent.sessionID
	if !routed {
		reviewAgent.cachedSystemPrompt = parent.cachedSystemPrompt
		reviewAgent.sessionStart = parent.sessionStart
		if scope := provider.ResolvePromptCacheScopeSafe(parent.cacheScopeAgent()); scope != "" {
			reviewAgent.inheritedCacheScope = scope
		}
		if writeOrigin == backgroundReviewOrigin {
			reviewAgent.promptCacheForkTag = reviewForkCacheTag
		}
		reviewAgent.cachedConversationRoot = parent.conversationRootID()
		if err := inheritParentToolSurface(reviewAgent, parent); err != nil {
			return nil, rt, routed, err
		}
	}
	detachForkCompression(reviewAgent)
	reviewAgent.reviewInputTokenBudget = reviewInputTokenBudget(ctx, taskCfg, reviewAgent)
	return reviewAgent, rt, routed, nil
}

// Isolates a fork's env so its write origin and approval asker never reach the parent session
func isolateForkEnv(fork *AIAgent) {
	if fork.env == nil {
		return
	}
	env := *fork.env
	values := &sync.Map{}
	if fork.env.Values != nil {
		fork.env.Values.Range(func(k, v any) bool {
			values.Store(k, v)
			return true
		})
	}
	env.Values = values
	fork.env = &env
}

// Releases only the fork's clients, never the session bound resources it shares with the parent
func releaseForkClients(ctx context.Context, reviewAgent *AIAgent) {
	defer func() { _ = recover() }()
	reviewAgent.ReleaseClients(ctx)
}

// Records a one shot fork's usage under task against the session and releases its clients, returning the usage
func releaseCacheParityFork(ctx context.Context, parent, fork *AIAgent, task, sessionID string) map[string]any {
	usage := snapshotReviewUsage(fork)
	recordReviewUsageToParent(ctx, parent, usage, task, sessionID)
	releaseForkClients(ctx, fork)
	return usage
}
