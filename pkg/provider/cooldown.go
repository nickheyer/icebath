package provider

import (
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Failover reasons that leave the primary rate limited and so arm its cooldown
var RateLimitFailoverReasons = map[FailoverReason]bool{ReasonRateLimit: true, ReasonBilling: true, ReasonUpstreamRateLimit: true}

// Longest exponential primary cooldown, four hours
const MaxRateLimitCooldown = 14400 * time.Second

// Cooldown holds the primary route rate limit cooldown and the per session entitlement rejections
type Cooldown struct {
	mu sync.Mutex
	// Opt in fallback.min_switch_reset_seconds threshold, zero disables the deferral
	MinSwitchReset time.Duration
	// Receives the user facing entitlement rejection status
	Diagnostic   func(string)
	now          func() time.Time
	backoffCount int
	until        time.Time
	rejected     map[[2]string]bool
	Log          *slog.Logger
}

// NewCooldown returns an unarmed cooldown reading the deferral threshold from config
func NewCooldown(cfg config.Tree) *Cooldown {
	return &Cooldown{MinSwitchReset: MinSwitchResetFromConfig(cfg), now: time.Now, rejected: map[[2]string]bool{}, Log: slog.Default()}
}

func (c *Cooldown) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

func (c *Cooldown) logger() *slog.Logger {
	if c.Log == nil {
		return slog.Default()
	}
	return c.Log
}

// MinSwitchResetFromConfig reads fallback.min_switch_reset_seconds, zero or unreadable means off
func MinSwitchResetFromConfig(cfg config.Tree) time.Duration {
	if cfg == nil {
		return 0
	}
	v, ok := config.AsFloat(cfg.Get("fallback.min_switch_reset_seconds"))
	if !ok || math.IsNaN(v) || v <= 0 {
		return 0
	}
	return time.Duration(v * float64(time.Second))
}

// ProviderResetDelay converts a provider declared reset instant into a positive wait or reports none
func ProviderResetDelay(resetAt any, now time.Time) (time.Duration, bool) {
	parsed, ok := ParseAbsoluteTimestamp(resetAt)
	if !ok {
		return 0, false
	}
	delay := parsed - float64(now.UnixNano())/1e9
	if math.IsNaN(delay) || math.IsInf(delay, 0) || delay <= 0 {
		return 0, false
	}
	return time.Duration(delay * float64(time.Second)), true
}

// SwitchDeferredByReset reports a rate limit failover that should wait out a short primary reset instead of switching
func (c *Cooldown) SwitchDeferredByReset(reason FailoverReason, resetAt any, fallbackActivated bool) bool {
	if !RateLimitFailoverReasons[reason] || fallbackActivated {
		return false
	}
	c.mu.Lock()
	threshold := c.MinSwitchReset
	c.mu.Unlock()
	if threshold <= 0 {
		return false
	}
	delay, ok := ProviderResetDelay(resetAt, c.clock())
	if !ok || delay >= threshold {
		return false
	}
	c.logger().Info(fmt.Sprintf("Rate limit resets in %.0f s (< fallback.min_switch_reset_seconds=%.0f): staying on the primary", delay.Seconds(), threshold.Seconds()))
	return true
}

// Arm sets the primary cooldown to the provider reset or the 60s doubling schedule capped at four hours
func (c *Cooldown) Arm(reason FailoverReason, resetAt any, currentProvider, primaryProvider string, fallbackActivated bool) (time.Duration, bool) {
	if !RateLimitFailoverReasons[reason] {
		return 0, false
	}
	current := strings.ToLower(strings.TrimSpace(currentProvider))
	primary := strings.ToLower(strings.TrimSpace(primaryProvider))
	if fallbackActivated && !(primary != "" && current == primary) {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	count := c.backoffCount
	c.backoffCount = count + 1
	var seconds int64
	source := "exponential fallback"
	if delay, ok := ProviderResetDelay(resetAt, c.clock()); ok {
		seconds = int64(math.Ceil(delay.Seconds()))
		source = "provider reset"
	} else {
		if count >= 8 {
			seconds = int64(MaxRateLimitCooldown / time.Second)
		} else {
			seconds = 60 << uint(count)
			if seconds > int64(MaxRateLimitCooldown/time.Second) {
				seconds = int64(MaxRateLimitCooldown / time.Second)
			}
		}
	}
	wait := time.Duration(seconds) * time.Second
	c.until = c.clock().Add(wait)
	c.logger().Info(fmt.Sprintf("Rate-limit backoff level %d: cooldown %d s (%.1f min, backoff#%d, %s)", count, seconds, float64(seconds)/60, count+1, source))
	return wait, true
}

// Until returns the instant the primary becomes eligible again, zero when unarmed
func (c *Cooldown) Until() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.until
}

// Remaining returns the time left on the primary cooldown clamped at zero
func (c *Cooldown) Remaining() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.until.IsZero() {
		return 0
	}
	if left := c.until.Sub(c.clock()); left > 0 {
		return left
	}
	return 0
}

// Active reports whether the primary is still inside its cooldown
func (c *Cooldown) Active() bool { return c.Remaining() > 0 }

// Extend pushes the cooldown out to the later of its current end and the given instant
func (c *Cooldown) Extend(until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if until.After(c.until) {
		c.until = until
	}
}

// Clear drops the cooldown so the next turn may restore the primary at once
func (c *Cooldown) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until = time.Time{}
}

// ResetBackoff zeroes the exponential schedule after a successful primary restore
func (c *Cooldown) ResetBackoff() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.backoffCount = 0
}

// BackoffCount returns how many times the exponential schedule has been armed
func (c *Cooldown) BackoffCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.backoffCount
}

// MarkEntitlementRejected records a Codex ChatGPT account 400 that rejects the model once no pool entry remains for it
func (c *Cooldown) MarkEntitlementRejected(apiErr *APIError, provider, model string, poolHasAvailable func(model string) bool) bool {
	if apiErr == nil || apiErr.StatusCode != 400 {
		return false
	}
	haystack := strings.ToLower(apiErr.Message)
	if haystack == "" {
		haystack = strings.ToLower(apiErr.Error())
	}
	if !strings.Contains(haystack, CodexAccountModelEntitlementMarker) {
		return false
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return false
	}
	if poolHasAvailable != nil && poolHasAvailable(model) {
		return false
	}
	key := [2]string{provider, model}
	c.mu.Lock()
	if c.rejected == nil {
		c.rejected = map[[2]string]bool{}
	}
	if c.rejected[key] {
		c.mu.Unlock()
		return true
	}
	c.rejected[key] = true
	c.mu.Unlock()
	c.logger().Warn(fmt.Sprintf("Model entitlement rejection: this account is not entitled to %s via %s; treating it as unavailable for this session", model, provider))
	if c.Diagnostic != nil {
		c.Diagnostic(fmt.Sprintf("🚫 This account is not entitled to %s via %s; it will be skipped until restart. Switch to an entitled model via /model or `icebath model`.", model, provider))
	}
	return true
}

// IsEntitlementRejected reports a provider and model pair rejected as unentitled under its raw or normalized id
func (c *Cooldown) IsEntitlementRejected(provider, model string) bool {
	c.mu.Lock()
	if len(c.rejected) == 0 {
		c.mu.Unlock()
		return false
	}
	if c.rejected[[2]string{provider, model}] {
		c.mu.Unlock()
		return true
	}
	c.mu.Unlock()
	normalized := NormalizeModelForProvider(model, provider)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rejected[[2]string{provider, normalized}]
}

// EntitlementRejected returns a copy of every rejected provider and model pair
func (c *Cooldown) EntitlementRejected() [][2]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][2]string, 0, len(c.rejected))
	for k := range c.rejected {
		out = append(out, k)
	}
	return out
}
