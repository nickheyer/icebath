package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Delivers the context engine session end now and returns the snapshot a memory manager must still extract
func (a *AIAgent) LaunchSessionBoundaryMemoryFlush(historySnapshot []map[string]any, sessionID string) []map[string]any {
	if len(historySnapshot) == 0 {
		return nil
	}
	if engine := a.contextCompressor; engine != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Debug("Context engine on_session_end failed at /new boundary")
				}
			}()
			engine.OnSessionEnd(sessionID, historySnapshot)
		}()
	}
	if a.memoryManager == nil {
		return nil
	}
	return historySnapshot
}

// Rotates the agent onto a fresh session id with cleared per session state, the CLI /new boundary
func (a *AIAgent) StartNewCLISession(ctx context.Context, sessionID string, start time.Time, reasoningConfig map[string]any) {
	a.sessionID = sessionID
	a.sessionStart = start
	a.reasoningConfig = reasoningConfig
	a.resetSessionState(nil, "", false)
	a.lastFlushedDBIdx = 0
	a.todoStore = tools.NewTodoStore()
	_ = InvalidateSystemPrompt(ctx, a)
}

// Marks whether the agent's session row exists in the store
func (a *AIAgent) SetSessionDBCreated(v bool) { a.sessionDBCreated = v }

// Queues end of session extraction then the switch on the memory worker, or switches inline without history
func (a *AIAgent) CommitNewSessionMemory(ctx context.Context, snapshot []map[string]any, newSessionID, parentSessionID string) {
	mm := a.memoryManager
	if mm == nil {
		return
	}
	defer func() { _ = recover() }()
	if len(snapshot) > 0 {
		mm.CommitSessionBoundaryAsync(ctx, snapshot, newSessionID, parentSessionID, "new_session")
		return
	}
	mm.OnSessionSwitch(ctx, newSessionID, memory.SessionSwitchOptions{ParentSessionID: parentSessionID, Reset: true, Reason: "new_session"})
}

// Tells memory providers the session rewound in place so per turn document caches drop
func (a *AIAgent) NotifyMemoryRewound(ctx context.Context, sessionID string) {
	mm := a.memoryManager
	if mm == nil || sessionID == "" {
		return
	}
	defer func() { _ = recover() }()
	mm.OnSessionSwitch(ctx, sessionID, memory.SessionSwitchOptions{Rewound: true})
}

// Mirrors a rewound CLI history onto the agent so the next turn flushes again from the truncated head
func (a *AIAgent) PublishTruncatedHistory(ctx context.Context, history []map[string]any, invalidatePrompt bool) {
	if invalidatePrompt {
		func() {
			defer func() { _ = recover() }()
			_ = InvalidateSystemPrompt(ctx, a)
		}()
	}
	a.lastFlushedDBIdx = len(history)
	messages := make([]*state.Message, len(history))
	keys := make([]any, len(history))
	for i, m := range history {
		messages[i] = state.MessageFromDict(m)
		keys[i] = rowIdentity(m)
	}
	a.sessionMessages = messages
	a.dbFlushScanPrefix = keys
}

// Flushes the CLI history to the session store with the history rows stamped as durable
func (a *AIAgent) FlushHistoryToSessionDB(messages, conversationHistory []map[string]any) (bool, error) {
	return a.flushMessagesToSessionDBWithHistory(messages, conversationHistory)
}

// Points the agent at a new session id, the continuation a compression rotated to
func (a *AIAgent) SetSessionID(sessionID string) { a.sessionID = sessionID }

// Sets the session start time the agent reports
func (a *AIAgent) SetSessionStart(start time.Time) { a.sessionStart = start }

// The built in memory store of the agent, nil when memory is off
func (a *AIAgent) MemoryStore() *memory.Store { return a.memoryStore }

// Forks the memory and skill review on demand with optional focus text, the explicit /refine run
func (a *AIAgent) SpawnExplicitBackgroundReview(ctx context.Context, snapshot []map[string]any, reviewMemory, reviewSkills bool, focus *string) {
	a.spawnBackgroundReviewWith(ctx, snapshot, reviewMemory, reviewSkills, focus, true)
}
