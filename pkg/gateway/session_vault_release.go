package gateway

import "log/slog"

// Process hook that locks the password manager tokens one gateway session unlocked, bound by the binary
var ReleaseVaultUnlocks func(sessionID string)

// Locks the password managers the closed session unlocked, a failing lock only logged so the close finishes
func releaseUnlocks(sessionID string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("vault manager lock on session close failed", "error", r)
		}
	}()
	ReleaseVaultUnlocks(sessionID)
}
