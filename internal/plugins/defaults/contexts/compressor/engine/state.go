package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/sessions"
)

// Keys of the session's model settings the compressor keeps state under
const (
	// When the summarising model benched after repeated fallbacks is tried again, in seconds since the epoch
	probeKey = "_fallback_probe_at"
	// Size the transcript must grow back to before the next early trim of tool output
	pruneRearmKey = "_proactive_prune_rearm_tokens"
)

const (
	// Wait after the guard against useless compressions trips, and after a compaction that changed nothing, before trying again
	recoveryWindow = 300 * time.Second
	backoffWindow  = 300 * time.Second
	// Wait after a summary failure that no other rung sets
	failureCooldown = 600 * time.Second
	// Failures of the session's automatic cleanup before a turn, which never hold the compressor of the turn itself
	hygieneMarkers = "session hygiene compression timed out|hygiene compression deferred: turn-hold budget expired"
)

// Rungs of the wait after a summary that timed out or was cut off, by failures in a row
var timeoutLadder = []time.Duration{60 * time.Second, 300 * time.Second, 900 * time.Second}

// Reads the session's stored guards: the failure cooldown, the streaks, the recovery deadline and the model settings the compressor keeps
func (c *compressor) load(ctx context.Context) {
	c.cooldownUntil, c.persistFailed, c.lastError = time.Time{}, false, ""
	c.timeoutFailures, c.truncationFailures, c.fallbackStreak, c.overloadAborts = 0, 0, 0, 0
	c.ineffective, c.prellmSkips = 0, 0
	c.recoveryDeadline, c.backoffUntil, c.probeAt = time.Time{}, time.Time{}, time.Time{}
	c.pruneRearm, c.reclaimed = 0, ""
	if c.store == nil || c.session == "" {
		return
	}
	row, err := c.store.CompressionCooldown(ctx, c.session)
	if err != nil {
		slog.Debug("compression state lookup failed", "session", c.session, "error", err)
		return
	}
	c.fallbackStreak, c.ineffective, c.overloadAborts = max(row.FallbackStreak, 0), max(row.IneffectiveCount, 0), max(row.OverloadStreak, 0)
	c.recoveryDeadline = row.RecoveryDeadline
	if time.Until(row.Until) > 0 && !hygieneOnly(row.Error) {
		c.cooldownUntil, c.lastError = row.Until, row.Error
	}
	config := c.modelConfig(ctx)
	if at := cast.ToFloat64(config[probeKey]); at > 0 {
		c.probeAt = time.Unix(0, int64(at*1e9))
	}
	c.pruneRearm = max(cast.ToInt(config[pruneRearmKey]), 0)
}

// Whether a stored failure belongs to the cleanup before a turn alone
func hygieneOnly(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	for _, marker := range strings.Split(hygieneMarkers, "|") {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// Model settings stored with the session
func (c *compressor) modelConfig(ctx context.Context) map[string]any {
	session, err := c.store.Get(ctx, c.session)
	if err != nil || session.GetModelConfig() == "" {
		return nil
	}
	var config map[string]any
	if json.Unmarshal([]byte(session.GetModelConfig()), &config) != nil {
		return nil
	}
	return config
}

// Writes one key of the session's model settings, nil removing it; false when it could not be stored
func (c *compressor) setModelConfig(ctx context.Context, key string, value any) bool {
	if c.store == nil || c.session == "" {
		return false
	}
	if err := c.store.Update(ctx, c.session, sessions.Change{ModelConfig: map[string]any{key: value}}); err != nil {
		slog.Debug("compression model settings persist failed", "key", key, "error", err)
		return false
	}
	return true
}

// Stores the cooldown, the streaks and the recovery deadline as they are now; false when they could not be
func (c *compressor) persist(ctx context.Context) bool {
	if c.store == nil || c.session == "" {
		return false
	}
	until := c.cooldownUntil
	if time.Until(until) <= 0 {
		until = time.Time{}
	}
	if err := c.store.SetCompressionCooldown(ctx, c.session, sessions.Cooldown{Until: until, Error: c.lastError, FallbackStreak: c.fallbackStreak,
		IneffectiveCount: c.ineffective, RecoveryDeadline: c.recoveryDeadline, OverloadStreak: c.overloadAborts}); err != nil {
		slog.Debug("compression state persist failed", "session", c.session, "error", err)
		return false
	}
	return true
}

func (c *compressor) persistCooldown(ctx context.Context) {
	c.persistFailed = !c.persist(ctx) && c.store != nil && c.session != ""
}

// Sets the count of compactions that left the request over the trigger, stored when it changed
func (c *compressor) setIneffective(count int) {
	if count == c.ineffective {
		return
	}
	c.ineffective = count
	c.persist(context.Background())
}

// Sets when the guard against useless compressions lets one probe through, stored when it changed
func (c *compressor) setRecovery(deadline time.Time) {
	if deadline.Equal(c.recoveryDeadline) {
		return
	}
	c.recoveryDeadline = deadline
	c.persist(context.Background())
}

// Sets when the benched summarising model is tried again, stored when it changed
func (c *compressor) setProbeAt(ctx context.Context, at time.Time) {
	if at.Equal(c.probeAt) {
		return
	}
	c.probeAt = at
	var stored any
	if !at.IsZero() {
		stored = float64(at.UnixNano()) / 1e9
	}
	c.setModelConfig(ctx, probeKey, stored)
}

// Holds summaries off for at least the time given after a failure, kept with the session
func (c *compressor) recordCooldown(wait time.Duration, failure string) {
	if until := time.Now().Add(wait); until.After(c.cooldownUntil) {
		c.cooldownUntil = until
	}
	c.lastError = failure
	c.persistCooldown(context.Background())
}

// Records a summary that timed out or stalled on the ladder of waits, never shorter than the idle limit of the summary step
func (c *compressor) recordTimeout(failure, kind string, idle time.Duration) {
	wait := nextRung(&c.timeoutFailures)
	c.recordCooldown(max(wait, idle), fmt.Sprintf("backoff:%s:strategy=%s: %s", cmp.Or(kind, "timeout"), cmp.Or(c.s.tailMode, "unknown"), failure))
}

// Next rung of the ladder of waits, the counter moved on
func nextRung(counter *int) time.Duration {
	*counter++
	return timeoutLadder[min(*counter, len(timeoutLadder))-1]
}

// Clears the failure cooldown and its counters
func (c *compressor) clearCooldown() {
	c.cooldownUntil, c.lastError = time.Time{}, ""
	c.timeoutFailures, c.truncationFailures = 0, 0
	c.persistFailed = false
	c.persist(context.Background())
}

// Cooldown of the session as it stands, read again from the store when asked: the time left and why; 0 when none
func (c *compressor) cooldown(ctx context.Context, refresh bool) (time.Duration, string) {
	local := time.Until(c.cooldownUntil)
	if local > 0 && !refresh {
		return local, c.lastError
	}
	if c.store == nil || c.session == "" {
		return max(local, 0), c.lastError
	}
	row, err := c.store.CompressionCooldown(ctx, c.session)
	if err != nil {
		slog.Debug("compression failure cooldown lookup failed", "error", err)
		return max(local, 0), c.lastError
	}
	remaining := time.Until(row.Until)
	if remaining <= 0 || hygieneOnly(row.Error) {
		if local > 0 && c.persistFailed {
			return local, c.lastError
		}
		c.cooldownUntil, c.lastError = time.Time{}, ""
		return 0, ""
	}
	c.cooldownUntil, c.lastError, c.persistFailed = row.Until, row.Error, false
	return remaining, c.lastError
}

// Reads the stored guards again before a gate blocks, another process may have cleared or set them
func (c *compressor) refresh(ctx context.Context) {
	c.cooldown(ctx, true)
	if c.store == nil || c.session == "" {
		return
	}
	if row, err := c.store.CompressionCooldown(ctx, c.session); err == nil {
		c.ineffective = max(row.IneffectiveCount, 0)
		c.fallbackStreak = max(row.FallbackStreak, 0)
	}
}

// Counts a summary that failed for the provider's overload, the stored count winning over the one held here
func (c *compressor) countOverload(ctx context.Context) {
	if c.store != nil && c.session != "" {
		if row, err := c.store.CompressionCooldown(ctx, c.session); err == nil {
			c.overloadAborts = max(row.OverloadStreak, c.overloadAborts)
		}
	}
	c.overloadAborts++
	c.persist(ctx)
}
