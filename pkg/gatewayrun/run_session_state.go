package gatewayrun

import (
	"github.com/nickheyer/icebath/pkg/agent"
)

// Running turn slot value claimed before the real agent is built
var agentPendingSentinel = &agent.AIAgent{}

// Snapshot of the per session state map taken under its lock, safe to iterate while turns run
func (r *GatewayRunner) sessionsMap() map[string]*SessionState {
	r.sessionsMu.RLock()
	defer r.sessionsMu.RUnlock()
	out := make(map[string]*SessionState, len(r.sessions))
	for key, state := range r.sessions {
		out[key] = state
	}
	return out
}

// State of sessionKey, created under the map lock when absent so bare runners work
func (r *GatewayRunner) sessionState(sessionKey string) *SessionState {
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	if r.sessions == nil {
		r.sessions = map[string]*SessionState{}
	}
	state := r.sessions[sessionKey]
	if state == nil {
		state = newSessionState()
		r.sessions[sessionKey] = state
	}
	return state
}

// State of sessionKey without creating one
func (r *GatewayRunner) peekSessionState(sessionKey string) *SessionState {
	r.sessionsMu.RLock()
	defer r.sessionsMu.RUnlock()
	return r.sessions[sessionKey]
}

// True when the session holds a running turn slot, agent or pending sentinel
func (r *GatewayRunner) isSessionRunning(sessionKey string) bool {
	state := r.peekSessionState(sessionKey)
	return state != nil && state.turn.agent != nil
}

// Session key and agent of each session with a running turn
type runningAgentItem struct {
	sessionKey string
	agent      *agent.AIAgent
}

// Session key and agent pairs of running turns including pending sentinels
func (r *GatewayRunner) runningAgentItems() []runningAgentItem {
	var items []runningAgentItem
	for key, state := range r.sessionsMap() {
		if state.turn.agent != nil {
			items = append(items, runningAgentItem{key, state.turn.agent})
		}
	}
	return items
}
