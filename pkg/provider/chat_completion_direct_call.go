package provider

import (
	"context"
	"math"
	"time"
)

var directAPIActivityHeartbeat = 15 * time.Second

// Socket backstop of an inline blocking call, the stale budget when finite and positive, zero when disarmed
func inlineNonstreamHardTimeout(stale float64) time.Duration {
	if math.IsInf(stale, 0) || math.IsNaN(stale) || stale <= 0 {
		return 0
	}
	return secondsDuration(stale)
}

// Clears the stale breaker of the context's retry session after a responsive call
func resetStaleStreak(ctx context.Context) {
	if s, _ := ctx.Value(staleStreakKey{}).(*RetrySession); s != nil {
		s.ResetStaleStreak()
	}
}
