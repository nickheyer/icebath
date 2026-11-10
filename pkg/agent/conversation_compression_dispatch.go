package agent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/tools"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// compressCall is the engine compress bound to only the keywords its signature accepts
type compressCall struct {
	engine ContextEngine
	kwargs map[string]any
}

type contextAwareEngine interface {
	CompressContext(ctx context.Context, messages []map[string]any, currentTokens *int, focusTopic *string, force bool, memoryContext string) ([]map[string]any, error)
}

// Runs the bound compress with the accepted keywords, dropped ones at their defaults
func (c compressCall) run(ctx context.Context, messages []map[string]any) ([]map[string]any, error) {
	currentTokens, _ := c.kwargs["current_tokens"].(*int)
	focusTopic, _ := c.kwargs["focus_topic"].(*string)
	force, _ := c.kwargs["force"].(bool)
	memoryContext, _ := c.kwargs["memory_context"].(string)
	bypass, _ := c.kwargs["bypass_cooldown"].(bool)
	if e, ok := c.engine.(cooldownBypassingEngine); ok {
		return e.CompressBypassingCooldown(ctx, messages, currentTokens, focusTopic, force, memoryContext, bypass)
	}
	if e, ok := c.engine.(contextAwareEngine); ok {
		return e.CompressContext(ctx, messages, currentTokens, focusTopic, force, memoryContext)
	}
	return c.engine.Compress(messages, currentTokens, focusTopic, force, memoryContext)
}

// Binds compress with its accepted keywords and warns once per engine that drops memory context
func resolveCompressCall(a *AIAgent, approxTokens *int, focusTopic *string, force bool, memoryContext string, bypassCooldown bool) compressCall {
	engine := a.contextCompressor
	kwargs := supportedCompressionKwargs(engine, approxTokens, focusTopic, force, memoryContext, bypassCooldown)
	if _, ok := kwargs["memory_context"]; strings.TrimSpace(memoryContext) != "" && !ok {
		name := engine.Name()
		if name == "" {
			t := reflect.TypeOf(engine)
			for t.Kind() == reflect.Pointer {
				t = t.Elem()
			}
			name = t.Name()
		}
		if a.lastMemoryContextUnsupportedEngine == nil || *a.lastMemoryContextUnsupportedEngine != name {
			a.lastMemoryContextUnsupportedEngine = &name
			compressionLog().Warn(fmt.Sprintf("context engine %s does not accept memory_context; continuing without provider-supplied summary context", name))
		}
	}
	return compressCall{engine: engine, kwargs: kwargs}
}

// Runs the compressor under the fence progress hook, the host deadline and the interrupt guard
func runSummaryDispatch(ctx context.Context, a *AIAgent, messages []map[string]any, call compressCall, fence *CompressionCommitFence, attemptGeneration int, hardCancel provider.AuxCancelEvent) (compressed []map[string]any, err error) {
	progressHook := func() error { return nil }
	if fence != nil {
		progressHook = func() error { fence.TouchProgress(); return nil }
		installCompressionCancelledCheck(a.contextCompressor, fence.IsCancelled, attemptGeneration)
	}
	cancelRequested := provider.AuxCancelFunc(func() (bool, error) {
		return (hardCancel != nil && hardCancel.IsSet()) || (fence != nil && fence.IsCancelled()), nil
	})
	defer func() {
		if fence != nil {
			clearCompressionCancelledCheckIfOwner(a.contextCompressor, attemptGeneration)
		}
	}()
	attemptCtx := withCompressorAttemptGeneration(ctx, attemptGeneration)
	if fence != nil && fence.IsCancelled() {
		sid := a.sessionID
		if sid == "" {
			sid = "none"
		}
		compressionLog().Info(fmt.Sprintf("Compression cancelled before summary dispatch (session=%s) — skipping summary work.", sid))
		return messages, nil
	}
	scoped := provider.AuxProgressHook(attemptCtx, progressHook)
	scoped = CompressionDeadlineScope(scoped, commitFenceForDeadline(fence))
	scoped = provider.AuxInterruptProtection(scoped, true, cancelRequested, nil)
	markCompressorWorkingAttempt(a.contextCompressor, attemptGeneration)
	compressed, err = call.run(scoped, messages)
	if err != nil {
		return nil, err
	}
	if hardCancel != nil && hardCancel.IsSet() {
		return nil, &provider.AuxiliaryExplicitCancellation{}
	}
	return compressed, nil
}

// Typed nil fence reads as no fence for the deadline scope
func commitFenceForDeadline(fence *CompressionCommitFence) CompressionDeadlineFence {
	if fence == nil {
		return nil
	}
	return fence
}

// Repairs compressed rows in place and keeps the flushed cursor on the rows it already wrote
func repairCompressedRows(a *AIAgent, rows *[]map[string]any) (int, error) {
	origin := make(map[uintptr]int, len(*rows))
	stamped := map[uintptr]bool{}
	for i, row := range *rows {
		key := reflect.ValueOf(row).Pointer()
		origin[key] = i
		if pyTruthy(row[state.DBPersistedMarker]) {
			stamped[key] = true
		}
	}
	cursor := a.lastFlushedDBIdx
	repairs, err := state.RepairMessageSequence(rows)
	if err != nil || repairs == 0 {
		return repairs, err
	}
	flushed, lostMarker := 0, false
	for _, row := range *rows {
		key := reflect.ValueOf(row).Pointer()
		idx, known := origin[key]
		if !known {
			continue
		}
		if cursor > 0 && idx < cursor {
			flushed++
		}
		if stamped[key] && !pyTruthy(row[state.DBPersistedMarker]) {
			lostMarker = true
		}
	}
	if lostMarker {
		a.dbFlushScanPrefix = nil
	}
	if cursor > 0 {
		a.lastFlushedDBIdx = flushed
	} else {
		a.lastFlushedDBIdx = min(a.lastFlushedDBIdx, len(*rows))
	}
	return repairs, nil
}

// Strips stale todo snapshots from the compressed rows and folds the live one in
func foldTodoSnapshot(a *AIAgent, compressed *[]map[string]any) error {
	snapshot := ""
	authoritative := false
	if a.todoStore != nil {
		snapshot = a.todoStore.FormatForInjection()
		func() {
			defer func() { _ = recover() }()
			authoritative = a.todoStore.HasItems()
		}()
	}
	if authoritative {
		rows := *compressed
		for idx := len(rows) - 1; idx >= 0; idx-- {
			msg := rows[idx]
			if msg == nil || msg["role"] != "user" {
				continue
			}
			content := msg["content"]
			stripped := stripStaleTodoSnapshot(content)
			if pyEqual(stripped, content) {
				continue
			}
			if pyTruthy(msg["_todo_snapshot_synthetic"]) && todoSnapshotIsOnlyContent(content, stripped) {
				*compressed = append(rows[:idx:idx], rows[idx+1:]...)
				if idx < len(*compressed) {
					if _, err := repairCompressedRows(a, compressed); err != nil {
						return err
					}
				}
			} else {
				replaceMessageContent(msg, stripped)
				delete(msg, "_todo_snapshot_synthetic")
			}
			break
		}
	}
	if snapshot == "" {
		return nil
	}
	if notice := prunedSkillReloadNotice(*compressed); notice != "" {
		snapshot = snapshot + "\n\n" + notice
	}
	merged := false
	if n := len(*compressed); n > 0 && (*compressed)[n-1] != nil && (*compressed)[n-1]["role"] == "user" {
		tail := (*compressed)[n-1]
		stripped := stripStaleTodoSnapshot(tail["content"])
		probe := make(map[string]any, len(tail))
		for k, v := range tail {
			if k != "content" {
				probe[k] = v
			}
		}
		probe["content"] = stripped
		if isRealUserMessage(probe) {
			text := snapshot
			if s, ok := stripped.(string); ok && s != "" {
				text = "\n\n" + snapshot
			}
			replaceMessageContent(tail, appendTextToContent(stripped, text, false))
			merged = true
		} else if !pyEqual(stripped, tail["content"]) && strings.TrimSpace(compressionMessageText(map[string]any{"role": "user", "content": stripped})) == "" {
			replaceMessageContent(tail, snapshot)
			tail["_todo_snapshot_synthetic"] = true
			merged = true
		}
	}
	if !merged {
		*compressed = append(*compressed, map[string]any{"role": "user", "content": snapshot, "_todo_snapshot_synthetic": true})
	}
	return nil
}

// Refreshes tool schemas and rebuilds the system prompt at the commit boundary
func rebuildSystemPromptAtBoundary(ctx context.Context, a *AIAgent, systemMessage *string) (string, error) {
	if a.retainSeededSystemPrompt {
		if a.cachedSystemPrompt != "" {
			ReconstructStaticPrefix(ctx, a, systemMessage, "compression seeded-prompt")
		}
		return a.cachedSystemPrompt, nil
	}
	cached := a.cachedSystemPrompt
	if err := InvalidateSystemPrompt(tools.WithEnvHome(ctx, a.env), a); err != nil {
		return "", err
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				compressionLog().Warn("compaction tool-definition refresh failed; keeping the session's existing tool snapshot", "error", fmt.Sprint(r))
			}
		}()
		refreshAgentToolDefinitions(ctx, a)
	}()
	rebuilt, err := BuildSystemPrompt(ctx, a, systemMessage)
	if err != nil {
		return "", err
	}
	if cached != "" && rebuilt == cached {
		a.cachedSystemPrompt = cached
		ReconstructStaticPrefix(ctx, a, systemMessage, "compression keep-prompt")
		return cached, nil
	}
	a.cachedSystemPrompt = rebuilt
	if cached != "" {
		sid := a.sessionID
		if sid == "" {
			sid = "none"
		}
		line := fmt.Sprintf("Compaction rebuilt a drifted system prompt (session=%s, %d -> %d chars): builder output changed since the stored snapshot (update, config change, or memory/skills growth)",
			sid, utf8.RuneCountInString(cached), utf8.RuneCountInString(rebuilt))
		if a.compactionPromptDriftLogged {
			compressionLog().Debug(line)
		} else {
			compressionLog().Info(line)
		}
		a.compactionPromptDriftLogged = true
	}
	return rebuilt, nil
}

// rejectedCompactionRecorder records a refused candidate as a strike on engines that count them
type rejectedCompactionRecorder interface{ RecordRejectedCompaction() }

// Proceeds with a shrinking candidate after one salvage pass, else refuses it with the existing prompt
func salvageOrRefuseGrownTranscript(ctx context.Context, a *AIAgent, original, compressed []map[string]any, systemMessage *string, attempt *compressionAttempt) ([]map[string]any, *string, error) {
	roughIn := estimateRowsRough(ctx, original)
	roughOut := estimateRowsRough(ctx, compressed)
	sid := a.sessionID
	if sid == "" {
		sid = "none"
	}
	if roughOut > roughIn {
		budget := roughIn
		if salvaged := SalvageGrownTranscript(ctx, original, compressed, &budget); salvaged != nil {
			if est := estimateRowsRough(ctx, salvaged); est < roughIn {
				compressionLog().Info(fmt.Sprintf("Compression salvage recovered a shrinking transcript (session=%s, ~%s -> ~%s tokens)", sid, groupThousands(roughIn), groupThousands(est)))
				compressed, roughOut = salvaged, est
			}
		}
	}
	if roughOut <= roughIn {
		return compressed, nil, nil
	}
	compressionLog().Warn(fmt.Sprintf("Compression refused: compressed transcript would be larger than the original (session=%s, ~%s -> ~%s tokens); keeping the original transcript unchanged", sid, groupThousands(roughIn), groupThousands(roughOut)))
	if cc, ok := a.contextCompressor.(*ContextCompressor); ok {
		cc.lastCompressRefusedWouldGrow = true
	}
	func() {
		defer func() { _ = recover() }()
		a.emitWarning("⚠️ Compression refused: the generated summary would have GROWN the conversation instead of shrinking it. No messages were dropped — conversation continues unchanged.")
	}()
	existing, err := existingSystemPrompt(ctx, a, systemMessage)
	if err != nil {
		return nil, nil, err
	}
	emitAbortedAttemptTelemetry(ctx, a, attempt.startedAt, "would_grow", attempt.seed)
	swallowCompression("could not record rejected-compaction strike", true, func() error {
		recorder, ok := a.contextCompressor.(rejectedCompactionRecorder)
		if !ok {
			return fmt.Errorf("%T object has no attribute 'record_rejected_compaction'", a.contextCompressor)
		}
		recorder.RecordRejectedCompaction()
		return nil
	})
	restorePruneRearmTokens(a.contextCompressor, attempt.snapshot)
	return nil, &existing, nil
}

// sessionRowReader is the get_session half of a session store
type sessionRowReader interface {
	GetSession(ctx context.Context, sessionID string) (*pyjson.Object, error)
}

// Reports a parent ended by a non automatic reason, failing open on an unreadable row
func parentDeliberatelyEnded(ctx context.Context, sessionDB any, sessionID string) (ended bool) {
	reader, ok := sessionDB.(sessionRowReader)
	if !ok || reflect.ValueOf(sessionDB).IsNil() {
		return false
	}
	defer func() {
		if recover() != nil {
			ended = false
		}
	}()
	row, err := reader.GetSession(ctx, sessionID)
	if err != nil || row == nil {
		return false
	}
	endedAt, has := row.Get("ended_at")
	reason, _ := row.Get("end_reason")
	return has && endedAt != nil && !state.IsAutomaticEndReason(reason)
}

// Migrates goal, heartbeat and loop automation from the parent to the compression child
func carryCompressionAutomation(ctx context.Context, a *AIAgent, oldSessionID string) {
	swallowCompression("Could not migrate goal on compression", false, func() error {
		goals.MigrateGoalToSession(ctx, oldSessionID, a.sessionID, "compression")
		return nil
	})
	swallowCompression("Could not migrate heartbeat on compression", false, func() error {
		migrateHeartbeatToSession(ctx, oldSessionID, a.sessionID)
		return nil
	})
	swallowCompression("Could not migrate loop on compression", false, func() error {
		migrateLoopToSession(ctx, oldSessionID, a.sessionID, "compression")
		return nil
	})
}

// Carries automation, rejected thinking and the title with its provenance onto the child session
func carrySessionStateToChild(ctx context.Context, a *AIAgent, oldSessionID, oldTitle string) {
	carryCompressionAutomation(ctx, a, oldSessionID)
	swallowCompression("Could not carry rejected thinking on compression", false, func() error {
		carryRejectedThinkingToSession(a, oldSessionID)
		return nil
	})
	if oldTitle == "" {
		return
	}
	var src *string
	swallowCompression("Could not read title provenance", false, func() error {
		if a.sessionDB == nil {
			return fmt.Errorf("'NoneType' object has no attribute 'get_session_title_source'")
		}
		s, found, err := a.sessionDB.GetSessionTitleSource(ctx, oldSessionID)
		if err == nil && found {
			src = &s
		}
		return err
	})
	if a.sessionDB == nil {
		compressionLog().Debug("Could not propagate title on compression: 'NoneType' object has no attribute 'set_session_title'")
		return
	}
	if _, err := a.sessionDB.SetSessionTitle(ctx, a.sessionID, oldTitle); err != nil {
		compressionLog().Debug(fmt.Sprintf("Could not propagate title on compression: %v", err))
		return
	}
	if src != nil {
		swallowCompression("Could not propagate title provenance", false, func() error {
			_, err := a.sessionDB.SetSessionTitleSource(ctx, a.sessionID, *src)
			return err
		})
	}
}

// Persisted source of the parent row so a compression child keeps its conversation label
func compressionChildSource(ctx context.Context, a *AIAgent, parentSessionID string) string {
	var parent *pyjson.Object
	if a.sessionDB != nil {
		func() {
			defer func() { _ = recover() }()
			parent, _ = a.sessionDB.GetSession(ctx, parentSessionID)
		}()
	}
	if parent != nil {
		if src, _ := parent.Get("source"); pyTruthy(src) {
			if s, ok := src.(string); ok {
				return s
			}
			return fmt.Sprint(src)
		}
	}
	return provider.SessionSourceFor(a.env, a.platform)
}
