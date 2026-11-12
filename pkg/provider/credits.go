package provider

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Gateway header naming the backing model served for a named account that asked for the free tier model
const ModelSwitchHeader = "x-nous-model-switch"

// Env var that streams every credits capture to the log
const DevCreditsEnv = "ICEBATH_DEV_CREDITS"

// CreditsCapture holds the last rate limit and credits headers of a session with its notice latch
type CreditsCapture struct {
	mu sync.Mutex
	// Live route the captures are attributed to
	Provider  string
	Model     string
	BaseURL   string
	SessionID string
	// Notice sinks, nil when no surface consumes notices
	Notice      func(AgentNotice)
	NoticeClear func(key string)
	Config      config.Tree
	Log         *slog.Logger
	now         func() time.Time

	rateLimit          *RateLimitState
	credits            *CreditsState
	sessionStartMicros *int64
	latch              map[string]any
	noticesEnabled     *bool
	orCacheHits        int
	pendingModelSwitch *[2]string
}

// NewCreditsCapture returns an empty capture bound to a route and session
func NewCreditsCapture(cfg config.Tree, provider, model, baseURL, sessionID string) *CreditsCapture {
	return &CreditsCapture{Config: cfg, Provider: provider, Model: model, BaseURL: baseURL, SessionID: sessionID, Log: slog.Default(), now: time.Now}
}

func (c *CreditsCapture) logger() *slog.Logger {
	if c.Log == nil {
		return slog.Default()
	}
	return c.Log
}

func (c *CreditsCapture) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

// SetRoute moves the capture onto a new live route after a fallback or model switch
func (c *CreditsCapture) SetRoute(provider, model, baseURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Provider, c.Model, c.BaseURL = provider, model, baseURL
}

// IsNousRoute reports the Nous provider or any route to a nousresearch.com host
func IsNousRoute(provider, baseURL string) bool {
	if strings.ToLower(strings.TrimSpace(provider)) == "nous" {
		return true
	}
	return baseURL != "" && HostMatches(baseURL, "nousresearch.com")
}

func pct(fraction float64, ok bool) string {
	if !ok {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", fraction*100)
}

// AdoptCreditsStateForTests installs a ready state the way a parsed response header does, without notices
func (c *CreditsCapture) AdoptCreditsStateForTests(state *CreditsState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.adoptCredits(state)
}

func (c *CreditsCapture) adoptCredits(state *CreditsState) {
	c.credits = state
	if c.sessionStartMicros == nil {
		v := state.RemainingMicros
		c.sessionStartMicros = &v
	}
}

// CaptureRateLimits parses x-ratelimit headers into the cached state keeping the last known on a miss
func (c *CreditsCapture) CaptureRateLimits(h http.Header) {
	if len(h) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if state := ParseRateLimitHeaders(h, c.Provider, c.clock()); state != nil {
		c.rateLimit = state
	}
}

// RateLimit returns the last captured rate limit state or nil
func (c *CreditsCapture) RateLimit() *RateLimitState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rateLimit
}

// NoteModelSwitch records the backing model the gateway named for the next call and returns it
func (c *CreditsCapture) NoteModelSwitch(h http.Header) (string, bool) {
	if h == nil {
		return "", false
	}
	backing := strings.TrimSpace(LowerHeaders(h)[ModelSwitchHeader])
	if backing == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if backing == c.Model {
		return "", false
	}
	c.pendingModelSwitch = &[2]string{c.Model, backing}
	return backing, true
}

// CaptureNousModelSwitch records the x-nous-model-switch header of a response
func (c *CreditsCapture) CaptureNousModelSwitch(h http.Header) {
	if len(h) == 0 {
		return
	}
	c.NoteModelSwitch(h)
}

// TakePendingModelSwitch returns and clears the recorded requested and backing model pair
func (c *CreditsCapture) TakePendingModelSwitch() (requested, backing string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pendingModelSwitch == nil {
		return "", "", false
	}
	pair := *c.pendingModelSwitch
	c.pendingModelSwitch = nil
	return pair[0], pair[1], true
}

// CaptureAnthropicResponseHeaders captures rate limit and credits state the aggregated message drops
func (c *CreditsCapture) CaptureAnthropicResponseHeaders(h http.Header) {
	c.CaptureRateLimits(h)
	c.CaptureCredits(h, false)
}

// CaptureCredits parses x-nous-credits headers, keeps the last known on a miss, and fires threshold notices
func (c *CreditsCapture) CaptureCredits(h http.Header, refused bool) {
	if fixture := DevFixtureCreditsState(); fixture != nil {
		c.mu.Lock()
		c.adoptCredits(fixture)
		if c.latch != nil {
			c.latch["seen_below_90"] = true
		}
		c.mu.Unlock()
		used, usedOK := fixture.UsedFraction()
		c.logger().Info(fmt.Sprintf("credits ▸ [FIXTURE] remaining=%d (%s) · paid=%t · denom=%s · used=%s (real headers bypassed — `echo clear` / unset %s to restore)",
			fixture.RemainingMicros, orUnknown(fixture.RemainingUSD), fixture.PaidAccess, fixture.DenominatorKind, pct(used, usedOK), DevCreditsFixtureEnv))
		c.EmitCreditsNotices()
		return
	}
	if len(h) == 0 {
		return
	}
	dev := envTruthy(DevCreditsEnv)
	c.mu.Lock()
	state := ParseCreditsHeaders(h, c.Provider)
	if state == nil {
		c.mu.Unlock()
		if dev {
			c.logger().Info("credits ▸ response had no valid x-nous-credits-* headers (miss — producer off / non-Nous path / >TTL stale)")
		}
		return
	}
	if state.IsUsageWindow() && !IsNousRoute(c.Provider, c.BaseURL) {
		c.mu.Unlock()
		return
	}
	if state.IsUsageWindow() && LowerHeaders(h)["x-nous-credits-purchased-micros"] == "" && c.credits != nil {
		next := *state
		next.PurchasedMicros = c.credits.PurchasedMicros
		next.PurchasedUSD = c.credits.PurchasedUSD
		state = &next
	}
	if state.IsUsageWindow() && !refused && state.SpendSource == "" {
		next := *state
		next.Unmetered = true
		state = &next
	}
	c.adoptCredits(state)
	spent, spentOK := c.spentMicrosLocked()
	c.mu.Unlock()
	if !SyncPlanSnapshot(state) {
		c.logger().Debug("Plan snapshot sync failed")
	}
	if dev {
		used, usedOK := state.UsedFraction()
		spentText := "n/a"
		if spentOK {
			spentText = fmt.Sprintf("%.1f¢", float64(spent)/10000)
		}
		age := "n/a"
		if a := state.AgeSeconds(); a < 1e308 {
			age = fmt.Sprintf("%.0fs", a)
		}
		disabled := ""
		if state.DisabledReason != "" {
			disabled = " · disabled=" + state.DisabledReason
		}
		c.logger().Info(fmt.Sprintf("credits ▸ remaining=%d (%s) · paid=%t · denom=%s · used=%s · Δspent=%s · age=%s%s",
			state.RemainingMicros, orUnknown(state.RemainingUSD), state.PaidAccess, state.DenominatorKind, pct(used, usedOK), spentText, age, disabled))
	}
	c.EmitCreditsNotices()
}

func orUnknown(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// EmitCreditsNotices runs the threshold policy, clears stale notices first, then shows new ones
func (c *CreditsCapture) EmitCreditsNotices() {
	if c.Notice == nil && c.NoticeClear == nil {
		return
	}
	c.mu.Lock()
	state := c.credits
	if !c.noticesEnabledLocked() || state == nil {
		c.mu.Unlock()
		return
	}
	if c.latch == nil {
		c.latch = NewCreditsLatch()
	}
	latch := c.latch
	model, baseURL, sessionID := c.Model, c.BaseURL, c.SessionID
	c.mu.Unlock()
	modelIsFree := IsFreeTierModel(model, baseURL)
	blocking := state.Depleted() || state.WindowExhausted() || state.TopTierExhausted()
	if blocking && !modelIsFree && RewarmPricingBeforeDepletedNotice(c) {
		return
	}
	modelIsTopTier := false
	if state.IsUsageWindow() && !modelIsFree {
		modelIsTopTier = IsTopTier(model, CachedPricing(baseURL))
	}
	if state.IsUsageWindow() {
		RestoreWindowNotices(sessionID, latch)
	}
	toShow, toClear := EvaluateCreditsNotices(state, latch, modelIsFree, modelIsTopTier)
	if state.IsUsageWindow() {
		RememberWindowNotices(sessionID, latch)
	} else if sessionID != "" {
		ForgetWindowNotices(sessionID)
	}
	for _, key := range toClear {
		if c.NoticeClear != nil {
			c.NoticeClear(key)
		}
	}
	for _, n := range toShow {
		if c.Notice != nil {
			c.Notice(n)
		}
	}
	RememberShownBand(sessionID, latch["usage_band"])
}

func (c *CreditsCapture) noticesEnabledLocked() bool {
	if c.noticesEnabled != nil {
		return *c.noticesEnabled
	}
	enabled := true
	if c.Config != nil {
		if display := c.Config.Map("display"); display != nil {
			if v, ok := display["credits_notices"]; ok {
				enabled = config.Truthy(v)
			}
		}
	}
	c.noticesEnabled = &enabled
	return enabled
}

// CreditsNoticesEnabled reads display.credits_notices once per capture, defaulting on
func (c *CreditsCapture) CreditsNoticesEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.noticesEnabledLocked()
}

// Credits returns the last captured credits state or nil
func (c *CreditsCapture) Credits() *CreditsState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.credits
}

// Latch returns the live notice latch creating it on first use
func (c *CreditsCapture) Latch() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.latch == nil {
		c.latch = NewCreditsLatch()
	}
	return c.latch
}

func (c *CreditsCapture) spentMicrosLocked() (int64, bool) {
	if c.sessionStartMicros == nil || c.credits == nil {
		return 0, false
	}
	return *c.sessionStartMicros - c.credits.RemainingMicros, true
}

// CreditsSpentMicros is the session cumulative spend, first seen remaining minus current remaining
func (c *CreditsCapture) CreditsSpentMicros() (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spentMicrosLocked()
}

// CheckOpenRouterCacheStatus logs X-OpenRouter-Cache-Status and counts HITs
func (c *CreditsCapture) CheckOpenRouterCacheStatus(h http.Header) {
	if len(h) == 0 {
		return
	}
	status := strings.TrimSpace(LowerHeaders(h)["x-openrouter-cache-status"])
	if status == "" {
		return
	}
	upper := strings.ToUpper(status)
	if upper == "HIT" {
		c.mu.Lock()
		c.orCacheHits++
		hits := c.orCacheHits
		c.mu.Unlock()
		c.logger().Info(fmt.Sprintf("OpenRouter response cache HIT (total: %d)", hits))
		return
	}
	c.logger().Debug("OpenRouter response cache " + upper)
}

// OpenRouterCacheHits returns how many HIT responses the session saw
func (c *CreditsCapture) OpenRouterCacheHits() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.orCacheHits
}
