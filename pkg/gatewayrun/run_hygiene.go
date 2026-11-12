package gatewayrun

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Platform stamp of a detached hygiene agent, a surface no live turn runs on
const gatewayHygienePlatform = "gateway_hygiene"

// Escalation ladder of the hygiene failure cooldown over its base, the cap, and the flat turn hold retry
var hygieneCooldownLadderMultipliers = []float64{1, 3, 9}

const (
	hygieneCooldownMaxSeconds  = 3600.0
	hygieneTurnholdRetrySecond = 60.0
)

// Hygiene turn hold budget elapsed mid stream, an availability boundary rather than a failure
type hygieneTurnHoldExceeded struct{ msg string }

func (e *hygieneTurnHoldExceeded) Error() string { return e.msg }

// Python class name the turn hold boundary carries
func (e *hygieneTurnHoldExceeded) PyTypeName() string { return "HygieneTurnHoldExceeded" }

// Gateway whose session state and state database carry the hygiene failure streak and cooldown
type hygieneGateway interface {
	sessionState(sessionKey string) *SessionState
	peekSessionState(sessionKey string) *SessionState
	SessionDB(ctx context.Context) *state.SessionDB
}

// Bumps the persisted hygiene failure streak and returns the escalated cooldown clamped to the cap
func hygieneCooldownForFailure(ctx context.Context, gw hygieneGateway, sessionKey string, baseCooldownSeconds float64) float64 {
	streak := 1
	var st *SessionState
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("hygiene failure streak update failed: %s", pyrepr.Str(rec)))
			}
		}()
		st = gw.sessionState(sessionKey)
	}()
	if db := gw.SessionDB(ctx); db != nil {
		n, err := db.IncrementHygieneFailureStreak(ctx, sessionKey)
		if err == nil {
			streak = max(1, n)
			if st != nil {
				st.mu.Lock()
				st.persistent.hygieneFailureStreak = streak
				st.mu.Unlock()
			}
		} else {
			runLog().Debug(fmt.Sprintf("hygiene failure streak persist failed: %s", err))
			if st != nil {
				streak = st.bumpHygieneFailureStreak()
			}
		}
	} else if st != nil {
		streak = st.bumpHygieneFailureStreak()
	}
	multiplier := hygieneCooldownLadderMultipliers[min(streak, len(hygieneCooldownLadderMultipliers))-1]
	return math.Min(baseCooldownSeconds*multiplier, hygieneCooldownMaxSeconds)
}

// Increments the in memory hygiene failure streak in one hold of the lock and returns the new count
func (s *SessionState) bumpHygieneFailureStreak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persistent.hygieneFailureStreak++
	return s.persistent.hygieneFailureStreak
}

// Clears the hygiene failure streak in memory and in the state database after a compression that reduced context
func resetHygieneFailureStreak(ctx context.Context, gw hygieneGateway, sessionKey string) {
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("hygiene failure streak reset failed: %s", pyrepr.Str(rec)))
			}
		}()
		if st := gw.peekSessionState(sessionKey); st != nil {
			st.mu.Lock()
			st.persistent.hygieneFailureStreak = 0
			st.mu.Unlock()
		}
	}()
	if db := gw.SessionDB(ctx); db != nil {
		if err := db.ResetHygieneFailureStreak(ctx, sessionKey); err != nil {
			runLog().Debug(fmt.Sprintf("hygiene failure streak persistent reset failed: %s", err))
		}
	}
}

// True when a hygiene run rewrote the transcript without aborting and materially shrank the request
func hygieneCompactionRecovered(aborted, rotated, inPlace bool, msgCount, newCount, approxTokens, newTokens int) bool {
	if aborted || !(rotated || inPlace) {
		return false
	}
	return agent.CompressionMadeProgress(msgCount, newCount, approxTokens, newTokens)
}

// User notice for the host timeout that ended hygiene, the doctor hint only for an idle timeout
func hygieneCompressionTimeoutMessage(ctx context.Context, totalExhausted bool) string {
	lead := i18n.T(ctx, "gateway.compress.hygiene_timeout", "", nil)
	if totalExhausted {
		return lead
	}
	return lead + i18n.T(ctx, "gateway.compress.hygiene_timeout_doctor_hint", "", nil)
}

// Cached live agent of the session read under the cache lock, nil when none
func cachedAgentForHygiene(r *GatewayRunner, sessionKey string) *agent.AIAgent {
	if r.agentCache == nil {
		return nil
	}
	if r.agentCacheLock != nil {
		r.agentCacheLock.Lock()
		defer r.agentCacheLock.Unlock()
	}
	entry, ok := r.agentCache.Get(sessionKey)
	if !ok {
		return nil
	}
	return entry.agent
}

// Compacts the live codex app server thread of the cached agent and keeps it cached, the outcome as text
func runCodexHygieneCompaction(ctx context.Context, r *GatewayRunner, sessionKey, sessionID, autoMode string, history []map[string]any, approxTokens int, timeoutSeconds, failureCooldownSeconds float64) (string, error) {
	mode := strings.ToLower(autoMode)
	if mode == "" {
		mode = "native"
	}
	if mode != "native" && mode != "hermes" && mode != "off" {
		mode = "native"
	}
	if mode != "hermes" {
		return "skipped:mode=" + mode, nil
	}
	a := cachedAgentForHygiene(r, sessionKey)
	if a == nil || a == agentPendingSentinel {
		return "skipped:no-cached-agent", nil
	}
	if a.CodexSession() == nil {
		return "skipped:no-live-thread", nil
	}
	compressionCount := func() int {
		if engine := a.ContextCompressor(); engine != nil {
			return engine.Base().CompressionCount
		}
		return 0
	}
	countBefore := compressionCount()
	taskID := sessionID
	if taskID == "" {
		taskID = "default"
	}
	worker := runInDefaultExecutor(ctx, func(ctx context.Context) (any, error) {
		return a.CompressContextUnderFence(ctx, history, approxTokens, nil, taskID, "gateway_hygiene")
	})
	r.trackDeferredAgentWorker(worker, a)
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(math.Max(timeoutSeconds, 1.0)*float64(time.Second)))
	defer cancel()
	_, err := worker.wait(waitCtx)
	if err != nil && waitCtx.Err() != nil && !worker.isDone() {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if failureCooldownSeconds >= 0 {
			reason := "codex app-server thread compaction timed out"
			recordHygieneCooldown(ctx, r, sessionID, failureCooldownSeconds, &reason)
		}
		runLog().Warn(fmt.Sprintf("Session hygiene: codex app-server thread compaction for session %s timed out after %.1fs; continuing without compaction", sessionID, timeoutSeconds))
		return "failed:timeout", nil
	}
	if err != nil {
		runLog().Warn(fmt.Sprintf("Session hygiene: codex app-server thread compaction for session %s failed: %s", sessionID, pyrepr.ExceptionMessage(err)))
		return "failed:" + pyrepr.ExceptionMessage(err), nil
	}
	if compressionCount() > countBefore {
		resetHygieneFailureStreak(ctx, r, sessionKey)
		return "compacted", nil
	}
	return "failed:no-boundary", nil
}

// Whether the hygiene host keeps waiting for a slow summary, never for a cancelled commit fence
func hygieneWaitShouldExtend(idle, timeout, waited, ceiling float64, fenceCancelled bool) bool {
	return !fenceCancelled && idle < timeout && waited < ceiling
}

// Persists a hygiene compression failure cooldown with its error in the state database
func recordHygieneCooldown(ctx context.Context, gw hygieneGateway, sessionID string, cooldownSeconds float64, errValue *string) {
	db := gw.SessionDB(ctx)
	if db == nil {
		return
	}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("session hygiene cooldown persist failed: %s", pyrepr.Str(rec)))
			}
		}()
		db.RecordCompressionFailureCooldown(sessionID, float64(time.Now().UnixNano())/1e9+cooldownSeconds, errValue)
	}()
}

// Stamps a hygiene compression transition on the agent activity clock, logging a failed stamp
func stampHygieneCompressionProvenance(a *agent.AIAgent, desc, provenance, debugLabel string) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(debugLabel, "error", pyrepr.Str(rec))
		}
	}()
	a.TouchActivityWithProvenance(desc, provenance)
}

// Seeds the persisted system prompt of the session so hygiene never rebuilds it, true when one was stored
func seedHygieneSystemPrompt(a *agent.AIAgent, sessionRow *pyjson.Object) bool {
	stored := ""
	if sessionRow != nil {
		if raw, ok := sessionRow.GetOr("system_prompt", nil).(string); ok && pystr.Strip(raw) != "" {
			stored = raw
		}
	}
	a.SetCachedSystemPrompt(stored)
	a.SetRetainSeededSystemPrompt(true)
	return stored != ""
}

// Runs fn on a fresh goroutine and settles the returned future with its outcome
func runInDefaultExecutor(ctx context.Context, fn func(ctx context.Context) (any, error)) *gatewayFuture {
	future := newGatewayFuture()
	workerCtx := context.WithoutCancel(ctx)
	go func() {
		var result any
		var err error
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%s", pyrepr.Str(rec))
			}
			future.settle(result, err)
		}()
		result, err = fn(workerCtx)
	}()
	return future
}
