package commands

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ConfirmTimeout is how long a pending slash confirm stays resolvable
const ConfirmTimeout = 300 * time.Second

// ConfirmHandler runs the confirmed command for a choice of once, always or cancel
type ConfirmHandler func(ctx context.Context, choice string) (string, error)

// PendingConfirm is one slash command awaiting the user's choice
type PendingConfirm struct {
	ConfirmID string
	Command   string
	Handler   ConfirmHandler
	CreatedAt time.Time
}

// ConfirmStore holds pending slash confirms per session so adapters resolve them without the runner
type ConfirmStore struct {
	mu      sync.Mutex
	pending map[string]PendingConfirm
	now     func() time.Time
}

// Confirms is the process wide pending confirm store
var Confirms = NewConfirmStore()

// NewConfirmStore returns an empty store
func NewConfirmStore() *ConfirmStore {
	return &ConfirmStore{pending: map[string]PendingConfirm{}, now: time.Now}
}

// Register records a pending confirm, superseding any prior one for the session
func (s *ConfirmStore) Register(sessionKey, confirmID, command string, handler ConfirmHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[sessionKey] = PendingConfirm{ConfirmID: confirmID, Command: command, Handler: handler, CreatedAt: s.now()}
}

// Pending returns the pending confirm for a session
func (s *ConfirmStore) Pending(sessionKey string) (PendingConfirm, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[sessionKey]
	return p, ok
}

// Clear drops the pending confirm for a session without running it
func (s *ConfirmStore) Clear(sessionKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, sessionKey)
}

func (s *ConfirmStore) stale(p PendingConfirm, timeout time.Duration) bool {
	return s.now().Sub(p.CreatedAt) > timeout
}

// ClearIfStale drops a pending confirm older than the timeout and reports whether it did
func (s *ConfirmStore) ClearIfStale(sessionKey string, timeout time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[sessionKey]
	if ok && s.stale(p, timeout) {
		delete(s.pending, sessionKey)
		return true
	}
	return false
}

// Resolve runs the pending handler with a choice, false when stale, superseded or already resolved
func (s *ConfirmStore) Resolve(ctx context.Context, sessionKey, confirmID, choice string, timeout time.Duration) (string, bool) {
	s.mu.Lock()
	p, ok := s.pending[sessionKey]
	if !ok || p.ConfirmID != confirmID {
		s.mu.Unlock()
		return "", false
	}
	delete(s.pending, sessionKey)
	if s.stale(p, timeout) {
		s.mu.Unlock()
		return "", false
	}
	s.mu.Unlock()
	if p.Handler == nil {
		return "", false
	}
	result, err := p.Handler(ctx, choice)
	if err != nil {
		return fmt.Sprintf("❌ Error handling confirmation: %v", err), true
	}
	return result, true
}

// ConfirmChoice is one answer offered by a slash confirm prompt
type ConfirmChoice struct {
	Key    string
	Label  string
	Detail string
}

// DestructiveChoices are the answers of the destructive slash confirm
var DestructiveChoices = []ConfirmChoice{
	{"once", "Approve Once", "proceed this time only"},
	{"always", "Always Approve", "proceed and silence this prompt permanently"},
	{"cancel", "Cancel", "keep current conversation"},
}

// DestructiveAlwaysMessage confirms the persisted opt out on the CLI
const DestructiveAlwaysMessage = "🔒 Future /clear, /new, /reset, and /undo will run without confirmation."

// PersistFailedProceeding is printed when the opt out could not be saved
const PersistFailedProceeding = "⚠️  Couldn't persist opt-out — proceeding once."

// DestructiveDetails are the chat details for the destructive session commands
var DestructiveDetails = map[string]string{
	"new":      "This starts a fresh session and discards the current conversation history.",
	"undo_one": "This removes the last user/assistant exchange from history.",
}
