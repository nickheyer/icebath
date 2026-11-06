package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	declaredScopePrefix = "gwk_"
	// ForkScopeSeparator joins a scope and a fork tag, doubled so single colon gateway keys never read as forks
	ForkScopeSeparator = "::"
)

var grokAggregatorModelPrefixes = []string{"x-ai/grok-", "xai/grok-"}

type cacheScopeLineageReader interface {
	GetCompressionLineage(sessionID string) ([]string, error)
}

type cacheScopeSessionReader interface {
	GetSession(ctx context.Context, id string) (*pyjson.Object, error)
}

type cacheScopeForkReader interface {
	IsExplicitForkChild(sessionID string) (bool, error)
}

type cacheScopeIdentityReader interface {
	DeclaredScopeIdentity(sessionID string) (isFork bool, source string, err error)
}

type cacheScopeBoundaryReader interface {
	LatestConversationBoundary(sessionKey, source string) (generation int64, ok bool, err error)
}

type cacheScopeMemoKey struct {
	sessionID string
	hasDB     bool
}

// CacheScopeAgent carries the agent state the prompt cache scope resolution reads and memoizes
type CacheScopeAgent struct {
	SessionID           string
	SessionDB           any
	GatewaySessionKey   string
	PersistDisabled     bool
	InheritedCacheScope string
	Platform            string
	Env                 *tools.Env
	PromptCacheForkTag  string
	CompressionCount    func() int
	Provider            string
	Model               string
	BaseURL             string

	memoMu    sync.Mutex
	memoSet   bool
	memoKey   cacheScopeMemoKey
	memoScope string
}

// Compression lineage root of the session, empty when the store is absent, failing or returns no root
func cacheScopeLineageRoot(sessionID string, db any) string {
	reader, ok := db.(cacheScopeLineageReader)
	if db == nil || !ok {
		return ""
	}
	lineage, err := reader.GetCompressionLineage(sessionID)
	if err != nil || len(lineage) == 0 {
		return ""
	}
	return lineage[0]
}

// Session source the conversation is recorded under, the row value first and the persistence resolver second
func cacheScopeAgentSource(a *CacheScopeAgent, sessionID string, db any, rowSource *string) string {
	if rowSource == nil && sessionID != "" && db != nil {
		read := ""
		if reader, ok := db.(cacheScopeSessionReader); ok {
			if row, err := reader.GetSession(context.Background(), sessionID); err == nil && row != nil {
				source, _ := row.Get("source")
				text, _ := source.(string)
				read = pyStripSpace(text)
			}
		}
		rowSource = &read
	}
	if rowSource != nil && *rowSource != "" {
		return *rowSource
	}
	return pyStripSpace(SessionSourceFor(a.Env, a.Platform))
}

// Durable reset generation of the declared conversation key, empty when the store records none
func cacheScopeConversationGeneration(sessionKey, source string, db any) (string, error) {
	reader, ok := db.(cacheScopeBoundaryReader)
	if !ok {
		return "", nil
	}
	generation, present, err := reader.LatestConversationBoundary(sessionKey, source)
	if err != nil || !present {
		return "", err
	}
	return strconv.FormatInt(generation, 10), nil
}

// DeclaredConversationScope hashes the host declared conversation key into a gwk scope, empty when none applies
func DeclaredConversationScope(a *CacheScopeAgent) string {
	if strings.HasPrefix(a.InheritedCacheScope, declaredScopePrefix) {
		return a.InheritedCacheScope
	}
	key := pyStripSpace(a.GatewaySessionKey)
	if key == "" || a.PersistDisabled {
		return ""
	}
	sid := a.SessionID
	db := a.SessionDB
	var rowSource *string
	if sid != "" && db != nil {
		var isFork bool
		if identity, ok := db.(cacheScopeIdentityReader); ok {
			fork, src, err := identity.DeclaredScopeIdentity(sid)
			if err != nil {
				return ""
			}
			isFork, rowSource = fork, &src
		} else if forks, ok := db.(cacheScopeForkReader); ok {
			fork, err := forks.IsExplicitForkChild(sid)
			if err != nil {
				return ""
			}
			isFork = fork
		} else {
			return ""
		}
		if isFork {
			return ""
		}
	}
	source := cacheScopeAgentSource(a, sid, db, rowSource)
	generation := ""
	if db != nil {
		g, err := cacheScopeConversationGeneration(key, source, db)
		if err != nil {
			return ""
		}
		generation = g
	}
	sum := sha256.Sum256(pyUTF8Replace(source + "|" + key + "|" + generation))
	return declaredScopePrefix + hex.EncodeToString(sum[:])[:24]
}

// ResolvePromptCacheScope returns the rotation stable cache scope, memoized per session id and store presence
func ResolvePromptCacheScope(a *CacheScopeAgent) string {
	if a.InheritedCacheScope != "" {
		return applyCacheScopeForkTag(a, a.InheritedCacheScope)
	}
	sid := a.SessionID
	if sid == "" {
		return ""
	}
	db := a.SessionDB
	key := cacheScopeMemoKey{sessionID: sid, hasDB: db != nil}
	a.memoMu.Lock()
	memoHit, memoScope := a.memoSet && a.memoKey == key, a.memoScope
	a.memoMu.Unlock()
	if memoHit {
		return applyCacheScopeForkTag(a, memoScope)
	}
	root := DeclaredConversationScope(a)
	if root == "" {
		root = cacheScopeLineageRoot(sid, db)
	}
	scope := root
	if scope == "" {
		scope = sid
	}
	if root != "" || db == nil || a.PersistDisabled {
		a.memoMu.Lock()
		a.memoSet, a.memoKey, a.memoScope = true, key, scope
		a.memoMu.Unlock()
	}
	return applyCacheScopeForkTag(a, scope)
}

// IsSlotKeyedCacheRoute reports whether the cache key pins one server side slot per conversation as xAI does
func IsSlotKeyedCacheRoute(providerName, model, baseURL string) bool {
	p := strings.ToLower(pyStripSpace(providerName))
	if p == "xai" || p == "xai-oauth" {
		return true
	}
	if baseURLHostname(baseURL) == "api.x.ai" {
		return true
	}
	m := strings.ToLower(pyStripSpace(model))
	for _, prefix := range grokAggregatorModelPrefixes {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}

// IsForkCacheScope reports whether the scope carries a fork tag
func IsForkCacheScope(scope string) bool {
	return strings.Contains(scope, ForkScopeSeparator)
}

// Appends the fork tag once the fork compacted on a slot keyed route, reading the route on every call
func applyCacheScopeForkTag(a *CacheScopeAgent, scope string) string {
	tag := a.PromptCacheForkTag
	if scope == "" || tag == "" {
		return scope
	}
	count := 0
	if a.CompressionCount != nil {
		count = a.CompressionCount()
	}
	if count < 1 || !IsSlotKeyedCacheRoute(a.Provider, a.Model, a.BaseURL) {
		return scope
	}
	return scope + ForkScopeSeparator + tag
}

// DeclaredConversationScopeSafe resolves the declared scope and turns any panic into an empty scope
func DeclaredConversationScopeSafe(a *CacheScopeAgent) (scope string) {
	defer func() {
		if recover() != nil {
			scope = ""
		}
	}()
	return DeclaredConversationScope(a)
}

// ResolvePromptCacheScopeSafe resolves the scope and turns any panic into an empty scope meaning the physical id
func ResolvePromptCacheScopeSafe(a *CacheScopeAgent) (scope string) {
	defer func() {
		if recover() != nil {
			scope = ""
		}
	}()
	return ResolvePromptCacheScope(a)
}

// Strips the characters Python str.strip treats as whitespace
func pyStripSpace(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return (r >= 0x1c && r <= 0x1f) || unicode.IsSpace(r)
	})
}

// UTF-8 bytes with each surrogate or invalid byte replaced by a question mark as Python errors replace encodes
func pyUTF8Replace(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if i+2 < len(s) && s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
			out = append(out, '?')
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			out = append(out, '?')
		} else {
			out = append(out, s[i:i+size]...)
		}
		i += size
	}
	return out
}

var validCacheTTLs = map[string]bool{"5m": true, "1h": true}

// CacheTTLMeansDisabled reports whether a configured cache_ttl value turns prompt caching off, unknown values never do
func CacheTTLMeansDisabled(ttl any) bool {
	if s, ok := ttl.(string); ok && validCacheTTLs[s] {
		return false
	}
	if ttl == nil || ttl == false {
		return true
	}
	switch strings.ToLower(pyrepr.Str(ttl)) {
	case "off", "false", "disabled", "no", "none":
		return true
	}
	return false
}

// Raw prompt_caching cache_ttl of the bound home config, the default when the config cannot be read
func rawCacheTTLFromConfig(ctx context.Context, def any) any {
	cfg := LoadConfigSafe(home.Dir(ctx))
	if cfg == nil {
		return def
	}
	section := cfg["prompt_caching"]
	if !pyTruthy(section) {
		return "5m"
	}
	m, ok := section.(map[string]any)
	if !ok {
		return def
	}
	if v, has := m["cache_ttl"]; has {
		return v
	}
	return "5m"
}

// PromptCachingDisabledFromConfig reports whether the bound config turns prompt caching off
func PromptCachingDisabledFromConfig(ctx context.Context) bool {
	return CacheTTLMeansDisabled(rawCacheTTLFromConfig(ctx, "5m"))
}

// Configured 5m or 1h cache tier, empty for auto, unknown or unreadable config
func configuredCacheTTL(ctx context.Context) string {
	if s, ok := rawCacheTTLFromConfig(ctx, nil).(string); ok && validCacheTTLs[s] {
		return s
	}
	return ""
}

// ConfiguredCacheTTL returns the configured 5m or 1h cache tier, empty when none applies
func ConfiguredCacheTTL(ctx context.Context) string { return configuredCacheTTL(ctx) }

// CachePolicyAgent is the agent state the cache policy falls back to, nil CustomProviders means none attached yet
type CachePolicyAgent struct {
	Provider        string
	BaseURL         string
	APIMode         string
	Model           any
	CacheDisabled   bool
	CustomProviders []map[string]any
}

// CacheDestination overrides agent fields per call, a nil field reads the agent value
type CacheDestination struct {
	Provider, BaseURL, APIMode, Model *string
}

// BlankCachePolicyAgent is the identity blank policy agent, a nil disable flag reads the bound config
func BlankCachePolicyAgent(ctx context.Context, cacheDisabled *bool) *CachePolicyAgent {
	disabled := false
	if cacheDisabled == nil {
		disabled = PromptCachingDisabledFromConfig(ctx)
	} else {
		disabled = *cacheDisabled
	}
	return &CachePolicyAgent{CacheDisabled: disabled}
}

// Agent field unless the destination names its own value
func cacheDestField(override *string, fallback string) string {
	if override != nil {
		return *override
	}
	return fallback
}

// Whether the resolved destination accepts native tool cache markers on the Anthropic wire
func directNativeAnthropicToolCacheCapability(agent *CachePolicyAgent, dest CacheDestination) bool {
	baseURL := cacheDestField(dest.BaseURL, agent.BaseURL)
	apiMode := cacheDestField(dest.APIMode, agent.APIMode)
	return apiMode == "anthropic_messages" && baseURLHostname(baseURL) == "api.anthropic.com"
}

// Cache policy of the MoA preset aggregator slot, false false when it cannot be resolved
func moaAggregatorCachePolicy(ctx context.Context, agent *CachePolicyAgent, effModel string) (bool, bool) {
	cfg := LoadConfigSafe(home.Dir(ctx))
	if cfg == nil {
		return false, false
	}
	var moaCfg any = map[string]any{}
	if pyTruthy(cfg["moa"]) {
		moaCfg = cfg["moa"]
	}
	preset, err := ResolveMoAPreset(moaCfg, effModel)
	if err != nil {
		slog.Debug("MoA aggregator cache-policy resolution failed", "err", err)
		return false, false
	}
	agg, _ := preset["aggregator"].(map[string]any)
	aggProvider := strings.TrimSpace(pyOrStr(agg["provider"]))
	aggModel := strings.TrimSpace(pyOrStr(agg["model"]))
	if aggProvider == "" || aggModel == "" {
		return false, false
	}
	aggBaseURL, aggAPIMode := "", ""
	if rt, rerr := ResolveRuntimeProvider(ctx, home.Dir(ctx), aggProvider, aggModel, "", ""); rerr == nil && rt != nil {
		aggBaseURL, aggAPIMode = rt.BaseURL, string(rt.APIMode)
	}
	return anthropicPromptCachePolicy(ctx, agent, CacheDestination{Provider: &aggProvider, BaseURL: &aggBaseURL, APIMode: &aggAPIMode, Model: &aggModel})
}

// Capability lookup and offline provider catalog probe of the cache policy, swapped by tests
var (
	cachePolicyCapabilityLookup = CustomProviderModelCapability
	cachePolicyProviderDef      = GetProviderDef
)

// Identity gate deciding whether a custom provider capability lookup is worth running
func routeMayBeCustom(ctx context.Context, agent *CachePolicyAgent, effProvider, providerLower, effBaseURL string) bool {
	if len(agent.CustomProviders) > 0 {
		ids := map[string]bool{providerLower: true, strings.TrimPrefix(providerLower, "custom:"): true}
		effNormalized := NormalizeRouteBaseURL(effBaseURL)
		for _, entry := range agent.CustomProviders {
			if entry == nil {
				continue
			}
			for alias := range CustomProviderAliases(pyOrStr(entry["name"]), pyOrStr(entry["provider_key"])) {
				if ids[alias] {
					return true
				}
			}
			if effNormalized != "" && NormalizeRouteBaseURL(cfgString(entry["base_url"])) == effNormalized {
				return true
			}
		}
		return false
	}
	if agent.CustomProviders != nil {
		return false
	}
	def := cachePolicyProviderDef(ctx, effProvider, false)
	return def == nil || (def.BaseURL != "" && baseURLHostname(def.BaseURL) != baseURLHostname(effBaseURL))
}

// Whether to apply Anthropic prompt caching and whether markers go on inner blocks, branch order matters
func anthropicPromptCachePolicy(ctx context.Context, agent *CachePolicyAgent, dest CacheDestination) (bool, bool) {
	if agent.CacheDisabled {
		return false, false
	}
	effProvider := cacheDestField(dest.Provider, agent.Provider)
	effBaseURL := cacheDestField(dest.BaseURL, agent.BaseURL)
	effAPIMode := cacheDestField(dest.APIMode, agent.APIMode)
	var rawModel any = agent.Model
	if dest.Model != nil {
		rawModel = *dest.Model
	}
	if strings.ToLower(pyStripSpace(effProvider)) == "moa" {
		return moaAggregatorCachePolicy(ctx, agent, pyOrStr(rawModel))
	}
	if m, ok := rawModel.(map[string]any); ok {
		rawModel = m["model"]
		if !pyTruthy(rawModel) {
			rawModel = m["default"]
		}
		if !pyTruthy(rawModel) {
			rawModel = ""
		}
	}
	effModel, ok := rawModel.(string)
	if !ok {
		effModel = pyOrStr(rawModel)
	}
	modelLower := strings.ToLower(effModel)
	providerLower := strings.ToLower(effProvider)
	isClaude := strings.Contains(modelLower, "claude")
	isKimi := modelNameIsKimiFamily(effModel) || strings.Contains(modelLower, "moonshot")
	isOpenRouter := baseURLHostMatches(effBaseURL, "openrouter.ai")
	isNousPortal := baseURLHostMatches(effBaseURL, "nousresearch.com")
	isAnthropicWire := effAPIMode == "anthropic_messages"
	isNativeAnthropic := isAnthropicWire && (effProvider == "anthropic" || baseURLHostname(effBaseURL) == "api.anthropic.com")
	supportsCacheMarkers := effAPIMode == "anthropic_messages" || effAPIMode == "chat_completions"
	litellmOpenAIWire := effAPIMode == "chat_completions" && isClaude && isLitellmRoute(providerLower, effBaseURL)
	if supportsCacheMarkers && (isAnthropicWire || litellmOpenAIWire || routeMayBeCustom(ctx, agent, effProvider, providerLower, effBaseURL)) {
		var cfg map[string]any
		if agent.CustomProviders == nil {
			cfg = LoadConfigSafe(home.Dir(ctx))
		}
		if cfg != nil || agent.CustomProviders != nil {
			if value, known := cachePolicyCapabilityLookup(effModel, effBaseURL, "prompt_caching", agent.CustomProviders, cfg); known {
				return value, value && isAnthropicWire
			}
		}
	}
	isMinimaxRoute := providerLower == "minimax" || providerLower == "minimax-cn" ||
		baseURLHostMatches(effBaseURL, "api.minimax.io") || baseURLHostMatches(effBaseURL, "api.minimaxi.com")
	if isAnthropicWire && isMinimaxRoute && modelNameSuggestsMinimaxM3(effModel) {
		return false, false
	}
	if isNativeAnthropic {
		return true, true
	}
	if (isOpenRouter || isNousPortal) && (isClaude || isKimi) && !isAnthropicWire {
		return true, false
	}
	if isNousPortal && strings.Contains(modelLower, "qwen") {
		return true, false
	}
	if isAnthropicWire && isClaude {
		return true, true
	}
	if litellmOpenAIWire {
		return true, false
	}
	if isAnthropicWire && isMinimaxRoute {
		return true, true
	}
	if alibabaFamilyProviders[providerLower] && isQwenModel(modelLower) {
		return true, false
	}
	return false, false
}

// ResolveAnthropicPromptCachePolicy returns should cache and native layout for the agent and destination overrides
func ResolveAnthropicPromptCachePolicy(ctx context.Context, agent *CachePolicyAgent, dest CacheDestination) (bool, bool) {
	return anthropicPromptCachePolicy(ctx, agent, dest)
}

// Request local cache sections for one resolved destination, stripped copies when the route never caches
func planCacheSectionsForDestination(ctx context.Context, messages, tools []map[string]any, provider, baseURL, apiMode, model string,
	cacheDisabled *bool, cacheTTL string, staticSystemPrefix *string) ([]map[string]any, []map[string]any) {
	blankAgent := BlankCachePolicyAgent(ctx, cacheDisabled)
	dest := CacheDestination{Provider: &provider, BaseURL: &baseURL, APIMode: &apiMode, Model: &model}
	shouldCache, nativeLayout := anthropicPromptCachePolicy(ctx, blankAgent, dest)
	if !shouldCache {
		canonical := make([]map[string]any, 0, len(messages))
		for _, msg := range messages {
			copyM, _ := asMap(deepCopyAny(msg))
			canonical = append(canonical, copyM)
		}
		stripAnthropicCacheControl(canonical)
		return canonical, stripAnthropicToolCacheControl(tools)
	}
	prefix := ""
	if staticSystemPrefix != nil {
		prefix = *staticSystemPrefix
	}
	return buildPromptCachePlan(messages, tools, cachePlanOptions{
		ttl:                   effectiveCacheTTL(cacheTTL, model, provider),
		nativeAnthropic:       nativeLayout,
		staticSystemPrefix:    prefix,
		directNativeToolCache: directNativeAnthropicToolCacheCapability(blankAgent, dest),
		toolPartMarkers:       envelopeToolPartCacheMarkersSupported(provider, baseURL),
	})
}

// PlanCacheSectionsForDestination plans request local cache sections for a MoA or auxiliary destination
func PlanCacheSectionsForDestination(ctx context.Context, messages, tools []map[string]any, provider, baseURL, apiMode, model string,
	cacheDisabled *bool, cacheTTL string, staticSystemPrefix *string) ([]map[string]any, []map[string]any) {
	return planCacheSectionsForDestination(ctx, messages, tools, provider, baseURL, apiMode, model, cacheDisabled, cacheTTL, staticSystemPrefix)
}
