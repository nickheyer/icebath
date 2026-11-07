package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Split failures arm the first timeout ladder rung instead of the summary provider cooldown
const splitFailureCooldownSeconds = 60

// Terminal timeout and cooldown stamps a detached heartbeat never overwrites
var terminalCompressionProvenances = map[state.ActivityProvenance]bool{
	state.ActivityProvenanceAgentCompressionTimeout:  true,
	state.ActivityProvenanceAgentCompressionCooldown: true,
}

// Session store surface the rotated parent recovery walks
type compressionChildDB interface {
	GetCompressionTip(ctx context.Context, sessionID string) (string, error)
	GetSession(ctx context.Context, sessionID string) (*pyjson.Object, error)
	GetMessagesAsConversation(ctx context.Context, sessionID string, o state.ConversationOptions) ([]map[string]any, error)
}

type orphanReopener interface {
	ReopenOrphanedCompressionSession(ctx context.Context, sessionID string) (bool, error)
}

type compressionLockHolderReader interface {
	GetCompressionLockHolder(sessionID string) (*string, error)
}

// Sleep between rotated parent recovery polls, a seam the tests shorten
var compressionRecoveryPoll = 50 * time.Millisecond

// Points the bound session id of the agent env and the log context at the new session
func rebindSessionContext(ctx context.Context, a *AIAgent, sessionID string) {
	bindCurrentSessionID(ctx, a, sessionID)
	setLoggingSessionContext(sessionID)
}

// Binds the session id the agent env and the process env resolve, the gateway set_current_session_id
func bindCurrentSessionID(ctx context.Context, a *AIAgent, sessionID string) {
	if a.env != nil {
		vars := map[string]string{}
		for _, name := range tools.SessionContextEnvVars {
			if v := tools.SessionEnvValue(a.env, name); v != "" {
				vars[name] = v
			}
		}
		vars["ICEBATH_SESSION_ID"] = sessionID
		tools.BindSessionContext(a.env, vars)
	}
	if !tools.IsDelegatedChildContext(ctx) {
		_ = os.Setenv("ICEBATH_SESSION_ID", sessionID)
	}
}

// Moves a stale contender onto the live continuation tip, nil when lineage is ambiguous or unreadable
func adoptLiveCompressionChild(ctx context.Context, a *AIAgent, sessionDB any, parentSessionID string) ([]map[string]any, error) {
	db, ok := sessionDB.(compressionChildDB)
	if !ok || isNilEngine(sessionDB) {
		return nil, nil
	}
	tip, err := db.GetCompressionTip(ctx, parentSessionID)
	if err != nil || tip == "" || tip == parentSessionID {
		return nil, err
	}
	child, err := db.GetSession(ctx, tip)
	if err != nil || child == nil {
		return nil, err
	}
	if ended, _ := child.Get("ended_at"); ended != nil {
		return nil, nil
	}
	recovered, err := db.GetMessagesAsConversation(ctx, tip, state.ConversationOptions{})
	if err != nil || len(recovered) == 0 {
		return nil, err
	}
	confirmed, err := db.GetCompressionTip(ctx, parentSessionID)
	if err != nil || confirmed != tip {
		return nil, err
	}
	a.sessionID = tip
	rebindSessionContext(ctx, a, tip)
	handOffMetricsSegment(ctx, parentSessionID, tip)
	a.sessionDBCreated = true
	childPrompt, _ := child.Get("system_prompt")
	prompt, _ := childPrompt.(string)
	a.cachedSystemPrompt = ""
	if prompt != "" {
		matches, err := storedPromptMatchesRuntime(a, prompt)
		if err != nil {
			return nil, err
		}
		if matches {
			a.cachedSystemPrompt = prompt
		}
	}
	a.lastFlushedDBIdx = len(recovered)
	childID := tip
	a.flushedDBMessageSessionID = &childID
	a.flushedDBMessageIDs = map[any]bool{}
	for _, m := range recovered {
		if id := rowIdentity(m); id != nil {
			a.flushedDBMessageIDs[id] = true
		}
	}
	if a.contextCompressor != nil && !isNilEngine(a.contextCompressor) {
		platform := a.platform
		if platform == "" {
			platform = "cli"
		}
		var conversationID any
		if a.gatewaySessionKey != "" {
			conversationID = a.gatewaySessionKey
		}
		swallowCompression("context engine compression-child adoption failed", false, func() error {
			a.contextCompressor.OnSessionStart(tip, map[string]any{"boundary_reason": "compression", "old_session_id": parentSessionID,
				"session_db": sessionDB, "platform": platform, "conversation_id": conversationID})
			return nil
		})
	}
	swallowCompression("memory manager compression-child adoption failed", false, func() error {
		if a.memoryManager != nil {
			a.memoryManager.OnSessionSwitch(ctx, tip, memory.SessionSwitchOptions{ParentSessionID: parentSessionID, Reason: "compression"})
		}
		return nil
	})
	return recovered, nil
}

// Reopens a compression ended parent that has no continuation and no lease holder
func reopenOrphanedParent(ctx context.Context, sessionDB any, sessionID string) {
	reopener, ok := sessionDB.(orphanReopener)
	if !ok || isNilEngine(sessionDB) {
		return
	}
	reopened, err := reopener.ReopenOrphanedCompressionSession(ctx, sessionID)
	if err != nil {
		compressionLog().Warn(fmt.Sprintf("orphaned compression session reopen failed for %s: %v", sessionID, err))
		return
	}
	if reopened {
		compressionLog().Warn(fmt.Sprintf("compression recovery: reopened orphaned session=%s with no continuation", sessionID))
	}
}

// Recovers a stale live agent onto the compression child before a new turn writes to the old parent
func recoverRotatedCompressionSession(ctx context.Context, a *AIAgent) (recovered []map[string]any, ok bool) {
	if a.sessionDB == nil || a.sessionID == "" {
		return nil, false
	}
	sessionID := a.sessionID
	defer func() {
		if r := recover(); r != nil {
			compressionLog().Warn(fmt.Sprintf("compression session recovery failed for session=%s (%T: %v)", sessionID, r, r))
			recovered, ok = nil, false
		}
	}()
	fail := func(err error) ([]map[string]any, bool) {
		compressionLog().Warn(fmt.Sprintf("compression session recovery failed for session=%s (%s: %v)", sessionID, pyErrorClass(err), err))
		return nil, false
	}
	rotated, err := sessionWasRotatedByCompression(ctx, a.sessionDB, sessionID)
	if err != nil {
		return fail(err)
	}
	if !rotated {
		return nil, false
	}
	for attempt := 0; attempt < 21; attempt++ {
		got, err := adoptLiveCompressionChild(ctx, a, a.sessionDB, sessionID)
		if err != nil {
			return fail(err)
		}
		if got != nil {
			return got, true
		}
		holder, err := a.sessionDB.GetCompressionLockHolder(sessionID)
		if err != nil {
			return fail(err)
		}
		if holder == nil || *holder == "" {
			reopenOrphanedParent(ctx, a.sessionDB, sessionID)
			return nil, false
		}
		if attempt == 20 {
			return nil, false
		}
		time.Sleep(compressionRecoveryPoll)
	}
	return nil, false
}

// Python exception class name shown in recovery warnings
func pyErrorClass(err error) string {
	var named interface{ PyClass() string }
	if errors.As(err, &named) {
		return named.PyClass()
	}
	return fmt.Sprintf("%T", err)
}

// Unique durable lock holder id from pid, pid namespace, goroutine, agent instance and a per acquire nonce
func compressionLockHolder(a *AIAgent) string {
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	return fmt.Sprintf("pid=%d%s:tid=%d:agent=%x:nonce=%s", os.Getpid(), state.HolderNamespaceToken(), pythread.GoroutineID(),
		uintptr(unsafe.Pointer(a)), hex.EncodeToString(nonce))
}

// Counts one conversation across a session rotation in the shared metrics
func handOffMetricsSegment(ctx context.Context, oldSessionID, newSessionID string) {
	swallowCompression("shared-metrics segment hand-off failed", true, func() error {
		observability.RotateSegment(ctx, oldSessionID, newSessionID)
		return nil
	})
}

// Notifies the relay coordinator and the context engine after a durable compression commit
func notifyContextEngineCompressionComplete(ctx context.Context, a *AIAgent, newSessionID, oldSessionID string) (notified bool) {
	swallowCompression("relay segment rotation notification failed", true, func() error {
		tools.RelayCoordinator.NotifySessionCompacted(ctx, tools.CurrentRelayProfileKey(ctx), newSessionID, oldSessionID)
		return nil
	})
	handOffMetricsSegment(ctx, oldSessionID, newSessionID)
	if a.contextCompressor == nil || isNilEngine(a.contextCompressor) {
		return false
	}
	platform := a.platform
	if platform == "" {
		platform = "cli"
	}
	var conversationID any
	if a.gatewaySessionKey != "" {
		conversationID = a.gatewaySessionKey
	}
	defer func() {
		if r := recover(); r != nil {
			compressionLog().Debug("context engine on_session_start (compression) failed", "error", fmt.Sprint(r))
			notified = false
		}
	}()
	a.contextCompressor.OnSessionStart(newSessionID, map[string]any{"boundary_reason": "compression", "old_session_id": oldSessionID,
		"platform": platform, "conversation_id": conversationID})
	return true
}

// Stages exactly one context engine notification for an outer host transaction
func queueContextEngineCompressionNotification(ctx context.Context, a *AIAgent, newSessionID, oldSessionID string) error {
	if a.pendingContextEngineCompressionNotification != nil {
		return errors.New("a compression notification is already pending")
	}
	a.pendingContextEngineCompressionNotification = func() bool {
		return notifyContextEngineCompressionComplete(ctx, a, newSessionID, oldSessionID)
	}
	return nil
}

// Emits or discards the deferred notification, repeated calls are no ops
func finalizeContextEngineCompressionNotification(a *AIAgent, committed bool) bool {
	pending := a.pendingContextEngineCompressionNotification
	a.pendingContextEngineCompressionNotification = nil
	if !committed || pending == nil {
		return false
	}
	return pending()
}
