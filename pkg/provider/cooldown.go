package provider

import (
	"math"
	"time"
)

// Failover reasons that leave the primary rate limited and so arm its cooldown
var RateLimitFailoverReasons = map[FailoverReason]bool{ReasonRateLimit: true, ReasonBilling: true, ReasonUpstreamRateLimit: true}

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
