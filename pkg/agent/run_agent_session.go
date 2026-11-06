package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var launchGetwd = os.Getwd

// Launch cwd stamped on a new local CLI family session row, empty for remote backends or an unlinked cwd
func launchCwdForSession(source string) string {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv("TERMINAL_ENV")))
	if os.Getenv("TERMINAL_ENV") == "" {
		backend = "local"
	}
	if !CLIFamilySources[source] || (backend != "" && backend != "local") {
		return ""
	}
	cwd, err := launchGetwd()
	if err != nil {
		return ""
	}
	return cwd
}

// Gateway routing origin json for a session row, empty when the agent carries no gateway identity
func gatewayOriginJSON(ctx context.Context, a *AIAgent) string {
	if a.chatID == "" && a.gatewaySessionKey == "" && a.userID == "" {
		return ""
	}
	origin := pyjson.NewObject()
	origin.Set("platform", a.platform)
	origin.Set("chat_id", noneIfEmpty(a.chatID))
	origin.Set("chat_name", noneIfEmpty(a.chatName))
	chatType := a.chatType
	if chatType == "" {
		chatType = "dm"
	}
	origin.Set("chat_type", chatType)
	origin.Set("user_id", noneIfEmpty(a.userID))
	origin.Set("user_name", noneIfEmpty(a.userName))
	origin.Set("thread_id", noneIfEmpty(a.threadID))
	if a.userIDAlt != "" {
		origin.Set("user_id_alt", a.userIDAlt)
	}
	profile := a.profileName
	if profile == "" {
		if profile = profiles.GetActiveProfileName(ctx); profile == "default" {
			profile = ""
		}
	}
	if profile != "" {
		origin.Set("profile", profile)
	}
	out, err := pyjson.Dumps(origin)
	if err != nil {
		return ""
	}
	return out
}

func noneIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Value when it is a positive int, zero standing for None otherwise
func positiveInt(value any) int {
	if v, ok := value.(int); ok && v > 0 {
		return v
	}
	return 0
}

// Whether a 429 waits for pool rotation rather than falling back, true only with somewhere to rotate to
func poolMayRecoverFromRateLimit(pool *provider.Pool) bool {
	return pool != nil && pool.HasAvailable("") && len(pool.Entries()) > 1
}

// Idle review queue key, the session id else the agent identity
func reviewQueueKey(a *AIAgent) string {
	if a.sessionID != "" {
		return a.sessionID
	}
	return fmt.Sprintf("%d", uintptr(unsafe.Pointer(a)))
}

// Tells the context engine the session ended, failures swallowed so teardown continues
func notifyContextEngineSessionEnd(a *AIAgent, messages []map[string]any) {
	engine := a.contextCompressor
	if engine == nil {
		return
	}
	if messages == nil {
		messages = []map[string]any{}
	}
	func() {
		defer func() { _ = recover() }()
		engine.OnSessionEnd(a.sessionID, messages)
	}()
}

// engineCarryOver is the optional carry_over_new_session_context hook of a context engine
type engineCarryOver interface {
	CarryOverNewSessionContext(oldSessionID, newSessionID string)
}

// engineSessionBinder is the optional bind_session_state hook and the session id the engine is bound to
type engineSessionBinder interface {
	BindSessionState(db *state.DB, sessionID string) error
	BoundSessionID() string
}

// Runs one engine lifecycle hook, a panic logged at debug and never raised
func callEngineHook(hook string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			runAgentLog().Debug(fmt.Sprintf("context engine %s during transition: %v", hook, r))
		}
	}()
	fn()
}

// Drives the context engine through session end, reset, start and carry over
func (a *AIAgent) transitionContextEngineSession(oldSessionID, newSessionID string, previousMessages []map[string]any,
	carryOverContext, resetEngine bool, extraContext map[string]any) {
	engine := a.contextCompressor
	if engine == nil {
		return
	}
	if oldSessionID != "" && previousMessages != nil {
		callEngineHook("on_session_end", func() { engine.OnSessionEnd(oldSessionID, previousMessages) })
	}
	if resetEngine {
		callEngineHook("on_session_reset", engine.OnSessionReset)
	}
	shouldStart := oldSessionID != "" || previousMessages != nil || carryOverContext || len(extraContext) > 0
	target := newSessionID
	if target == "" {
		target = a.sessionID
	}
	if shouldStart && target != "" {
		start := pyjson.NewObject()
		start.Set("old_session_id", oldSessionID)
		start.Set("carry_over_context", carryOverContext)
		start.Set("platform", provider.SessionSourceFor(a.env, a.platform))
		start.Set("model", a.model)
		if base := engine.Base(); base != nil {
			start.Set("context_length", base.ContextLength)
		}
		start.Set("conversation_id", a.gatewaySessionKey)
		for k, v := range extraContext {
			start.Set(k, v)
		}
		kwargs := map[string]any{}
		for _, k := range start.Keys {
			v := start.Vals[k]
			if v == nil || v == "" {
				continue
			}
			kwargs[k] = v
		}
		callEngineHook("on_session_start", func() { engine.OnSessionStart(target, kwargs) })
	}
	if carry, ok := engine.(engineCarryOver); ok && carryOverContext && oldSessionID != "" && target != "" {
		callEngineHook("carry_over_new_session_context", func() { carry.CarryOverNewSessionContext(oldSessionID, target) })
	}
}

// Zeroes session counters and anchors, transitions the context engine and rebinds its session state
func (a *AIAgent) resetSessionState(previousMessages []map[string]any, oldSessionID string, carryOverContext bool) {
	a.sessionTotalTokens, a.sessionInputTokens, a.sessionOutputTokens = 0, 0, 0
	a.sessionPromptTokens, a.sessionCompletionTokens = 0, 0
	a.sessionCacheReadTokens, a.sessionCacheWriteTokens, a.sessionReasoningTokens, a.sessionAPICalls = 0, 0, 0, 0
	a.sessionEstimatedCostUSD, a.sessionCostStatus, a.sessionCostSource = 0.0, "unknown", "none"
	a.usageAnchor, a.turnBaseUsageAnchor, a.frozenWorkspaceSnapshot = nil, nil, nil
	a.userTurnCount, a.compactionPromptDriftLogged, a.turnAuthor = 0, false, nil
	a.providerHookFailures, a.isUserInitiatedTurn = map[string]bool{}, false
	a.transitionContextEngineSession(oldSessionID, a.sessionID, previousMessages, carryOverContext, true, nil)
	binder, ok := a.contextCompressor.(engineSessionBinder)
	if ok && a.sessionID != "" && a.sessionID != binder.BoundSessionID() {
		if err := binder.BindSessionState(a.sessionDB, a.sessionID); err != nil {
			runAgentLog().Debug(fmt.Sprintf("context engine bind_session_state during reset: %v", err))
		}
	}
}

// Model config of the lazily created session row with a live yolo toggle folded in
func (a *AIAgent) sessionRowModelConfig() map[string]any {
	return tools.WithSessionYolo(a.sessionInitModelConfig, a.sessionID)
}

// Creates the session row on first use, a failure leaving it for the next turn to retry
func (a *AIAgent) ensureDBSession() error {
	if a.persistDisabled || a.sessionDBCreated || a.sessionDB == nil {
		return nil
	}
	ctx := a.homeContext()
	source := provider.SessionSourceFor(a.env, a.platform)
	displayName := a.chatName
	if displayName == "" {
		displayName = a.userName
	}
	err := a.sessionDB.CreateSession(state.NewSession{
		ID: a.sessionID, Source: source, Model: a.model, ModelConfig: a.sessionRowModelConfig(),
		SystemPrompt: a.cachedSystemPrompt, UserID: a.userID, SessionKey: a.gatewaySessionKey, ChatID: a.chatID,
		ChatType: a.chatType, ThreadID: a.threadID, DisplayName: displayName, OriginJSON: gatewayOriginJSON(ctx, a),
		ParentSessionID: a.parentSessionID, Cwd: launchCwdForSession(source),
		ProfileName: profiles.GetActiveProfileName(ctx),
	})
	if err != nil {
		runAgentLog().Warn(fmt.Sprintf("Session DB creation failed (will retry next turn): %v", err))
		return nil
	}
	a.sessionDBCreated = true
	return nil
}
