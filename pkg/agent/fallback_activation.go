package agent

import (
	"context"
	"fmt"
	"math"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Provider client resolver the fallback walk binds candidates through
var agentResolveProviderClient = provider.ResolveProviderClient

// Cooldown armed after a non rate limit failure walked the whole chain
const fallbackExhaustedCooldownSeconds = 5.0

// Fallback chain walk state the agent keeps for itself
type fallbackWalkState struct {
	rateLimitedUntil        float64
	rateLimitBackoffCount   int
	unavailableFallbackKeys map[[3]string]bool
	providerFallbackActive  bool
	providerFallbackRoute   [2]string
}

// Replaces the fallback chain with the configured one unless a cooldown holds the agent on an activated fallback
func (a *AIAgent) ApplyFallbackChain(chain []map[string]any) {
	if a == nil {
		return
	}
	newChain := append([]map[string]any{}, chain...)
	if a.fallbackActivated && a.rateLimitedUntil > pythread.Monotonic() {
		return
	}
	oldChain := a.fallbackChain
	a.fallbackChain = newChain
	if !a.fallbackActivated {
		a.fallbackIndex = 0
	}
	if !reflect.DeepEqual(newChain, append([]map[string]any{}, oldChain...)) {
		clear(a.unavailableFallbackKeys)
	}
}

// Rewrites the last Model and Provider lines of the cached prompt after a provider switch
func (a *AIAgent) rewritePromptModelIdentity(model, providerName string) {
	sp := a.cachedSystemPrompt
	if sp == "" {
		return
	}
	for _, lv := range [][2]string{{"Model", model}, {"Provider", providerName}} {
		if lv[1] == "" {
			continue
		}
		matches := regexp.MustCompile(`(?m)^`+lv[0]+`: .*$`).FindAllStringIndex(sp, -1)
		if len(matches) > 0 {
			last := matches[len(matches)-1]
			sp = sp[:last[0]] + lv[0] + ": " + lv[1] + sp[last[1]:]
		}
	}
	a.cachedSystemPrompt = sp
}

// Normalized provider, model and base url identity of a chain entry
func fallbackEntryKey(fb map[string]any) [3]string {
	return [3]string{pystr.Lower(pystr.Strip(pyrepr.Str(orEmpty(fb["provider"])))), pystr.Strip(pyrepr.Str(orEmpty(fb["model"]))),
		strings.TrimRight(pystr.Strip(pyrepr.Str(orEmpty(fb["base_url"]))), "/")}
}

// Value or the empty string when falsy, Python's value or ""
func orEmpty(v any) any {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return v
}

// Skip reason for a chain entry known to be unusable locally, empty when usable
func fallbackEntryUnavailableWithoutNetwork(ctx context.Context, fb map[string]any) string {
	if pystr.Lower(pystr.Strip(pyrepr.Str(orEmpty(fb["provider"])))) != "nous" {
		return ""
	}
	state, err := provider.NewStore(home.Dir(ctx)).GetProviderAuthState("nous")
	if err != nil {
		return "nous_auth_unreadable:" + pyErrorClass(err)
	}
	for _, key := range []string{"access_token", "refresh_token"} {
		if s, ok := state[key].(string); ok && pystr.Strip(s) != "" {
			return ""
		}
	}
	return "nous_token_missing"
}

// Messages wire host match shared with the api mode detection
func isAnthropicWireURL(url string) bool {
	return provider.HostMandatedMode(url) == provider.ModeAnthropicMessages
}

// Explicit flag and api mode of a chain entry from its original base url before client resolution rewrites it
func fallbackAPIModeHint(ctx context.Context, fb map[string]any, fbProvider, baseURLHint string) (bool, string) {
	raw := fb["api_mode"]
	if !pyrepr.Truthy(raw) {
		raw = fb["transport"]
	}
	if explicit := provider.ParseConfigAPIMode(ctx, raw); explicit != "" {
		return true, explicit
	}
	if fbProvider != "" && fbProvider != "custom" && fbProvider != "moa" {
		if declared := provider.GetNamedCustomProvider(ctx, fbProvider)["api_mode"]; pyrepr.Truthy(declared) {
			return true, pyrepr.Str(declared)
		}
	}
	if fbProvider == "anthropic" || (baseURLHint != "" && isAnthropicWireURL(baseURLHint)) {
		return false, "anthropic_messages"
	}
	return false, "chat_completions"
}

// Api mode re-detected from provider, resolved base url and model when the hint landed on the default
func (a *AIAgent) fallbackAPIModeResolved(ctx context.Context, fbProvider, fbModel, fbBaseURL string) string {
	if fbProvider == "openai-codex" {
		return "codex_responses"
	}
	if family := provider.OpencodeFamilyForCustom(fbProvider, fbBaseURL); family != "" {
		return string(provider.OpenCodeModelMode(family, fbModel))
	}
	if fbProvider == "nous" || fbProvider == "nous-portal" || fbProvider == "nousresearch" {
		return provider.NousAPIMode(ctx, fbModel)
	}
	if isAnthropicWireURL(fbBaseURL) {
		return "anthropic_messages"
	}
	if a.isAzureOpenAIURL(&fbBaseURL) {
		return "chat_completions"
	}
	if a.isDirectOpenAIURL(&fbBaseURL) || providerModelRequiresResponsesAPI(ctx, fbModel, fbProvider) {
		return "codex_responses"
	}
	host, err := pyurl.BaseURLHostname(fbBaseURL)
	if err != nil {
		panic(err)
	}
	if fbProvider == "bedrock" || (strings.HasPrefix(host, "bedrock-runtime.") && hostMatches(fbBaseURL, "amazonaws.com")) {
		return "bedrock_converse"
	}
	return "chat_completions"
}

// Clears a cross provider pool and attaches the fallback provider's own pool when it has credentials
func (a *AIAgent) rebindFallbackCredentialPool(ctx context.Context, fbProvider, fbModel string) {
	if a.credentialPool != nil {
		poolProvider := pystr.Lower(pystr.Strip(a.credentialPool.Provider()))
		if poolProvider != "" && poolProvider != fbProvider {
			chatCompletionHelpersLog().Info(fmt.Sprintf("Fallback to %s/%s: clearing primary credential pool (pool_provider=%s) to prevent cross-provider contamination", fbProvider, fbModel, poolProvider))
			a.credentialPool, a.credentialPoolEntryID = nil, ""
		}
	}
	if a.credentialPool != nil {
		return
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				chatCompletionHelpersLog().Debug(fmt.Sprintf("Fallback to %s/%s: could not attach credential pool: %v", fbProvider, fbModel, r))
			}
		}()
		if pool := agentLoadPool(agentPoolEnv(ctx), fbProvider); pool != nil && pool.HasCredentials() {
			a.credentialPool = pool
			chatCompletionHelpersLog().Info(fmt.Sprintf("Fallback to %s/%s: attached fallback credential pool", fbProvider, fbModel))
		}
	}()
}

// Billing switches warn with the profile and remedy, every other reason logs at info
func logFallbackActivated(ctx context.Context, reason provider.FailoverReason, oldModel, oldProvider, fbModel, fbProvider string) {
	if reason != provider.ReasonBilling {
		chatCompletionHelpersLog().Info(fmt.Sprintf("Fallback activated: %s → %s (%s)", oldModel, fbModel, fbProvider))
		return
	}
	profile := home.ProfileNameFor(home.Dir(ctx))
	if profile == "" {
		profile = "default"
	}
	remedy := "icebath model"
	if profile != "default" {
		remedy = "icebath -p " + profile + " model"
	}
	chatCompletionHelpersLog().Warn(fmt.Sprintf("Profile %s: %s via %s refused for billing/credits — using fallback %s via %s. "+
		"Top up credits, or run `%s` to pick a model this account can use.", profile, oldModel, oldProvider, fbModel, fbProvider, remedy))
}

// Always false, arming a short cooldown when a non rate limit failure walked a non empty chain
func (a *AIAgent) fallbackChainExhausted(reason provider.FailoverReason) bool {
	if len(a.fallbackChain) > 0 && !provider.RateLimitFailoverReasons[reason] {
		a.rateLimitedUntil = math.Max(a.rateLimitedUntil, pythread.Monotonic()+fallbackExhaustedCooldownSeconds)
	}
	return false
}

// True when every credential the candidate would use sits in a cooldown longer than the retry loop's cap
func (a *AIAgent) candidatePoolExhausted(ctx context.Context, fbProvider, fbModel string) bool {
	pool := a.credentialPool
	if pool == nil || pystr.Lower(pystr.Strip(pool.Provider())) != fbProvider {
		loaded, ok := func() (p *provider.Pool, ok bool) {
			defer func() {
				if recover() != nil {
					ok = false
				}
			}()
			return agentLoadPool(agentPoolEnv(ctx), fbProvider), true
		}()
		if !ok {
			return false
		}
		pool = loaded
	}
	if pool == nil || !pool.HasCredentials() || pool.HasAvailable(fbModel) {
		return false
	}
	until, ok := pool.NextAvailableAt(fbModel)
	return !ok || time.Until(until).Seconds() > 600
}

// Provider and model pair rejected as unentitled for this account under its raw or normalized id
func isEntitlementRejected(a *AIAgent, providerName, model string) bool {
	if len(a.entitlementRejectedModels) == 0 {
		return false
	}
	if a.entitlementRejectedModels[[2]string{providerName, model}] {
		return true
	}
	return a.entitlementRejectedModels[[2]string{providerName, provider.NormalizeModelForProvider(model, providerName)}]
}

// True for an unavailable, malformed, unentitled, exhausted or locally unusable entry or the backend that just failed
func (a *AIAgent) shouldSkipFallbackCandidate(ctx context.Context, fb map[string]any, key [3]string, fbProvider, fbModel string) bool {
	log := chatCompletionHelpersLog()
	if a.unavailableFallbackKeys[key] {
		log.Debug(fmt.Sprintf("Fallback skip: %s previously marked unavailable", pyrepr.Repr(pyrepr.Tuple{key[0], key[1], key[2]})))
		return true
	}
	if fbProvider == "" || fbModel == "" {
		return true
	}
	if isEntitlementRejected(a, fbProvider, fbModel) {
		log.Info(fmt.Sprintf("Fallback skip: %s/%s was rejected as unentitled for this account", fbProvider, fbModel))
		return true
	}
	if a.candidatePoolExhausted(ctx, fbProvider, fbModel) {
		log.Warn(fmt.Sprintf("Fallback skip: %s/%s credential pool is exhausted (every entry in cooldown)", fbProvider, fbModel))
		return true
	}
	if reason := fallbackEntryUnavailableWithoutNetwork(ctx, fb); reason != "" {
		a.unavailableFallbackKeys[key] = true
		log.Warn(fmt.Sprintf("Fallback skip: %s/%s is not locally usable (%s); suppressing for this session", fbProvider, fbModel, reason))
		return true
	}
	current := provider.BuildBackendIdentity(a.provider, a.model, a.baseURL)
	candidate := provider.BuildBackendIdentity(fbProvider, fbModel, pyrepr.Str(orEmpty(fb["base_url"])))
	if provider.ShouldSkipCandidate(candidate, current, provider.ScopeModel) {
		where := current.BaseURL
		if where == "" {
			where = current.Provider
		}
		log.Warn(fmt.Sprintf("Fallback skip: chain entry %s/%s resolves to the same backend as the current one (%s)", fbProvider, fbModel, where))
		return true
	}
	return false
}

// Points compression limits at the fallback model's window and refreshes an existing feasibility verdict
func (a *AIAgent) updateFallbackContextCompressor(ctx context.Context) {
	if a.contextCompressor == nil {
		return
	}
	customProviders := make([]map[string]any, 0, len(a.customProviders))
	for _, e := range a.customProviders {
		if m, ok := e.(map[string]any); ok {
			customProviders = append(customProviders, m)
		}
	}
	window := provider.GetModelContextLength(ctx, provider.ContextLengthRequest{Model: a.model, BaseURL: a.baseURL, APIKey: a.apiKey,
		Provider: a.provider, ConfigContextLength: a.configContextLength, CustomProviders: customProviders})
	a.contextCompressor.UpdateModel(a.model, window, a.baseURL, a.apiKey, a.provider, a.apiMode)
	if a.compressionFeasibilityChecked {
		revalidateCompressionFeasibility(ctx, a)
	}
}

// Re-resolves reasoning_config for the fallback model, keeping the current one when config fails to load
func (a *AIAgent) reresolveFallbackReasoningConfig(ctx context.Context) {
	cfg, err := config.Load(home.Dir(ctx), os.LookupEnv)
	if err != nil {
		chatCompletionHelpersLog().Debug(fmt.Sprintf("Failed to resolve reasoning_config for fallback %s; keeping current: %v", a.model, err))
		return
	}
	a.reasoningConfig = config.ResolveReasoningConfig(map[string]any(cfg), a.model)
	chatCompletionHelpersLog().Info(fmt.Sprintf("Fallback %s: reasoning_config resolved: %s", a.model, pyrepr.Repr(a.reasoningConfig)))
}

// Drops the old provider's injected extra_body keys still at their injected values, then merges the new provider's
func (a *AIAgent) rescopeFallbackExtraBody(oldModel, oldProvider, oldBaseURL string) {
	customProviders := make([]map[string]any, 0, len(a.customProviders))
	for _, e := range a.customProviders {
		if m, ok := e.(map[string]any); ok {
			customProviders = append(customProviders, m)
		}
	}
	oldEB := customProviderExtraBodyForAgent(oldProvider, oldModel, oldBaseURL, customProviders)
	overrides := map[string]any{}
	for k, v := range a.requestOverrides {
		overrides[k] = v
	}
	if existing, ok := overrides["extra_body"].(map[string]any); ok && len(oldEB) > 0 {
		scrubbed := map[string]any{}
		for k, v := range existing {
			if old, has := oldEB[k]; !has || !pyrepr.Equal(v, old) {
				scrubbed[k] = v
			}
		}
		if len(scrubbed) > 0 {
			overrides["extra_body"] = scrubbed
		} else {
			delete(overrides, "extra_body")
		}
		a.requestOverrides = overrides
	}
	a.mergeCustomProviderExtraBody(customProviders)
	chatCompletionHelpersLog().Info(fmt.Sprintf("Fallback %s: extra_body resolved: %s", a.model, pyrepr.Repr(a.requestOverrides["extra_body"])))
}

// Buffers the switch notice for a terminal failure and keeps it as the pending one shot notice
func (a *AIAgent) bufferFallbackNotice(notice string) {
	a.BufferDiagnosticStatus(notice)
	a.pendingFallbackNotice = append(a.pendingFallbackNotice, notice)
}

// Rate limit switch skipped when the primary reopens sooner than the configured minimum
func (a *AIAgent) switchDeferredByReset(ctx context.Context, reason provider.FailoverReason, resetAt any) bool {
	if !provider.RateLimitFailoverReasons[reason] || a.fallbackActivated {
		return false
	}
	cfg, err := config.Load(home.Dir(ctx), os.LookupEnv)
	if err != nil {
		return false
	}
	threshold := provider.MinSwitchResetFromConfig(cfg)
	if threshold <= 0 {
		return false
	}
	delay, ok := provider.ProviderResetDelay(resetAt, time.Now())
	if !ok || delay >= threshold {
		return false
	}
	chatCompletionHelpersLog().Info(fmt.Sprintf("Rate limit resets in %.0f s (< fallback.min_switch_reset_seconds=%.0f): staying on the primary", delay.Seconds(), threshold.Seconds()))
	return true
}

// Arms the primary cooldown until the provider reset or the doubling schedule, nil when not leaving the primary
func (a *AIAgent) armRateLimitCooldown(reason provider.FailoverReason, resetAt any) any {
	if !provider.RateLimitFailoverReasons[reason] {
		return nil
	}
	current := pystr.Lower(pystr.Strip(a.provider))
	primary := pystr.Lower(pystr.Strip(pyrepr.Str(orEmpty(a.primaryRuntime["provider"]))))
	if a.fallbackActivated && !(primary != "" && current == primary) {
		return nil
	}
	count := a.rateLimitBackoffCount
	a.rateLimitBackoffCount = count + 1
	var seconds int64
	source := "exponential fallback"
	if delay, ok := provider.ProviderResetDelay(resetAt, time.Now()); ok {
		seconds = int64(math.Ceil(delay.Seconds()))
		source = "provider reset"
	} else {
		seconds = int64(math.Min(60*math.Pow(2, float64(count)), 14400))
	}
	a.rateLimitedUntil = pythread.Monotonic() + float64(seconds)
	chatCompletionHelpersLog().Info(fmt.Sprintf("Rate-limit backoff level %d: cooldown %d s (%.1f min, backoff#%d, %s)", count, seconds, float64(seconds)/60, count+1, source))
	return seconds
}

// Binds a chain shaped entry onto the agent in place, the old model and provider, false when no client resolves
func (a *AIAgent) bindRouteEntry(ctx context.Context, entry map[string]any, providerName, model string) (oldModel, oldProvider string, bound bool, err error) {
	baseURLHint := pystr.Strip(pyrepr.Str(orEmpty(entry["base_url"])))
	apiKeyHint := provider.ResolveFallbackEntryAPIKey(ctx, "", entry)
	explicit, apiMode := fallbackAPIModeHint(ctx, entry, providerName, baseURLHint)
	if baseURLHint != "" && hostMatches(baseURLHint, "ollama.com") && apiKeyHint == "" {
		apiKeyHint = provider.GetSecretStr(ctx, "OLLAMA_API_KEY", "")
	}
	var keyHint any
	if apiKeyHint != "" {
		keyHint = apiKeyHint
	}
	client, _, err := agentResolveProviderClient(ctx, providerName, model, provider.ProviderClientOptions{
		RawCodex: true, ExplicitBaseURL: baseURLHint, ExplicitAPIKey: keyHint, APIMode: apiMode})
	if err != nil {
		return "", "", false, err
	}
	if client == nil {
		return "", "", false, nil
	}
	var baseURL string
	if providerName == "moa" {
		baseURL, apiMode = "moa://local", "chat_completions"
	} else {
		model = provider.NormalizeModelForProvider(model, providerName)
		baseURL = client.BaseURL()
		if provider.IsActualRoute(providerName, baseURL) {
			apiMode = "chat_completions"
		} else if !explicit && apiMode == "chat_completions" {
			apiMode = a.fallbackAPIModeResolved(ctx, providerName, model, baseURL)
		}
	}
	oldModel, oldProvider, oldBaseURL := a.model, a.provider, a.baseURL
	a.configContextLength = 0
	a.model, a.provider, a.requestedProvider = model, providerName, providerName
	a.setBaseURL(baseURL)
	a.apiMode = apiMode
	a.reasoningEchoFlag = pyrepr.Truthy(entry["reasoning_echo"])
	clear(a.transportCache)
	resetCodexReasoningReplay(a)
	a.fallbackActivated = true
	a.rebindFallbackCredentialPool(ctx, providerName, model)
	if providerName == "moa" {
		if err := bindMoaRuntime(ctx, a, model, nil); err != nil {
			return "", "", false, err
		}
	} else if err := swapFallbackClients(ctx, a, client, providerName, model, baseURL, apiMode); err != nil {
		return "", "", false, err
	}
	syncCredentialPoolEntryID(a)
	a.usePromptCaching, a.useNativeCacheLayout = a.anthropicPromptCachePolicy(ctx)
	a.ensureLMStudioRuntimeLoaded(ctx, nil)
	a.updateFallbackContextCompressor(ctx)
	a.reresolveFallbackReasoningConfig(ctx)
	a.rescopeFallbackExtraBody(oldModel, oldProvider, oldBaseURL)
	return oldModel, oldProvider, true, nil
}

// Switches to the next usable chain entry in place, false when the chain is exhausted
func (a *AIAgent) tryActivateFallback(ctx context.Context, reason provider.FailoverReason) bool {
	return a.tryActivateFallbackAt(ctx, reason, nil)
}

// Fallback walk honoring a provider declared reset instant for the deferral and the cooldown
func (a *AIAgent) tryActivateFallbackAt(ctx context.Context, reason provider.FailoverReason, resetAt any) bool {
	if a.switchDeferredByReset(ctx, reason, resetAt) {
		return false
	}
	cooldownSeconds := a.armRateLimitCooldown(reason, resetAt)
	for {
		if a.fallbackIndex >= len(a.fallbackChain) {
			return a.fallbackChainExhausted(reason)
		}
		fb := a.fallbackChain[a.fallbackIndex]
		a.fallbackIndex++
		key := fallbackEntryKey(fb)
		if a.unavailableFallbackKeys == nil {
			a.unavailableFallbackKeys = map[[3]string]bool{}
		}
		fbProvider := pystr.Lower(pystr.Strip(pyrepr.Str(orEmpty(fb["provider"]))))
		fbModel := pystr.Strip(pyrepr.Str(orEmpty(fb["model"])))
		if a.shouldSkipFallbackCandidate(ctx, fb, key, fbProvider, fbModel) {
			continue
		}
		if a.activateFallbackEntry(ctx, fb, key, reason, fbProvider, fbModel, cooldownSeconds) {
			return true
		}
	}
}

// Binds one chain entry and announces the switch, false so the walk continues when the swap fails
func (a *AIAgent) activateFallbackEntry(ctx context.Context, fb map[string]any, key [3]string, reason provider.FailoverReason, fbProvider, fbModel string, cooldownSeconds any) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			ok = a.fallbackActivationFailed(key, fbProvider, fbModel, fmt.Sprint(r))
		}
	}()
	oldModel, oldProvider, bound, err := a.bindRouteEntry(ctx, fb, fbProvider, fbModel)
	if err != nil {
		return a.fallbackActivationFailed(key, fbProvider, fbModel, err.Error())
	}
	if !bound {
		chatCompletionHelpersLog().Warn(fmt.Sprintf("Fallback to %s failed: provider not configured", fbProvider))
		a.unavailableFallbackKeys[key] = true
		return false
	}
	fbModel = a.model
	a.rewritePromptModelIdentity(fbModel, fbProvider)
	notice := fmt.Sprintf("⚠️ Model fallback: %s via %s unavailable (%s); using %s via %s.",
		oldModel, oldProvider, provider.FallbackReasonText(reason), fbModel, fbProvider)
	if cooldownSeconds != nil {
		remaining := math.Max(0, math.Ceil(a.rateLimitedUntil-pythread.Monotonic()))
		notice += fmt.Sprintf(" Primary retry eligible in ~%d s; recovery is not guaranteed.", int64(remaining))
	}
	a.bufferFallbackNotice(notice)
	a.providerFallbackActive = true
	a.providerFallbackRoute = [2]string{fbModel, fbProvider}
	logFallbackActivated(ctx, reason, oldModel, oldProvider, fbModel, fbProvider)
	var reasonValue any
	if reason != "" {
		reasonValue = string(reason)
	}
	observability.RecordFallback(ctx, oldProvider, fbProvider, reasonValue)
	a.RetrySession().ResetStaleStreak()
	a.runtimeCapabilities = map[string]bool{"native_compaction": provider.ResolveNativeCompactionCapability(a.model, a.baseURL, fbProvider, fbProvider == "openai-codex")}
	return true
}

// Logs a failed swap and marks a failed Nous entry unavailable, false so the walk tries the next entry
func (a *AIAgent) fallbackActivationFailed(key [3]string, fbProvider, fbModel, detail string) bool {
	if fbProvider == "nous" {
		a.unavailableFallbackKeys[key] = true
	}
	chatCompletionHelpersLog().Error(fmt.Sprintf("Failed to activate fallback %s: %s", fbModel, detail))
	return false
}
