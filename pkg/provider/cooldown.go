package provider

import (
	"math"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Failover reasons that leave the primary rate limited and so arm its cooldown
var RateLimitFailoverReasons = map[FailoverReason]bool{ReasonRateLimit: true, ReasonBilling: true, ReasonUpstreamRateLimit: true}

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
