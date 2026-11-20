package tuiserver

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Shutdown steps, the real functions swapped only where a test installs a fake
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
		for _, live := range tuihost.LiveSessions() {
			sids = append(sids, live.SID)
		}
	}()
	for _, sid := range sids {
		shutdownCloseSession(ctx, sid, "tui_shutdown", nil)
	}
}

// Whether the import time effects of the server module already ran in this process
var serverModule struct {
	mu     sync.Mutex
	loaded bool
}

// Loads the home dotenv, registers the exit teardown of every live session and starts the idle reaper, once per process
func loadServerModule() error {
	serverModule.mu.Lock()
	defer serverModule.mu.Unlock()
	if serverModule.loaded {
		return nil
	}
	options := provider.DotenvLoadOptions{ProjectEnv: filepath.Join(pm.RepoRoot(), ".env")}
	if _, err := provider.LoadIcebathDotenv(context.Background(), options); err != nil {
		return err
	}
	registerAtExit(shutdownSessions)
	startIdleReaper()
	serverModule.loaded = true
	return nil
}
