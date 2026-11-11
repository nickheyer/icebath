//go:build windows

package tools

import "context"

// Turn lock without fcntl, a no op the way those installs never had this race path
func acquireTurnLock(ctx context.Context, root, profile string, timeoutSeconds *float64) (func(), error) {
	botRelayLog().Debug("bot turn lock disabled: fcntl unavailable on this platform")
	return func() {}, nil
}
