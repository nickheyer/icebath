package agent

import (
	"context"
	"sync"
)

// HostCancelledToolReason names the host as the system issuer of a scope cancel
const HostCancelledToolReason = "host cancelled the command"

// InterruptScope hard interrupts every agent turn started under it, including turns that begin after the cancel
type InterruptScope struct {
	mu         sync.Mutex
	agents     []any
	reason     *string
	toolReason *string
}

// Scope with no latched reason whose cancels default to the host issuer
func NewInterruptScope() *InterruptScope {
	toolReason := HostCancelledToolReason
	return &InterruptScope{toolReason: &toolReason}
}

// Latched cancel reason, nil before any cancel
func (s *InterruptScope) Reason() *string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

// Latches the reason and hard interrupts every tracked agent, a nil tool reason attributing the stop to the user
func (s *InterruptScope) Cancel(reason string, toolReason *string) {
	s.mu.Lock()
	s.reason = &reason
	s.toolReason = toolReason
	agents := append([]any(nil), s.agents...)
	s.mu.Unlock()
	for _, agent := range agents {
		RequestHardInterruptOf(agent, &reason, toolReason)
	}
}

// Cancel attributed to the host issuer
func (s *InterruptScope) CancelByHost(reason string) {
	toolReason := HostCancelledToolReason
	s.Cancel(reason, &toolReason)
}

// Registers the agent for its turn, interrupting it at once after a cancel, and returns the release
func (s *InterruptScope) Track(agent any) func() {
	s.mu.Lock()
	s.agents = append(s.agents, agent)
	reason, toolReason := s.reason, s.toolReason
	s.mu.Unlock()
	if reason != nil {
		RequestHardInterruptOf(agent, reason, toolReason)
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, tracked := range s.agents {
			if tracked == agent {
				s.agents = append(s.agents[:i], s.agents[i+1:]...)
				return
			}
		}
		panic("list.remove(x): x not in list")
	}
}

type interruptScopeKey struct{}

// Context whose agent turns the scope owns, nil unbinding any outer scope
func WithInterruptScope(ctx context.Context, scope *InterruptScope) context.Context {
	return context.WithValue(ctx, interruptScopeKey{}, scope)
}

// Scope bound to the context, nil without one
func boundInterruptScope(ctx context.Context) *InterruptScope {
	scope, _ := ctx.Value(interruptScopeKey{}).(*InterruptScope)
	return scope
}

// Registers any agent value with the bound scope for its turn, a no op release without one
func TrackInInterruptScope(ctx context.Context, agent any) func() {
	scope := boundInterruptScope(ctx)
	if scope == nil {
		return func() {}
	}
	return scope.Track(agent)
}

// Registers the agent with the bound scope for the turn and returns the turn context with its release
func trackInInterruptScope(ctx context.Context, a *AIAgent) (context.Context, func()) {
	return ctx, TrackInInterruptScope(ctx, a)
}
