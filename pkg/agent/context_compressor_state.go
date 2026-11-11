package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	minCtxTriggerRatio            = 0.85
	antiThrashRecoverySeconds     = 300.0
	structuralNoOpBackoffSeconds  = 300.0
	feasibilitySkipMiddleFraction = 0.10
	fallbackProbeAtModelConfigKey = "_fallback_probe_at"
)

var compressorProcessStart = time.Now()

var (
	compressorMonotonic          = func() float64 { return time.Since(compressorProcessStart).Seconds() }
	compressorWallNow            = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	compressorModelContextLength = provider.GetModelContextLength
)

// Durable per session compaction state, the optional methods of the hermes session store the compressor reads
type (
	fallbackStreakStore interface {
		CompressionFallbackStreak(sessionID string) (int, error)
		SetCompressionFallbackStreak(sessionID string, streak int) error
	}
	ineffectiveCountStore interface {
		CompressionIneffectiveCount(sessionID string) (int, error)
		SetCompressionIneffectiveCount(sessionID string, count int) error
	}
	overloadStreakStore interface {
		CompressionOverloadStreak(sessionID string) (int, error)
		SetCompressionOverloadStreak(sessionID string, streak int) error
		IncrementCompressionOverloadStreak(sessionID string) (*int, error)
	}
	recoveryDeadlineStore interface {
		CompressionRecoveryDeadline(sessionID string) (float64, error)
		SetCompressionRecoveryDeadline(sessionID string, deadline float64) error
	}
	modelConfigStore  = provider.RejectedThinkingStore
	modelConfigReader interface {
		GetSessionModelConfigValue(ctx context.Context, sessionID, key string, def any) (any, error)
	}
	failureCooldownStore interface {
		CompressionCooldown(sessionID string) (state.CompressionCooldown, error)
		SetCompressionCooldown(sessionID string, until float64, reason string) error
		ClearCompressionCooldown(sessionID string) error
	}
)

var (
	_ fallbackStreakStore   = (*state.DB)(nil)
	_ ineffectiveCountStore = (*state.DB)(nil)
	_ overloadStreakStore   = (*state.DB)(nil)
	_ recoveryDeadlineStore = (*state.DB)(nil)
	_ modelConfigStore      = (*state.DB)(nil)
	_ modelConfigReader     = (*state.DB)(nil)
	_ failureCooldownStore  = (*state.DB)(nil)
)

// Session store as an untyped value, nil for a nil database pointer
func sessionStoreOf(db SessionStore) any {
	if store := sessionStoreOrNil(db); store != nil {
		return store
	}
	return nil
}

// Calls the named hermes getter on the store, ok false when the store lacks it
func callDurableGetter(store any, method, sessionID string, args []any) (any, bool, error) {
	switch method {
	case "get_compression_fallback_streak":
		if s, ok := store.(fallbackStreakStore); ok {
			v, err := s.CompressionFallbackStreak(sessionID)
			return v, true, err
		}
	case "get_compression_ineffective_count":
		if s, ok := store.(ineffectiveCountStore); ok {
			v, err := s.CompressionIneffectiveCount(sessionID)
			return v, true, err
		}
	case "get_compression_overload_streak":
		if s, ok := store.(overloadStreakStore); ok {
			v, err := s.CompressionOverloadStreak(sessionID)
			return v, true, err
		}
	case "increment_compression_overload_streak":
		if s, ok := store.(overloadStreakStore); ok {
			v, err := s.IncrementCompressionOverloadStreak(sessionID)
			if v == nil {
				return nil, true, err
			}
			return *v, true, err
		}
	case "get_compression_recovery_deadline":
		if s, ok := store.(recoveryDeadlineStore); ok {
			v, err := s.CompressionRecoveryDeadline(sessionID)
			return v, true, err
		}
	case "get_session_model_config_value":
		if s, ok := store.(modelConfigReader); ok {
			key, _ := args[0].(string)
			v, err := s.GetSessionModelConfigValue(context.Background(), sessionID, key, args[1])
			return v, true, err
		}
	default:
		panic("unknown durable getter " + method)
	}
	return nil, false, nil
}

// Calls the named hermes setter on the store, ok false when the store lacks it
func callDurableSetter(store any, method, sessionID string, args []any) (bool, error) {
	switch method {
	case "set_compression_fallback_streak":
		if s, ok := store.(fallbackStreakStore); ok {
			return true, s.SetCompressionFallbackStreak(sessionID, args[0].(int))
		}
	case "set_compression_ineffective_count":
		if s, ok := store.(ineffectiveCountStore); ok {
			return true, s.SetCompressionIneffectiveCount(sessionID, args[0].(int))
		}
	case "set_compression_overload_streak":
		if s, ok := store.(overloadStreakStore); ok {
			return true, s.SetCompressionOverloadStreak(sessionID, args[0].(int))
		}
	case "set_compression_recovery_deadline":
		if s, ok := store.(recoveryDeadlineStore); ok {
			return true, s.SetCompressionRecoveryDeadline(sessionID, args[0].(float64))
		}
	case "patch_session_model_config":
		if s, ok := store.(modelConfigStore); ok {
			return true, s.PatchModelConfig(sessionID, args[0].(map[string]any))
		}
	case "record_compression_failure_cooldown":
		if s, ok := store.(failureCooldownStore); ok {
			reason, _ := args[1].(string)
			return true, s.SetCompressionCooldown(sessionID, args[0].(float64), reason)
		}
	case "clear_compression_failure_cooldown":
		if s, ok := store.(failureCooldownStore); ok {
			return true, s.ClearCompressionCooldown(sessionID)
		}
	default:
		panic("unknown durable setter " + method)
	}
	return false, nil
}

// Suffix the debug line carries for a failure that is not a sqlite or coercion error
func durableErrSuffix(err error, coercion bool) string {
	var sqliteErr interface{ Code() int }
	if errors.As(err, &sqliteErr) || errors.Is(err, sql.ErrNoRows) || errors.Is(err, sql.ErrConnDone) {
		return ""
	}
	var pe *pyint.Error
	if coercion && errors.As(err, &pe) {
		return ""
	}
	return " (non-sqlite)"
}

// Python int or float coercion of a stored number, string or bool
func coerceDurable(stored any, asFloat bool) (any, bool, error) {
	switch v := stored.(type) {
	case int, int64, float64, string, bool:
		if asFloat {
			f, err := pyint.Float(v)
			return f, true, err
		}
		n, err := pyint.Of(v)
		if err != nil {
			return nil, true, err
		}
		if !n.IsInt64() {
			return nil, true, &pyint.Error{Msg: "int too large to convert"}
		}
		return int(n.Int64()), true, nil
	case *big.Int:
		if asFloat {
			f, _ := new(big.Float).SetInt(v).Float64()
			return f, true, nil
		}
		if !v.IsInt64() {
			return nil, true, &pyint.Error{Msg: "int too large to convert"}
		}
		return int(v.Int64()), true, nil
	}
	return nil, false, nil
}

// Reads a durable value, found false without a read, nil value for a non numeric row
func (c *ContextCompressor) durableRead(method, label string, asFloat bool, def any, store any, sessionID *string, args ...any) (bool, any) {
	if store == nil {
		store = c.sessionDB
	}
	sid := c.sessionID
	if sessionID != nil {
		sid = *sessionID
	}
	if sid == "" || store == nil {
		return false, def
	}
	stored, ok, err := callDurableGetter(store, method, sid, args)
	if !ok {
		return false, def
	}
	if err == nil {
		value, numeric, cerr := coerceDurable(stored, asFloat)
		if !numeric {
			return true, nil
		}
		if cerr == nil {
			if asFloat {
				return true, max(def.(float64), value.(float64))
			}
			return true, max(def.(int), value.(int))
		}
		err = cerr
	}
	slog.Debug(fmt.Sprintf("%s lookup failed%s: %v", label, durableErrSuffix(err, true), err))
	return false, def
}

// Writes a durable value on the bound row, true only when the write succeeded
func (c *ContextCompressor) durableWrite(method, label string, args ...any) bool {
	if c.sessionID == "" || c.sessionDB == nil {
		return false
	}
	ok, err := callDurableSetter(c.sessionDB, method, c.sessionID, args)
	if !ok {
		return false
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("%s persist failed%s: %v", label, durableErrSuffix(err, false), err))
		return false
	}
	return true
}

// Restores an int field from the bound row, a non numeric row resets it to the default
func (c *ContextCompressor) loadDurableInt(field *int, method, label string, args ...any) {
	if found, value := c.durableRead(method, label, false, 0, nil, nil, args...); found {
		if value == nil {
			*field = 0
		} else {
			*field = value.(int)
		}
	}
}

// Restores a float field from the bound row, a non numeric row resets it to zero
func (c *ContextCompressor) loadDurableFloat(field *float64, method, label string, args ...any) {
	if found, value := c.durableRead(method, label, true, 0.0, nil, nil, args...); found {
		if value == nil {
			*field = 0
		} else {
			*field = value.(float64)
		}
	}
}

func (c *ContextCompressor) loadFallbackCompressionStreak() {
	c.loadDurableInt(&c.fallbackCompressionStreak, "get_compression_fallback_streak", "compression fallback streak")
}

// Restores the cache boundary runway of a resumed durable session
func (c *ContextCompressor) loadProactivePruneRearmTokens() {
	c.loadDurableInt(&c.proactivePruneRearmTokens, "get_session_model_config_value", "proactive prune runway", ProactivePruneRearmModelConfigKey, 0)
}

// Removes the persisted prune runway key without touching the transcript
func (c *ContextCompressor) clearDurableProactivePruneRearm() {
	c.durableWrite("patch_session_model_config", "proactive prune runway clear", map[string]any{ProactivePruneRearmModelConfigKey: nil})
}

func (c *ContextCompressor) persistFallbackCompressionStreak() {
	c.durableWrite("set_compression_fallback_streak", "compression fallback streak", c.fallbackCompressionStreak)
}

// Restores the sustained overload budget so a rebuilt compressor inherits it
func (c *ContextCompressor) loadConsecutiveOverloadAborts() {
	c.loadDurableInt(&c.consecutiveOverloadAborts, "get_compression_overload_streak", "compression overload streak")
}

func (c *ContextCompressor) persistConsecutiveOverloadAborts() {
	c.durableWrite("set_compression_overload_streak", "compression overload streak", c.consecutiveOverloadAborts)
}

// Zeroes the overload budget and always writes the row since memory is not authoritative
func (c *ContextCompressor) resetConsecutiveOverloadAborts() {
	c.consecutiveOverloadAborts = 0
	c.persistConsecutiveOverloadAborts()
}

// Counts one overload abort, the atomically bumped row wins over memory when bound
func (c *ContextCompressor) incrementConsecutiveOverloadAborts() {
	found, streak := c.durableRead("increment_compression_overload_streak", "compression overload streak increment", false, 0, nil, nil)
	if n, ok := streak.(int); found && ok && n != 0 {
		c.consecutiveOverloadAborts = n
		return
	}
	c.consecutiveOverloadAborts++
}

// Loads the durable anti thrash strike count so a restart never disarms the guard
func (c *ContextCompressor) loadIneffectiveCompressionCount() {
	c.loadDurableInt(&c.ineffectiveCompressionCount, "get_compression_ineffective_count", "compression ineffective count")
}

func (c *ContextCompressor) persistIneffectiveCompressionCount() {
	c.durableWrite("set_compression_ineffective_count", "compression ineffective count", c.ineffectiveCompressionCount)
}

// Restores the durable wall clock recovery deadline, missing storage leaves it disarmed
func (c *ContextCompressor) loadAntiThrashRecoveryDeadline() {
	c.loadDurableFloat(&c.antiThrashRecoveryDeadline, "get_compression_recovery_deadline", "compression recovery deadline")
}

// Sets the recovery deadline and persists it only when it changed
func (c *ContextCompressor) setAntiThrashRecoveryDeadline(deadline float64) {
	if deadline == c.antiThrashRecoveryDeadline {
		return
	}
	c.antiThrashRecoveryDeadline = deadline
	c.durableWrite("set_compression_recovery_deadline", "compression recovery deadline", deadline)
}

// Sets the anti thrash strike counter and persists it only when it changed
func (c *ContextCompressor) recordIneffectiveCompressionVerdict(count int) {
	if count == c.ineffectiveCompressionCount {
		return
	}
	c.ineffectiveCompressionCount = count
	c.persistIneffectiveCompressionCount()
}

// Defers retries after a structural no-op without striking the anti thrash breaker
func (c *ContextCompressor) recordStructuralNoOp(reason string, nMessages int) {
	c.structuralNoOpBackoffUntil = compressorMonotonic() + structuralNoOpBackoffSeconds
	c.structuralNoOpLen = nMessages
	if !c.quietMode {
		slog.Warn(fmt.Sprintf("Compression skipped (%s): retrying in %.0fs (structural no-op backoff)", reason, structuralNoOpBackoffSeconds))
	}
}

// Strikes the breaker once for a pre commit rejection without arming real usage
func (c *ContextCompressor) RecordRejectedCompaction() {
	c.recordIneffectiveCompressionVerdict(c.ineffectiveCompressionCount + 1)
	if !c.quietMode {
		slog.Warn(fmt.Sprintf("Compaction rejected before commit (would grow the transcript); ineffective_compression_count=%d", c.ineffectiveCompressionCount))
	}
}

// Records one completed boundary, a feasibility skip leaves the fallback streak untouched
func (c *ContextCompressor) recordCompletedCompaction(usedFallback, feasibilitySkip bool) {
	c.structuralNoOpBackoffUntil = 0
	c.verifyCompactionClearedThreshold = true
	if feasibilitySkip {
		if !c.quietMode {
			slog.Info(fmt.Sprintf("Compaction completed via pre-LLM summary skip; fallback_compression_streak unchanged (%d)", c.fallbackCompressionStreak))
		}
		return
	}
	if usedFallback {
		c.fallbackCompressionStreak++
		if !c.quietMode {
			slog.Warn(fmt.Sprintf("Compaction completed with a deterministic fallback summary. fallback_compression_streak=%d", c.fallbackCompressionStreak))
		}
	} else if c.fallbackCompressionStreak != 0 {
		c.fallbackCompressionStreak = 0
	}
	c.persistFallbackCompressionStreak()
	c.resetConsecutiveOverloadAborts()
}

// Live compression failure cooldown of the bound session, nil when none is active
func (c *ContextCompressor) GetActiveCompressionFailureCooldown(refresh bool) map[string]any {
	if refresh {
		c.lastCooldownRefreshWasAuthoritative = nil
	}
	nowMono := compressorMonotonic()
	var localState map[string]any
	localRemaining := c.summaryFailureCooldownUntil - nowMono
	if localRemaining > 0 {
		localState = map[string]any{"cooldown_until": compressorWallNow() + localRemaining, "remaining_seconds": localRemaining, "error": optStr(c.lastSummaryError)}
		if !refresh {
			return localState
		}
	}
	store, ok := c.sessionDB.(failureCooldownStore)
	if c.sessionID == "" || !ok {
		return localState
	}
	row, err := store.CompressionCooldown(c.sessionID)
	if err != nil {
		if refresh {
			c.lastCooldownRefreshWasAuthoritative = boolPtr(false)
		}
		if durableErrSuffix(err, false) == "" {
			slog.Debug(fmt.Sprintf("compression failure cooldown lookup failed: %v", err))
		}
		return localState
	}
	if refresh {
		c.lastCooldownRefreshWasAuthoritative = boolPtr(true)
	}
	remaining := 0.0
	if row.Until > 0 {
		remaining = row.Until - compressorWallNow()
	}
	if remaining <= 0 {
		if refresh && localState != nil && c.cooldownPersistFailed {
			return localState
		}
		if refresh {
			c.summaryFailureCooldownUntil, c.lastSummaryError = 0, nil
		}
		return nil
	}
	if isHygienePreagentOnlyCooldown(row.Error) {
		c.summaryFailureCooldownUntil, c.lastSummaryError = 0, nil
		return nil
	}
	c.summaryFailureCooldownUntil = nowMono + remaining
	if row.Error == "" {
		c.lastSummaryError = nil
	} else {
		errText := row.Error
		c.lastSummaryError = &errText
	}
	c.cooldownPersistFailed = false
	return map[string]any{"cooldown_until": row.Until, "remaining_seconds": remaining, "error": optStr(c.lastSummaryError)}
}

func boolPtr(b bool) *bool { return &b }

// Python None for a nil string pointer, the string otherwise
func optStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Extends the local cooldown to at least the given seconds and persists the wall clock deadline
func (c *ContextCompressor) recordCompressionFailureCooldown(cooldownSeconds float64, errText *string) {
	c.summaryFailureCooldownUntil = max(c.summaryFailureCooldownUntil, compressorMonotonic()+cooldownSeconds)
	c.lastSummaryError = errText
	cooldownUntil := compressorWallNow() + max(0.0, c.summaryFailureCooldownUntil-compressorMonotonic())
	if c.sessionDB == nil || c.sessionID == "" {
		return
	}
	reason := ""
	if errText != nil {
		reason = *errText
	}
	c.cooldownPersistFailed = !c.durableWrite("record_compression_failure_cooldown", "compression failure cooldown", cooldownUntil, reason)
}

// Records a timeout or stall cooldown on the ladder, never shorter than the idle stall window
func (c *ContextCompressor) RecordTimeoutFailure(errText, failureKind string) {
	if failureKind == "" {
		failureKind = "timeout"
	}
	mode := c.tailMode
	if mode == "" {
		mode = "unknown"
	}
	stamped := "backoff:" + failureKind + ":strategy=" + mode + ": " + errText
	seconds := float64(nextTimeoutCooldown(&c.consecutiveTimeoutFailures))
	func() {
		defer func() { _ = recover() }()
		idle, _ := ResolveContextCompressionTimeouts(c.ctx, nil)
		seconds = max(seconds, idle)
	}()
	c.recordCompressionFailureCooldown(seconds, &stamped)
}

// Clears the cooldown unless the host already cancelled this compression attempt
func (c *ContextCompressor) clearCompressionFailureCooldown() {
	if c.compressionCancelled() {
		slog.Info("Skipping compression cooldown clear: host already cancelled this compression attempt")
		return
	}
	c.summaryFailureCooldownUntil, c.lastSummaryError = 0, nil
	c.consecutiveTimeoutFailures, c.consecutiveTruncationFailures = 0, 0
	c.cooldownPersistFailed = false
	c.durableWrite("clear_compression_failure_cooldown", "compression failure cooldown clear")
}

// Reads the host owned cooperative cancellation signal, false when none is installed or it panics
func (c *ContextCompressor) compressionCancelled() (cancelled bool) {
	if c.compressionCancelledCheck == nil {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("compression cancellation check failed: %v", r))
			cancelled = false
		}
	}()
	return c.compressionCancelledCheck()
}

// Base percent, effective percent and trigger tokens for a model and window without mutating state
func (c *ContextCompressor) deriveTrigger(model string, contextLength int, providerName string) (float64, float64, int) {
	cfg := 0.0
	if c.configThresholdPercent != nil {
		cfg = *c.configThresholdPercent
	}
	base := resolveModelThreshold(model, c.ModelThresholds, cfg, providerName)
	effective := effectiveThresholdPercent(contextLength, base)
	threshold := computeThresholdTokens(contextLength, effective, c.maxTokens)
	if limit := c.effectiveThresholdCap(contextLength); limit != nil {
		threshold = min(threshold, *limit)
	}
	return base, effective, threshold
}

// Configured threshold cap clamped to the window, nil when no cap is configured
func (c *ContextCompressor) effectiveThresholdCap(contextLength int) *int {
	if c.thresholdTokensCap == nil || *c.thresholdTokensCap <= 0 {
		return nil
	}
	v := min(*c.thresholdTokensCap, contextLength)
	return &v
}

// Trigger the model update would install, state untouched
func (c *ContextCompressor) PreviewThresholdTokens(model string, contextLength int, providerName string) int {
	_, _, threshold := c.deriveTrigger(model, contextLength, providerName)
	return threshold
}

func (c *ContextCompressor) updateModel(model string, contextLength int, baseURL, apiKey, providerName, apiMode string) {
	c.UpdateModelWithMaxTokens(model, contextLength, baseURL, apiKey, providerName, apiMode, nil)
}

// Rebinds the runtime after a model switch, a changed runtime clears streaks, cooldowns and the aux ceiling
func (c *ContextCompressor) UpdateModelWithMaxTokens(model string, contextLength int, baseURL, apiKey, providerName, apiMode string, maxTokens any) {
	runtimeChanged := model != c.model || providerName != c.provider || baseURL != c.baseURL || apiMode != c.apiMode
	c.model, c.baseURL, c.apiKey, c.provider, c.apiMode = model, baseURL, apiKey, providerName, apiMode
	c.SetContextLength(contextLength)
	if maxTokens != nil {
		c.maxTokens = coerceMaxTokens(maxTokens)
	}
	if runtimeChanged {
		c.auxContextCeiling = nil
	}
	base, effective, threshold := c.deriveTrigger(model, contextLength, providerName)
	c.BaseThresholdPercent, c.ThresholdPercent = base, effective
	c.setThresholdTokens(threshold)
	c.applyThresholdTokensCap()
	c.tailTokenBudget = nil
	_ = c.TailTokenBudget()
	c.setMaxSummaryTokens(min(int(float64(contextLength)*0.05), summaryTokensCeiling))
	c.LastPromptTokens, c.LastCompletionTokens, c.LastTotalTokens = 0, 0, 0
	c.resetRealUsagePairing()
	c.recordIneffectiveCompressionVerdict(0)
	c.prellmSkipCount = 0
	if runtimeChanged {
		c.fallbackCompressionStreak = 0
		c.persistFallbackCompressionStreak()
		c.clearCompressionFailureCooldown()
		c.resetConsecutiveOverloadAborts()
	}
	c.verifyCompactionClearedThreshold, c.lastCompressionMadeProgress = false, false
	c.resetProactivePruneRearm()
	c.clearDurableProactivePruneRearm()
}

// Clamps the trigger to the configured cap and to the auxiliary summariser window
func (c *ContextCompressor) applyThresholdTokensCap() {
	if limit := c.effectiveThresholdCap(c.contextLength()); limit != nil && *limit < c.thresholdTokensValue() {
		c.setThresholdTokens(*limit)
	}
	if c.auxContextCeiling != nil && *c.auxContextCeiling > 0 && *c.auxContextCeiling < c.thresholdTokensValue() {
		c.setThresholdTokens(*c.auxContextCeiling)
	}
}

// Raise only small window floor, models under 512K trigger at no less than 75 percent
func effectiveThresholdPercent(contextLength int, thresholdPercent float64) float64 {
	if contextLength != 0 && contextLength < smallCtxWindowLimit {
		return max(thresholdPercent, smallCtxThresholdPercent)
	}
	return thresholdPercent
}

// Window minus the output reservation, or the whole window when the reservation leaves nothing
func effectiveInputWindow(contextLength int, maxTokens *int) int {
	reserve := 0
	if maxTokens != nil {
		reserve = *maxTokens
	}
	if w := contextLength - reserve; w > 0 {
		return w
	}
	return contextLength
}

// Trigger tokens from the input budget, floored at the minimum window and capped so small windows fire
func computeThresholdTokens(contextLength int, thresholdPercent float64, maxTokens *int) int {
	window := effectiveInputWindow(contextLength, maxTokens)
	pctValue := int(float64(window) * thresholdPercent)
	floored := max(pctValue, provider.MinimumContextLength)
	triggerCap := int(float64(window) * minCtxTriggerRatio)
	if window > 0 && floored > pctValue && floored > triggerCap {
		floored = max(pctValue, triggerCap)
	}
	if window > 0 && floored >= window {
		return max(1, min(triggerCap, window-1))
	}
	return floored
}

// Resolves and caches the model window on first access, flooring the percent before the trigger derives
func (c *ContextCompressor) contextLength() int {
	if c.resolvedContextLength == nil {
		cfg := 0
		if c.configContextLength != nil {
			cfg = *c.configContextLength
		}
		var custom []map[string]any
		for _, entry := range c.customProviders {
			if m, ok := entry.(map[string]any); ok {
				custom = append(custom, m)
			}
		}
		n := compressorModelContextLength(c.ctx, provider.ContextLengthRequest{Model: c.model, BaseURL: c.baseURL, APIKey: c.apiKey, Provider: c.provider, ConfigContextLength: cfg, CustomProviders: custom})
		c.resolvedContextLength = &n
		c.ContextEngineBase.ContextLength = n
		c.ThresholdPercent = effectiveThresholdPercent(n, c.BaseThresholdPercent)
		c.emitInitSummaryOnce()
	}
	return *c.resolvedContextLength
}

// Model window, resolving it on first access
func (c *ContextCompressor) ContextLength() int { return c.contextLength() }

// Compaction trigger in tokens, derived from the resolved window on first access
func (c *ContextCompressor) thresholdTokensValue() int {
	if c.thresholdTokens == nil {
		window := c.contextLength()
		v := computeThresholdTokens(window, c.ThresholdPercent, c.maxTokens)
		c.setThresholdTokens(v)
		c.applyThresholdTokensCap()
	}
	return *c.thresholdTokens
}

// Compaction trigger in tokens
func (c *ContextCompressor) ThresholdTokens() int { return c.thresholdTokensValue() }

func (c *ContextCompressor) setThresholdTokens(v int) {
	c.thresholdTokens = &v
	c.ContextEngineBase.ThresholdTokens = v
}

// Overrides the compaction trigger
func (c *ContextCompressor) SetThresholdTokens(v int) { c.setThresholdTokens(v) }

// Protected tail budget, a clamped recency window in lean mode and a trigger ratio otherwise
func (c *ContextCompressor) TailTokenBudget() int {
	if c.tailTokenBudget == nil {
		var budget int
		if c.tailMode == "lean" {
			budget = LeanTailBudget(c.contextLength(), c.thresholdTokensValue())
		} else {
			budget = int(float64(c.thresholdTokensValue()) * c.summaryTargetRatio)
		}
		if c.contextLength() > 0 {
			budget = min(budget, int(float64(c.contextLength())*tailMaxContextFraction))
		}
		v := max(1, budget)
		c.tailTokenBudget = &v
	}
	return *c.tailTokenBudget
}

// Overrides the protected tail budget
func (c *ContextCompressor) SetTailTokenBudget(v int) { c.tailTokenBudget = &v }

// Summary output budget, five percent of the window under the ceiling
func (c *ContextCompressor) maxSummaryTokensValue() int {
	if c.maxSummaryTokens == nil {
		c.setMaxSummaryTokens(min(int(float64(c.contextLength())*0.05), summaryTokensCeiling))
	}
	return *c.maxSummaryTokens
}

func (c *ContextCompressor) setMaxSummaryTokens(v int) { c.maxSummaryTokens = &v }

// Logs the init line once, on the first window resolution
func (c *ContextCompressor) emitInitSummaryOnce() {
	if !c.logInitSummary {
		return
	}
	c.logInitSummary = false
	slog.Info(fmt.Sprintf("Context compressor initialized: model=%s context_length=%d threshold=%d (%.0f%%) target_ratio=%.0f%% tail_budget=%d provider=%s base_url=%s",
		c.model, *c.resolvedContextLength, c.thresholdTokensValue(), c.ThresholdPercent*100, c.summaryTargetRatio*100, c.TailTokenBudget(), orNoneText(c.provider), orNoneText(c.baseURL)))
}

func orNoneText(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// Usage summary for display over the lazily resolved window and trigger
func (c *ContextCompressor) GetStatus() map[string]any {
	c.thresholdTokensValue()
	return c.ContextEngineBase.GetStatus()
}

// Clears every per session compaction field at a real session end
func (c *ContextCompressor) onSessionEnd(sessionID string, messages []map[string]any) {
	c.resetSessionCompactionState(c.ctx)
}

// Binds the session row so durable cooldowns and streaks round trip
func (c *ContextCompressor) bindSessionState(store any, sessionID string) {
	if c.preparedCompaction != nil && sessionID != c.sessionID {
		c.preparedCompaction.Discard(c.ctx, "session rebind", "session_boundary")
	}
	c.sessionDB = store
	c.sessionID = sessionID
	c.summaryFailureCooldownUntil = 0
	c.cooldownPersistFailed = false
	c.lastSummaryError = nil
	c.consecutiveTimeoutFailures, c.consecutiveTruncationFailures, c.fallbackCompressionStreak = 0, 0, 0
	c.consecutiveOverloadAborts = 0
	c.ineffectiveCompressionCount, c.prellmSkipCount = 0, 0
	c.antiThrashRecoveryDeadline, c.structuralNoOpBackoffUntil, c.fallbackProbeAt = 0, 0, 0
	c.resetProactivePruneRearm()
	c.GetActiveCompressionFailureCooldown(false)
	c.loadFallbackCompressionStreak()
	c.loadIneffectiveCompressionCount()
	c.loadAntiThrashRecoveryDeadline()
	c.loadFallbackProbeAt()
	c.loadConsecutiveOverloadAborts()
	c.loadProactivePruneRearmTokens()
}

// Binds session scoped state, carrying parent streaks and strikes across a compression rotation
func (c *ContextCompressor) onSessionStart(sessionID string, kwargs map[string]any) {
	boundaryReason := kwargs["boundary_reason"]
	oldSessionID, _ := kwargs["old_session_id"].(string)
	store := c.sessionDB
	if v, ok := kwargs["session_db"]; ok {
		store = v
		if db, isDB := v.(*state.DB); isDB {
			store = sessionStoreOf(db)
		}
	}
	prevStreak, prevIneffective, prevOverload, prevProbe := c.fallbackCompressionStreak, c.ineffectiveCompressionCount, c.consecutiveOverloadAborts, c.fallbackProbeAt
	if boundaryReason == "compression" && oldSessionID != "" {
		parent := func(method, label string, current int) int {
			found, value := c.durableRead(method, label, false, 0, store, &oldSessionID)
			if n, ok := value.(int); found && ok {
				return n
			}
			return current
		}
		prevStreak = parent("get_compression_fallback_streak", "compression parent fallback streak", prevStreak)
		prevIneffective = parent("get_compression_ineffective_count", "compression parent ineffective count", prevIneffective)
		prevOverload = parent("get_compression_overload_streak", "compression parent overload streak", prevOverload)
	}
	c.bindSessionState(store, sessionID)
	if boundaryReason == "compression" {
		c.fallbackCompressionStreak = prevStreak
		c.setFallbackProbeAt(prevProbe)
		if c.ineffectiveCompressionCount != prevIneffective {
			c.ineffectiveCompressionCount = prevIneffective
			c.persistIneffectiveCompressionCount()
		}
		if c.consecutiveOverloadAborts != prevOverload {
			c.consecutiveOverloadAborts = prevOverload
			c.persistConsecutiveOverloadAborts()
		}
	}
}

// Sets the summary model probe deadline, persisting on change so a rebuilt compressor resumes the bench
func (c *ContextCompressor) setFallbackProbeAt(at float64) {
	if at == c.fallbackProbeAt {
		return
	}
	c.fallbackProbeAt = at
	var stored any
	if at != 0 {
		stored = at
	}
	c.durableWrite("patch_session_model_config", "summary-model probe deadline", map[string]any{fallbackProbeAtModelConfigKey: stored})
}

func (c *ContextCompressor) loadFallbackProbeAt() {
	c.loadDurableFloat(&c.fallbackProbeAt, "get_session_model_config_value", "summary-model probe deadline", fallbackProbeAtModelConfigKey, 0.0)
}

// Skips the summary model while two summaries in a row fell back, one probe per recovery window
func (c *ContextCompressor) fallbackStreakSkip(telemetry *pyjson.Object) bool {
	if c.fallbackCompressionStreak < 2 {
		c.setFallbackProbeAt(0)
		return false
	}
	now := compressorWallNow()
	if c.fallbackProbeAt != 0 && now >= c.fallbackProbeAt {
		c.setFallbackProbeAt(0)
		if !c.quietMode {
			slog.Info(fmt.Sprintf("Compression: probing the summary model again after %d fallback summaries in a row", c.fallbackCompressionStreak))
		}
		return false
	}
	if c.fallbackProbeAt == 0 || c.fallbackProbeAt-now > antiThrashRecoverySeconds {
		c.setFallbackProbeAt(now + antiThrashRecoverySeconds)
	}
	c.lastFeasibilitySkip = true
	telemetry.Set("failure_class", "summary_model_benched")
	if !c.quietMode {
		slog.Warn(fmt.Sprintf("Compression: %d fallback summaries in a row — skipping LLM summarization, proceeding with deterministic message dropping. Next summary-model probe in %.0fs.",
			c.fallbackCompressionStreak, max(0.0, c.fallbackProbeAt-now)))
	}
	return true
}

// Skips the summary model after a real usage strike when the middle is tiny against the trigger
func (c *ContextCompressor) feasibilitySkip(telemetry *pyjson.Object, turnsToSummarize []map[string]any, compressStart, compressEnd int) bool {
	if c.ineffectiveCompressionCount < 1 {
		return false
	}
	var middle int
	if v, ok := telemetry.Get("middle_window_tokens"); ok && v != nil {
		middle = telemetryInt(telemetry, "middle_window_tokens")
	} else {
		middle = estimateRowsRough(c.ctx, turnsToSummarize)
	}
	if middle >= int(float64(c.thresholdTokensValue())*feasibilitySkipMiddleFraction) {
		return false
	}
	c.lastFeasibilitySkip = true
	c.prellmSkipCount++
	telemetry.Set("prellm_skip_count", c.prellmSkipCount)
	if !c.quietMode {
		slog.Warn(fmt.Sprintf("Compression: middle section (%d tokens at indices %d-%d) is below %.0f%% of threshold (%d tokens) — skipping LLM summarization, proceeding with deterministic message dropping. prellm_skip_count=%d",
			middle, compressStart, compressEnd, feasibilitySkipMiddleFraction*100, c.thresholdTokensValue(), c.prellmSkipCount))
	}
	return true
}
