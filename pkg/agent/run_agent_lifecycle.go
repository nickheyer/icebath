package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// reviewRequeueMaxAttempts bounds how often a preempted automatic review is requeued
const reviewRequeueMaxAttempts = 3

// backgroundReviewKwargs carries the keyword arguments of one background review spawn
type backgroundReviewKwargs struct {
	messagesSnapshot []map[string]any
	reviewMemory     bool
	reviewSkills     bool
	focus            *string
	taskCfg          map[string]any
	requeueAttempts  int
	explicit         bool
}

// Spawns or idle queues a background review, skipping automatic ones for subagents or when disabled
func (a *AIAgent) spawnBackgroundReview(ctx context.Context, messagesSnapshot []map[string]any, reviewMemory, reviewSkills bool, focus *string, explicit bool) {
	SpawnBackgroundReviewOn(ctx, a, messagesSnapshot, reviewMemory, reviewSkills, focus, explicit)
}

// Decides when the review of self runs and spawns it, the snapshot cloned structurally at this single chokepoint
func SpawnBackgroundReviewOn(ctx context.Context, self BackgroundReviewSelf, messagesSnapshot []map[string]any, reviewMemory, reviewSkills bool, focus *string, explicit bool) {
	if !explicit && focus == nil && self.BackgroundReviewDelegateDepth() > 0 {
		return
	}
	var taskCfg map[string]any
	if !explicit && focus == nil {
		enabled, cfg := loadBackgroundReviewSettings(ctx)
		if !enabled {
			return
		}
		taskCfg = cfg
	}
	snapshot := cloneBackgroundReviewMessages(messagesSnapshot)
	if focus == nil && !explicit && self.BackgroundReviewShouldDefer(ctx, taskCfg) {
		self.EnqueueBackgroundReview(ctx, snapshot, reviewMemory, reviewSkills, focus, taskCfg, explicit)
		return
	}
	self.SpawnBackgroundReviewNow(ctx, snapshot, reviewMemory, reviewSkills, focus, taskCfg, explicit)
}

// Starts the review worker goroutine, replaced by tests that capture or run the worker inline
var startReviewWorker = func(fn func()) { go fn() }

// Starts the review fork goroutine, requeueing a preempted deferred review and finishing the run on spawn failure
func (a *AIAgent) spawnBackgroundReviewNow(ctx context.Context, kw backgroundReviewKwargs) {
	run := prepareBackgroundReviewRun(a)
	if run == nil {
		return
	}
	ok := false
	defer func() {
		if !ok {
			finishBackgroundReviewRun(a, run)
		}
	}()
	target, _, err := spawnBackgroundReviewThread(ctx, a, kw, run)
	if err != nil {
		panic(err)
	}
	next := kw
	next.requeueAttempts = kw.requeueAttempts + 1
	threadCtx := context.WithoutCancel(ctx)
	startReviewWorker(func() {
		target(threadCtx)
		a.maybeRequeuePreemptedReview(threadCtx, run, next)
	})
	ok = true
}

// Requeues an automatic managed local review a live turn preempted, bounded by the attempt cap
func (a *AIAgent) maybeRequeuePreemptedReview(ctx context.Context, run *backgroundReviewRun, kw backgroundReviewKwargs) {
	defer func() {
		if r := recover(); r != nil {
			runAgentLog().Debug(fmt.Sprintf("Preempted-review requeue failed: %v", r))
		}
	}()
	if !run.cancelRequestedSet() || kw.focus != nil || kw.explicit {
		return
	}
	if kw.requeueAttempts > reviewRequeueMaxAttempts {
		runAgentLog().Info(fmt.Sprintf("Preempted background review dropped after %d requeues", reviewRequeueMaxAttempts))
		return
	}
	if !reviewShouldDefer(ctx, a, kw.taskCfg) {
		return
	}
	enqueueIdleReview(ctx, a, reviewQueueKey(a), kw)
}

// Ends the memory provider session and shuts providers down once, then notifies the context engine
func (a *AIAgent) ShutdownMemoryProvider(ctx context.Context, messages []map[string]any) {
	if a.memoryProviderShutdown {
		return
	}
	a.memoryProviderShutdown = true
	if a.memoryManager != nil && a.memoryManager.ClaimTeardown(true) {
		func() {
			defer func() {
				if r := recover(); r != nil {
					runAgentLog().Warn(fmt.Sprintf("Memory provider on_session_end failed during shutdown: %v", r))
				}
			}()
			a.memoryManager.OnSessionEnd(ctx, nonNilMessages(messages))
		}()
		quietly(func() error { a.memoryManager.ShutdownAll(ctx); return nil })
	}
	notifyContextEngineSessionEnd(a, messages)
}

// Commits the memory session at a boundary without shutting providers down
func (a *AIAgent) CommitMemorySession(ctx context.Context, messages []map[string]any) {
	if a.memoryManager != nil {
		quietly(func() error { a.memoryManager.OnSessionEnd(ctx, nonNilMessages(messages)); return nil })
	}
	notifyContextEngineSessionEnd(a, messages)
}

// Messages or an empty list for None
func nonNilMessages(messages []map[string]any) []map[string]any {
	if messages == nil {
		return []map[string]any{}
	}
	return messages
}

// Mirrors a completed turn into external memory and queues the next prefetch, skipping interrupted turns
func (a *AIAgent) syncExternalMemoryForTurn(originalUserMessage, finalResponse any, interrupted bool, messages []map[string]any) {
	if interrupted || a.memoryManager == nil || !pyTruthy(finalResponse) || !pyTruthy(originalUserMessage) {
		return
	}
	userText := provider.SummarizeUserMessageForLog(originalUserMessage, "\n")
	responseText := provider.SummarizeUserMessageForLog(finalResponse, "\n")
	if userText == "" || responseText == "" {
		return
	}
	defer func() { _ = recover() }()
	ctx := context.Background()
	opts := memory.SyncOptions{SessionID: a.sessionID, Messages: messages}
	if author, ok := a.turnAuthor.(map[string]any); ok {
		opts.TurnAuthor = author
	}
	a.memoryManager.SyncAll(ctx, userText, responseText, opts)
	if !memory.IsTrivialPrompt(userText) {
		a.memoryManager.QueuePrefetchAll(ctx, userText, a.sessionID)
	}
}

// Python truthiness of a message or response value
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case []map[string]any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case bool:
		return x
	case int:
		return x != 0
	case float64:
		return x != 0
	}
	return true
}

// Releases LLM clients, children, the codex session and soft memory state while keeping task resources
func (a *AIAgent) ReleaseClients(ctx context.Context) {
	a.closeActiveChildren(ctx, true)
	quietly(func() error {
		a.dropSharedClient(func(c any) { a.retireSharedOpenAIClient(c, "cache_evict") })
		return nil
	})
	a.closeRequestClients("cache_evict")
	quietly(func() error { a.closeCodexSession(); return nil })
	if a.memoryManager != nil {
		quietly(func() error { a.memoryManager.ReleaseSoft(ctx); return nil })
	}
	quietly(func() error { ReleaseForAgent(ctx, a); return nil })
}

// Releases every resource the agent holds, each step isolated so one failure never blocks the rest
func (a *AIAgent) Close() error { return a.CloseContext(context.Background()) }

// Releases every resource the agent holds under the caller's profile scope, each step isolated
func (a *AIAgent) CloseContext(ctx context.Context) error {
	sessionMessages := a.liveSessionMessages()
	var dicts []map[string]any
	if sessionMessages != nil {
		dicts = make([]map[string]any, 0, len(sessionMessages))
		for _, m := range sessionMessages {
			dicts = append(dicts, state.MessageToDict(m))
		}
	}
	quietly(func() error { a.ShutdownMemoryProvider(ctx, dicts); return nil })
	a.closeTaskResources(a.sessionID)
	a.closeActiveChildren(ctx, false)
	quietly(func() error { ReleaseForAgent(ctx, a); return nil })
	quietly(func() error {
		a.dropSharedClient(func(c any) { a.closeOpenAIClient(c, "agent_close", true) })
		return nil
	})
	a.closeRequestClients("agent_close")
	quietly(func() error { a.closeCodexSession(); return nil })
	a.setSessionMessages([]*state.Message{})
	a.dbFlushScanPrefix = nil
	a.streamedAssistantTextParts = []string{}
	if sessionMessages != nil {
		quietly(func() error {
			provider.ForgetMessageTokenMemo([]any{dicts, a.cachedSystemPrompt, a.cachedSystemPromptStatic}, nil)
			return nil
		})
	}
	quietly(func() error { trimProcessMemory(ctx, a.env); return nil })
	quietly(func() error { a.finalizeOwnedSessionRow(); return nil })
	return nil
}

// Detaches per turn child agents and closes them, soft releasing clients first with close as the fallback
func (a *AIAgent) closeActiveChildren(ctx context.Context, soft bool) {
	if a.activeChildrenLock == nil {
		return
	}
	a.activeChildrenLock.Lock()
	children := append([]*AIAgent(nil), a.activeChildren...)
	a.activeChildren = a.activeChildren[:0]
	a.activeChildrenLock.Unlock()
	for _, child := range children {
		if soft {
			released := func() (ok bool) {
				defer func() {
					if recover() != nil {
						ok = false
					}
				}()
				child.ReleaseClients(ctx)
				return true
			}()
			if released {
				continue
			}
		}
		quietly(child.Close)
	}
}

// Hands the shared client to the close function and clears it
func (a *AIAgent) dropSharedClient(closeFn func(any)) {
	if a.client != nil {
		closeFn(a.client)
		a.client = nil
	}
}

// Drops the cached per request OpenAI and Anthropic wire clients
func (a *AIAgent) closeRequestClients(reason string) {
	quietly(func() error { a.closeCachedRequestOpenAIClient(reason); return nil })
	quietly(func() error { a.closeCachedRequestAnthropicClient(reason); return nil })
}

// Clears the codex app server session before closing it so readers never see a half closed session
func (a *AIAgent) closeCodexSession() {
	if s := a.codexSession; s != nil {
		a.codexSession = nil
		s.Close()
	}
}

// Returns freed heap pages to the OS without a full collection
func trimProcessMemory(ctx context.Context, env *tools.Env) {
	memory.NoteAgentClosed()
	memory.TrimMemory(ctx, env, memory.TrimOptions{Force: true, Reason: "agent close", AllocatorOnly: true})
}

// Ends the owned session row and releases the SQLite handle only when this agent opened it
func (a *AIAgent) finalizeOwnedSessionRow() {
	db, sid := a.sessionDB, a.sessionID
	if a.endSessionOnClose && db != nil && sid != "" {
		quietly(func() error { return db.EndSession(sid, "agent_close") })
	}
	if a.recallDB != nil {
		recall := a.recallDB
		a.recallDB = nil
		quietly(recall.Close)
	}
	if a.ownsSessionDB && db != nil {
		a.ownsSessionDB = false
		releaseOrCloseStore(db)
	}
}

// True when an interrupt has been requested
func (a *AIAgent) IsInterrupted() bool { return a.interruptRequested.Load() }

// Session lineage root id for Portal usage attribution, empty for None
func (a *AIAgent) conversationRootID() string {
	if a.cachedConversationRoot != "" {
		return a.cachedConversationRoot
	}
	if a.sessionID == "" {
		return ""
	}
	start := a.parentSessionID
	if start == "" {
		start = a.sessionID
	}
	if a.sessionDB == nil {
		return start
	}
	root, err := a.sessionDB.GetConversationRoot(start)
	if err != nil {
		runAgentLog().Debug("Conversation root lineage walk failed", "error", err)
		return start
	}
	if root == "" {
		return start
	}
	return root
}
