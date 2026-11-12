package gatewayrun

import (
	"slices"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Cross process active session slot a running turn holds until its release
type activeSessionLease interface {
	Release() error
}

// Override set by /fast, present with a nil tier for an explicit normal tier
type serviceTierOverride struct {
	set  bool
	tier *string
}

// Value of an override that /fast never set, key presence deciding rather than truthiness
var serviceTierUnset = serviceTierOverride{}

// Pinned session context prompt with the change key it was rendered for
type ephemeralPin struct {
	changeKey string
	text      string
	redactPII bool
}

// Channel prompt and parent chat of the last non internal turn
type channelPin struct {
	channelPrompt *string
	parentChatID  *string
}

// State scoped to one running gateway turn, lease tokens owned by the turn lease release
type TurnState struct {
	agent         *agent.AIAgent
	event         *gateway.MessageEvent
	ctx           *TurnContext
	startedTS     float64
	lease         activeSessionLease
	busyAckTS     float64
	leaseTokens   map[int]*TurnLeaseToken
	retiredAgents []*agent.AIAgent
}

// Resets the per turn slot after the caller popped the lease to release it
func (t *TurnState) clear() {
	t.agent, t.lease, t.event, t.ctx = nil, nil, nil, nil
	t.startedTS, t.busyAckTS = 0, 0
}

// State scoped to one conversation that survives turns but not boundaries
type ConversationState struct {
	modelOverride       map[string]any
	oneTurnRestore      map[string]any
	reasoningOverride   map[string]any
	serviceTierOverride serviceTierOverride
	lastResolvedModel   string
	queuedEvents        []*gateway.MessageEvent
	sidecarNotes        []string
	ephemeralPin        *ephemeralPin
	channelPin          *channelPin
	vcLast              *string
}

// Resets every field to its default
func (c *ConversationState) clear() {
	*c = ConversationState{}
}

// State with its own lifecycle that turn and boundary resets leave in place
type PersistentState struct {
	approvals            map[string]any
	updatePromptPending  bool
	nativeImagePaths     []string
	pendingCommandText   *string
	runGeneration        int
	hygieneFailureStreak int
}

// All per session gateway state grouped by lifecycle scope, every scope field read and written under mu
type SessionState struct {
	mu           sync.Mutex
	turn         TurnState
	conversation ConversationState
	persistent   PersistentState
}

// Fresh state with empty turn lease tokens
func newSessionState() *SessionState {
	return &SessionState{turn: TurnState{leaseTokens: map[int]*TurnLeaseToken{}}}
}

// Agent or pending sentinel holding the running slot, read under the lock and nil for a nil state
func (s *SessionState) runningAgent() *agent.AIAgent {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn.agent
}

// Start time of the running turn read under the lock, zero for a nil state or an idle session
func (s *SessionState) turnStartedTS() float64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn.startedTS
}

// One legacy dict attribute as a scope field with its default and presence test
type fieldSpec[T any] struct {
	scope, name string
	get         func(*SessionState) T
	set         func(*SessionState, T)
	def         func() T
	present     func(T) bool
}

// Live dict shaped view of one SessionState field across the sessions of a runner
type SessionFieldView[T any] struct {
	runner *GatewayRunner
	spec   fieldSpec[T]
}

// Field value of a session state read under its lock together with whether the field is present
func (v SessionFieldView[T]) read(state *SessionState) (T, bool) {
	var zero T
	if state == nil {
		return zero, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	value := v.spec.get(state)
	if !v.spec.present(value) {
		return zero, false
	}
	return value, true
}

// Field value of key and whether it is present
func (v SessionFieldView[T]) Get(key string) (T, bool) {
	return v.read(v.runner.peekSessionState(key))
}

// Stores value on the session state of key under its lock, creating that state when absent
func (v SessionFieldView[T]) Set(key string, value T) {
	state := v.runner.sessionState(key)
	state.mu.Lock()
	defer state.mu.Unlock()
	v.spec.set(state, value)
}

// Resets the field of key to its default in one hold of the lock, false when it was not present
func (v SessionFieldView[T]) Delete(key string) bool {
	state := v.runner.peekSessionState(key)
	if state == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !v.spec.present(v.spec.get(state)) {
		return false
	}
	v.spec.set(state, v.spec.def())
	return true
}

// Session keys whose field is present, in session map order of a snapshot
func (v SessionFieldView[T]) Keys() []string {
	var keys []string
	for key, state := range v.runner.sessionsMap() {
		if _, ok := v.read(state); ok {
			keys = append(keys, key)
		}
	}
	return keys
}

// Whether the field of key is present
func (v SessionFieldView[T]) Contains(key string) bool {
	_, ok := v.Get(key)
	return ok
}

// Count of sessions whose field is present
func (v SessionFieldView[T]) Len() int {
	return len(v.Keys())
}

// Present fields as a plain map, each value read with its presence under one hold
func (v SessionFieldView[T]) Items() map[string]T {
	items := map[string]T{}
	for key, state := range v.runner.sessionsMap() {
		if value, ok := v.read(state); ok {
			items[key] = value
		}
	}
	return items
}

// Resets the field on every session
func (v SessionFieldView[T]) Clear() {
	for _, state := range v.runner.sessionsMap() {
		state.mu.Lock()
		v.spec.set(state, v.spec.def())
		state.mu.Unlock()
	}
}

// Assigns the view from a plain map, clearing every session first
func (v SessionFieldView[T]) Replace(mapping map[string]T) {
	v.Clear()
	for key, value := range mapping {
		v.Set(key, value)
	}
}

// Session key and run generation naming one held turn lease token
type leaseTokenKey struct {
	sessionKey string
	generation int
}

// Legacy view of held turn lease tokens keyed by session key and generation
type TurnLeaseTokenView struct {
	runner *GatewayRunner
}

// Token held for key and whether one is held, both read under one hold of the state lock
func (v TurnLeaseTokenView) Get(key leaseTokenKey) (*TurnLeaseToken, bool) {
	state := v.runner.peekSessionState(key.sessionKey)
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	token, ok := state.turn.leaseTokens[key.generation]
	return token, ok
}

// Records token for key on the session state under its lock, creating that state when absent
func (v TurnLeaseTokenView) Set(key leaseTokenKey, token *TurnLeaseToken) {
	state := v.runner.sessionState(key.sessionKey)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.turn.leaseTokens == nil {
		state.turn.leaseTokens = map[int]*TurnLeaseToken{}
	}
	state.turn.leaseTokens[key.generation] = token
}

// Drops the token of key in one hold of the state lock, false when none was held
func (v TurnLeaseTokenView) Delete(key leaseTokenKey) bool {
	state := v.runner.peekSessionState(key.sessionKey)
	if state == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if _, ok := state.turn.leaseTokens[key.generation]; !ok {
		return false
	}
	delete(state.turn.leaseTokens, key.generation)
	return true
}

// Every held session key and generation pair
func (v TurnLeaseTokenView) Keys() []leaseTokenKey {
	var keys []leaseTokenKey
	for key := range v.Items() {
		keys = append(keys, key)
	}
	return keys
}

// Count of held tokens
func (v TurnLeaseTokenView) Len() int { return len(v.Items()) }

// Whether a token is held for key
func (v TurnLeaseTokenView) Contains(key leaseTokenKey) bool {
	_, ok := v.Get(key)
	return ok
}

// Held tokens as a plain map keyed by session and generation, each session read under its own lock
func (v TurnLeaseTokenView) Items() map[leaseTokenKey]*TurnLeaseToken {
	items := map[leaseTokenKey]*TurnLeaseToken{}
	for sessionKey, state := range v.runner.sessionsMap() {
		state.mu.Lock()
		for generation, token := range state.turn.leaseTokens {
			items[leaseTokenKey{sessionKey, generation}] = token
		}
		state.mu.Unlock()
	}
	return items
}

// Drops every held token
func (v TurnLeaseTokenView) Clear() {
	for _, key := range v.Keys() {
		v.Delete(key)
	}
}

// Assigns the view from a plain map, clearing every held token first
func (v TurnLeaseTokenView) Replace(mapping map[leaseTokenKey]*TurnLeaseToken) {
	v.Clear()
	for key, token := range mapping {
		v.Set(key, token)
	}
}

// Spec of a field whose zero value is absent and any other value present
func comparableSpec[T comparable](scope, name string, get func(*SessionState) T, set func(*SessionState, T)) fieldSpec[T] {
	var zero T
	return fieldSpec[T]{scope: scope, name: name, get: get, set: set,
		def: func() T { return zero }, present: func(v T) bool { return v != zero }}
}

// Spec of a map field present whenever it is not nil
func mapSpec(scope, name string, get func(*SessionState) map[string]any, set func(*SessionState, map[string]any)) fieldSpec[map[string]any] {
	return fieldSpec[map[string]any]{scope: scope, name: name, get: get, set: set,
		def: func() map[string]any { return nil }, present: func(v map[string]any) bool { return v != nil }}
}

// List field spec present when nonempty, copied on every read and write so callers never share its backing array
func listSpec[E any](scope, name string, get func(*SessionState) []E, set func(*SessionState, []E)) fieldSpec[[]E] {
	return fieldSpec[[]E]{scope: scope, name: name, get: func(s *SessionState) []E { return slices.Clone(get(s)) }, set: func(s *SessionState, v []E) { set(s, slices.Clone(v)) },
		def: func() []E { return nil }, present: func(v []E) bool { return len(v) > 0 }}
}

// Live view of the running agent of each session
func (r *GatewayRunner) runningAgents() SessionFieldView[*agent.AIAgent] {
	return SessionFieldView[*agent.AIAgent]{r, comparableSpec("turn", "agent",
		func(s *SessionState) *agent.AIAgent { return s.turn.agent },
		func(s *SessionState, v *agent.AIAgent) { s.turn.agent = v })}
}

// Live view of the start time of each running turn
func (r *GatewayRunner) runningAgentsTS() SessionFieldView[float64] {
	return SessionFieldView[float64]{r, comparableSpec("turn", "started_ts",
		func(s *SessionState) float64 { return s.turn.startedTS },
		func(s *SessionState, v float64) { s.turn.startedTS = v })}
}

// Live view of the active session slot lease of each running turn
func (r *GatewayRunner) activeSessionLeases() SessionFieldView[activeSessionLease] {
	return SessionFieldView[activeSessionLease]{r, comparableSpec("turn", "lease",
		func(s *SessionState) activeSessionLease { return s.turn.lease },
		func(s *SessionState, v activeSessionLease) { s.turn.lease = v })}
}

// Live view of the busy acknowledgement time of each running turn
func (r *GatewayRunner) busyAckTS() SessionFieldView[float64] {
	return SessionFieldView[float64]{r, comparableSpec("turn", "busy_ack_ts",
		func(s *SessionState) float64 { return s.turn.busyAckTS },
		func(s *SessionState, v float64) { s.turn.busyAckTS = v })}
}

// Live view of held turn lease tokens keyed by session key and generation
func (r *GatewayRunner) turnLeaseTokens() TurnLeaseTokenView {
	return TurnLeaseTokenView{r}
}

// Live view of the run generation counter of each session
func (r *GatewayRunner) sessionRunGeneration() SessionFieldView[int] {
	return SessionFieldView[int]{r, comparableSpec("persistent", "run_generation",
		func(s *SessionState) int { return s.persistent.runGeneration },
		func(s *SessionState, v int) { s.persistent.runGeneration = v })}
}

// Live view of the /model override of each session
func (r *GatewayRunner) sessionModelOverrides() SessionFieldView[map[string]any] {
	return SessionFieldView[map[string]any]{r, mapSpec("conversation", "model_override",
		func(s *SessionState) map[string]any { return s.conversation.modelOverride },
		func(s *SessionState, v map[string]any) { s.conversation.modelOverride = v })}
}

// Live view of the /model --once snapshot of each session
func (r *GatewayRunner) pendingOneTurnModelRestores() SessionFieldView[map[string]any] {
	return SessionFieldView[map[string]any]{r, mapSpec("conversation", "one_turn_restore",
		func(s *SessionState) map[string]any { return s.conversation.oneTurnRestore },
		func(s *SessionState, v map[string]any) { s.conversation.oneTurnRestore = v })}
}

// Live view of the /reasoning override of each session
func (r *GatewayRunner) sessionReasoningOverrides() SessionFieldView[map[string]any] {
	return SessionFieldView[map[string]any]{r, mapSpec("conversation", "reasoning_override",
		func(s *SessionState) map[string]any { return s.conversation.reasoningOverride },
		func(s *SessionState, v map[string]any) { s.conversation.reasoningOverride = v })}
}

// Live view of the /fast override of each session, present only when set
func (r *GatewayRunner) sessionServiceTierOverrides() SessionFieldView[serviceTierOverride] {
	return SessionFieldView[serviceTierOverride]{r, fieldSpec[serviceTierOverride]{scope: "conversation",
		name:    "service_tier_override",
		get:     func(s *SessionState) serviceTierOverride { return s.conversation.serviceTierOverride },
		set:     func(s *SessionState, v serviceTierOverride) { s.conversation.serviceTierOverride = v },
		def:     func() serviceTierOverride { return serviceTierUnset },
		present: func(v serviceTierOverride) bool { return v.set }}}
}

// Live view of the last resolved model of each session
func (r *GatewayRunner) lastResolvedModel() SessionFieldView[string] {
	return SessionFieldView[string]{r, comparableSpec("conversation", "last_resolved_model",
		func(s *SessionState) string { return s.conversation.lastResolvedModel },
		func(s *SessionState, v string) { s.conversation.lastResolvedModel = v })}
}

// Live view of the /queue overflow of each session
func (r *GatewayRunner) queuedEvents() SessionFieldView[[]*gateway.MessageEvent] {
	return SessionFieldView[[]*gateway.MessageEvent]{r, listSpec("conversation", "queued_events",
		func(s *SessionState) []*gateway.MessageEvent { return s.conversation.queuedEvents },
		func(s *SessionState, v []*gateway.MessageEvent) { s.conversation.queuedEvents = v })}
}

// Live view of the one shot sidecar notes of each session
func (r *GatewayRunner) pendingTurnSidecarNotes() SessionFieldView[[]string] {
	return SessionFieldView[[]string]{r, listSpec("conversation", "sidecar_notes",
		func(s *SessionState) []string { return s.conversation.sidecarNotes },
		func(s *SessionState, v []string) { s.conversation.sidecarNotes = v })}
}

// Live view of the runner level pending command text of each session
func (r *GatewayRunner) pendingMessages() SessionFieldView[*string] {
	return SessionFieldView[*string]{r, comparableSpec("persistent", "pending_command_text",
		func(s *SessionState) *string { return s.persistent.pendingCommandText },
		func(s *SessionState, v *string) { s.persistent.pendingCommandText = v })}
}

// Live view of the native image paths waiting for the next turn of each session
func (r *GatewayRunner) pendingNativeImagePathsBySession() SessionFieldView[[]string] {
	return SessionFieldView[[]string]{r, listSpec("persistent", "native_image_paths",
		func(s *SessionState) []string { return s.persistent.nativeImagePaths },
		func(s *SessionState, v []string) { s.persistent.nativeImagePaths = v })}
}

// Live view of the pinned session context of each session
func (r *GatewayRunner) sessionEphemeralPin() SessionFieldView[*ephemeralPin] {
	return SessionFieldView[*ephemeralPin]{r, comparableSpec("conversation", "ephemeral_pin",
		func(s *SessionState) *ephemeralPin { return s.conversation.ephemeralPin },
		func(s *SessionState, v *ephemeralPin) { s.conversation.ephemeralPin = v })}
}

// Live view of the last voice channel context of each session
func (r *GatewayRunner) sessionVCLast() SessionFieldView[*string] {
	return SessionFieldView[*string]{r, comparableSpec("conversation", "vc_last",
		func(s *SessionState) *string { return s.conversation.vcLast },
		func(s *SessionState, v *string) { s.conversation.vcLast = v })}
}

// Live view of the pending dangerous command approval of each session
func (r *GatewayRunner) pendingApprovals() SessionFieldView[map[string]any] {
	return SessionFieldView[map[string]any]{r, mapSpec("persistent", "approvals",
		func(s *SessionState) map[string]any { return s.persistent.approvals },
		func(s *SessionState, v map[string]any) { s.persistent.approvals = v })}
}

// Live view of the /update prompt awaiting a reply in each session
func (r *GatewayRunner) updatePromptPending() SessionFieldView[bool] {
	return SessionFieldView[bool]{r, comparableSpec("persistent", "update_prompt_pending",
		func(s *SessionState) bool { return s.persistent.updatePromptPending },
		func(s *SessionState, v bool) { s.persistent.updatePromptPending = v })}
}
