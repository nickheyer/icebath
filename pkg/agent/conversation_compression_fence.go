package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	CompactionStatusMarker    = "Compacting context"
	CompactionStatus          = "🗜️ " + CompactionStatusMarker + " — summarizing earlier conversation so I can continue..."
	CompactionHeartbeatStatus = "🗜️ " + CompactionStatusMarker + " — still summarizing earlier conversation so I can continue..."
	CompactionDoneStatus      = "✓ Context compaction complete — continuing turn..."
)

var compressionLog = func() *slog.Logger { return slog.Default().With("logger", "agent.conversation_compression") }

// Runs fn and logs a returned error or panic at debug instead of propagating it
func swallowCompression(message string, excInfo bool, fn func() error) {
	defer func() {
		if r := recover(); r != nil {
			compressionLog().Debug(message, "error", fmt.Sprint(r), "exc_info", excInfo)
		}
	}()
	if err := fn(); err != nil {
		compressionLog().Debug(message, "error", err.Error(), "exc_info", excInfo)
	}
}

// Copies rows without the persisted marker so an identical no op copy never reads as progress
func stripMarkerForComparison(msgs []map[string]any) []map[string]any {
	out := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		if m == nil {
			continue
		}
		clone := make(map[string]any, len(m))
		for k, v := range m {
			if k != state.DBPersistedMarker {
				clone[k] = v
			}
		}
		out[i] = clone
	}
	return out
}

// Emits the compacted terminal edge on the status callback of a started compaction
func emitCompactionDone(a *AIAgent) {
	if a == nil || a.statusCallback == nil {
		return
	}
	swallowCompression("status_callback error in compaction completion", true, func() error {
		return callStatusCallback(a.statusCallback, "compacted", CompactionDoneStatus)
	})
}

// Invokes a two argument status callback of any supported Go shape
func callStatusCallback(cb any, kind, text string) error {
	switch f := cb.(type) {
	case func(string, string):
		f(kind, text)
	case func(string, string) error:
		return f(kind, text)
	case func(string, any):
		f(kind, text)
	case func(...any):
		f(kind, text)
	default:
		return fmt.Errorf("status_callback of type %T is not callable with (event, message)", cb)
	}
	return nil
}

func formatThousandsAny(v any) string {
	switch n := v.(type) {
	case int:
		return groupThousands(n)
	case int64:
		return groupThousands(int(n))
	case float64:
		return groupThousands(int(math.Round(n)))
	}
	return fmt.Sprint(v)
}

// Routine compression status lines, formatted from the same wording the emission sites use
func PreAPICompressionStatus(tokens int) string {
	return "📦 Pre-API compression: ~" + groupThousands(tokens) + " tokens near the context/output limit. Compacting before the next model call."
}

func PreflightCompressionStatus(tokens, threshold int) string {
	return "📦 Preflight compression: ~" + groupThousands(tokens) + " tokens >= " + groupThousands(threshold) + " threshold. This may take a moment."
}

func IdleCompactionStatus(idleSeconds any, tokens int) string {
	return fmt.Sprintf("💤 Resumed after %vs idle — compacting ~%s tokens before continuing.", idleSeconds, groupThousands(tokens))
}

func CompressionRetryTooLargeStatus(tokens, attempt, limit int) string {
	return fmt.Sprintf("🗜️ Context too large (~%s tokens) — compressing (%d/%d)...", groupThousands(tokens), attempt, limit)
}

func CompressionRetryMessagesStatus(before, after int) string {
	return fmt.Sprintf("🗜️ Compressed %d → %d messages, retrying...", before, after)
}

func CompressionRetryTokensStatus(before, after int) string {
	return "🗜️ Compressed ~" + groupThousands(before) + " → ~" + groupThousands(after) + " tokens, retrying..."
}

func CompressionRetryContextReducedStatus(newCtx, oldCtx int) string {
	return "🗜️ Context reduced to " + groupThousands(newCtx) + " tokens (was " + groupThousands(oldCtx) + "), retrying..."
}

// Failure class notice for an over threshold session whose compression is blocked, never routine
func ContextOverflowBlockedWarning(tokens, threshold any, reason string) string {
	return "⚠ Context is over the compression threshold (~" + formatThousandsAny(tokens) + " tokens >= " + formatThousandsAny(threshold) + ") " +
		"but compression is currently blocked (" + reason + "). The model may stop responding. Run /new to start a fresh " +
		"session or /compress to retry immediately."
}

// Every routine compression status line in its emitted wording for the gateway noise filter
func RoutineCompressionStatusSamples() []string {
	return []string{CompactionStatus, CompactionHeartbeatStatus, CompactionDoneStatus, PreAPICompressionStatus(123456),
		PreflightCompressionStatus(120000, 100000), IdleCompactionStatus(3600, 120000), CompressionRetryTooLargeStatus(250000, 1, 3),
		CompressionRetryMessagesStatus(30, 12), CompressionRetryTokensStatus(250000, 120000), CompressionRetryContextReducedStatus(120000, 250000)}
}

// True for in progress auto compaction lifecycle lines, false for the done edge and the blocked notice
func IsCompactionProgressStatus(text any) bool {
	s, _ := text.(string)
	body := strings.TrimSpace(s)
	if body == "" {
		return false
	}
	if strings.Contains(body, CompactionStatusMarker) {
		return true
	}
	if body == CompactionDoneStatus {
		return false
	}
	lowered := strings.ToLower(body)
	if strings.Contains(lowered, "compaction complete") || strings.Contains(lowered, "compression is currently blocked") {
		return false
	}
	return strings.Contains(lowered, "compact") || strings.Contains(lowered, "compress") || strings.Contains(lowered, "context reduced to")
}

// Rebuilds the agent tools at the compaction commit boundary, true when tools were added
func refreshAgentToolDefinitions(ctx context.Context, a *AIAgent) bool {
	added := mcp.RefreshAgentMCPTools(ctx, a, mcp.RefreshOptions{ContentAware: true})
	if len(added) > 0 {
		names := make([]string, 0, len(added))
		for n := range added {
			names = append(names, n)
		}
		sort.Strings(names)
		compressionLog().Info(fmt.Sprintf("Compaction tool refresh added tools: %v", pyListRepr(names)))
	}
	return len(added) > 0
}

func pyListRepr(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "'" + n + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

var compressorCooldownStateFields = []string{"_summary_failure_cooldown_until", "_last_summary_error", "_cooldown_persist_failed"}

// Engine whose per attempt bookkeeping can be snapshotted and restored by its hermes attribute names
type AttemptStateSnapshotter interface {
	SnapshotAttemptState() map[string]any
	RestoreAttemptState(values map[string]any)
}

var _ AttemptStateSnapshotter = (*ContextCompressor)(nil)

// Deep copy of the attempt owned bookkeeping fields, telemetry aliases preserved by one copy of the set
func (c *ContextCompressor) SnapshotAttemptState() map[string]any {
	values := map[string]any{
		"_previous_summary": c.previousSummary, "_summary_has_user_turn": c.summaryHasUserTurn, "compression_count": c.Base().CompressionCount,
		"_last_compression_savings_pct": c.lastCompressionSavingsPct, "_ineffective_compression_count": c.ineffectiveCompressionCount,
		"_anti_thrash_recovery_deadline": c.antiThrashRecoveryDeadline, "_fallback_compression_streak": c.fallbackCompressionStreak,
		"_verify_compaction_cleared_threshold": c.verifyCompactionClearedThreshold, "_last_compression_made_progress": c.lastCompressionMadeProgress,
		"_summary_failure_cooldown_until": c.summaryFailureCooldownUntil, "_cooldown_persist_failed": c.cooldownPersistFailed,
		"_last_summary_error": c.lastSummaryError, "_consecutive_timeout_failures": c.consecutiveTimeoutFailures,
		"_consecutive_truncation_failures": c.consecutiveTruncationFailures, "_last_summary_dropped_count": c.LastSummaryDroppedCount,
		"_last_summary_fallback_used": c.LastSummaryFallbackUsed, "_last_compress_aborted": c.lastCompressAborted,
		"_last_summary_auth_failure": c.lastSummaryAuthFailure, "_last_summary_network_failure": c.lastSummaryNetworkFailure,
		"_last_summary_empty_content_failure": c.lastSummaryEmptyContentFailure, "_last_summary_truncated_failure": c.lastSummaryTruncatedFailure,
		"_last_summary_overload_failure": c.lastSummaryOverloadFailure, "_consecutive_overload_aborts": c.consecutiveOverloadAborts,
		"_last_summary_overload_degraded": c.lastSummaryOverloadDegraded, "_last_aux_model_failure_error": c.LastAuxModelFailureError,
		"_last_aux_model_failure_model": c.LastAuxModelFailureModel, "_last_aux_resolved_model": c.lastAuxResolvedModel,
		"_summary_model_fallen_back": c.summaryModelFallenBack, "summary_model": c.summaryModel,
		"_last_compression_telemetry": c.Last, "_active_compression_telemetry": c.Active, "_compression_telemetry_seed": c.Seed,
		"_proactive_prune_rearm_tokens": c.proactivePruneRearmTokens,
	}
	return pycopy.DeepCopy(values).(map[string]any)
}

// Writes back every snapshotted field present in values
func (c *ContextCompressor) RestoreAttemptState(values map[string]any) {
	for name, v := range values {
		c.setAttemptStateField(name, v)
	}
}

func (c *ContextCompressor) setAttemptStateField(name string, v any) {
	switch name {
	case "_previous_summary":
		c.previousSummary, _ = v.(*string)
	case "_summary_has_user_turn":
		c.summaryHasUserTurn, _ = v.(*bool)
	case "compression_count":
		c.Base().CompressionCount, _ = v.(int)
	case "_last_compression_savings_pct":
		c.lastCompressionSavingsPct, _ = v.(float64)
	case "_ineffective_compression_count":
		c.ineffectiveCompressionCount, _ = v.(int)
	case "_anti_thrash_recovery_deadline":
		c.antiThrashRecoveryDeadline, _ = v.(float64)
	case "_fallback_compression_streak":
		c.fallbackCompressionStreak, _ = v.(int)
	case "_verify_compaction_cleared_threshold":
		c.verifyCompactionClearedThreshold, _ = v.(bool)
	case "_last_compression_made_progress":
		c.lastCompressionMadeProgress, _ = v.(bool)
	case "_summary_failure_cooldown_until":
		c.summaryFailureCooldownUntil, _ = v.(float64)
	case "_cooldown_persist_failed":
		c.cooldownPersistFailed, _ = v.(bool)
	case "_last_summary_error":
		c.lastSummaryError, _ = v.(*string)
	case "_consecutive_timeout_failures":
		c.consecutiveTimeoutFailures, _ = v.(int)
	case "_consecutive_truncation_failures":
		c.consecutiveTruncationFailures, _ = v.(int)
	case "_last_summary_dropped_count":
		c.LastSummaryDroppedCount, _ = v.(int)
	case "_last_summary_fallback_used":
		c.LastSummaryFallbackUsed, _ = v.(bool)
	case "_last_compress_aborted":
		c.lastCompressAborted, _ = v.(bool)
	case "_last_summary_auth_failure":
		c.lastSummaryAuthFailure, _ = v.(bool)
	case "_last_summary_network_failure":
		c.lastSummaryNetworkFailure, _ = v.(bool)
	case "_last_summary_empty_content_failure":
		c.lastSummaryEmptyContentFailure, _ = v.(bool)
	case "_last_summary_truncated_failure":
		c.lastSummaryTruncatedFailure, _ = v.(bool)
	case "_last_summary_overload_failure":
		c.lastSummaryOverloadFailure, _ = v.(bool)
	case "_consecutive_overload_aborts":
		c.consecutiveOverloadAborts, _ = v.(int)
	case "_last_summary_overload_degraded":
		c.lastSummaryOverloadDegraded, _ = v.(bool)
	case "_last_aux_model_failure_error":
		c.LastAuxModelFailureError, _ = v.(string)
	case "_last_aux_model_failure_model":
		c.LastAuxModelFailureModel, _ = v.(string)
	case "_last_aux_resolved_model":
		c.lastAuxResolvedModel, _ = v.(string)
	case "_summary_model_fallen_back":
		c.summaryModelFallenBack, _ = v.(bool)
	case "summary_model":
		c.summaryModel, _ = v.(string)
	case "_last_compression_telemetry":
		c.Last = asPyObject(v)
	case "_active_compression_telemetry":
		c.Active = asPyObject(v)
	case "_compression_telemetry_seed":
		c.Seed = asPyObject(v)
	case "_proactive_prune_rearm_tokens":
		c.proactivePruneRearmTokens, _ = v.(int)
	}
}

// Copy of only the attempt owned bookkeeping, empty for an engine that exposes none
func snapshotCompressorAttemptState(compressor any) map[string]any {
	if s, ok := compressor.(AttemptStateSnapshotter); ok && !isNilEngine(compressor) {
		return s.SnapshotAttemptState()
	}
	return map[string]any{}
}

type cooldownRowDB interface {
	GetCompressionFailureCooldownRow(sessionID string) (state.CompressionCooldownRow, error)
	RestoreCompressionFailureCooldownRow(sessionID string, snapshot state.CompressionCooldownRow) error
	RecordCompressionFailureCooldown(sessionID string, cooldownUntil float64, errValue *string)
	ClearCompressionFailureCooldown(sessionID string)
}

// Recreates or clears the durable cooldown row from the attempt snapshot before the memory restore
func rollbackDurableCooldown(compressor any, snapshot map[string]any, authoritative *bool, durableState *state.CompressionCooldownRow) error {
	c, ok := compressor.(*ContextCompressor)
	if !ok || c == nil || c.sessionDB == nil || c.sessionID == "" {
		return nil
	}
	db, _ := c.sessionDB.(cooldownRowDB)
	if authoritative != nil && *authoritative {
		if db == nil || durableState == nil {
			return errors.New("exact compression cooldown rollback API is unavailable")
		}
		return db.RestoreCompressionFailureCooldownRow(c.sessionID, *durableState)
	}
	swallowCompression("compression cooldown persistence rollback failed", true, func() error {
		deadline, _ := snapshot["_summary_failure_cooldown_until"].(float64)
		remaining := math.Max(0, deadline-pythread.Monotonic())
		if db == nil {
			return nil
		}
		if remaining > 0 {
			errText, _ := snapshot["_last_summary_error"].(*string)
			db.RecordCompressionFailureCooldown(c.sessionID, pyTimeNow()+remaining, errText)
		} else {
			db.ClearCompressionFailureCooldown(c.sessionID)
		}
		return nil
	})
	return nil
}

func pyTimeNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Restores the per attempt snapshot after a pre commit hard cancel unless a newer attempt owns the compressor
func restoreCompressorAttemptState(compressor any, snapshot map[string]any, durableAuthoritative *bool, durableState *state.CompressionCooldownRow, attemptGeneration *int) error {
	if attemptGeneration != nil && !compressorAttemptIsCurrent(compressor, *attemptGeneration) {
		compressionLog().Warn(fmt.Sprintf("Skipping stale compressor attempt-state restore: attempt generation %d no longer owns the compressor (current: %d). A newer (stall-fallback) attempt's state is preserved.",
			*attemptGeneration, currentAttemptGeneration(compressor)))
		return nil
	}
	restored := pycopy.DeepCopy(snapshot).(map[string]any)
	if lock := compressorAttemptSerialLock(compressor); lock != nil {
		lock.Lock()
		defer lock.Unlock()
	}
	if attemptGeneration != nil && *attemptGeneration != 0 && currentAttemptGeneration(compressor) != *attemptGeneration {
		compressionLog().Warn(fmt.Sprintf("Skipping stale compressor attempt-state restore at write time: attempt generation %d lost the compressor mid-restore.", *attemptGeneration))
		return nil
	}
	if _, has := snapshot["_summary_failure_cooldown_until"]; has && !(durableAuthoritative != nil && !*durableAuthoritative) {
		persistFailed, _ := snapshot["_cooldown_persist_failed"].(bool)
		if (durableAuthoritative != nil && *durableAuthoritative) || !persistFailed {
			if err := rollbackDurableCooldown(compressor, snapshot, durableAuthoritative, durableState); err != nil {
				return err
			}
		}
	}
	if s, ok := compressor.(AttemptStateSnapshotter); ok {
		s.RestoreAttemptState(restored)
	}
	return nil
}

// Generation that currently owns the compressor, zero when none was claimed
func currentAttemptGeneration(compressor any) int {
	compressorAttemptLock.Lock()
	defer compressorAttemptLock.Unlock()
	return entryGenerationLocked(compressor)
}

// Refreshes and snapshots the built in durable cooldown under the lease, false on a durable read failure
func captureAuthoritativeCooldownUnderLease(compressor any, attemptSnapshot map[string]any) (authoritative *bool, durable *state.CompressionCooldownRow) {
	no := false
	c, ok := compressor.(*ContextCompressor)
	if !ok || c == nil || c.sessionDB == nil || c.sessionID == "" {
		return nil, nil
	}
	db, ok := c.sessionDB.(cooldownRowDB)
	if !ok {
		return &no, nil
	}
	row, err := db.GetCompressionFailureCooldownRow(c.sessionID)
	if err != nil {
		compressionLog().Debug(fmt.Sprintf("authoritative compression cooldown capture failed: %v", err))
		return &no, nil
	}
	c.GetActiveCompressionFailureCooldown(true)
	if c.lastCooldownRefreshWasAuthoritative == nil || !*c.lastCooldownRefreshWasAuthoritative {
		return c.lastCooldownRefreshWasAuthoritative, nil
	}
	current := c.SnapshotAttemptState()
	for _, name := range compressorCooldownStateFields {
		attemptSnapshot[name] = current[name]
	}
	yes := true
	return &yes, &row
}

type compressionCancelEvent interface {
	Set()
	IsSet() bool
}

// Holder qualified lease release a worker publishes on the fence, compared by identity
type cancelledLockRelease struct{ fn func() }

// Fences timeout cancellation against post summary session mutation so cancel wins before commit or waits for it
type CompressionCommitFence struct {
	mu                                 sync.Mutex
	cancelled                          atomic.Bool
	commitStarted                      atomic.Bool
	commitPhase                        pythread.Event
	admissionRevoked                   atomic.Bool
	lockReleaseGuard                   sync.Mutex
	cancelledLockRelease               *cancelledLockRelease
	cancelledLockReleaseRequested      bool
	lastProgress                       atomic.Uint64
	progressObserved                   atomic.Bool
	deadline                           atomic.Pointer[float64]
	retainCancelledLockUntilWorkerDone atomic.Bool
	commitWatermarkFenced              atomic.Bool
}

var _ CompressionDeadlineFence = (*CompressionCommitFence)(nil)

// Fence with no deadline armed and its progress clock started now
func NewCompressionCommitFence() *CompressionCommitFence {
	f := &CompressionCommitFence{}
	f.lastProgress.Store(math.Float64bits(pythread.Monotonic()))
	return f
}

// Fence whose total wall clock ceiling is armed at construction
func NewCompressionCommitFenceWithCeiling(totalCeilingSeconds float64) (*CompressionCommitFence, error) {
	f := NewCompressionCommitFence()
	return f, f.SetTotalCeilingSeconds(totalCeilingSeconds)
}

// Arms the wall clock deadline shared by the host and the worker
func (f *CompressionCommitFence) SetTotalCeilingSeconds(seconds float64) error {
	if seconds <= 0 {
		return errors.New("total compression ceiling must be positive")
	}
	d := pythread.Monotonic() + seconds
	f.deadline.Store(&d)
	return nil
}

// Arms one absolute pre commit deadline shared across the primary and stall retry attempts
func (f *CompressionCommitFence) SetPrecommitDeadlineMonotonic(deadline float64) error {
	if deadline <= 0 {
		return errors.New("compression deadline must be positive")
	}
	f.deadline.Store(&deadline)
	return nil
}

// Pushes a live deadline back, false when cancelled, passed, or a commit or cancel holds the fence
func (f *CompressionCommitFence) TryExtendDeadline(seconds float64) bool {
	if seconds <= 0 || !f.mu.TryLock() {
		return false
	}
	defer f.mu.Unlock()
	d := f.deadline.Load()
	alive := d != nil && *d > pythread.Monotonic() && !f.IsCancelled()
	if alive {
		next := *d + seconds
		f.deadline.Store(&next)
	}
	return alive
}

func (f *CompressionCommitFence) ExtendDeadline(seconds float64) { f.TryExtendDeadline(seconds) }

// Records forward progress such as a streamed token
func (f *CompressionCommitFence) TouchProgress() {
	f.lastProgress.Store(math.Float64bits(pythread.Monotonic()))
	f.progressObserved.Store(true)
}

func (f *CompressionCommitFence) ProgressObserved() bool { return f.progressObserved.Load() }

func (f *CompressionCommitFence) DeadlineExceeded() bool {
	d := f.deadline.Load()
	return d != nil && pythread.Monotonic() >= *d
}

// Armed absolute monotonic deadline, false while none is armed
func (f *CompressionCommitFence) DeadlineMonotonic() (float64, bool) {
	if d := f.deadline.Load(); d != nil {
		return *d, true
	}
	return 0, false
}

func (f *CompressionCommitFence) SecondsSinceProgress() float64 {
	return math.Max(0, pythread.Monotonic()-math.Float64frombits(f.lastProgress.Load()))
}

// Cancels a pending commit and returns true, or blocks until an active commit finishes and returns false
func (f *CompressionCommitFence) CancelBeforeCommit(cancelEvent any) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.commitStarted.Load() {
		f.cancelled.Store(true)
	}
	if ev, ok := cancelEvent.(compressionCancelEvent); ok && ev != nil {
		ev.Set()
	}
	return !f.commitStarted.Load()
}

// Non blocking cancel, nil while an active commit owns the fence
func (f *CompressionCommitFence) TryCancelBeforeCommit() *bool {
	if !f.mu.TryLock() {
		return nil
	}
	defer f.mu.Unlock()
	if !f.commitStarted.Load() {
		f.cancelled.Store(true)
	}
	won := !f.commitStarted.Load()
	return &won
}

// Admits the commit and holds the fence unless a hard cancellation already won
func (f *CompressionCommitFence) BeginCommit(cancelEvent any) bool {
	f.mu.Lock()
	ev, _ := cancelEvent.(compressionCancelEvent)
	if f.IsCancelled() || f.admissionRevoked.Load() || (ev != nil && ev.IsSet()) {
		f.cancelled.Store(true)
		f.mu.Unlock()
		if f.admissionRevoked.Load() {
			f.ReleaseCancelledCompressionLock()
		}
		return false
	}
	f.commitStarted.Store(true)
	f.commitPhase.Set()
	return true
}

// Leaves the commit boundary BeginCommit entered
func (f *CompressionCommitFence) FinishCommit() {
	f.commitPhase.Clear()
	f.mu.Unlock()
	if f.admissionRevoked.Load() {
		f.ReleaseCancelledCompressionLock()
	}
}

// Lock free read that an admitted commit is in progress
func (f *CompressionCommitFence) CommitInFlight() bool { return f.commitPhase.IsSet() }

// True after cancellation won before the commit boundary, admission was revoked or the deadline passed
func (f *CompressionCommitFence) IsCancelled() bool {
	return f.cancelled.Load() || f.admissionRevoked.Load() || f.DeadlineExceeded()
}

func (f *CompressionCommitFence) RetainCompressionLockUntilWorkerDone() {
	f.retainCancelledLockUntilWorkerDone.Store(true)
}

// Records a watermark bounded commit so a detached worker may keep its admission
func (f *CompressionCommitFence) MarkCommitWatermarkFenced() { f.commitWatermarkFenced.Store(true) }

func (f *CompressionCommitFence) CommitWatermarkFenced() bool { return f.commitWatermarkFenced.Load() }

func (f *CompressionCommitFence) AllowCancelledLockRelease() {
	f.retainCancelledLockUntilWorkerDone.Store(false)
}

// Revokes future commit admission without blocking, releasing the lease now only when no commit holds the fence
func (f *CompressionCommitFence) RevokeCommitAdmission() {
	f.admissionRevoked.Store(true)
	if f.mu.TryLock() {
		defer f.mu.Unlock()
		f.ReleaseCancelledCompressionLock()
	}
}

// Holds the fence across lock acquisition and release hook publication, false once cancelled
func (f *CompressionCommitFence) BeginLockSetup() bool {
	f.mu.Lock()
	if f.IsCancelled() || f.admissionRevoked.Load() {
		f.mu.Unlock()
		return false
	}
	return true
}

func (f *CompressionCommitFence) FinishLockSetup() { f.mu.Unlock() }

// Publishes the worker release and runs it at once when cleanup was already requested
func (f *CompressionCommitFence) RegisterCancelledLockRelease(release *cancelledLockRelease) bool {
	f.lockReleaseGuard.Lock()
	f.cancelledLockRelease = release
	requested := f.cancelledLockReleaseRequested
	f.lockReleaseGuard.Unlock()
	if requested && release != nil {
		release.fn()
	}
	return requested
}

// Forgets the release after the worker's own cleanup finished
func (f *CompressionCommitFence) ClearCancelledLockRelease(release *cancelledLockRelease) {
	f.lockReleaseGuard.Lock()
	defer f.lockReleaseGuard.Unlock()
	if f.cancelledLockRelease == release {
		f.cancelledLockRelease = nil
	}
}

// Releases the worker lease after cancellation won, remembering a request made before the hook was published
func (f *CompressionCommitFence) ReleaseCancelledCompressionLock() {
	if f.retainCancelledLockUntilWorkerDone.Load() {
		return
	}
	f.lockReleaseGuard.Lock()
	f.cancelledLockReleaseRequested = true
	release := f.cancelledLockRelease
	f.lockReleaseGuard.Unlock()
	if release != nil {
		release.fn()
	}
}

func asPyObject(v any) *pyjson.Object {
	o, _ := v.(*pyjson.Object)
	return o
}

// True for a nil interface or a typed nil pointer engine
func isNilEngine(e any) bool {
	if e == nil {
		return true
	}
	v := reflect.ValueOf(e)
	return v.Kind() == reflect.Pointer && v.IsNil()
}
