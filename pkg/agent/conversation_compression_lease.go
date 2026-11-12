package agent

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Agent slots the compress_context phases read and latch
type conversationCompressionState struct {
	lastFeasibilityNotice                       *string
	compressionFeasibilityChecked               bool
	compressionActivityHeartbeatInterval        any
	compressionLockTTLSeconds                   any
	compressionLockRefreshInterval              *float64
	lastCompressionLockWarningSID               *string
	lastCompressionLockErrorSID                 *string
	lastMemoryContextUnsupportedEngine          *string
	pendingContextEngineCompressionNotification func() bool
	retainSeededSystemPrompt                    bool
	lastCompressionSummaryWarning               *string
	lastAuxFallbackWarningKey                   *[2]string
}

// Durable compression lock surface of the session store
type compressionLockDB interface {
	TryAcquireCompressionLock(sessionID, holder string, ttlSeconds float64) bool
	ReleaseCompressionLock(sessionID, holder string)
	RefreshCompressionLock(sessionID, holder string, ttlSeconds float64) bool
	GetCompressionLockHolder(sessionID string) (*string, error)
	GetActiveMessageWatermark(ctx context.Context, sessionID string) (int64, error)
}

// Per attempt ownership state threaded through the compress_context phases
type compressionAttempt struct {
	snapshot                     map[string]any
	generation                   int
	startedAt                    time.Time
	seed                         *pyjson.Object
	durableCooldownAuthoritative *bool
	durableCooldownState         *state.CompressionCooldownRow
}

// Rolls the compressor back to the attempt snapshot with its durable cooldown
func (t *compressionAttempt) restoreCompressor(compressor any) error {
	generation := t.generation
	return restoreCompressorAttemptState(compressor, t.snapshot, t.durableCooldownAuthoritative, t.durableCooldownState, &generation)
}

// Keeps the inactivity tracker fresh and republishes the compacting status while compression blocks
type compressionActivityHeartbeat struct {
	a                 *AIAgent
	fence             *CompressionCommitFence
	mu                sync.Mutex
	suppressed        bool
	interval          time.Duration
	emitClientStatus  bool
	stopCh            chan struct{}
	stopOnce          sync.Once
	done              chan struct{}
	started           bool
	heartbeatRoutine  uint64
	heartbeatRoutineM sync.Mutex
}

func newCompressionActivityHeartbeat(a *AIAgent, intervalSeconds any, emitClientStatus bool, fence *CompressionCommitFence) *compressionActivityHeartbeat {
	if intervalSeconds == nil {
		intervalSeconds = a.compressionActivityHeartbeatInterval
		if intervalSeconds == nil {
			intervalSeconds = 60.0
		}
	}
	seconds := heartbeatSeconds(intervalSeconds)
	return &compressionActivityHeartbeat{a: a, fence: fence, interval: time.Duration(math.Max(0.1, seconds) * float64(time.Second)),
		emitClientStatus: emitClientStatus, stopCh: make(chan struct{}), done: make(chan struct{})}
}

// float(interval or 60.0) with the non finite and unparsable values read as 60
func heartbeatSeconds(v any) float64 {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case float32:
		f = float64(x)
	case int:
		f = float64(x)
	case int64:
		f = float64(x)
	case bool:
		if x {
			f = 1
		}
	case string:
		parsed, err := strconv.ParseFloat(x, 64)
		if x == "" {
			parsed, err = 60, nil
		}
		if err != nil {
			return 60
		}
		f = parsed
	default:
		return 60
	}
	if f == 0 {
		f = 60
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return 60
	}
	return f
}

func (h *compressionActivityHeartbeat) start() *compressionActivityHeartbeat {
	h.mu.Lock()
	h.suppressed = false
	h.mu.Unlock()
	h.touch("context compression started", true, false)
	h.started = true
	go h.run()
	return h
}

// Stops the ticker and stamps the completion unless a host timeout already owns the terminal stamp
func (h *compressionActivityHeartbeat) stop(desc string) {
	h.stopOnce.Do(func() { close(h.stopCh) })
	h.heartbeatRoutineM.Lock()
	self := h.heartbeatRoutine == pythread.GoroutineID()
	h.heartbeatRoutineM.Unlock()
	if h.started && !self {
		select {
		case <-h.done:
		case <-time.After(time.Second):
		}
	}
	if h.shouldSuppress() {
		return
	}
	h.touch(desc, false, true)
}

func (h *compressionActivityHeartbeat) shouldSuppress() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.suppressed && h.fence != nil && h.fence.IsCancelled() {
		h.suppressed = true
	}
	return h.suppressed
}

func (h *compressionActivityHeartbeat) touch(desc string, allowTerminalOverwrite, forcePersist bool) {
	swallowCompression("compression activity heartbeat touch failed", true, func() error {
		if !allowTerminalOverwrite {
			if h.shouldSuppress() {
				return nil
			}
			if terminalCompressionProvenances[h.a.lastActivityProvenanceValue()] {
				h.mu.Lock()
				h.suppressed = true
				h.mu.Unlock()
				return nil
			}
			if h.shouldSuppress() {
				return nil
			}
		}
		h.a.touchActivity(desc, string(state.ActivityProvenanceAgentCompression), forcePersist)
		return nil
	})
}

// Republishes the compacting status so remote idle watchdogs see a live turn
func (h *compressionActivityHeartbeat) emitProgressStatus() {
	if !h.emitClientStatus {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			compressionLog().Debug("status emit error in compression heartbeat", "error", fmt.Sprint(r))
		}
	}()
	h.a.emitStatus(CompactionHeartbeatStatus)
}

func (h *compressionActivityHeartbeat) run() {
	h.heartbeatRoutineM.Lock()
	h.heartbeatRoutine = pythread.GoroutineID()
	h.heartbeatRoutineM.Unlock()
	defer close(h.done)
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
		}
		if h.shouldSuppress() {
			return
		}
		h.touch("context compression in progress", false, false)
		h.emitProgressStatus()
	}
}

// Activity provenance read under the activity lock
func (a *AIAgent) lastActivityProvenanceValue() state.ActivityProvenance {
	lock := a.livenessActivityLock()
	lock.Lock()
	defer lock.Unlock()
	return state.NormalizeActivityProvenance(string(a.lastActivityProvenance))
}

// Direct user and assistant evidence for a memory checkpoint, without summaries, tool rows or tool call wrappers
func directMessagesForPreCompressMemory(messages []map[string]any) []map[string]any {
	direct := []map[string]any{}
	for _, message := range messages {
		if message == nil {
			continue
		}
		role, _ := message["role"].(string)
		if (role != "user" && role != "assistant") || pyTruthy(message[state.CompressedSummaryMetadataKey]) {
			continue
		}
		if role == "assistant" && pyTruthy(message["tool_calls"]) {
			content := message["content"]
			hasProse := pyTruthy(content)
			if s, ok := content.(string); ok {
				hasProse = pystr.Strip(s) != ""
			}
			if !hasProse {
				continue
			}
			stripped := make(map[string]any, len(message))
			for k, v := range message {
				if k != "tool_calls" {
					stripped[k] = v
				}
			}
			message = stripped
		}
		direct = append(direct, message)
	}
	return direct
}

// Keeps the durable compression lease alive, stopping after a TTL worth of consecutive refresh failures
type compressionLockLeaseRefresher struct {
	db                     compressionLockDB
	sessionID, holder      string
	ttlSeconds             float64
	interval               time.Duration
	maxConsecutiveFailures int
	stopCh                 chan struct{}
	stopOnce               sync.Once
	done                   chan struct{}
}

func newCompressionLockLeaseRefresher(db compressionLockDB, sessionID, holder string, ttlSeconds float64, refreshIntervalSeconds *float64) *compressionLockLeaseRefresher {
	interval := math.Max(1.0, math.Min(60.0, ttlSeconds/2.0))
	if refreshIntervalSeconds != nil {
		interval = *refreshIntervalSeconds
	}
	interval = math.Max(0.1, interval)
	return &compressionLockLeaseRefresher{db: db, sessionID: sessionID, holder: holder, ttlSeconds: ttlSeconds,
		interval: time.Duration(interval * float64(time.Second)), maxConsecutiveFailures: max(1, int(ttlSeconds/interval)),
		stopCh: make(chan struct{}), done: make(chan struct{})}
}

func (r *compressionLockLeaseRefresher) start() *compressionLockLeaseRefresher {
	go r.run()
	return r
}

// Signals the refresher and waits up to a second, a late refresh on a released lock is a no op
func (r *compressionLockLeaseRefresher) stop() {
	r.stopOnce.Do(func() { close(r.stopCh) })
	select {
	case <-r.done:
	case <-time.After(time.Second):
	}
}

func (r *compressionLockLeaseRefresher) stopped() bool {
	select {
	case <-r.stopCh:
		return true
	default:
		return false
	}
}

func (r *compressionLockLeaseRefresher) run() {
	defer close(r.done)
	failures := 0
	first := true
	for {
		if first {
			first = false
			if r.stopped() {
				return
			}
		} else {
			select {
			case <-r.stopCh:
				return
			case <-time.After(r.interval):
			}
		}
		refreshed := false
		swallowCompression("compression lock refresh raised", false, func() error {
			refreshed = r.db.RefreshCompressionLock(r.sessionID, r.holder, r.ttlSeconds)
			return nil
		})
		if refreshed {
			failures = 0
			continue
		}
		failures++
		if failures >= r.maxConsecutiveFailures {
			compressionLog().Debug(fmt.Sprintf("compression lock refresh failed %d times in a row; stopping lease refresher for session %s", failures, r.sessionID))
			return
		}
	}
}

// One shot terminal edge of the compaction status lifecycle, emitted only on commit or a forced terminal
type compactionLifecycle struct {
	a             *AIAgent
	statusEmitted bool
	doneEmitted   bool
	commitStatus  string
}

func newCompactionLifecycle(a *AIAgent, statusEmitted bool) *compactionLifecycle {
	return &compactionLifecycle{a: a, statusEmitted: statusEmitted, commitStatus: "aborted"}
}

func (l *compactionLifecycle) complete(forceTerminal bool) {
	if l.doneEmitted {
		return
	}
	l.doneEmitted = true
	if l.statusEmitted && (l.commitStatus == "committed" || forceTerminal) {
		emitCompactionDone(l.a)
	}
}

// Per attempt durable compression lock with its refresher, release hook and lifecycle edge
type compressionLease struct {
	a                *AIAgent
	db               compressionLockDB
	sid              string
	ttl              float64
	refreshInterval  *float64
	fence            *CompressionCommitFence
	lifecycle        *compactionLifecycle
	holder           *string
	watermark        *int64
	refresher        *compressionLockLeaseRefresher
	released         bool
	releaseGuard     sync.Mutex
	lockSetupEntered bool
	releaseHook      *cancelledLockRelease
}

func newCompressionLease(a *AIAgent, db compressionLockDB, sid string, ttl float64, refreshInterval *float64, fence *CompressionCommitFence, lifecycle *compactionLifecycle) *compressionLease {
	l := &compressionLease{a: a, db: db, sid: sid, ttl: ttl, refreshInterval: refreshInterval, fence: fence, lifecycle: lifecycle}
	l.releaseHook = &cancelledLockRelease{fn: l.releaseHolderOnly}
	return l
}

func (l *compressionLease) statusEmitted() bool { return l.lifecycle.statusEmitted }

func (l *compressionLease) beginLockSetup() bool {
	if l.fence == nil {
		return true
	}
	l.lockSetupEntered = l.fence.BeginLockSetup()
	return l.lockSetupEntered
}

func (l *compressionLease) finishLockSetup() {
	if !l.lockSetupEntered || l.fence == nil {
		return
	}
	l.lockSetupEntered = false
	l.fence.FinishLockSetup()
}

// Starts the refresher unless cancellation already released the holder
func (l *compressionLease) startRefresher() {
	if l.holder == nil {
		return
	}
	candidate := newCompressionLockLeaseRefresher(l.db, l.sid, *l.holder, l.ttl, l.refreshInterval)
	l.releaseGuard.Lock()
	defer l.releaseGuard.Unlock()
	if !l.released {
		l.refresher = candidate.start()
	}
}

// Stops this holder's refresher and releases only its own durable lock, idempotent
func (l *compressionLease) releaseHolderOnly() {
	l.releaseGuard.Lock()
	defer l.releaseGuard.Unlock()
	if l.released {
		return
	}
	l.released = true
	if l.holder != nil && l.a.activeCompressionLockHolder != nil && *l.a.activeCompressionLockHolder == *l.holder {
		l.a.activeCompressionLockHolder = nil
	}
	if l.refresher != nil {
		swallowCompression("compression lock refresher stop failed", false, func() error { l.refresher.stop(); return nil })
	}
	if l.db != nil && !isNilEngine(l.db) && l.sid != "" && l.holder != nil && *l.holder != "" {
		holder := *l.holder
		swallowCompression("compression lock release failed", false, func() error { l.db.ReleaseCompressionLock(l.sid, holder); return nil })
	}
}

// Finishes the lifecycle edge and releases the old session lock once
func (l *compressionLease) release() {
	defer func() {
		defer func() {
			defer l.finishLockSetup()
			if l.fence != nil {
				l.fence.ClearCancelledLockRelease(l.releaseHook)
			}
		}()
		l.releaseHolderOnly()
	}()
	l.lifecycle.complete(false)
}

// Lock API of the session store, nil with no error when absent so the caller fails open
func resolveLockAPI(lockDB any) (db compressionLockDB, lookupErr error) {
	if lockDB == nil || isNilEngine(lockDB) {
		return nil, nil
	}
	defer func() {
		if r := recover(); r != nil {
			db, lookupErr = nil, fmt.Errorf("%v", r)
		}
	}()
	if lockAPIIsAbsentOnSessionDB(lockDB) {
		return nil, nil
	}
	api, ok := lockDB.(compressionLockDB)
	if !ok {
		return nil, &pyTypeError{"compression lock API is present but not callable"}
	}
	return api, nil
}

type pyTypeError struct{ msg string }

func (e *pyTypeError) Error() string      { return e.msg }
func (e *pyTypeError) PyTypeName() string { return "TypeError" }

// Sit out return of lease acquisition with the prompt, aborted telemetry and the terminal status edge
func abortLease(ctx context.Context, a *AIAgent, lifecycle *compactionLifecycle, systemMessage *string, attempt *compressionAttempt, failureClass string, prompt *string) (string, error) {
	var sp string
	if prompt != nil {
		sp = *prompt
	} else {
		built, err := existingSystemPrompt(ctx, a, systemMessage)
		if err != nil {
			return "", err
		}
		sp = built
	}
	emitAbortedAttemptTelemetry(ctx, a, attempt.startedAt, failureClass, attempt.seed)
	lifecycle.complete(true)
	return sp, nil
}

// Acquires the durable lock for the lease holder and captures the start watermark, failing closed on a raise
func tryAcquireDurableLock(ctx context.Context, lease *compressionLease, api compressionLockDB, fence *CompressionCommitFence) (acquired bool) {
	holder := *lease.holder
	defer func() {
		if r := recover(); r != nil {
			swallowCompression("compression lock cleanup after failed acquire failed", false, func() error {
				lease.db.ReleaseCompressionLock(lease.sid, holder)
				return nil
			})
			lease.holder = nil
			compressionLog().Warn(fmt.Sprintf("compression lock acquisition raised unexpectedly for session=%s (%T: %v) — skipping compression this cycle", lease.sid, r, r))
			acquired = false
		}
	}()
	acquired = api.TryAcquireCompressionLock(lease.sid, holder, lease.ttl)
	if acquired {
		watermark, err := lease.db.GetActiveMessageWatermark(ctx, lease.sid)
		if err != nil {
			compressionLog().Warn(fmt.Sprintf("compression watermark capture failed for session=%s (%v) — concurrent appends this cycle will be archived with the snapshot", lease.sid, err))
			lease.watermark = nil
		} else {
			lease.watermark = &watermark
			if fence != nil {
				fence.MarkCommitWatermarkFenced()
			}
		}
	}
	return acquired
}

// Another path holds the lock so publish the skip signal, warn once per session and sit out
func sitOutLockContention(ctx context.Context, a *AIAgent, lease *compressionLease, lifecycle *compactionLifecycle, systemMessage *string, attempt *compressionAttempt) (string, error) {
	var existing *string
	swallowCompression("compression lock holder read failed", false, func() error {
		var err error
		existing, err = lease.db.GetCompressionLockHolder(lease.sid)
		return err
	})
	holderText := "None"
	if existing != nil {
		holderText = *existing
	}
	compressionLog().Warn(fmt.Sprintf("compression skipped: another path is compressing session=%s (holder=%s) — returning messages unchanged to avoid session fork", lease.sid, holderText))
	lease.holder = nil
	if existing != nil && *existing != "" {
		a.compressionSkippedDueToLock = *existing
	} else {
		a.compressionSkippedDueToLock = true
	}
	if a.lastCompressionLockWarningSID == nil || *a.lastCompressionLockWarningSID != lease.sid {
		sid := lease.sid
		a.lastCompressionLockWarningSID = &sid
		swallowCompression("compression lock warning emit failed", false, func() error {
			a.emitWarning("⚠ Skipping concurrent compression — another path is already compressing this session. Will retry after it finishes.")
			return nil
		})
	}
	prompt, err := existingSystemPrompt(ctx, a, systemMessage)
	if err != nil {
		return "", err
	}
	return abortLease(ctx, a, lifecycle, systemMessage, attempt, "lock_contended", &prompt)
}

// Takes the per session compression lock, a nil lease with the prompt means sit out
func acquireCompressionLease(ctx context.Context, a *AIAgent, fence *CompressionCommitFence, lifecycle *compactionLifecycle, systemMessage *string, attempt *compressionAttempt) (*compressionLease, string, error) {
	var lockDB any
	if a.sessionDB != nil {
		lockDB = a.sessionDB
	}
	sid := a.sessionID
	a.compressionSkippedDueToLock = nil
	api, lookupErr := resolveLockAPI(lockDB)
	ttl := 300.0
	if v := heartbeatTTL(a.compressionLockTTLSeconds); v != nil {
		ttl = *v
	}
	var db compressionLockDB
	if lockDB != nil {
		db, _ = lockDB.(compressionLockDB)
	}
	lease := newCompressionLease(a, db, sid, ttl, a.compressionLockRefreshInterval, fence, lifecycle)
	if lockDB != nil && sid != "" {
		holder := compressionLockHolder(a)
		lease.holder = &holder
		var acquired bool
		switch {
		case lookupErr != nil:
			lease.holder = nil
			compressionLog().Warn(fmt.Sprintf("compression lock lookup raised unexpectedly for session=%s (%s: %v) — skipping compression this cycle", sid, pyErrorClass(lookupErr), lookupErr))
		case api == nil:
			lease.holder = nil
			if a.lastCompressionLockErrorSID == nil || *a.lastCompressionLockErrorSID != sid {
				s := sid
				a.lastCompressionLockErrorSID = &s
				compressionLog().Warn(fmt.Sprintf("compression lock subsystem unavailable for session=%s — proceeding without lock. This usually means a stale in-memory module after an update; restart the process (or `icebath update`) to resync.", sid))
			}
			acquired = true
		default:
			if !lease.beginLockSetup() {
				compressionLog().Info(fmt.Sprintf("Compression commit cancelled before lock acquisition (session=%s).", sessionLabel(a)))
				a.lastCompactionInPlace = false
				prompt, err := abortLease(ctx, a, lifecycle, systemMessage, attempt, "commit_fence_cancelled", nil)
				return nil, prompt, err
			}
			acquired = tryAcquireDurableLock(ctx, lease, api, fence)
		}
		if !acquired {
			lease.finishLockSetup()
			prompt, err := sitOutLockContention(ctx, a, lease, lifecycle, systemMessage, attempt)
			return nil, prompt, err
		}
	}
	if lease.holder != nil {
		holder := *lease.holder
		a.activeCompressionLockHolder = &holder
		if fence != nil && fence.RegisterCancelledLockRelease(lease.releaseHook) {
			compressionLog().Info(fmt.Sprintf("Compression commit cancelled before summary dispatch (session=%s).", sessionLabel(a)))
			a.lastCompactionInPlace = false
			prompt, err := existingSystemPrompt(ctx, a, systemMessage)
			if err != nil {
				return nil, "", err
			}
			emitAbortedAttemptTelemetry(ctx, a, attempt.startedAt, "commit_fence_cancelled", attempt.seed)
			lease.release()
			return nil, prompt, nil
		}
	}
	return lease, "", nil
}

// float(ttl or 300.0) with an unparsable value keeping the default
func heartbeatTTL(v any) *float64 {
	var f float64
	switch x := v.(type) {
	case nil:
		return nil
	case float64:
		f = x
	case int:
		f = float64(x)
	case int64:
		f = float64(x)
	case string:
		parsed, err := strconv.ParseFloat(x, 64)
		if err != nil && x != "" {
			return nil
		}
		f = parsed
	default:
		return nil
	}
	if f == 0 {
		f = 300
	}
	return &f
}

func sessionLabel(a *AIAgent) string {
	if a.sessionID == "" {
		return "none"
	}
	return a.sessionID
}

// Sits out or adopts the live child when the parent was already rotated, done false means proceed
func adoptIfParentRotated(ctx context.Context, a *AIAgent, lease *compressionLease, messages []map[string]any, systemMessage *string, attempt *compressionAttempt, approxTokens any) (result []map[string]any, prompt string, done bool, err error) {
	if lease.db == nil || isNilEngine(lease.db) || lease.sid == "" {
		return nil, "", false, nil
	}
	rotated, lookupErr := sessionWasRotatedByCompression(ctx, lease.db, lease.sid)
	if lookupErr != nil {
		compressionLog().Warn(fmt.Sprintf("compression session ownership lookup failed for session=%s (%s: %v) - skipping compression this cycle", lease.sid, pyErrorClass(lookupErr), lookupErr))
		lease.release()
		emitBypassedAttemptTelemetry(ctx, a, attempt.startedAt, attemptBypass{CommitStatus: "aborted", FailureClass: "session_ownership_unreadable", ApproxTokens: approxTokens, AttemptSeed: attempt.seed})
		prompt, err = existingSystemPrompt(ctx, a, systemMessage)
		return messages, prompt, true, err
	}
	if !rotated {
		return nil, "", false, nil
	}
	recovered, adoptErr := adoptLiveCompressionChild(ctx, a, lease.db, lease.sid)
	lease.release()
	if adoptErr != nil {
		return nil, "", true, adoptErr
	}
	emitBypassedAttemptTelemetry(ctx, a, attempt.startedAt, attemptBypass{CommitStatus: "skipped", FailureClass: "session_ownership_lost", ApproxTokens: approxTokens, AttemptSeed: attempt.seed})
	prompt, err = existingSystemPrompt(ctx, a, systemMessage)
	if err != nil {
		return nil, "", true, err
	}
	if recovered != nil {
		compressionLog().Warn(fmt.Sprintf("compression recovery: stale session=%s adopted live child=%s", lease.sid, a.sessionID))
		return recovered, prompt, true, nil
	}
	compressionLog().Warn(fmt.Sprintf("compression skipped: session=%s was already rotated by another compression path, but no unique live child could be adopted", lease.sid))
	return messages, prompt, true, nil
}

type durableConversationReader interface {
	GetMessagesAsConversation(ctx context.Context, sessionID string, o state.ConversationOptions) ([]map[string]any, error)
}

// Durable parent transcript when it outgrew the in memory snapshot, advancing the lease watermark to its newest row
func adoptGrownDurableParent(ctx context.Context, a *AIAgent, lease *compressionLease, messages []map[string]any) ([]map[string]any, error) {
	if lease.db == nil || isNilEngine(lease.db) || lease.sid == "" {
		return nil, nil
	}
	loader, ok := lease.db.(durableConversationReader)
	if !ok {
		return nil, nil
	}
	durable, err := loader.GetMessagesAsConversation(ctx, lease.sid, state.ConversationOptions{})
	if err != nil {
		return nil, err
	}
	if len(durable) <= len(messages) {
		return nil, nil
	}
	preflushOK := true
	if idx := a.persistUserMessageIdx; idx != nil && *idx >= 0 && *idx < len(messages) {
		preflushOK = false
		func() {
			defer func() { _ = recover() }()
			written, err := a.flushMessagesToSessionDBWithHistory(messages, messages[:*idx])
			preflushOK = err == nil && written
		}()
	}
	if !preflushOK {
		compressionLog().Warn(fmt.Sprintf("compression: session=%s grew before lease (%d → %d msgs) but the pre-adoption flush of the live tail failed; skipping durable-snapshot adoption so un-persisted user input is kept", lease.sid, len(messages), len(durable)))
		return nil, nil
	}
	durable, err = loader.GetMessagesAsConversation(ctx, lease.sid, state.ConversationOptions{IncludeRowIDs: true})
	if err != nil {
		return nil, err
	}
	if len(durable) <= len(messages) {
		return nil, nil
	}
	var adopted []int64
	for _, m := range durable {
		rid, has := m["_row_id"]
		delete(m, "_row_id")
		if !has {
			continue
		}
		switch id := rid.(type) {
		case int64:
			adopted = append(adopted, id)
		case int:
			adopted = append(adopted, int64(id))
		}
	}
	if lease.watermark != nil && len(adopted) > 0 {
		top := *lease.watermark
		for _, id := range adopted {
			top = max(top, id)
		}
		lease.watermark = &top
	}
	compressionLog().Info(fmt.Sprintf("compression: session=%s grew before lease (%d → %d msgs); adopting durable snapshot", lease.sid, len(messages), len(durable)))
	return durable, nil
}

// Checkpoint refusal that keeps the provider failure as its cause
type chainedCheckpointError struct {
	*provider.CompressionCheckpointUnavailable
	cause error
}

func (e *chainedCheckpointError) Unwrap() []error {
	return []error{e.CompressionCheckpointUnavailable, e.cause}
}

// Provider pre compress insights for the summary, failing closed when a required checkpoint cannot be taken
func preCompressMemoryContext(ctx context.Context, a *AIAgent, messages []map[string]any, checkpointRequired bool) (string, error) {
	memoryContext := ""
	mm := a.memoryManager
	evidence := directMessagesForPreCompressMemory(messages)
	if checkpointRequired && a.persistDisabled && mm == nil {
		checkpointRequired = a.sessionDB != nil
	}
	version := memory.PreCompressCheckpointAPIVersion
	if checkpointRequired {
		if mm == nil {
			return "", checkpointIncapable(fmt.Sprintf("no active provider implements checkpoint API v%d", version))
		}
		compatible, probeErr := probeCheckpointSupport(mm)
		if probeErr != nil {
			return "", &chainedCheckpointError{provider.CheckpointBlocked("provider capability probe failed"), probeErr}
		}
		if !compatible {
			return "", checkpointIncapable(fmt.Sprintf("active provider does not implement checkpoint API v%d", version))
		}
		got, err := callPreCompress(ctx, mm, messages, evidence, true, version)
		if err != nil {
			compressionLog().Warn(fmt.Sprintf("Required pre-compress checkpoint failed (%s)", pyErrorClass(err)))
			return "", &chainedCheckpointError{provider.CheckpointBlocked(fmt.Sprintf("provider checkpoint API v%d failed", version)), err}
		}
		return SanitizeMemoryContext(a.env, got), nil
	}
	if mm != nil {
		if got, err := callPreCompress(ctx, mm, messages, evidence, false, 0); err == nil {
			memoryContext = SanitizeMemoryContext(a.env, got)
		}
	}
	return memoryContext, nil
}

// on_pre_compress with a provider panic turned into the returned failure
func callPreCompress(ctx context.Context, mm *memory.MemoryManager, messages, evidence []map[string]any, require bool, version int) (got string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return mm.OnPreCompress(ctx, messages, evidence, require, version)
}

// Engine side keyword introspection a plugin hosted compressor answers for its compress callable
type compressKeywordInspector interface {
	AcceptsCompressKeyword(keyword string) bool
}

type cooldownBypassingEngine interface {
	CompressBypassingCooldown(ctx context.Context, messages []map[string]any, currentTokens *int, focusTopic *string, force bool, memoryContext string, bypassCooldown bool) ([]map[string]any, error)
}

// Only the compression keywords the engine's compress accepts
func supportedCompressionKwargs(engine ContextEngine, currentTokens *int, focusTopic *string, force bool, memoryContext string, bypassCooldown bool) map[string]any {
	candidates := map[string]any{"current_tokens": currentTokens, "focus_topic": focusTopic, "force": force}
	if bypassCooldown {
		candidates["bypass_cooldown"] = true
	}
	if memoryContext != "" {
		candidates["memory_context"] = memoryContext
	}
	accepts := func(name string) bool {
		if inspector, ok := engine.(compressKeywordInspector); ok {
			return inspector.AcceptsCompressKeyword(name)
		}
		if name == "bypass_cooldown" {
			_, ok := engine.(cooldownBypassingEngine)
			return ok
		}
		return true
	}
	out := map[string]any{}
	for name, value := range candidates {
		if accepts(name) {
			out[name] = value
		}
	}
	return out
}
