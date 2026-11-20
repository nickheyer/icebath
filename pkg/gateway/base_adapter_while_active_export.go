package gateway

import "context"

// Installs the guard as the session's active marker with no processing task behind it
func (b *BasePlatformAdapter) InstallSessionGuard(sessionKey string, guard *InterruptEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	lazyAttr(&b.activeSessions)[sessionKey] = guard
}

// Routes a message arriving while the session is busy, the adapter busy path without the stale lock heal
func (b *BasePlatformAdapter) HandleMessageWhileActive(ctx context.Context, event *MessageEvent, sessionKey string) error {
	return b.handleMessageWhileActive(ctx, event, sessionKey)
}
