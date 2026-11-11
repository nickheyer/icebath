package gatewayrun

import (
	"context"
	"fmt"
	"maps"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Current in memory model override of a session, nil when absent
func (r *GatewayRunner) sessionModelOverride(sessionKey string) map[string]any {
	if state := r.peekSessionState(sessionKey); state != nil {
		return state.conversation.modelOverride
	}
	return nil
}

// Session override captured before a one turn switch
func (r *GatewayRunner) snapshotSessionModelOverride(sessionKey string) map[string]any {
	override := r.sessionModelOverride(sessionKey)
	snapshot := map[string]any{"had_override": override != nil, "override": nil}
	if override != nil {
		snapshot["override"] = maps.Clone(override)
	}
	return snapshot
}

// Arms the one shot restore snapshot, a repeated one shot command keeping the earliest snapshot
func (r *GatewayRunner) claimOneTurnRestore(sessionKey string, snapshot map[string]any) {
	conv := &r.sessionState(sessionKey).conversation
	if len(conv.oneTurnRestore) > 0 {
		return
	}
	if snapshot != nil {
		conv.oneTurnRestore = maps.Clone(snapshot)
		return
	}
	conv.oneTurnRestore = r.snapshotSessionModelOverride(sessionKey)
}

// Restores the session override captured before a one turn switch and evicts the cached agent
func (r *GatewayRunner) restoreSessionModelOverride(ctx context.Context, sessionKey string, snapshot map[string]any) {
	if sessionKey == "" {
		return
	}
	if pyrepr.Truthy(snapshot["had_override"]) {
		override, _ := snapshot["override"].(map[string]any)
		r.sessionState(sessionKey).conversation.modelOverride = maps.Clone(override)
		if r.sessionState(sessionKey).conversation.modelOverride == nil {
			r.sessionState(sessionKey).conversation.modelOverride = map[string]any{}
		}
	} else if state := r.peekSessionState(sessionKey); state != nil {
		state.conversation.modelOverride = nil
	}
	r.evictCachedAgent(ctx, sessionKey, false, false)
}

// True when the agent runs another model than the config on purpose, a session override or the recorded Nous switch
func (r *GatewayRunner) isIntentionalModelSwitch(sessionKey string, a *agent.AIAgent, configModel string) bool {
	if override := r.sessionModelOverride(sessionKey); override != nil && pyrepr.Equal(override["model"], a.Model()) {
		return true
	}
	switched := a.NousModelSwitch()
	return switched != nil && switched[0] == configModel && switched[1] == a.Model()
}

// Fresh monotonically increasing run generation for a session, never reset
func (r *GatewayRunner) beginSessionRunGeneration(sessionKey string) int {
	if sessionKey == "" {
		return 0
	}
	persistent := &r.sessionState(sessionKey).persistent
	persistent.runGeneration++
	return persistent.runGeneration
}

// Invalidates the in flight run token after settling a pending one shot model override
func (r *GatewayRunner) invalidateSessionRunGeneration(ctx context.Context, sessionKey, reason string) int {
	r.restorePendingOneTurnModelOverride(ctx, sessionKey, nil)
	generation := r.beginSessionRunGeneration(sessionKey)
	if reason != "" {
		runLog().Info(fmt.Sprintf("Invalidated run generation for %s → %d (%s)", sessionKey, generation, reason))
	}
	return generation
}

// Current run generation of a session, zero when it tracks no run
func (r *GatewayRunner) currentSessionRunGeneration(sessionKey string) int {
	if state := r.peekSessionState(sessionKey); state != nil {
		return state.persistent.runGeneration
	}
	return 0
}

// True when the generation is still current for the session, always for an empty key
func (r *GatewayRunner) isSessionRunCurrent(sessionKey string, generation int) bool {
	if sessionKey == "" {
		return true
	}
	return r.currentSessionRunGeneration(sessionKey) == generation
}

// Binds a run generation to the adapter's active session interrupt event
func (r *GatewayRunner) bindAdapterRunGeneration(adapter gateway.PlatformAdapter, sessionKey string, generation *int) {
	if adapter == nil || sessionKey == "" || generation == nil {
		return
	}
	defer func() { _ = recover() }()
	if event := adapter.Base().ActiveSessionEvent(sessionKey); event != nil {
		event.SetRunGeneration(*generation)
	}
}

// Registry and token map when the session holds a lease token for the generation
func (r *GatewayRunner) heldTurnLease(sessionKey string, runGeneration int) (*SessionTurnLeaseRegistry, map[int]*TurnLeaseToken) {
	registry := r.turnLeases
	if sessionKey == "" || registry == nil {
		return nil, nil
	}
	state := r.peekSessionState(sessionKey)
	if state == nil || state.turn.leaseTokens == nil {
		return nil, nil
	}
	if _, ok := state.turn.leaseTokens[runGeneration]; !ok {
		return nil, nil
	}
	return registry, state.turn.leaseTokens
}

// Releases the turn lease the session and generation acquired, a stale unwind popping only its own token
func (r *GatewayRunner) releaseTurnLease(sessionKey string, runGeneration int) (released bool) {
	registry, tokens := r.heldTurnLease(sessionKey, runGeneration)
	if registry == nil {
		return false
	}
	token := tokens[runGeneration]
	delete(tokens, runGeneration)
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("Failed to release turn lease", "error", rec)
			released = false
		}
	}()
	return registry.Release(token)
}

// Moves the held turn lease to a session id a mid turn rotation produced
func (r *GatewayRunner) rebindTurnLease(sessionKey string, runGeneration int, newSessionID string) (rebound bool) {
	if newSessionID == "" {
		return false
	}
	registry, tokens := r.heldTurnLease(sessionKey, runGeneration)
	if registry == nil {
		return false
	}
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("Failed to rebind turn lease", "error", rec)
			rebound = false
		}
	}()
	return registry.Rebind(tokens[runGeneration], newSessionID)
}

// Stages one shot must deliver notes for the next agent run
func (r *GatewayRunner) setPendingTurnSidecarNotes(sessionKey string, notes []string) {
	if sessionKey == "" || len(notes) == 0 {
		return
	}
	r.sessionState(sessionKey).conversation.sidecarNotes = append([]string(nil), notes...)
}

// Staged sidecar notes of a session, cleared as they are read
func (r *GatewayRunner) consumePendingTurnSidecarNotes(sessionKey string) []string {
	var state *SessionState
	if sessionKey != "" {
		state = r.peekSessionState(sessionKey)
	}
	if state == nil {
		return []string{}
	}
	staged := state.conversation.sidecarNotes
	state.conversation.sidecarNotes = []string{}
	return append([]string{}, staged...)
}

// Session context prompt pinned per session, reused verbatim on a key hit and by internal events
func (r *GatewayRunner) pinnedSessionContextPrompt(ctx context.Context, sc *gateway.SessionContext, redactPII bool, sessionKey string, internal bool) string {
	var pin *ephemeralPin
	if sessionKey != "" {
		if state := r.peekSessionState(sessionKey); state != nil {
			pin = state.conversation.ephemeralPin
		}
	}
	if pin != nil && pin.redactPII != redactPII {
		pin = nil
	}
	if internal && pin != nil {
		return pin.text
	}
	key := ephemeralChangeKey(ctx, sc, redactPII)
	if pin != nil && pin.changeKey == key {
		return pin.text
	}
	text := gateway.BuildSessionContextPrompt(ctx, sc, redactPII)
	if sessionKey != "" {
		r.sessionState(sessionKey).conversation.ephemeralPin = &ephemeralPin{changeKey: key, text: text, redactPII: redactPII}
	}
	return text
}

// Channel prompt and source of this run, human turns recording them and internal turns reusing the pin
func (r *GatewayRunner) pinnedChannelInputs(sessionKey string, channelPrompt *string, source *gateway.SessionSource, internal bool) (*string, *gateway.SessionSource) {
	if sessionKey == "" {
		return channelPrompt, source
	}
	if !internal {
		r.sessionState(sessionKey).conversation.channelPin = &channelPin{channelPrompt: channelPrompt, parentChatID: source.ParentChatID}
		return channelPrompt, source
	}
	state := r.peekSessionState(sessionKey)
	if state == nil || state.conversation.channelPin == nil {
		return channelPrompt, source
	}
	pin := state.conversation.channelPin
	if pin.parentChatID != nil && *pin.parentChatID != "" && (source.ParentChatID == nil || *source.ParentChatID == "") {
		parent := *pin.parentChatID
		source = gateway.ReplaceSource(source, func(s *gateway.SessionSource) { s.ParentChatID = &parent })
	}
	return pin.channelPrompt, source
}

// Single async facade over the runner's session store, rebuilt when the store was swapped
func (r *GatewayRunner) asyncStore() *gateway.AsyncSessionStore {
	facade := r.asyncSessionStore
	if facade == nil || facade.Store() != r.sessionStore {
		facade = gateway.NewAsyncSessionStore(r.sessionStore)
		r.asyncSessionStore = facade
	}
	return facade
}

// Adopts the durable pin snapshot for an internal turn when this process holds no context pin
func (r *GatewayRunner) rehydratePromptPins(ctx context.Context, sessionKey string, expectedSessionID *string) {
	if state := r.peekSessionState(sessionKey); state != nil && state.conversation.ephemeralPin != nil {
		return
	}
	var raw *pyjson.Object
	failed := func() (failed bool) {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("Failed to read persisted prompt pin for %s", sessionKey), "error", rec)
				failed = true
			}
		}()
		raw = gateway.SanitizePromptPin(r.asyncStore().Store().GetPromptPin(ctx, sessionKey, expectedSessionID))
		return false
	}()
	if failed || raw == nil {
		return
	}
	get := func(key string) any { v, _ := raw.Get(key); return v }
	contextKey, _ := get("context_key").(string)
	contextPrompt, _ := get("context_prompt").(string)
	redact, _ := get("redact_pii").(bool)
	conversation := &r.sessionState(sessionKey).conversation
	conversation.ephemeralPin = &ephemeralPin{changeKey: contextKey, text: contextPrompt, redactPII: redact}
	if conversation.channelPin == nil {
		conversation.channelPin = &channelPin{channelPrompt: optionalText(get("channel_prompt")), parentChatID: optionalText(get("parent_chat_id"))}
	}
}

// String pointer of a JSON value, nil for null or a non string
func optionalText(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// JSON value of an optional string, nil for nil
func textOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Persists this conversation's pins before the agent runs, a store outage never blocking the turn
func (r *GatewayRunner) persistPromptPins(ctx context.Context, sessionKey string, expectedSessionID *string) {
	if sessionKey == "" {
		return
	}
	state := r.peekSessionState(sessionKey)
	if state == nil || state.conversation.ephemeralPin == nil || state.conversation.channelPin == nil {
		return
	}
	pin, channel := state.conversation.ephemeralPin, state.conversation.channelPin
	snapshot := pyjson.ObjectOf(
		"version", gateway.PromptPinVersion, "context_key", pin.changeKey,
		"context_prompt", pin.text, "redact_pii", pin.redactPII,
		"channel_prompt", textOrNil(channel.channelPrompt), "parent_chat_id", textOrNil(channel.parentChatID),
	)
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("Failed to persist prompt pin for %s", sessionKey), "error", rec)
			}
		}()
		if _, err := r.asyncStore().Store().SetPromptPin(ctx, sessionKey, snapshot, expectedSessionID); err != nil {
			runLog().Debug(fmt.Sprintf("Failed to persist prompt pin for %s", sessionKey), "error", err)
		}
	}()
}
