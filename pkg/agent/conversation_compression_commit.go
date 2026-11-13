package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Compressor attributes saved around the memory flush so the live attempt record survives it
var attemptTelemetryFields = []string{"_last_compression_telemetry", "_active_compression_telemetry", "_compression_telemetry_seed"}

type compressionMetricWorker struct{ _ byte }

// Keyword arguments of one compress_context call
type compressContextOptions struct {
	ApproxTokens                   *int
	TaskID                         string
	FocusTopic                     *string
	Force                          bool
	BypassCooldown                 bool
	DeferContextEngineNotification bool
	CommitFence                    *CompressionCommitFence
	VerbatimTail                   []map[string]any
	Trigger                        *string
	SnapshotIsCurrent              func() (bool, error)
	OverflowReason                 *string
}

type commitOutcome struct {
	compressed             []map[string]any
	commitStartedAt        time.Time
	refusedPrompt          *string
	oldSessionID           *string
	splitStatus            string
	sessionCommitSucceeded bool
	compactedInPlace       bool
	madeProgress           bool
}

type summaryPhase struct {
	messages                  []map[string]any
	compressed                []map[string]any
	messagesBeforeCompression []map[string]any
	approxTokens              *int
	preMsgCount               int
	abortPrompt               *string
}

// Python monotonic reading of a wall instant on the process clock
func monotonicOf(t time.Time) float64 { return pythread.Monotonic() - time.Since(t).Seconds() }

func optionalIntAny(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// Hard interrupt event as an interface value, nil when the agent has none
func hardCancelEventOf(a *AIAgent) provider.AuxCancelEvent {
	if a.hardInterruptRequested == nil {
		return nil
	}
	return a.hardInterruptRequested
}

func cancelEventArg(ev provider.AuxCancelEvent) any {
	if ev == nil {
		return nil
	}
	return ev
}

// Whether both slices are the same list object, the identity Python compares with is
func sameRowList(x, y []map[string]any) bool {
	if len(x) != len(y) || (x == nil) != (y == nil) {
		return false
	}
	return len(x) == 0 || &x[0] == &y[0]
}

func isPyInt(v any) bool {
	switch v.(type) {
	case int, int64, int32, bool:
		return true
	}
	return false
}

func anyList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []int64:
		out := make([]any, len(x))
		for i, n := range x {
			out[i] = n
		}
		return out
	case []int:
		out := make([]any, len(x))
		for i, n := range x {
			out[i] = n
		}
		return out
	}
	return nil
}

// Reads one attempt attribute of the compressor, the hosted object answering for a plugin engine
func compressorAttr(e ContextEngine, name string) any {
	switch c := e.(type) {
	case *ContextCompressor:
		switch name {
		case "_last_compression_made_progress":
			return c.lastCompressionMadeProgress
		case "_last_summary_fallback_used":
			return c.LastSummaryFallbackUsed
		case "_last_feasibility_skip":
			return c.lastFeasibilitySkip
		case "_last_compress_aborted":
			return c.lastCompressAborted
		case "_last_summary_error":
			if c.lastSummaryError == nil {
				return nil
			}
			return *c.lastSummaryError
		case "_last_compression_telemetry":
			return c.Last
		case "_active_compression_telemetry":
			return c.Active
		case "_compression_telemetry_seed":
			return c.Seed
		}
	}
	v, _ := engineAttr(e, name)
	return v
}

// Writes one compressor attribute, raising a hosted write failure like setattr
func setEngineAttr(e ContextEngine, name string, value any) error {
	switch c := e.(type) {
	case *ContextCompressor:
		obj, _ := value.(*pyjson.Object)
		switch name {
		case "_last_compression_telemetry":
			c.Last = obj
		case "_active_compression_telemetry":
			c.Active = obj
		case "_compression_telemetry_seed":
			c.Seed = obj
		case "last_compression_rough_tokens":
			c.lastCompressionRoughTokens, _ = value.(int)
		case "awaiting_real_usage_after_compression":
			c.awaitingRealUsageAfterCompression = pyrepr.Truthy(value)
		case "_verify_compaction_cleared_threshold":
			c.verifyCompactionClearedThreshold = pyrepr.Truthy(value)
		}
	case *hostedContextEngine:
		return c.obj.Set(c.ctx, name, value)
	}
	return nil
}

// Restore function putting the live attempt telemetry back after a call that may reset it
func holdAttemptTelemetry(compressor ContextEngine) func() {
	held := make([]any, len(attemptTelemetryFields))
	for i, name := range attemptTelemetryFields {
		held[i] = compressorAttr(compressor, name)
	}
	return func() {
		for i, name := range attemptTelemetryFields {
			swallowCompression("attempt telemetry restore failed", false, func() error { return setEngineAttr(compressor, name, held[i]) })
		}
	}
}

// Stamps the persisted flag on unstamped live twins of the anchor source, exact timestamp twins preferred
func stampScopedTwinMessages(targets []*state.Message, source map[string]any, exactCountsStamped bool) {
	asDict := func(m *state.Message) map[string]any {
		d := map[string]any{"role": m.Role, "content": m.Content}
		if m.Timestamp != 0 {
			d["timestamp"] = m.Timestamp
		}
		return d
	}
	sourceTimestamp := source["timestamp"]
	if sourceTimestamp != nil {
		exactHit := false
		for _, target := range targets {
			if target == nil {
				continue
			}
			view := asDict(target)
			if !pyEqual(view["timestamp"], sourceTimestamp) || !messagesMatchScopedIdentity(view, source) {
				continue
			}
			if target.Persisted {
				exactHit = exactHit || exactCountsStamped
				continue
			}
			target.Persisted = true
			exactHit = true
		}
		if exactHit {
			return
		}
	}
	for _, target := range targets {
		if target != nil && !target.Persisted && messagesMatchScopedIdentity(asDict(target), source) {
			target.Persisted = true
		}
	}
}

// Flushes the parent, publishes the compression child and re points the agent at it
func publishRotatedCompaction(ctx context.Context, a *AIAgent, messages, compressed []map[string]any, newSystemPrompt string, lease *compressionLease, oldSessionID, userTurnOutcome string) error {
	var persistedHistory []map[string]any
	if idx := a.persistUserMessageIdx; idx != nil && *idx >= 0 && *idx <= len(messages) {
		persistedHistory = append([]map[string]any{}, messages[:*idx]...)
	}
	if parentDeliberatelyEnded(ctx, a.sessionDB, oldSessionID) {
		return fmt.Errorf("Compression parent already ended: %s", oldSessionID)
	}
	var foreignTailCeiling *int64
	swallowCompression("active message watermark read failed", false, func() error {
		wm, err := a.sessionDB.GetActiveMessageWatermark(ctx, a.sessionID)
		if err == nil {
			foreignTailCeiling = &wm
		}
		return err
	})
	swallowCompression("pre rotation flush failed", false, func() error {
		a.flushMessagesToSessionDBWithHistory(messages, persistedHistory)
		return nil
	})
	var profileForChild *string
	swallowCompression("active profile lookup failed", false, func() error {
		if name := profiles.GetActiveProfileName(ctx); name != "default" {
			profileForChild = &name
		}
		return nil
	})
	oldTitle, _, err := a.sessionDB.GetSessionTitle(ctx, a.sessionID)
	if err != nil {
		return err
	}
	newSessionID := state.NewSessionID()
	var modelConfig *pyjson.Object
	if cfg := tools.WithSessionYolo(a.sessionInitModelConfig, oldSessionID); cfg != nil {
		keys := make([]string, 0, len(cfg))
		for k := range cfg {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		modelConfig = pyjson.NewObject()
		for _, k := range keys {
			modelConfig.Set(k, cfg[k])
		}
	}
	model, prompt := a.model, newSystemPrompt
	p := state.CompressionChildPublish{
		ParentSessionID: oldSessionID, ChildSessionID: newSessionID, Source: compressionChildSource(ctx, a, oldSessionID),
		Messages: compressed, Model: &model, ModelConfig: modelConfig, SystemPrompt: &prompt, ProfileName: profileForChild,
		CompressionLockHolder: lease.holder, RequireCompressionLease: lease.holder != nil, RequireLeaseRefresh: lease.holder != nil,
		LeaseTTLSeconds: lease.ttl, WatermarkCeiling: foreignTailCeiling,
	}
	if foreignTailCeiling != nil {
		p.Watermark = lease.watermark
	}
	if err := a.sessionDB.PublishCompressionChild(ctx, p); err != nil {
		return err
	}
	if userTurnOutcome == "inserted" || userTurnOutcome == "merged" {
		for i := len(messages) - 1; i >= 0; i-- {
			if source := messages[i]; isRealUserMessage(source) {
				source[dbPersistedMarker] = true
				if live := a.liveSessionMessages(); live != nil {
					stampScopedTwinMessages(live, source, true)
				}
				break
			}
		}
	}
	for _, handoff := range compressed {
		if handoff != nil {
			handoff[dbPersistedMarker] = true
		}
	}
	a.sessionID = newSessionID
	a.dbFlushScanPrefix = nil
	rebindSessionContext(ctx, a, a.sessionID)
	a.sessionDBCreated = true
	carrySessionStateToChild(ctx, a, oldSessionID, oldTitle)
	return nil
}

// Post commit bookkeeping that notifies engines and hooks and re arms usage tracking, the rough estimate returned
func finishCompactionBoundary(ctx context.Context, a *AIAgent, compressed []map[string]any, newSystemPrompt string, oldSessionID *string, inPlace, compactedInPlace, sessionCommitSucceeded, deferNotification, madeProgress, usedFallback, feasibilitySkip bool, taskID string) (int, error) {
	oldSID := ""
	if oldSessionID != nil {
		oldSID = *oldSessionID
	}
	boundaryParent := oldSID
	if boundaryParent == "" {
		boundaryParent = a.sessionID
	}
	if oldSID != "" && sessionCommitSucceeded && a.sessionDB != nil {
		swallowCompression("failed to clear archived compression parent's activity labels (ignored)", true, func() error {
			return a.sessionDB.ClearSessionActivityLabels(oldSID)
		})
	}
	if sessionCommitSucceeded && (oldSID != "" || compactedInPlace) {
		if deferNotification {
			if err := queueContextEngineCompressionNotification(ctx, a, a.sessionID, boundaryParent); err != nil {
				return 0, err
			}
		} else {
			notifyContextEngineCompressionComplete(ctx, a, a.sessionID, boundaryParent)
		}
	}
	swallowCompression("memory manager on_session_switch (compression): %s", false, func() error {
		if (oldSID != "" || inPlace) && a.memoryManager != nil {
			a.memoryManager.OnSessionSwitch(ctx, a.sessionID, memory.SessionSwitchOptions{ParentSessionID: boundaryParent, Reason: "compression"})
		}
		return nil
	})
	compressor := a.contextCompressor
	if cc := compressor.Base().CompressionCount; cc >= 2 {
		msg := fmt.Sprintf("%s⚠️  Session compressed %d times — accuracy may degrade. Consider /new to start fresh.", a.logPrefix, cc)
		a.compressionWarning = msg
		a.emitDiagnosticStatus(msg)
	}
	if a.eventCallback != nil {
		swallowCompression("event_callback error on session:compress: %s", false, func() error {
			a.eventCallback("session:compress", map[string]any{
				"platform": a.platform, "session_id": a.sessionID, "old_session_id": oldSID, "in_place": inPlace,
				"compression_count": compressor.Base().CompressionCount,
			})
			return nil
		})
	}
	inPlaceFlag := compactedInPlace
	a.lastCompressionAttemptInPlace = &inPlaceFlag
	a.lastCompactionInPlace = compactedInPlace
	estimate := estimateRequestRowsRough(ctx, compressed, newSystemPrompt, toolDefsOf(a.tools))
	if err := setEngineAttr(compressor, "last_compression_rough_tokens", estimate); err != nil {
		return 0, err
	}
	compressor.Base().LastPromptTokens = -1
	compressor.Base().LastCompletionTokens = 0
	if err := setEngineAttr(compressor, "awaiting_real_usage_after_compression", true); err != nil {
		return 0, err
	}
	a.setUsageAnchor(nil, false)
	if madeProgress {
		switch c := compressor.(type) {
		case *ContextCompressor:
			c.recordCompletedCompaction(usedFallback, feasibilitySkip)
		case *hostedContextEngine:
			if c.obj.HasMethod("record_completed_compaction") {
				if _, err := c.callErr("record_completed_compaction", nil, map[string]any{"used_fallback": usedFallback, "feasibility_skip": feasibilitySkip}); err != nil {
					return 0, err
				}
			} else if err := setEngineAttr(c, "_verify_compaction_cleared_threshold", true); err != nil {
				return 0, err
			}
		}
	}
	resetReadDedupCaches(ctx, taskID, a.sessionID)
	return estimate, nil
}

// Rejects an unusable candidate before any session mutation, each branch with its own warning and record
func candidateRejected(ctx context.Context, a *AIAgent, compressed, messages, messagesBefore []map[string]any, attempt *compressionAttempt) bool {
	sid := a.sessionID
	if sid == "" {
		sid = "none"
	}
	compressor := a.contextCompressor
	if pyrepr.Truthy(compressorAttr(compressor, "_last_compress_aborted")) {
		summaryError := compressorAttr(compressor, "_last_summary_error")
		errText := "unknown error"
		if pyrepr.Truthy(summaryError) {
			errText = pyStr(summaryError)
		}
		if a.lastCompressionSummaryWarning == nil || *a.lastCompressionSummaryWarning != errText {
			a.lastCompressionSummaryWarning = &errText
			a.emitWarning(fmt.Sprintf("⚠ Compression aborted: %s. No messages were dropped — conversation continues unchanged. Run /compress to retry, or /new to start a fresh session.", errText))
		}
		failureClass := ""
		if pyrepr.Truthy(summaryError) {
			failureClass = "summary_generation_aborted"
		}
		emitAbortedAttemptTelemetry(ctx, a, attempt.startedAt, failureClass, attempt.seed)
		return true
	}
	if reflect.DeepEqual(compressed, messagesBefore) || reflect.DeepEqual(stripMarkerForComparison(compressed), stripMarkerForComparison(messagesBefore)) {
		compressionLog().Info(fmt.Sprintf("Compression made no progress (session=%s) — skipping boundary rewrite.", sid))
		swallowCompression("no-progress backoff arm failed", true, func() error {
			if c, ok := compressor.(*ContextCompressor); ok {
				c.recordStructuralNoOp("compaction returned the transcript unchanged (no_progress)", 0)
			}
			return nil
		})
		emitAbortedAttemptTelemetry(ctx, a, attempt.startedAt, "no_progress", attempt.seed)
		return true
	}
	if len(compressed) == 0 {
		compressionLog().Error(fmt.Sprintf("context compression returned an empty transcript; refusing to rotate session=%s so the parent remains resumable", sid))
		swallowCompression("empty transcript warning failed", false, func() error {
			a.emitWarning("⚠ Compression returned an empty transcript. No session split was performed; conversation continues unchanged.")
			return nil
		})
		emitAbortedAttemptTelemetry(ctx, a, attempt.startedAt, "empty_transcript", attempt.seed)
		return true
	}
	if !workingAttemptIsCurrent(compressor, attempt.generation) {
		var working any
		if owned, ok := compressor.(attemptOwnedCompressor); ok {
			compressorAttemptLock.Lock()
			if slots := owned.attemptSlotsLocked(false); slots != nil && slots.workingGeneration != nil {
				working = *slots.workingGeneration
			}
			compressorAttemptLock.Unlock()
		}
		compressionLog().Warn(fmt.Sprintf("Discarding late compression candidate: attempt generation %d was superseded by a newer working attempt (current working: %s) (session=%s).", attempt.generation, pyStr(working), sid))
		a.lastCompactionInPlace = false
		emitAbortedAttemptTelemetry(ctx, a, attempt.startedAt, "attempt_superseded", attempt.seed)
		return true
	}
	return false
}

// In place commit cap over the held rows, the lease watermark compared like Python when it is absent
func heldWatermark(ctx context.Context, a *AIAgent, watermark *int64, messages, verbatimTail []map[string]any) (*int64, error) {
	if watermark == nil {
		var last map[string]any
		copied := false
		for _, m := range messages {
			if m != nil {
				last = m
			}
		}
		for _, m := range verbatimTail {
			if m != nil {
				last, copied = m, true
			}
		}
		if last == nil {
			return nil, nil
		}
		if _, exact := exactHeldRowID(last, copied); !exact {
			return nil, nil
		}
		return nil, errors.New("TypeError: '>=' not supported between instances of 'int' and 'NoneType'")
	}
	return heldArchiveWatermark(ctx, a.sessionDB, a.sessionID, *watermark, messages, verbatimTail, false)
}

// Persists the compacted transcript in place or through the parent to child rotation, rolling back on failure
func commitCompaction(ctx context.Context, a *AIAgent, messages *[]map[string]any, compressed []map[string]any, inPlace bool, lease *compressionLease, newSystemPrompt string, systemMessage *string, userTurnOutcome string, messagesBefore []map[string]any, madeProgress bool, attempt *compressionAttempt, verbatimTail, carriedMessages []map[string]any) (commitOutcome, error) {
	out := commitOutcome{compressed: compressed, commitStartedAt: time.Now(), splitStatus: "not_applicable", madeProgress: madeProgress}
	if a.sessionDB == nil {
		return out, nil
	}
	out.splitStatus = "pending"
	var oldSessionID *string
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		tailTagged := map[any]bool{}
		for _, m := range compressed {
			if m == nil {
				continue
			}
			if v, had := m[compactionTailMarker]; had {
				delete(m, compactionTailMarker)
				if pyrepr.Truthy(v) {
					tailTagged[rowIdentity(m)] = true
				}
			}
		}
		original := messagesBefore
		if original == nil {
			original = *messages
		}
		salvaged, refused, err := salvageOrRefuseGrownTranscript(ctx, a, original, compressed, systemMessage, attempt)
		if err != nil {
			return err
		}
		if salvaged == nil {
			out = commitOutcome{compressed: *messages, refusedPrompt: refused, commitStartedAt: out.commitStartedAt, splitStatus: "not_applicable"}
			return nil
		}
		compressed = salvaged
		restoreTelemetry := holdAttemptTelemetry(a.contextCompressor)
		func() {
			defer restoreTelemetry()
			a.CommitMemorySession(ctx, original)
		}()
		live := *messages
		if inPlace {
			tailCount := 0
			for _, m := range compressed {
				if m != nil && tailTagged[rowIdentity(m)] {
					tailCount++
				}
			}
			tailHeld := live[max(0, len(live)-tailCount):]
			if idx := a.persistUserMessageIdx; a.activeSessionTurnLeaseHolder != nil && idx != nil && *idx >= 0 && *idx < len(live) {
				for _, m := range live[max(*idx, len(live)-tailCount):] {
					if m != nil && !pyrepr.Truthy(m[dbPersistedMarker]) && !isPyInt(m["_row_id"]) {
						tailCount--
					}
				}
			}
			absorbed := map[int64]bool{}
			for _, m := range tailHeld {
				if m == nil {
					continue
				}
				for _, raw := range anyList(m[state.AbsorbedRowIDs]) {
					rid, ok := state.PositiveID(raw)
					if !ok {
						continue
					}
					_, live, err := a.sessionDB.GetMessageRole(ctx, a.sessionID, rid)
					if err != nil {
						return err
					}
					if live {
						absorbed[rid] = true
					}
				}
			}
			tailCount += len(absorbed)
			persisted := compressed
			if len(verbatimTail) > 0 {
				persisted = provider.RejoinCompressedHeadAndTail(compressed, verbatimTail)
				tailCount += len(verbatimTail)
			}
			covered, unresolved, ok, err := state.CoverageForCommit(a.sessionDB, a.sessionID, original, verbatimTail)
			if err != nil {
				return err
			}
			watermark, err := heldWatermark(ctx, a, lease.watermark, live, verbatimTail)
			if err != nil {
				return err
			}
			o := state.ArchiveCompactOptions{
				ModelConfigPatch: pyjson.ObjectOf(ProactivePruneRearmModelConfigKey, nil), Watermark: watermark,
				LockHolder: lease.holder, TailCount: tailCount, CarriedMessages: carriedMessages,
			}
			if ok {
				o.CoveredIDs, o.UnresolvedHeld = covered, unresolved
			}
			if _, err := a.sessionDB.ArchiveAndCompact(ctx, a.sessionID, persisted, o); err != nil {
				return err
			}
			compressed = persisted
			out.splitStatus = "in_place_committed"
			StampDBPersistedMarkers(slices.Concat(compressed, verbatimTail))
			a.flushedDBMessageIDs = map[any]bool{}
			out.compactedInPlace = true
			prompt := newSystemPrompt
			if err := a.sessionDB.UpdateSystemPrompt(ctx, a.sessionID, &prompt); err != nil {
				return err
			}
			a.lastFlushedDBIdx = 0
		} else {
			sid := a.sessionID
			oldSessionID = &sid
			if len(verbatimTail) > 0 {
				compressed = provider.RejoinCompressedHeadAndTail(compressed, verbatimTail)
			}
			if err := publishRotatedCompaction(ctx, a, live, compressed, newSystemPrompt, lease, sid, userTurnOutcome); err != nil {
				return err
			}
			if len(verbatimTail) > 0 {
				StampDBPersistedMarkers(verbatimTail)
			}
			out.splitStatus = "rotated_committed"
			a.lastFlushedDBIdx = len(compressed)
			childSID := a.sessionID
			a.flushedDBMessageSessionID = &childSID
		}
		out.sessionCommitSucceeded = true
		return nil
	}()
	if out.refusedPrompt != nil {
		return out, nil
	}
	if err != nil {
		rotationRollback := !inPlace && oldSessionID != nil && a.sessionID == *oldSessionID
		if rotationRollback || (inPlace && out.splitStatus != "in_place_committed" && messagesBefore != nil) {
			if rotationRollback {
				oldSessionID = nil
			}
			*messages, _ = pycopy.DeepCopy(messagesBefore).([]map[string]any)
			compressed = *messages
			out.madeProgress = false
			restorePruneRearmTokens(a.contextCompressor, attempt.snapshot)
		}
		if oldSessionID == nil && !inPlace {
			out.splitStatus = "aborted"
			sid := a.sessionID
			if sid == "" {
				sid = "?"
			}
			compressionLog().Warn(fmt.Sprintf("Compression rotation aborted and rolled back to the parent session (%s): %s", sid, err))
		} else {
			out.splitStatus = "failed_not_indexed"
			compressionLog().Warn(fmt.Sprintf("Session DB compression split failed — new session will NOT be indexed: %s", err))
		}
		swallowCompression("could not record split-failure cooldown", true, func() error {
			if c, ok := a.contextCompressor.(*ContextCompressor); ok {
				text := "session_split_failed: " + err.Error()
				c.recordCompressionFailureCooldown(splitFailureCooldownSeconds, &text)
			}
			return nil
		})
	}
	out.compressed = compressed
	out.oldSessionID = oldSessionID
	return out, nil
}

// Adopts a grown parent, gathers memory context and runs the summarizer, a hard cancel aborting with a stall record
func runSummaryPhase(ctx context.Context, a *AIAgent, messages []map[string]any, lease *compressionLease, inPlace, checkpointRequired bool, approxTokens *int, focusTopic *string, force, bypassCooldown bool, fence *CompressionCommitFence, hardCancel provider.AuxCancelEvent, systemMessage *string, attempt *compressionAttempt) (phase summaryPhase, err error) {
	preMsgCount := len(messages)
	var heartbeat *compressionActivityHeartbeat
	var before []map[string]any
	stopHeartbeat := func(desc string) {
		if heartbeat != nil {
			heartbeat.stop(desc)
			heartbeat = nil
		}
	}
	defer stopHeartbeat("context compression completed")
	failed := func(cause any) {
		restoreMessagesSnapshot(&messages, before)
		stopHeartbeat("context compression failed")
		lease.release()
		emitAbortedAttemptTelemetry(ctx, a, attempt.startedAt, "exception:"+compressionErrorClass(ctx, cause), attempt.seed)
	}
	handled := false
	defer func() {
		if handled {
			return
		}
		if r := recover(); r != nil {
			failed(r)
			panic(r)
		}
	}()
	compressed, err := func() ([]map[string]any, error) {
		lease.startRefresher()
		if !inPlace {
			adopted, err := adoptGrownDurableParent(ctx, a, lease, messages)
			if err != nil {
				return nil, err
			}
			if adopted != nil {
				messages = adopted
				preMsgCount = len(messages)
				zero, idx := 0, len(messages)
				approxTokens = &zero
				a.persistUserMessageIdx = &idx
			}
		}
		memoryContext, err := preCompressMemoryContext(ctx, a, messages, checkpointRequired)
		if err != nil {
			return nil, err
		}
		call := resolveCompressCall(a, approxTokens, focusTopic, force, memoryContext, bypassCooldown)
		before, _ = pycopy.DeepCopy(messages).([]map[string]any)
		heartbeat = newCompressionActivityHeartbeat(a, nil, lease.statusEmitted(), fence).start()
		for i, value := range []any{nil, nil, attempt.seed.Copy()} {
			swallowCompression("attempt telemetry seed failed", false, func() error {
				return setEngineAttr(a.contextCompressor, attemptTelemetryFields[i], value)
			})
		}
		return runSummaryDispatch(ctx, a, messages, call, fence, attempt.generation, hardCancel)
	}()
	handled = true
	var explicit *provider.AuxiliaryExplicitCancellation
	if errors.As(err, &explicit) {
		var rollbackPanic any
		rollbackErr := func() (rbErr error) {
			defer func() {
				if r := recover(); r != nil {
					rollbackPanic = r
					rbErr = fmt.Errorf("%v", r)
				}
			}()
			return attempt.restoreCompressor(a.contextCompressor)
		}()
		if rollbackErr != nil {
			restoreMessagesSnapshot(&messages, before)
			stopHeartbeat("context compression rollback failed")
			lease.release()
			cause := any(rollbackErr)
			if rollbackPanic != nil {
				cause = rollbackPanic
			}
			emitAbortedAttemptTelemetry(ctx, a, attempt.startedAt, "rollback:"+compressionErrorClass(ctx, cause), attempt.seed)
			if rollbackPanic != nil {
				panic(rollbackPanic)
			}
			return phase, rollbackErr
		}
		restoreMessagesSnapshot(&messages, before)
		stallBackoff := recordStallInterruptedBackoff(ctx, a, fence, monotonicOf(attempt.startedAt), messages, approxTokens)
		stopHeartbeat("context compression cancelled")
		lease.release()
		hostCancel := fence != nil && fence.IsCancelled() && !(hardCancel != nil && hardCancel.IsSet())
		failureClass := "explicit_interrupt"
		switch {
		case stallBackoff:
			failureClass = StallInterruptedFailureClass
		case hostCancel:
			failureClass = "commit_fence_cancelled"
		}
		emitAbortedAttemptTelemetry(ctx, a, attempt.startedAt, failureClass, attempt.seed)
		prompt, err := existingSystemPrompt(ctx, a, systemMessage)
		return summaryPhase{messages: messages, abortPrompt: &prompt}, err
	}
	if err != nil {
		failed(err)
		return phase, err
	}
	return summaryPhase{messages: messages, compressed: compressed, messagesBeforeCompression: before, approxTokens: approxTokens, preMsgCount: preMsgCount}, nil
}

// Snapshots and claims the compressor, clears the per attempt signals and opens the shared metric lifecycle
func beginCompressionAttempt(ctx context.Context, a *AIAgent, force, deferNotification bool, trigger *string, approxTokens *int, overflowReason *string) (context.Context, *compressionAttempt, error) {
	snapshot := snapshotCompressorAttemptState(a.contextCompressor)
	generation := claimCompressorAttempt(a.contextCompressor)
	if deferNotification && a.pendingContextEngineCompressionNotification != nil {
		return ctx, nil, errors.New("a compression notification is already pending")
	}
	a.lastCompressionAttemptRecorded = true
	a.lastCompressionAttemptInPlace = nil
	a.compressionSkippedDueToLock = nil
	a.compressionBlockedTransient = nil
	startedAt := time.Now()
	attemptID := strings.ReplaceAll(uuid.NewString(), "-", "")
	triggerSource := "auto"
	if force {
		triggerSource = "manual"
	}
	if trigger != nil && *trigger != "" {
		triggerSource = *trigger
	}
	seed := pyjson.ObjectOf("attempt_id", attemptID, "session_id", a.sessionID, "trigger_source", triggerSource, "overflow_reason", optionalStringAny(overflowReason))
	a.compressionAttemptID = attemptID
	owner, ok := compressionMetricOwner(ctx)
	if !ok {
		owner = &compressionMetricWorker{}
		ctx = withCompressionMetricOwner(ctx, owner)
	}
	var tokensBefore any
	if approxTokens != nil && *approxTokens != 0 {
		tokensBefore = *approxTokens
	} else if a.contextCompressor != nil {
		tokensBefore = a.contextCompressor.Base().LastPromptTokens
	}
	observability.BeginCompressionAttempt(owner, triggerSource, tokensBefore)
	return ctx, &compressionAttempt{snapshot: snapshot, generation: generation, startedAt: startedAt, seed: seed}, nil
}

func optionalStringAny(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Lets codex run its own compact inside the commit fence bracket
func routeCodexCompaction(ctx context.Context, a *AIAgent, messages []map[string]any, systemMessage *string, fence *CompressionCommitFence, attempt *compressionAttempt, approxTokens *int, taskID string, force bool) ([]map[string]any, string, error) {
	if fence != nil && !fence.BeginCommit(cancelEventArg(hardCancelEventOf(a))) {
		if err := attempt.restoreCompressor(a.contextCompressor); err != nil {
			return nil, "", err
		}
		emitBypassedAttemptTelemetry(ctx, a, attempt.startedAt, attemptBypass{CommitStatus: "aborted", FailureClass: "commit_fence_cancelled",
			ApproxTokens: optionalIntAny(approxTokens), Route: "codex_app_server", AttemptSeed: attempt.seed})
		prompt, err := existingSystemPrompt(ctx, a, systemMessage)
		return messages, prompt, err
	}
	if fence != nil {
		defer fence.FinishCommit()
	}
	return compressContextViaCodexAppServer(ctx, a, messages, systemMessage, approxTokens, taskID, force, attempt.startedAt, attempt.seed)
}

// Re resolves a rotating Anthropic OAuth token so the summary call never carries a revoked one
func refreshMainCredential(ctx context.Context, a *AIAgent) {
	if a.apiMode != "anthropic_messages" {
		return
	}
	swallowCompression("pre-compression credential refresh failed: %s", false, func() error {
		_, err := a.tryRefreshAnthropicClientCredentials(ctx)
		return err
	})
}

// Logs the attempt, emits the engine customisable compacting status and returns the lifecycle
func announceCompressionStart(a *AIAgent, messageCount int, approxTokens *int, focusTopic *string, force bool) *compactionLifecycle {
	sid := a.sessionID
	if sid == "" {
		sid = "none"
	}
	tokens := "unknown"
	if approxTokens != nil && *approxTokens != 0 {
		tokens = groupThousands(*approxTokens)
	}
	compressionLog().Info(fmt.Sprintf("context compression started: session=%s messages=%d tokens=~%s model=%s focus=%s", sid, messageCount, tokens, a.model, pyrepr.Repr(optionalStringAny(focusTopic))))
	status := CompactionStatus
	if !force {
		msg := AutomaticCompactionStatusMessage(a.contextCompressor, "compress", status, map[string]any{
			"approx_tokens": optionalIntAny(approxTokens), "message_count": messageCount, "model": a.model, "focus_topic": optionalStringAny(focusTopic),
		})
		status = ""
		if msg != nil {
			status = *msg
		}
	}
	if status != "" {
		a.emitStatus(status)
	}
	return newCompactionLifecycle(a, status != "")
}

// Compresses the transcript and splits or rewrites the session, the input handed back unchanged on any abort
func compressContext(ctx context.Context, a *AIAgent, messages []map[string]any, systemMessage *string, o compressContextOptions) (_ []map[string]any, _ string, retErr error) {
	if o.TaskID == "" {
		o.TaskID = "default"
	}
	ctx, attempt, err := beginCompressionAttempt(ctx, a, o.Force, o.DeferContextEngineNotification, o.Trigger, o.ApproxTokens, o.OverflowReason)
	if err != nil {
		return nil, "", err
	}
	existing := func() ([]map[string]any, string, error) {
		prompt, err := existingSystemPrompt(ctx, a, systemMessage)
		return messages, prompt, err
	}
	checkpointRequired := a.compressionCheckpointRequired
	if a.apiMode == "codex_app_server" {
		if checkpointRequired {
			blocked := provider.CheckpointBlocked("codex_app_server owns the authoritative thread and does not expose a truthful pre-compaction transcript boundary")
			emitBypassedAttemptTelemetry(ctx, a, attempt.startedAt, attemptBypass{CommitStatus: "aborted", FailureClass: "exception:" + compressionErrorClass(ctx, blocked),
				ApproxTokens: optionalIntAny(o.ApproxTokens), Route: "codex_app_server", AttemptSeed: attempt.seed})
			return nil, "", blocked
		}
		return routeCodexCompaction(ctx, a, messages, systemMessage, o.CommitFence, attempt, o.ApproxTokens, o.TaskID, o.Force)
	}
	if !o.Force {
		if reason := automaticCompressionGateBlocks(a, o.BypassCooldown, true); reason != nil {
			emitBlockedAttemptTelemetry(ctx, a, attempt.startedAt, optionalIntAny(o.ApproxTokens), attempt.seed, *reason)
			return existing()
		}
	}
	inPlace := a.compressionInPlace
	lifecycle := announceCompressionStart(a, len(messages), o.ApproxTokens, o.FocusTopic, o.Force)
	refreshMainCredential(ctx, a)
	if !a.compressionFeasibilityChecked {
		if err := checkCompressionModelFeasibility(ctx, a); err != nil {
			lifecycle.complete(true)
			return nil, "", err
		}
		a.compressionFeasibilityChecked = true
	}
	lease, abortPrompt, err := acquireCompressionLease(ctx, a, o.CommitFence, lifecycle, systemMessage, attempt)
	if err != nil {
		return nil, "", err
	}
	if lease == nil {
		return messages, abortPrompt, nil
	}
	lease.finishLockSetup()
	current, err := func() (cur bool, err error) {
		defer func() {
			if r := recover(); r != nil {
				lease.release()
				panic(r)
			}
		}()
		if o.SnapshotIsCurrent == nil {
			return true, nil
		}
		return o.SnapshotIsCurrent()
	}()
	if err != nil {
		lease.release()
		return nil, "", err
	}
	if !current {
		emitAbortedAttemptTelemetry(ctx, a, attempt.startedAt, "snapshot_stale", attempt.seed)
		lease.release()
		return existing()
	}
	adopted, adoptedPrompt, done, err := adoptIfParentRotated(ctx, a, lease, messages, systemMessage, attempt, optionalIntAny(o.ApproxTokens))
	if err != nil {
		return nil, "", err
	}
	if done {
		return adopted, adoptedPrompt, nil
	}
	attempt.durableCooldownAuthoritative, attempt.durableCooldownState = captureAuthoritativeCooldownUnderLease(a.contextCompressor, attempt.snapshot)
	if attempt.durableCooldownAuthoritative != nil && !*attempt.durableCooldownAuthoritative {
		lease.release()
		emitBypassedAttemptTelemetry(ctx, a, attempt.startedAt, attemptBypass{CommitStatus: "aborted", FailureClass: "cooldown_state_unreadable",
			ApproxTokens: optionalIntAny(o.ApproxTokens), AttemptSeed: attempt.seed})
		return existing()
	}
	if !o.Force {
		if reason := automaticCompressionGateBlocks(a, o.BypassCooldown, false); reason != nil {
			lease.release()
			emitBlockedAttemptTelemetry(ctx, a, attempt.startedAt, optionalIntAny(o.ApproxTokens), attempt.seed, *reason)
			return existing()
		}
	}
	hardCancel := hardCancelEventOf(a)
	phase, err := runSummaryPhase(ctx, a, messages, lease, inPlace, checkpointRequired, o.ApproxTokens, o.FocusTopic, o.Force, o.BypassCooldown, o.CommitFence, hardCancel, systemMessage, attempt)
	if err != nil {
		return nil, "", err
	}
	if phase.abortPrompt != nil {
		return phase.messages, *phase.abortPrompt, nil
	}
	messages, compressed := phase.messages, phase.compressed
	before := phase.messagesBeforeCompression
	approxTokens, preMsgCount := phase.approxTokens, phase.preMsgCount
	effectBefore := snapshotCompressionEffectInput(before, o.VerbatimTail)
	fenceEntered := false
	defer func() {
		defer func() {
			if fenceEntered {
				o.CommitFence.FinishCommit()
			}
		}()
		lease.release()
	}()
	compressor := a.contextCompressor
	madeProgress := pyrepr.Truthy(compressorAttr(compressor, "_last_compression_made_progress"))
	usedFallback := pyrepr.Truthy(compressorAttr(compressor, "_last_summary_fallback_used"))
	feasibilitySkip := pyrepr.Truthy(compressorAttr(compressor, "_last_feasibility_skip"))
	if candidateRejected(ctx, a, compressed, messages, before, attempt) {
		restoreMessagesSnapshot(&messages, before)
		return existing()
	}
	if o.CommitFence != nil {
		fenceEntered = o.CommitFence.BeginCommit(cancelEventArg(hardCancel))
		if !fenceEntered {
			if err := attempt.restoreCompressor(compressor); err != nil {
				return nil, "", err
			}
			restoreMessagesSnapshot(&messages, before)
			sid := a.sessionID
			if sid == "" {
				sid = "none"
			}
			compressionLog().Info(fmt.Sprintf("Compression commit cancelled before session mutation (session=%s).", sid))
			a.lastCompactionInPlace = false
			stallBackoff := recordStallInterruptedBackoff(ctx, a, o.CommitFence, monotonicOf(attempt.startedAt), messages, approxTokens)
			_, prompt, err := existing()
			failureClass := "commit_fence_cancelled"
			if stallBackoff {
				failureClass = StallInterruptedFailureClass
			}
			emitAbortedAttemptTelemetry(ctx, a, attempt.startedAt, failureClass, attempt.seed)
			return messages, prompt, err
		}
	}
	warnSummaryOrAuxFallback(a)
	var reinsertedReply map[string]any
	if len(o.VerbatimTail) == 0 {
		compressed, reinsertedReply = ensureCompressedKeepsLastAssistantReply(ctx, messages, compressed, a.sessionID)
	}
	if reinsertedReply != nil {
		sid := a.sessionID
		if sid == "" {
			sid = "none"
		}
		compressionLog().Info(fmt.Sprintf("Compression: engine folded away the just-delivered assistant reply; reinserted it into the active set (session=%s).", sid))
	}
	if err := foldTodoSnapshot(a, &compressed); err != nil {
		return nil, "", err
	}
	userTurnOutcome := ensureCompressedHasUserTurn(messages, &compressed)
	newSystemPrompt, err := rebuildSystemPromptAtBoundary(ctx, a, systemMessage)
	if err != nil {
		return nil, "", err
	}
	var carried []map[string]any
	if reinsertedReply != nil {
		carried = []map[string]any{reinsertedReply}
	}
	commit, err := commitCompaction(ctx, a, &messages, compressed, inPlace, lease, newSystemPrompt, systemMessage, userTurnOutcome, before, madeProgress, attempt, o.VerbatimTail, carried)
	if err != nil {
		return nil, "", err
	}
	if commit.refusedPrompt != nil {
		restoreMessagesSnapshot(&messages, before)
		return messages, *commit.refusedPrompt, nil
	}
	compressed = commit.compressed
	splitStatus := commit.splitStatus
	estimate, err := finishCompactionBoundary(ctx, a, compressed, newSystemPrompt, commit.oldSessionID, inPlace, commit.compactedInPlace, commit.sessionCommitSucceeded,
		o.DeferContextEngineNotification, commit.madeProgress, usedFallback, feasibilitySkip, o.TaskID)
	if err != nil {
		return nil, "", err
	}
	sid := a.sessionID
	if sid == "" {
		sid = "none"
	}
	compressionLog().Info(fmt.Sprintf("context compression done: session=%s messages=%d->%d rough_tokens=~%s awaiting_real_usage=true", sid, preMsgCount, len(compressed), groupThousands(estimate)))
	lifecycle.commitStatus = "aborted"
	if splitStatus == "not_applicable" || splitStatus == "in_place_committed" || splitStatus == "rotated_committed" {
		lifecycle.commitStatus = "committed"
	}
	rewritten := commit.compactedInPlace || !sameRowList(compressed, messages)
	if rewritten || lifecycle.commitStatus == "committed" {
		recordCommittedAttemptEffect(ctx, a, effectBefore, compressed, o.VerbatimTail, splitStatus)
	}
	failureClass := ""
	if splitStatus == "failed_not_indexed" || splitStatus == "aborted" {
		failureClass = "session_split_failed"
	}
	commitStartedAt := commit.commitStartedAt
	emitCompressionAttemptTelemetry(ctx, a, attemptTelemetryEmit{StartedAt: attempt.startedAt, CommitStatus: lifecycle.commitStatus, SplitStatus: splitStatus,
		FailureClass: failureClass, CommitStartedAt: &commitStartedAt, AttemptSeed: attempt.seed, HistoryRewritten: &rewritten})
	return compressed, newSystemPrompt, nil
}
