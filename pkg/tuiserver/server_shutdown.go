package tuiserver

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Shutdown steps, the real functions swapped only where a hermes case installs a fake
var (
	shutdownFlushSessions    = flushSessionsBeforeExit
	shutdownReleaseWakeOwner = releaseGatewayWakeOwner
	shutdownCloseSession     = closeSessionByID
)

// Flushes transcripts first, releases the wake owner, stops turns, then closes every live session
func shutdownSessions() {
	ctx := context.Background()
	suppress(func() { shutdownFlushSessions(nil) })
	suppress(func() { shutdownReleaseWakeOwner() })
	suppress(func() { stopTurnsBeforeExit(ctx, nil) })
	var sids []string
	func() {
		sessionsLock.Lock()
		defer sessionsLock.Unlock()
		for _, live := range tuigateway.LiveSessions() {
			sids = append(sids, live.SID)
		}
	}()
	for _, sid := range sids {
		shutdownCloseSession(ctx, sid, "tui_shutdown", nil)
	}
}

// Runs the import time effects of the server module once
var serverModuleOnce sync.Once

// Registers the exit teardown of every live session and starts the idle reaper, once per process
func loadServerModule() {
	serverModuleOnce.Do(func() {
		registerAtExit(shutdownSessions)
		startIdleReaper()
	})
}
