package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// One attempt's entry on the commit fence stack, the stall retry swaps its fence in place
type commitFenceRegistration struct {
	fence *CompressionCommitFence
}

// One compress_context run over a fence and target list, the stall retry bypassing the failure cooldown
type facadeRun func(ctx context.Context, fence *CompressionCommitFence, target []map[string]any, sameTurnFallbackRecovery bool) ([]map[string]any, string, error)

// Seams the facade tests patch like compress_context, the progress timeout runner and the prompt rebuild
var (
	facadeCompressContext        = compressContext
	facadeRunWithProgressTimeout = runCompressContextWithProgressTimeout
	facadeBuildSystemPrompt      = BuildSystemPrompt
)

// Commit fence of the newest live compression attempt, the one a hard interrupt serializes against
func (a *AIAgent) publishedCompressionCommitFence() any {
	a.compressionCommitFenceLock.Lock()
	defer a.compressionCommitFenceLock.Unlock()
	return a.activeCompressionCommitFence
}

// Emits the compaction lifecycle status a provider native compaction boundary announces
func (a *AIAgent) emitCompactionStatus() { a.emitStatus(CompactionStatus) }

// Pushes the attempt onto the fence stack and publishes its fence on the slot
func (a *AIAgent) registerCommitFence(registration *commitFenceRegistration) {
	a.compressionCommitFenceLock.Lock()
	defer a.compressionCommitFenceLock.Unlock()
	a.compressionCommitFenceStack = append(a.compressionCommitFenceStack, registration)
	a.activeCompressionCommitFence = registration.fence
}

// Removes only this attempt's entry so the slot keeps the newest live fence, nil once the stack empties
func (a *AIAgent) unregisterCommitFence(registration *commitFenceRegistration) {
	a.compressionCommitFenceLock.Lock()
	defer a.compressionCommitFenceLock.Unlock()
	stack := a.compressionCommitFenceStack
	for idx := len(stack) - 1; idx >= 0; idx-- {
		if stack[idx] == registration {
			stack = append(stack[:idx:idx], stack[idx+1:]...)
			break
		}
	}
	a.compressionCommitFenceStack = stack
	if len(stack) > 0 {
		a.activeCompressionCommitFence = stack[len(stack)-1].fence
	} else {
		a.activeCompressionCommitFence = nil
	}
}

// Cached prompt, else a fresh build, else the raw system message, never failing
func timeoutFallbackPrompt(ctx context.Context, a *AIAgent, systemMessage *string) (prompt string) {
	if a.cachedSystemPrompt != "" {
		return a.cachedSystemPrompt
	}
	raw := ""
	if systemMessage != nil {
		raw = *systemMessage
	}
	defer func() {
		if r := recover(); r != nil {
			runAgentLog().Debug("compress_context timeout fallback prompt rebuild failed; using raw system_message", "error", r)
			prompt = raw
		}
	}()
	built, err := facadeBuildSystemPrompt(ctx, a, systemMessage)
	if err != nil {
		runAgentLog().Debug("compress_context timeout fallback prompt rebuild failed; using raw system_message", "error", err)
		return raw
	}
	return built
}

// Logs the timeout, stamps activity, arms the timeout cooldown ladder and warns the user
func reportCompressionTimeout(a *AIAgent, idle, waited, sinceProgress, totalCeiling float64, totalExhausted, progressObserved bool) {
	markContextCompressionTimedOut(a)
	if totalExhausted {
		runAgentLog().Warn(fmt.Sprintf("Context compression reached its total ceiling after %.1fs (progress observed=%s); continuing without compression", waited, pyBoolStr(progressObserved)))
	} else {
		runAgentLog().Warn(fmt.Sprintf("Context compression made no progress for %.1fs (total wait %.1fs, ceiling %.1fs); continuing without compression", sinceProgress, waited, totalCeiling))
	}
	a.touchActivity("context compression timed out", string(state.ActivityProvenanceAgentCompressionTimeout), false)
	if rec, ok := a.contextCompressor.(timeoutFailureRecorder); ok && !isNilEngine(a.contextCompressor) {
		if totalExhausted {
			rec.RecordTimeoutFailure("host compress_context total ceiling exhausted", "ceiling_exhausted")
		} else {
			rec.RecordTimeoutFailure("host compress_context timeout (no summary progress)", "stalled")
		}
	}
	if totalExhausted {
		progress := ""
		if progressObserved {
			progress = " after summary output was observed"
		}
		a.emitWarning(fmt.Sprintf("⚠ Context compression reached its total ceiling after %.1fs%s. No messages were dropped — continuing without compression. Run /compress to retry or /new for a clean session.", waited, progress))
		return
	}
	a.emitWarning(fmt.Sprintf("⚠ Context compression timed out after %.1fs with no output from the summary model. No messages were dropped — continuing without compression. Run /compress to retry, /new for a clean session, or check auxiliary.compression.", idle))
}

// Surfaces a commit phase ceiling breach while the SessionDB mutation runs to completion
func warnCommitOverrun(a *AIAgent, waited, ceiling float64) {
	a.emitWarning(fmt.Sprintf("⚠ Context compression commit is taking unusually long (%.0fs, ceiling %.0fs). Waiting for it to finish safely — if this persists, check SessionDB health (disk / lock contention).", waited, ceiling))
}

// Mirrors the persisted marker stamps of the worker result onto every scoped twin in a live list
func syncPersistedMarkers(target, source []map[string]any) {
	for _, sourceMessage := range source {
		if sourceMessage != nil && pyTruthy(sourceMessage[dbPersistedMarker]) {
			stampScopedTwins(target, sourceMessage, false)
		}
	}
}

// Runs the compression on a deep snapshot under the progress timeout, handing back the caller's list on a no-op
func runUnderProgressTimeout(ctx context.Context, a *AIAgent, run facadeRun, messages []map[string]any, systemMessage *string, activeFence *CompressionCommitFence, registration *commitFenceRegistration, idleTimeout, totalCeiling float64, approxTokens *int) ([]map[string]any, string, error) {
	snapshotWorker := func(sameTurnFallbackRecovery bool) compressionWorker {
		return func(ctx context.Context, fence *CompressionCommitFence) ([]map[string]any, string, error) {
			snapshot, _ := pycopy.DeepCopy(messages).([]map[string]any)
			if snapshot == nil {
				snapshot = []map[string]any{}
			}
			out, prompt, err := run(ctx, fence, snapshot, sameTurnFallbackRecovery)
			if err != nil {
				return out, prompt, err
			}
			if sameMessageList(out, snapshot) {
				return messages, prompt, nil
			}
			return out, prompt, nil
		}
	}
	var totalExhausted, progressObserved bool
	publishNewFence := func() (*CompressionCommitFence, error) {
		retry := NewCompressionCommitFence()
		a.compressionCommitFenceLock.Lock()
		defer a.compressionCommitFenceLock.Unlock()
		registration.fence = retry
		if stack := a.compressionCommitFenceStack; len(stack) > 0 && stack[len(stack)-1] == registration {
			a.activeCompressionCommitFence = retry
		}
		return retry, nil
	}
	exceeds := false
	if approxTokens != nil {
		if v := requestExceedsModelWindow(a, *approxTokens); v != nil {
			exceeds = *v
		}
	}
	return facadeRunWithProgressTimeout(ctx, compressionTimeoutRun{
		Worker:               snapshotWorker(false),
		Messages:             messages,
		SystemPromptFallback: func() string { return timeoutFallbackPrompt(ctx, a, systemMessage) },
		IdleTimeoutSeconds:   idleTimeout,
		TotalCeilingSeconds:  totalCeiling,
		OnTimeout: func(idle, waited, sinceProgress float64) {
			reportCompressionTimeout(a, idle, waited, sinceProgress, totalCeiling, totalExhausted, progressObserved)
		},
		OnTimeoutCause:       func(exhausted, observed bool) { totalExhausted, progressObserved = exhausted, observed },
		OnCommitOverrun:      func(waited, ceiling float64) { warnCommitOverrun(a, waited, ceiling) },
		Fence:                activeFence,
		TelemetryAgent:       a,
		NewFence:             publishNewFence,
		FallbackWorker:       snapshotWorker(true),
		RequestExceedsWindow: exceeds,
	})
}

// Mirrors the persisted marker stamps of the result onto the live lists and drops memo entries of replaced bodies
func mirrorResultOntoLiveLists(a *AIAgent, result, messages []map[string]any, directPath bool) {
	if result == nil {
		return
	}
	same := sameMessageList(result, messages)
	if directPath || !same {
		syncPersistedMarkers(messages, result)
	}
	if live := a.liveSessionMessages(); live != nil {
		for _, source := range result {
			if source != nil && pyTruthy(source[dbPersistedMarker]) {
				stampScopedTwinMessages(live, source, false)
			}
		}
	}
	if !same {
		func() {
			defer func() { _ = recover() }()
			provider.ForgetMessageTokenMemo(messages, result)
		}()
	}
}

// Rebinds the caller's log context and current session id to the session the compression left the agent on
func rebindCallerSessionContext(ctx context.Context, a *AIAgent) {
	setLoggingSessionContext(a.sessionID)
	if a.sessionID != "" {
		bindCurrentSessionID(ctx, a, a.sessionID)
	}
}

// Compresses under a published commit fence and the progress timeout with the ambient conversation and affinity tags
func (a *AIAgent) compressContext(ctx context.Context, messages []map[string]any, systemMessage *string, o compressContextOptions) ([]map[string]any, string, error) {
	resetContextCompressionTimeoutOutcome(a)
	if provider.PortalConversationContext(ctx) == "" {
		if root := a.conversationRootID(); root != "" {
			ctx = provider.WithPortalConversationContext(ctx, root)
		}
	}
	if provider.PortalAffinityScope(ctx) == "" {
		if declared := provider.DeclaredConversationScopeSafe(a.cacheScopeAgent()); declared != "" {
			ctx = provider.WithPortalAffinityScope(ctx, declared)
		}
	}
	activeFence := o.CommitFence
	if activeFence == nil {
		activeFence = NewCompressionCommitFence()
	}
	registration := &commitFenceRegistration{fence: activeFence}
	defer a.unregisterCommitFence(registration)
	a.registerCommitFence(registration)
	run := func(ctx context.Context, fence *CompressionCommitFence, target []map[string]any, sameTurnFallbackRecovery bool) ([]map[string]any, string, error) {
		attempt := o
		attempt.BypassCooldown = o.BypassCooldown || sameTurnFallbackRecovery
		attempt.CommitFence = fence
		return facadeCompressContext(ctx, a, target, systemMessage, attempt)
	}
	directPath := o.CommitFence != nil
	var idleTimeout, totalCeiling float64
	if !directPath {
		idleTimeout, totalCeiling = ResolveContextCompressionTimeouts(ctx, nil)
		directPath = idleTimeout <= 0
	}
	var out []map[string]any
	var prompt string
	var err error
	if directPath {
		out, prompt, err = run(ctx, activeFence, messages, false)
	} else {
		out, prompt, err = runUnderProgressTimeout(ctx, a, run, messages, systemMessage, activeFence, registration, idleTimeout, totalCeiling, o.ApproxTokens)
	}
	if err != nil {
		return out, prompt, err
	}
	mirrorResultOntoLiveLists(a, out, messages, directPath)
	rebindCallerSessionContext(ctx, a)
	return out, prompt, nil
}
