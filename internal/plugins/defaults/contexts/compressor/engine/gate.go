package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Share of the trigger below which a middle is not worth a summary once a compaction failed to clear the trigger
const feasibilityMiddle = 0.10

// Whether the guard against useless compressions tripped: two compactions in a row the provider confirmed left the request over the trigger
func (c *compressor) tripped() bool {
	return c.ineffective >= 2
}

// Why automatic compression is held off now: a cooldown or a backoff with the seconds left, or the guard tripped; empty when nothing holds
// it off
func (c *compressor) blockReason(ignoreCooldown bool) string {
	until := c.cooldownUntil
	if ignoreCooldown {
		until = time.Time{}
	}
	for _, gate := range []struct {
		label string
		until time.Time
	}{{"cooldown", until}, {"structural_backoff", c.backoffUntil}} {
		if left := time.Until(gate.until); left > 0 {
			return fmt.Sprintf("%s:%.0f", gate.label, left.Seconds())
		}
	}
	if c.tripped() {
		return "ineffective"
	}
	return ""
}

// Block reason without touching anything, for the views of the session
func (c *compressor) blockReasonQuiet() string {
	return c.blockReason(false)
}

// Whether automatic compression is held off, the stored guards read again first since another process may have cleared them
func (c *compressor) blocked(ctx context.Context, ignoreCooldown bool) bool {
	if !c.blockedHere(ignoreCooldown) {
		return false
	}
	c.refresh(ctx)
	return c.blockedHere(ignoreCooldown)
}

// Whether automatic compression is held off by what this process holds, arming or spending the one probe the tripped guard lets through
// after its recovery window
func (c *compressor) blockedHere(ignoreCooldown bool) bool {
	for _, gate := range []struct {
		until time.Time
		skip  bool
		what  string
	}{{c.cooldownUntil, ignoreCooldown, "summary LLM in cooldown"}, {c.backoffUntil, false, "structural no-op backoff"}} {
		if left := time.Until(gate.until); left > 0 && !gate.skip {
			slog.Debug(fmt.Sprintf("Compression deferred — %s for %.0fs more", gate.what, left.Seconds()))
			return true
		}
	}
	if !c.tripped() {
		c.setRecovery(time.Time{})
		return false
	}
	now := time.Now()
	switch {
	case c.recoveryDeadline.IsZero() || c.recoveryDeadline.Sub(now) > recoveryWindow:
		c.setRecovery(now.Add(recoveryWindow))
	case !now.Before(c.recoveryDeadline):
		c.setRecovery(time.Time{})
		if c.ineffective >= 2 {
			c.setIneffective(1)
		}
		slog.Info(fmt.Sprintf("Anti-thrashing recovery: %.0fs elapsed since the guard tripped — allowing one compaction probe (ineffective=%d fallback=%d).",
			recoveryWindow.Seconds(), c.ineffective, c.fallbackStreak))
		return false
	}
	slog.Warn(fmt.Sprintf("Compression skipped — repeated compaction attempts did not restore healthy context. ineffective=%d fallback=%d. Auto-compaction "+
		"will retry once in %.0fs. Consider /new to start fresh, or /compress <topic> for focused compression.", c.ineffective, c.fallbackStreak,
		max(0, time.Until(c.recoveryDeadline).Seconds())))
	return true
}

// Holds retries off after a compaction that could not change anything, without striking the guard against useless compressions
func (c *compressor) structuralNoOp(reason string, messages int) {
	c.backoffUntil, c.backoffLen = time.Now().Add(backoffWindow), messages
	c.savings = 0
	slog.Warn(fmt.Sprintf("Compression skipped (%s): retrying in %.0fs (structural no-op backoff)", reason, backoffWindow.Seconds()))
}

// Lifts the backoff once the transcript grew past the length that could not be compressed
func (c *compressor) liftBackoff(messages int) {
	if !c.backoffUntil.IsZero() && messages > c.backoffLen {
		c.backoffUntil = time.Time{}
	}
}

// Strikes the guard once for a compaction refused before it was stored because it would grow the transcript
func (c *compressor) rejected() {
	c.setIneffective(c.ineffective + 1)
	slog.Warn(fmt.Sprintf("Compaction rejected before commit (would grow the transcript); ineffective_compression_count=%d", c.ineffective))
}

// Records a compaction that was stored: the next real count is checked against the trigger, and a fallback summary counts toward
// benching the summarising model unless the summary was skipped as not worth it
func (c *compressor) completed(usedFallback, skipped bool) {
	c.backoffUntil = time.Time{}
	c.verify = true
	if skipped {
		slog.Info(fmt.Sprintf("Compaction completed via pre-LLM summary skip; fallback_compression_streak unchanged (%d)", c.fallbackStreak))
		return
	}
	if usedFallback {
		c.fallbackStreak++
		slog.Warn(fmt.Sprintf("Compaction completed with a deterministic fallback summary. fallback_compression_streak=%d", c.fallbackStreak))
	} else {
		c.fallbackStreak = 0
	}
	c.overloadAborts = 0
	c.persist(context.Background())
}

// Whether the summarising model sits this compaction out after two fallback summaries in a row, one probe let through per recovery window
func (c *compressor) benched() bool {
	ctx := context.Background()
	if c.fallbackStreak < 2 {
		c.setProbeAt(ctx, time.Time{})
		return false
	}
	now := time.Now()
	if !c.probeAt.IsZero() && !now.Before(c.probeAt) {
		c.setProbeAt(ctx, time.Time{})
		slog.Info(fmt.Sprintf("Compression: probing the summary model again after %d fallback summaries in a row", c.fallbackStreak))
		return false
	}
	if c.probeAt.IsZero() || c.probeAt.Sub(now) > recoveryWindow {
		c.setProbeAt(ctx, now.Add(recoveryWindow))
	}
	c.feasibilitySkip = true
	c.record.Failure = "summary_model_benched"
	slog.Warn(fmt.Sprintf("Compression: %d fallback summaries in a row — skipping LLM summarization, proceeding with deterministic message dropping. "+
		"Next summary-model probe in %.0fs.", c.fallbackStreak, max(0, time.Until(c.probeAt).Seconds())))
	return true
}

// Whether the summary is skipped because the middle is tiny against the trigger, once a compaction already failed to clear it
func (c *compressor) tooSmall(middle, start, end int) bool {
	if c.ineffective < 1 || middle >= int(float64(c.threshold)*feasibilityMiddle) {
		return false
	}
	c.feasibilitySkip = true
	c.prellmSkips++
	slog.Warn(fmt.Sprintf("Compression: middle section (%d tokens at indices %d-%d) is below %.0f%% of threshold (%d tokens) — skipping LLM summarization, "+
		"proceeding with deterministic message dropping. prellm_skip_count=%d", middle, start, end, feasibilityMiddle*100, c.threshold, c.prellmSkips))
	return true
}
