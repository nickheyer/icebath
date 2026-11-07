package agent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
)

// Per agent owned compression timeout outcome, one slot per goroutine with the attribute mirror
type compressionTimeoutState struct {
	compressionTimeoutMu    sync.Mutex
	compressionTimeoutLocal *pythread.Local
	lastCompressionTimedOut bool
}

// Whether the live session DB predates the durable compression lock API, never true for the Go store
func lockAPIIsAbsentOnSessionDB(lockDB any) bool {
	if lockDB == nil {
		return false
	}
	if _, ok := lockDB.(*state.SessionDB); !ok {
		return false
	}
	_, has := reflect.TypeOf(lockDB).MethodByName("TryAcquireCompressionLock")
	return !has
}

// Refreshes the durable automatic compression guards of a built in compressor
func refreshPersistedCompressionGuards(compressor any, includeCooldown bool) {
	c, ok := compressor.(*ContextCompressor)
	if !ok || c == nil {
		return
	}
	calls := []struct {
		name string
		fn   func()
	}{{"_load_fallback_compression_streak", c.loadFallbackCompressionStreak}, {"_load_ineffective_compression_count", c.loadIneffectiveCompressionCount}}
	if includeCooldown {
		calls = append([]struct {
			name string
			fn   func()
		}{{"get_active_compression_failure_cooldown", func() { c.GetActiveCompressionFailureCooldown(true) }}}, calls...)
	}
	for _, call := range calls {
		func() {
			defer func() {
				if r := recover(); r != nil {
					compressionLog().Debug(fmt.Sprintf("compression guard refresh failed (%s): %v", call.name, r))
				}
			}()
			call.fn()
		}()
	}
}

type sessionRowGetter interface {
	GetSession(ctx context.Context, sessionID string) (*pyjson.Object, error)
}

// Whether another path already rotated this compression parent
func sessionWasRotatedByCompression(ctx context.Context, sessionDB any, sessionID string) (bool, error) {
	getter, ok := sessionDB.(sessionRowGetter)
	if !ok || isNilEngine(sessionDB) {
		return false, nil
	}
	row, err := getter.GetSession(ctx, sessionID)
	if err != nil || row == nil {
		return false, err
	}
	ended, _ := row.Get("ended_at")
	reason, _ := row.Get("end_reason")
	return ended != nil && reason == "compression", nil
}

// Cached system prompt, or a fresh build when nothing is cached on an abort path
func existingSystemPrompt(ctx context.Context, a *AIAgent, systemMessage *string) (string, error) {
	if a.cachedSystemPrompt != "" {
		return a.cachedSystemPrompt, nil
	}
	return BuildSystemPrompt(ctx, a, systemMessage)
}

// Puts the pre compression deep snapshot back into the caller visible list in place when it drifted
func restoreMessagesSnapshot(messages *[]map[string]any, snapshot []map[string]any) {
	if snapshot != nil && !reflect.DeepEqual(*messages, snapshot) {
		restored := pycopy.DeepCopy(snapshot).([]map[string]any)
		if cap(*messages) >= len(restored) {
			live := (*messages)[:len(restored)]
			copy(live, restored)
			restored = live
		}
		*messages = restored
	}
}

// Restores only the prune runway from the attempt snapshot so a kept transcript keeps its cached prefix
func restorePruneRearmTokens(compressor any, snapshot map[string]any) {
	v, has := snapshot["_proactive_prune_rearm_tokens"]
	if c, ok := compressor.(*ContextCompressor); has && ok && c != nil {
		c.proactivePruneRearmTokens, _ = v.(int)
	}
}

// Type pinned read of the per session lock skip signal, a holder string or true
func compressionSkippedDueToLock(a *AIAgent) bool {
	switch v := a.compressionSkippedDueToLock.(type) {
	case bool:
		return v
	case string:
		return true
	}
	return false
}

// Lock and goroutine slot of the timeout outcome, the slot created on demand
func (s *compressionTimeoutState) timeoutSlot(create bool) *pythread.Local {
	if create && s.compressionTimeoutLocal == nil {
		s.compressionTimeoutLocal = &pythread.Local{}
	}
	return s.compressionTimeoutLocal
}

// Writes this goroutine's owned compression timeout outcome and the attribute mirror
func setContextCompressionTimeoutOutcome(a *AIAgent, timedOut bool) {
	a.compressionTimeoutMu.Lock()
	defer a.compressionTimeoutMu.Unlock()
	slot := a.timeoutSlot(true)
	if timedOut {
		slot.Set(true)
	} else {
		slot.Delete()
	}
	a.lastCompressionTimedOut = timedOut
}

// Clears the calling goroutine's owned compression timeout outcome
func resetContextCompressionTimeoutOutcome(a *AIAgent) { setContextCompressionTimeoutOutcome(a, false) }

// Marks the calling goroutine's owned compression as host timed out
func markContextCompressionTimedOut(a *AIAgent) { setContextCompressionTimeoutOutcome(a, true) }

// Whether the calling goroutine's owned compression hit its host timeout
func contextCompressionTimedOut(a *AIAgent) bool {
	a.compressionTimeoutMu.Lock()
	defer a.compressionTimeoutMu.Unlock()
	if slot := a.timeoutSlot(false); slot != nil {
		v, _ := slot.Get()
		return v == true
	}
	return a.lastCompressionTimedOut
}

// Refreshes durable guards then evaluates the automatic breaker gate, returning its block reason or nil
func automaticCompressionGateBlocks(a *AIAgent, bypassCooldown, includeCooldown bool) *string {
	compressor := a.contextCompressor
	refreshPersistedCompressionGuards(compressor, includeCooldown)
	c, ok := compressor.(*ContextCompressor)
	if !ok || c == nil {
		return nil
	}
	if !c.automaticCompressionBlocked(bypassCooldown) {
		return nil
	}
	var reason *string
	swallowCompression("compression block-reason read failed", true, func() error {
		reason = c.compressionBlockReason(bypassCooldown)
		return nil
	})
	r := "unknown"
	if reason != nil && *reason != "" {
		r = *reason
	}
	markCompressionBlockedTransient(a, c, r)
	return &r
}

// Type pinned read of the transient block signal a cooldown or structural backoff no op publishes
func compressionBlockedTransiently(a *AIAgent) bool {
	s, ok := a.compressionBlockedTransient.(string)
	return ok && s != ""
}

// Publishes the transient block signal when the active guard is a cooldown or structural backoff
func markCompressionBlockedTransient(a *AIAgent, c *ContextCompressor, reason string) {
	if !strings.HasPrefix(reason, "cooldown") && !strings.HasPrefix(reason, "structural_backoff") {
		return
	}
	lastErr := "unknown"
	if c != nil && c.lastSummaryError != nil && *c.lastSummaryError != "" {
		lastErr = *c.lastSummaryError
	}
	compressionLog().Info(fmt.Sprintf("Skipping automatic compression re-entry: transient guard active (%s, session=%s, last failure: %s) — will retry after the backoff lapses; /compress forces an immediate retry",
		reason, pyStrOr(a.sessionID, "none"), lastErr))
	a.compressionBlockedTransient = reason
}
