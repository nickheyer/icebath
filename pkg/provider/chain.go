package provider

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"
)

// Rotator moves a failed route onto another credential or backend and reports whether it did
type Rotator interface {
	Rotate(ctx context.Context, route Route, apiErr *APIError) (Route, bool)
}

// FailureScope names which identity axis a failure invalidates
type FailureScope string

const (
	// Timeout, overload, connection blip, invalid response, evidence against one model deployment
	ScopeModel FailureScope = "model"
	// Auth 401 or payment 402, evidence against the shared credential
	ScopeCredential FailureScope = "credential"
	// DNS, connection refused, unreachable host, evidence against the endpoint
	ScopeEndpoint FailureScope = "endpoint"
)

// FailureScopes lists every scope in declaration order
var FailureScopes = []FailureScope{ScopeModel, ScopeCredential, ScopeEndpoint}

func normIdentity(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// BackendIdentity is the normalized provider, model, and endpoint of one deployment with empty meaning unknown
type BackendIdentity struct {
	Provider string
	Model    string
	BaseURL  string
}

// BuildBackendIdentity normalizes the three axes of a candidate route
func BuildBackendIdentity(provider, model, baseURL string) BackendIdentity {
	return BackendIdentity{Provider: normIdentity(provider), Model: normIdentity(model), BaseURL: NormalizeRouteBaseURL(baseURL)}
}

// RouteIdentity builds the identity of a resolved route
func RouteIdentity(r Route) BackendIdentity {
	return BuildBackendIdentity(r.Provider, r.Model, r.BaseURL)
}

// Registry probe the first class provider check consults, swapped by tests
var providerIsFirstClass = func(provider string) bool {
	_, ok := ProfileFor(provider)
	return ok
}

func bothFirstClass(a, b BackendIdentity) bool {
	if a.Provider == "" || b.Provider == "" || a.Provider == b.Provider {
		return false
	}
	return providerIsFirstClass(a.Provider) && providerIsFirstClass(b.Provider)
}

// SameCredentialSurface reports two identities sharing the credential a 401 or 402 just invalidated
func SameCredentialSurface(a, b BackendIdentity) bool {
	if a.Provider != "" && b.Provider != "" {
		return a.Provider == b.Provider
	}
	return a.BaseURL != "" && a.BaseURL == b.BaseURL
}

// SameEndpoint reports two identities behind the endpoint that just went unreachable
func SameEndpoint(a, b BackendIdentity) bool {
	if a.BaseURL != "" && b.BaseURL != "" {
		return a.BaseURL == b.BaseURL
	}
	return a.Provider != "" && a.Provider == b.Provider
}

// SameDeployment reports the exact same model deployment, the thing a timeout kills
func SameDeployment(a, b BackendIdentity) bool {
	if !(a.Provider != "" && b.Provider != "" && a.Provider == b.Provider) {
		return a.BaseURL != "" && a.BaseURL == b.BaseURL && a.Model != "" && a.Model == b.Model && !bothFirstClass(a, b)
	}
	if !(a.Model != "" && b.Model != "" && a.Model == b.Model) {
		return false
	}
	return !(a.BaseURL != "" && b.BaseURL != "" && a.BaseURL != b.BaseURL)
}

// ShouldSkipCandidate reports whether trying the candidate would just repeat the failure along the scope axis
func ShouldSkipCandidate(candidate, failed BackendIdentity, scope FailureScope) bool {
	switch scope {
	case ScopeCredential:
		return SameCredentialSurface(candidate, failed)
	case ScopeEndpoint:
		return SameEndpoint(candidate, failed)
	}
	return SameDeployment(candidate, failed)
}

// PoolProbe is the credential pool view the fallback walk and primary restore consult
type PoolProbe interface {
	Provider() string
	HasCredentials() bool
	HasAvailable(model string) bool
	NextAvailableAt(model string) (time.Time, bool)
	EntryCount() int
	LiftReopenedCooldowns(model string) bool
}

// Short cooldown armed when a non empty chain exhausts on a non rate limit failure
const FallbackExhaustedCooldown = 5 * time.Second

// Credential cooldowns longer than the Retry-After cap make a candidate pointless
const candidatePoolExhaustedWindow = 600 * time.Second

var fallbackReasonLabels = map[FailoverReason]string{
	ReasonAuth: "authentication failed", ReasonAuthPermanent: "authentication permanently failed",
	ReasonBilling: "billing or quota exhausted", ReasonRateLimit: "rate limit",
	ReasonUpstreamRateLimit: "upstream model rate limit", ReasonOverloaded: "provider overloaded",
	ReasonServerError: "provider server error", ReasonTimeout: "request timeout",
	ReasonSSLCertVerification: "TLS certificate verification failed", ReasonContextOverflow: "context window exceeded",
	ReasonPayloadTooLarge: "request payload too large", ReasonImageTooLarge: "image payload too large",
	ReasonModelNotFound: "model not found", ReasonProviderPolicyBlocked: "provider policy blocked the request",
	ReasonContentPolicyBlocked: "content policy blocked the request", ReasonFormatError: "request format rejected",
	ReasonRoleAlternation: "adjacent same-role messages rejected", ReasonInvalidEncryptedContent: "encrypted reasoning state rejected",
	ReasonMultimodalToolContent: "multimodal tool content unsupported", ReasonThinkingSignature: "thinking signature rejected",
	ReasonLongContextTier: "long-context tier unavailable", ReasonOAuthLongContextBetaForbid: "OAuth long-context beta unavailable",
	ReasonLlamaCppGrammarPattern: "grammar pattern rejected", ReasonUnknown: "provider failure",
}

// FallbackReasonText is the concise operator facing explanation for a fallback switch
func FallbackReasonText(reason FailoverReason) string {
	if label, ok := fallbackReasonLabels[reason]; ok {
		return label
	}
	if reason == "" {
		return "provider failure"
	}
	return strings.ReplaceAll(string(reason), "_", " ")
}

// ChainOptions wires the fallback chain to the agent's pool, store, client swap, and notice surfaces
type ChainOptions struct {
	Primary   Route
	Fallbacks []Route
	Cooldown  *Cooldown
	// Pool bound to the primary at construction, nil when the primary has none
	Pool PoolProbe
	// Loads a provider pool for a candidate, nil result means none
	LoadPool func(ctx context.Context, provider string) PoolProbe
	// Loads the primary's own pool, defaults to LoadPool on the primary provider
	LoadPrimaryPool func(ctx context.Context) PoolProbe
	// Auth store read for the nous local usability check
	Store *Store
	// Buffered status line sink, shown only when every retry and fallback fails
	Diagnostic func(string)
	// Immediate status line sink
	Emit func(string)
	// Swaps the agent's client, compressor, reasoning config, and prompt identity onto the next route
	OnActivate func(ctx context.Context, old, next Route) error
	// Reinstalls the primary runtime and re selects its pool entry, pool may be nil
	OnRestore func(ctx context.Context, primary Route, pool PoolProbe) error
	// Moves a live session back onto the credential a quota bench rotated it off
	RevertCredentialRotation func(ctx context.Context)
	RecordFallback           func(from, to string, reason FailoverReason)
	// Profile name for the billing fallback warning remedy, empty means default
	ProfileName string
	// Minimum spacing of the Codex early reopen probe, zero takes the auth slice constant
	CodexReopenProbeInterval time.Duration
	Log                      *slog.Logger
	Now                      func() time.Time
}

// Chain walks the fallback routes in order when the primary fails and restores the primary per turn
type Chain struct {
	mu                     sync.Mutex
	opts                   ChainOptions
	primary                Route
	current                Route
	entries                []Route
	index                  int
	activated              bool
	providerFallbackActive bool
	providerFallbackRoute  *[2]string
	unavailable            map[[3]string]bool
	cooldown               *Cooldown
	pool                   PoolProbe
	pendingNotices         []string
	restoreWaitLogged      bool
	codexReopenCheckedAt   time.Time
	codexReopenChecked     bool
}

// NewChain binds a primary route and its fallback routes with an unarmed cooldown
func NewChain(opts ChainOptions) *Chain {
	if opts.Cooldown == nil {
		opts.Cooldown = NewCooldown(nil)
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.CodexReopenProbeInterval <= 0 {
		opts.CodexReopenProbeInterval = CodexQuotaProbeMinInterval
	}
	entries := make([]Route, len(opts.Fallbacks))
	copy(entries, opts.Fallbacks)
	return &Chain{opts: opts, primary: opts.Primary, current: opts.Primary, entries: entries, unavailable: map[[3]string]bool{}, cooldown: opts.Cooldown, pool: opts.Pool}
}

// Current is the route the session is on right now
func (c *Chain) Current() Route {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

// Primary is the route configured before any fallback
func (c *Chain) Primary() Route {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.primary
}

// SetCurrent records a route change made outside the walk, such as a credential rotation
func (c *Chain) SetCurrent(r Route) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = r
}

// Activated reports whether a fallback route is live
func (c *Chain) Activated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activated
}

// ProviderFallbackActive reports a real provider fallback as opposed to a once only model switch
func (c *Chain) ProviderFallbackActive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.providerFallbackActive
}

// HasPending reports whether untried chain entries remain
func (c *Chain) HasPending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.index < len(c.entries)
}

// Index is how many chain entries have been consumed this turn
func (c *Chain) Index() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.index
}

// Entries returns a copy of the fallback routes
func (c *Chain) Entries() []Route {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Route, len(c.entries))
	copy(out, c.entries)
	return out
}

// Cooldown exposes the primary cooldown state
func (c *Chain) Cooldown() *Cooldown { return c.cooldown }

// HasDiagnosticSink reports whether switch notices already reach a buffered status surface
func (c *Chain) HasDiagnosticSink() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opts.Diagnostic != nil
}

// SetDiagnostic binds the buffered status surface switch notices are written to
func (c *Chain) SetDiagnostic(fn func(string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opts.Diagnostic = fn
}

// Pool returns the credential pool bound to the current route
func (c *Chain) Pool() PoolProbe {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pool
}

// SetPool rebinds the pool after a rotation made outside the chain
func (c *Chain) SetPool(p PoolProbe) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pool = p
}

// ResetForFreshAttempt reopens the chain after a primary transport recovery
func (c *Chain) ResetForFreshAttempt() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.index = 0
	c.activated = false
}

// TakePendingNotices returns and clears the buffered fallback switch notices
func (c *Chain) TakePendingNotices() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.pendingNotices
	c.pendingNotices = nil
	return out
}

// PoolMayRecoverFromRateLimit reports a multi entry pool with somewhere to rotate to after a 429
func PoolMayRecoverFromRateLimit(pool PoolProbe) bool {
	return pool != nil && pool.HasAvailable("") && pool.EntryCount() > 1
}

// Rotate classifies the failure and walks to the next fallback route, satisfying Rotator
func (c *Chain) Rotate(ctx context.Context, route Route, apiErr *APIError) (Route, bool) {
	cl := ClassifyError(ClassifyInput{Err: apiErr, Provider: route.Provider, Model: route.Model, BaseURL: route.BaseURL, APIKey: route.APIKey})
	var resetAt any
	if v, ok := ExtractAPIErrorContext(apiErr)["reset_at"]; ok {
		resetAt = v
	}
	if !c.TryActivateFallback(ctx, cl.Reason, resetAt) {
		return route, false
	}
	return c.Current(), true
}

func entryKey(r Route) [3]string {
	return [3]string{strings.ToLower(strings.TrimSpace(r.Provider)), strings.TrimSpace(r.Model), strings.TrimRight(strings.TrimSpace(r.BaseURL), "/")}
}

func (c *Chain) bufferDiagnostic(text string) {
	if c.opts.Diagnostic != nil {
		c.opts.Diagnostic(text)
	}
}

func (c *Chain) bufferFallbackNotice(notice string) {
	c.bufferDiagnostic(notice)
	c.pendingNotices = append(c.pendingNotices, notice)
}

// nousUnavailableLocally returns a skip reason for a nous entry without any token in the store
func (c *Chain) nousUnavailableLocally(r Route) string {
	if strings.ToLower(strings.TrimSpace(r.Provider)) != "nous" {
		return ""
	}
	if c.opts.Store == nil {
		return "nous_auth_unreadable:no_store"
	}
	state, err := c.opts.Store.GetProviderAuthState("nous")
	if err != nil {
		return "nous_auth_unreadable:" + fmt.Sprintf("%T", err)
	}
	for _, key := range []string{"access_token", "refresh_token"} {
		if s, ok := state[key].(string); ok && strings.TrimSpace(s) != "" {
			return ""
		}
	}
	return "nous_token_missing"
}

func (c *Chain) candidatePoolExhausted(ctx context.Context, provider, model string) bool {
	pool := c.pool
	if pool == nil || strings.ToLower(strings.TrimSpace(pool.Provider())) != provider {
		if c.opts.LoadPool == nil {
			return false
		}
		pool = c.opts.LoadPool(ctx, provider)
	}
	if pool == nil || !pool.HasCredentials() || pool.HasAvailable(model) {
		return false
	}
	until, ok := pool.NextAvailableAt(model)
	return !ok || until.Sub(c.opts.Now()) > candidatePoolExhaustedWindow
}

func (c *Chain) shouldSkipCandidate(ctx context.Context, fb Route, key [3]string, provider, model string) bool {
	if c.unavailable[key] {
		c.opts.Log.Debug(fmt.Sprintf("Fallback skip: %v previously marked unavailable", key))
		return true
	}
	if provider == "" || model == "" {
		return true
	}
	if c.cooldown.IsEntitlementRejected(provider, model) {
		c.opts.Log.Info(fmt.Sprintf("Fallback skip: %s/%s was rejected as unentitled for this account", provider, model))
		return true
	}
	if c.candidatePoolExhausted(ctx, provider, model) {
		c.opts.Log.Warn(fmt.Sprintf("Fallback skip: %s/%s credential pool is exhausted (every entry in cooldown)", provider, model))
		return true
	}
	if reason := c.nousUnavailableLocally(fb); reason != "" {
		c.unavailable[key] = true
		c.opts.Log.Warn(fmt.Sprintf("Fallback skip: %s/%s is not locally usable (%s); suppressing for this session", provider, model, reason))
		return true
	}
	currentIdent := RouteIdentity(c.current)
	fbIdent := BuildBackendIdentity(provider, model, fb.BaseURL)
	if ShouldSkipCandidate(fbIdent, currentIdent, ScopeModel) {
		where := currentIdent.BaseURL
		if where == "" {
			where = currentIdent.Provider
		}
		c.opts.Log.Warn(fmt.Sprintf("Fallback skip: chain entry %s/%s resolves to the same backend as the current one (%s)", provider, model, where))
		return true
	}
	return false
}

// rebindFallbackPool drops a pool of another provider and attaches the candidate's own pool
func (c *Chain) rebindFallbackPool(ctx context.Context, provider, model string) {
	if c.pool != nil {
		poolProvider := strings.ToLower(strings.TrimSpace(c.pool.Provider()))
		if poolProvider != "" && poolProvider != provider {
			c.opts.Log.Info(fmt.Sprintf("Fallback to %s/%s: clearing primary credential pool (pool_provider=%s) to prevent cross-provider contamination", provider, model, poolProvider))
			c.pool = nil
		}
	}
	if c.pool == nil && c.opts.LoadPool != nil {
		if pool := c.opts.LoadPool(ctx, provider); pool != nil && pool.HasCredentials() {
			c.pool = pool
			c.opts.Log.Info(fmt.Sprintf("Fallback to %s/%s: attached fallback credential pool", provider, model))
		}
	}
}

func (c *Chain) logActivated(reason FailoverReason, oldModel, oldProvider, fbModel, fbProvider string) {
	if reason != ReasonBilling {
		c.opts.Log.Info(fmt.Sprintf("Fallback activated: %s → %s (%s)", oldModel, fbModel, fbProvider))
		return
	}
	profile := c.opts.ProfileName
	if profile == "" {
		profile = "default"
	}
	remedy := "icebath model"
	if profile != "default" {
		remedy = "icebath -p " + profile + " model"
	}
	c.opts.Log.Warn(fmt.Sprintf("Profile %s: %s via %s refused for billing/credits — using fallback %s via %s. Top up credits, or run `%s` to pick a model this account can use.",
		profile, oldModel, oldProvider, fbModel, fbProvider, remedy))
}

// exhausted arms a short cooldown on a non rate limit walk so the next turn does not replay the whole chain
func (c *Chain) exhausted(reason FailoverReason) bool {
	if len(c.entries) > 0 && !RateLimitFailoverReasons[reason] {
		c.cooldown.Extend(c.opts.Now().Add(FallbackExhaustedCooldown))
	}
	return false
}

// TryActivateFallback switches to the next usable chain entry and reports false when the chain is exhausted or deferred
func (c *Chain) TryActivateFallback(ctx context.Context, reason FailoverReason, resetAt any) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cooldown.SwitchDeferredByReset(reason, resetAt, c.activated) {
		return false
	}
	_, armed := c.cooldown.Arm(reason, resetAt, c.current.Provider, c.primary.Provider, c.activated)
	for {
		if c.index >= len(c.entries) {
			return c.exhausted(reason)
		}
		fb := c.entries[c.index]
		c.index++
		key := entryKey(fb)
		provider := strings.ToLower(strings.TrimSpace(fb.Provider))
		model := strings.TrimSpace(fb.Model)
		if c.shouldSkipCandidate(ctx, fb, key, provider, model) {
			continue
		}
		next := fb
		next.Provider = provider
		if provider != "moa" {
			next.Model = NormalizeModelForProvider(model, provider)
		} else {
			next.Model = model
		}
		old := c.current
		c.current = next
		c.activated = true
		c.rebindFallbackPool(ctx, provider, next.Model)
		if c.opts.OnActivate != nil {
			if err := c.opts.OnActivate(ctx, old, next); err != nil {
				if provider == "nous" {
					c.unavailable[key] = true
				}
				c.opts.Log.Error(fmt.Sprintf("Failed to activate fallback %s: %v", next.Model, err))
				continue
			}
		}
		notice := fmt.Sprintf("⚠️ Model fallback: %s via %s unavailable (%s); using %s via %s.", old.Model, old.Provider, FallbackReasonText(reason), next.Model, provider)
		if armed {
			remaining := int(math.Ceil(c.cooldown.Remaining().Seconds()))
			if remaining < 0 {
				remaining = 0
			}
			notice += fmt.Sprintf(" Primary retry eligible in ~%d s; recovery is not guaranteed.", remaining)
		}
		c.bufferFallbackNotice(notice)
		c.providerFallbackActive = true
		c.providerFallbackRoute = &[2]string{next.Model, provider}
		c.logActivated(reason, old.Model, old.Provider, next.Model, provider)
		if c.opts.RecordFallback != nil {
			c.opts.RecordFallback(old.Provider, provider, reason)
		}
		return true
	}
}

func (c *Chain) matchesPrimary(pool PoolProbe) bool {
	if pool == nil {
		return false
	}
	return CredentialPoolMatchesProvider(pool.Provider(), strings.ToLower(strings.TrimSpace(c.primary.Provider)), c.primary.BaseURL)
}

func (c *Chain) loadPrimaryPool(ctx context.Context) PoolProbe {
	var loaded PoolProbe
	if c.opts.LoadPrimaryPool != nil {
		loaded = c.opts.LoadPrimaryPool(ctx)
	} else if c.opts.LoadPool != nil {
		loaded = c.opts.LoadPool(ctx, strings.ToLower(strings.TrimSpace(c.primary.Provider)))
	}
	if loaded != nil && c.matchesPrimary(loaded) {
		return loaded
	}
	return nil
}

// primaryQuotaReopenedEarly asks the Codex pool once per probe interval whether a benched window reopened
func (c *Chain) primaryQuotaReopenedEarly(ctx context.Context, primaryProvider, primaryModel string) bool {
	if primaryProvider != "openai-codex" {
		return false
	}
	now := c.opts.Now()
	if c.codexReopenChecked && now.Sub(c.codexReopenCheckedAt) < c.opts.CodexReopenProbeInterval {
		return false
	}
	c.codexReopenChecked = true
	c.codexReopenCheckedAt = now
	pool := c.pool
	if pool == nil || !c.matchesPrimary(pool) {
		pool = c.loadPrimaryPool(ctx)
	}
	if pool == nil {
		return false
	}
	benched, ok := pool.NextAvailableAt(primaryModel)
	if !ok || !benched.After(now) {
		return false
	}
	return pool.LiftReopenedCooldowns(primaryModel)
}

// primaryResetGateBlocks skips a guaranteed to fail restore while the primary pool reports a future reset
func (c *Chain) primaryResetGateBlocks(ctx context.Context, primaryProvider, primaryModel string) (bool, PoolProbe, bool) {
	var prefetchedPool PoolProbe
	prefetched := false
	pool := c.pool
	if !c.matchesPrimary(pool) {
		pool = c.loadPrimaryPool(ctx)
		prefetchedPool = pool
		prefetched = true
	}
	if pool == nil {
		return false, prefetchedPool, prefetched
	}
	nextAt, ok := pool.NextAvailableAt(primaryModel)
	if ok && nextAt.After(c.opts.Now()) {
		if !c.restoreWaitLogged {
			c.restoreWaitLogged = true
			label := primaryProvider
			if label == "" {
				label = "?"
			}
			c.opts.Log.Info(fmt.Sprintf("Primary %s rate-limited until %s; staying on fallback %s/%s until the reset elapses",
				label, nextAt.Format("2006-01-02T15:04:05"), c.current.Provider, c.current.Model))
		}
		return true, prefetchedPool, prefetched
	}
	return false, prefetchedPool, prefetched
}

// rebindPrimaryPool reloads the primary's pool after a cross provider fallback attached another
func (c *Chain) rebindPrimaryPool(ctx context.Context, prefetchedPool PoolProbe, prefetched bool) {
	pool := c.pool
	if pool != nil && strings.TrimSpace(pool.Provider()) != "" && !c.matchesPrimary(pool) {
		c.pool = nil
		if prefetched {
			c.pool = prefetchedPool
		} else {
			c.pool = c.loadPrimaryPool(ctx)
		}
	}
}

// RestorePrimary moves the session back onto the primary at a turn boundary when its cooldowns allow
func (c *Chain) RestorePrimary(ctx context.Context) (Route, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.activated {
		c.index = 0
		if c.opts.RevertCredentialRotation != nil {
			c.opts.RevertCredentialRotation(ctx)
		}
		return c.current, false
	}
	primaryProvider := strings.ToLower(strings.TrimSpace(c.primary.Provider))
	primaryModel := strings.TrimSpace(c.primary.Model)
	if primaryModel != "" && (c.cooldown.IsEntitlementRejected(primaryProvider, primaryModel) || IsKnownNonChatModel(primaryModel)) {
		return c.current, false
	}
	if c.primaryQuotaReopenedEarly(ctx, primaryProvider, primaryModel) {
		c.cooldown.Clear()
	}
	if c.cooldown.Active() {
		return c.current, false
	}
	blocked, prefetchedPool, prefetched := c.primaryResetGateBlocks(ctx, primaryProvider, primaryModel)
	if blocked {
		return c.current, false
	}
	c.restoreWaitLogged = false
	previousModel, previousProvider := c.current.Model, c.current.Provider
	if c.providerFallbackRoute != nil {
		previousModel, previousProvider = c.providerFallbackRoute[0], c.providerFallbackRoute[1]
	}
	if previousModel == "" {
		previousModel = "unknown"
	}
	if previousProvider == "" {
		previousProvider = "unknown"
	}
	providerFallbackActive := c.providerFallbackActive
	c.rebindPrimaryPool(ctx, prefetchedPool, prefetched)
	if c.opts.OnRestore != nil {
		if err := c.opts.OnRestore(ctx, c.primary, c.pool); err != nil {
			c.opts.Log.Warn(fmt.Sprintf("Failed to restore primary runtime: %v", err))
			return c.current, false
		}
	}
	c.current = c.primary
	c.activated = false
	c.index = 0
	c.cooldown.ResetBackoff()
	c.opts.Log.Info(fmt.Sprintf("Primary runtime restored for new turn: %s (%s)", c.primary.Model, c.primary.Provider))
	c.providerFallbackActive = false
	c.providerFallbackRoute = nil
	if providerFallbackActive && c.opts.Emit != nil {
		c.opts.Emit(fmt.Sprintf("✅ Primary model restored: %s via %s; fallback %s via %s is no longer active.", c.primary.Model, c.primary.Provider, previousModel, previousProvider))
	}
	return c.primary, true
}
