package gateway

import (
	"context"
	"sync"
)

// Chats a fresh onboarding launch may delete, the lock held across a delete's write
type onboardingDiscardSet struct {
	lock  sync.Mutex
	idsMu sync.Mutex
	ids   map[string]bool
}

// True while the chat is still marked for discard
func (d *onboardingDiscardSet) holds(sessionID string) bool {
	d.idsMu.Lock()
	defer d.idsMu.Unlock()
	return d.ids[sessionID]
}

// Unmarks a chat so a fresh launch keeps it
func (d *onboardingDiscardSet) release(sessionID string) {
	d.idsMu.Lock()
	defer d.idsMu.Unlock()
	delete(d.ids, sessionID)
}

// Keeps a chat a fresh launch was about to delete, waiting out a delete in flight and retrying
func keepFromDiscard(ctx context.Context, a *SessionAuthority, sessionID string) error {
	a.mu.Lock()
	discards := a.onboardingDiscards
	a.mu.Unlock()
	for discards != nil && discards.holds(sessionID) {
		if discards.lock.TryLock() {
			discards.release(sessionID)
			discards.lock.Unlock()
			return nil
		}
		finished := make(chan struct{})
		go func() {
			discards.lock.Lock()
			discards.lock.Unlock()
			close(finished)
		}()
		select {
		case <-finished:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
