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

// Current in memory model override of a session read under its state lock, nil when absent
func (r *GatewayRunner) sessionModelOverride(sessionKey string) map[string]any {
	var override map[string]any
	r.withPeekedSessionState(sessionKey, func(s *SessionState) { override = s.conversation.modelOverride })
	return override
}

// Restore snapshot of an override, recording whether one was set and a copy of it
func modelOverrideSnapshot(override map[string]any) map[string]any {
	snapshot := map[string]any{"had_override": override != nil, "override": nil}
	if override != nil {
		snapshot["override"] = maps.Clone(override)
	}
	return snapshot
}

// Session override captured before a one turn switch
func (r *GatewayRunner) snapshotSessionModelOverride(sessionKey string) map[string]any {
	return modelOverrideSnapshot(r.sessionModelOverride(sessionKey))
}

// Arms the one shot restore snapshot in one hold, a repeated one shot command keeping the earliest snapshot
func (r *GatewayRunner) claimOneTurnRestore(sessionKey string, snapshot map[string]any) {
	r.withSessionState(sessionKey, func(s *SessionState) { claimOneTurnRestoreIn(s, snapshot) })
}

// Arms the one shot restore snapshot on a state whose lock the caller holds, keeping an earlier snapshot
func claimOneTurnRestoreIn(s *SessionState, snapshot map[string]any) {
	conv := &s.conversation
	if len(conv.oneTurnRestore) > 0 {
		return
	}
	if snapshot != nil {
		conv.oneTurnRestore = maps.Clone(snapshot)
		return
	}
	conv.oneTurnRestore = modelOverrideSnapshot(conv.modelOverride)
}

// Restores the session override captured before a one turn switch and evicts the cached agent
func (r *GatewayRunner) restoreSessionModelOverride(ctx context.Context, sessionKey string, snapshot map[string]any) {
	if sessionKey == "" {
		return
	}
	if pyrepr.Truthy(snapshot["had_override"]) {
		override, _ := snapshot["override"].(map[string]any)
		restored := maps.Clone(override)
		if restored == nil {
			restored = map[string]any{}
		}
		r.withSessionState(sessionKey, func(s *SessionState) { s.conversation.modelOverride = restored })
	} else {
		r.withPeekedSessionState(sessionKey, func(s *SessionState) { s.conversation.modelOverride = nil })
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
	generation := 0
	r.withSessionState(sessionKey, func(s *SessionState) { generation = s.bumpRunGeneration() })
	return generation
}

// Increments the run generation of a state whose lock the caller holds and returns the new value
func (s *SessionState) bumpRunGeneration() int {
	s.persistent.runGeneration++
	return s.persistent.runGeneration
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
	generation := 0
	r.withPeekedSessionState(sessionKey, func(s *SessionState) { generation = s.persistent.runGeneration })
	return generation
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

// Registry and the token the session holds for the generation, popped in the same hold when asked
func (r *GatewayRunner) heldTurnLease(sessionKey string, runGeneration int, pop bool) (*SessionTurnLeaseRegistry, *TurnLeaseToken) {
	registry := r.turnLeases
	if sessionKey == "" || registry == nil {
		return nil, nil
	}
	var token *TurnLeaseToken
	held := false
	r.withPeekedSessionState(sessionKey, func(s *SessionState) {
		if token, held = s.turn.leaseTokens[runGeneration]; held && pop {
			delete(s.turn.leaseTokens, runGeneration)
		}
	})
	if !held {
		return nil, nil
	}
	return registry, token
}

// Releases the turn lease the session and generation acquired, a stale unwind popping only its own token
func (r *GatewayRunner) releaseTurnLease(sessionKey string, runGeneration int) (released bool) {
	registry, token := r.heldTurnLease(sessionKey, runGeneration, true)
	if registry == nil {
		return false
	}
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
	registry, token := r.heldTurnLease(sessionKey, runGeneration, false)
	if registry == nil {
		return false
	}
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("Failed to rebind turn lease", "error", rec)
			rebound = false
		}
	}()
	return registry.Rebind(token, newSessionID)
}

// Stages one shot must deliver notes for the next agent run
func (r *GatewayRunner) setPendingTurnSidecarNotes(sessionKey string, notes []string) {
	if sessionKey == "" || len(notes) == 0 {
		return
	}
	staged := append([]string(nil), notes...)
	r.withSessionState(sessionKey, func(s *SessionState) { s.conversation.sidecarNotes = staged })
}

// Staged sidecar notes of a session, cleared as they are read
func (r *GatewayRunner) consumePendingTurnSidecarNotes(sessionKey string) []string {
	var staged []string
	if sessionKey != "" {
		r.withPeekedSessionState(sessionKey, func(s *SessionState) {
			staged = s.conversation.sidecarNotes
			s.conversation.sidecarNotes = []string{}
		})
	}
	return append([]string{}, staged...)
}

// Session context prompt pinned per session, reused verbatim on a key hit and by internal events
func (r *GatewayRunner) pinnedSessionContextPrompt(ctx context.Context, sc *gateway.SessionContext, redactPII bool, sessionKey string, internal bool) string {
	var pin *ephemeralPin
	if sessionKey != "" {
		r.withPeekedSessionState(sessionKey, func(s *SessionState) { pin = s.conversation.ephemeralPin })
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
		r.withSessionState(sessionKey, func(s *SessionState) {
			s.conversation.ephemeralPin = &ephemeralPin{changeKey: key, text: text, redactPII: redactPII}
		})
	}
	return text
}

// Channel prompt and source of this run, human turns recording them and internal turns reusing the pin
func (r *GatewayRunner) pinnedChannelInputs(sessionKey string, channelPrompt *string, source *gateway.SessionSource, internal bool) (*string, *gateway.SessionSource) {
	if sessionKey == "" {
		return channelPrompt, source
	}
	if !internal {
		recorded := &channelPin{channelPrompt: channelPrompt, parentChatID: source.ParentChatID}
		r.withSessionState(sessionKey, func(s *SessionState) { s.conversation.channelPin = recorded })
		return channelPrompt, source
	}
	var pin *channelPin
	r.withPeekedSessionState(sessionKey, func(s *SessionState) { pin = s.conversation.channelPin })
	if pin == nil {
		return channelPrompt, source
	}
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
	pinned := false
	r.withPeekedSessionState(sessionKey, func(s *SessionState) { pinned = s.conversation.ephemeralPin != nil })
	if pinned {
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
	adopted := &ephemeralPin{changeKey: contextKey, text: contextPrompt, redactPII: redact}
	channel := &channelPin{channelPrompt: optionalText(get("channel_prompt")), parentChatID: optionalText(get("parent_chat_id"))}
	r.withSessionState(sessionKey, func(s *SessionState) {
		s.conversation.ephemeralPin = adopted
		if s.conversation.channelPin == nil {
			s.conversation.channelPin = channel
		}
	})
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
	var pin *ephemeralPin
	var channel *channelPin
	r.withPeekedSessionState(sessionKey, func(s *SessionState) { pin, channel = s.conversation.ephemeralPin, s.conversation.channelPin })
	if pin == nil || channel == nil {
		return
	}
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
