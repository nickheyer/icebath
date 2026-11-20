package gateway

import (
	"fmt"
	"math"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

// Whether a pending inbound session idle past the timeout owes its one stall notice
func ShouldEmitSessionStallNotification(timeoutSeconds float64, idleSeconds *float64, hasPendingInbound, alreadyNotified bool) bool {
	return timeoutSeconds > 0 && hasPendingInbound && !alreadyNotified && idleSeconds != nil && *idleSeconds >= timeoutSeconds
}

// Whether the stall latch clears, unknown progress holding it
func ShouldClearSessionStallNotification(timeoutSeconds float64, idleSeconds *float64, hasPendingInbound bool) bool {
	if !hasPendingInbound || timeoutSeconds <= 0 {
		return true
	}
	return idleSeconds != nil && *idleSeconds < timeoutSeconds
}

// User facing stall notice with the idle minutes, at least one
func FormatSessionStallNotification(idleSeconds float64) string {
	mins := max(1, int(math.Floor(idleSeconds/60)))
	return fmt.Sprintf("⚠️ I seem to be stuck (no activity for %d min). Send /stop to cancel the current "+
		"task, or /new to start a fresh conversation.", mins)
}

// Finite float of a value, nil for None, a bool, an unconvertible value or a non finite one
func finiteFloat(value any) *float64 {
	if value == nil {
		return nil
	}
	if _, isBool := value.(bool); isBool {
		return nil
	}
	result, err := pyint.Float(value)
	if err != nil || math.IsInf(result, 0) || math.IsNaN(result) {
		return nil
	}
	return &result
}

// Idle seconds of an activity summary, its own idle figure else derived from the last activity stamp, nil without one
func ResolveSessionIdleSecondsFromActivity(activity map[string]any, now *float64) *float64 {
	if len(activity) == 0 {
		return nil
	}
	if idle := finiteFloat(activity["seconds_since_activity"]); idle != nil {
		v := max(0.0, *idle)
		return &v
	}
	ts, has := activity["last_activity_at"]
	if !has || ts == nil {
		ts = activity["last_activity_ts"]
	}
	when := finiteFloat(ts)
	if when == nil {
		return nil
	}
	at := float64(time.Now().UnixNano()) / 1e9
	if now != nil {
		at = *now
	}
	v := max(0.0, at-*when)
	return &v
}
