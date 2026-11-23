package tuiserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Grace before an orphaned WebSocket session is interrupted and reaped, zero parking it forever
var wsOrphanReapGraceS = sync.OnceValue(func() float64 {
	return settings.SessionsWsOrphanReapGraceS.Get(context.Background()).Seconds()
})

// Hours scale last resort idle window of a session, zero disabling the TTL sweeps
var sessionTTLS = math.Max(0, envFloat("ICEBATH_TUI_SESSION_TTL_S", float64(6*3600)))

// Session sources the startup sweep may close
var orphanSweepSources = []string{"tui", "subagent", "unknown"}

var (
	startupOrphanSweepMu  sync.Mutex
	startupOrphanSweepRan bool
)

// Timer and config seams of the reaper, the real functions swapped only where a test installs a fake
var (
	reaperAfterFunc            = time.AfterFunc
	sessionOrphanReaperEnabled = orphanReaperEnabled
)

// Runs fn once after delay on its own goroutine, logging a panic at the given level instead of raising
func reaperDaemonTimer(delay float64, fn func(), failLog string, level string) {
	reaperAfterFunc(time.Duration(delay*float64(time.Second)), func() {
		defer func() {
			if failure := recover(); failure != nil {
				text := fmt.Sprintf("%s: %s", failLog, failureText(failure))
				if level == "warning" {
					pkgLog().Warn(text)
				} else {
					pkgLog().Debug(text)
				}
			}
		}()
		fn()
	})
}

// Whether the startup orphan sweep is on
func orphanReaperEnabled(ctx context.Context) bool {
	return settings.SessionsStartupOrphanSweep.Get(ctx)
}

// Ends the tui and subagent rows a dead process left open, sparing every id this process holds
func sweepOrphanedSessionRows(ctx context.Context) []string {
	if sessionTTLS <= 0 {
		return []string{}
	}
	db, err := reaperDB(ctx)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Orphaned session row sweep failed: %s", err))
		return []string{}
	}
	liveIDs := map[string]bool{}
	for _, live := range tuihost.LiveSessions() {
		candidates := []any{live.SID}
		if a := sessionAgent(live.Session); a != nil {
			candidates = append(candidates, a.SessionID())
		}
		candidates = append(candidates, live.Session.Get("session_key"))
		for _, candidate := range candidates {
			if text := strOr(candidate); text != "" && truthy(candidate) {
				liveIDs[text] = true
			}
		}
	}
	excluded := make([]string, 0, len(liveIDs))
	for id := range liveIDs {
		excluded = append(excluded, id)
	}
	sort.Strings(excluded)
	options := state.NewSweepOrphanedOptions(sessionTTLS)
	options.Sources = orphanSweepSources
	options.ExcludeIDs = excluded
	options.ExcludePinned = true
	swept, err := db.SweepOrphanedSessions(ctx, options)
	if err != nil {
		panic(err)
	}
	if len(swept) > 0 {
		pkgLog().Info(fmt.Sprintf("Closed %d orphaned session row(s) from a previous gateway process (startup_orphan_reap): %s",
			len(swept), strings.Join(swept, ", ")))
	}
	return swept
}

// Seconds between refreshes of the backend heartbeat row, zero registering once
var heartbeatRefreshS = math.Max(0, envFloat("ICEBATH_GATEWAY_HEARTBEAT_REFRESH_S", 60.0))

var (
	heartbeatRefresherMu      sync.Mutex
	heartbeatRefresherStarted bool
	backendNonce              = randomHex(4)
	gatewayStartedAtOnce      = sync.OnceValue(func() float64 { return float64(time.Now().UnixNano()) / 1e9 })
)

// Hex text of n random bytes, the secrets.token_hex of Python
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Node name of this host
func reaperHostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "host"
	}
	return name
}

// Heartbeat seams, the real functions swapped only where a test installs a fake
var (
	reaperDB                = getDB
	backendIDForThisProcess = backendIDOfProcess
	refreshBackendHeartbeat = refreshHeartbeatRow
)

// Heartbeat row identity of this process, its pid plus a startup nonce so a reused pid never inherits a row
func backendIDOfProcess(ctx context.Context) string {
	return fmt.Sprintf("%s@%s:%d:%s", currentProfileName(ctx), reaperHostname(), os.Getpid(), backendNonce)
}

// Refreshes the heartbeat row of this backend, a store that would not open logged
func refreshHeartbeatRow(ctx context.Context) {
	db, err := reaperDB(ctx)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Backend heartbeat refresh failed: %s", err))
		return
	}
	err = db.RegisterBackendHeartbeat(ctx, state.BackendHeartbeat{BackendID: backendIDForThisProcess(ctx), PID: os.Getpid(),
		StartedAt: gatewayStartedAtOnce(), Profile: currentProfileName(ctx), Host: reaperHostname()})
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("backend heartbeat refresh failed: %s", err))
	}
}

// Registers this backend synchronously and refreshes its row on a loop, once per process, clearing it at exit
func startBackendHeartbeatRefresher(ctx context.Context) {
	heartbeatRefresherMu.Lock()
	if heartbeatRefresherStarted {
		heartbeatRefresherMu.Unlock()
		return
	}
	heartbeatRefresherStarted = true
	heartbeatRefresherMu.Unlock()
	func() {
		defer func() {
			if failure := recover(); failure != nil {
				pkgLog().Debug(fmt.Sprintf("initial backend heartbeat write failed: %s", failureText(failure)))
			}
		}()
		refreshBackendHeartbeat(ctx)
	}()
	if heartbeatRefreshS <= 0 {
		return
	}
	stop := make(chan struct{})
	var stopOnce sync.Once
	registerAtExit(func() {
		stopOnce.Do(func() { close(stop) })
		func() {
			defer func() { _ = recover() }()
			exitCtx := context.Background()
			if db, err := reaperDB(exitCtx); err == nil {
				_, _ = db.ClearBackendHeartbeat(exitCtx, backendIDForThisProcess(exitCtx))
			}
		}()
	})
	go func() {
		defer threadExceptHook("icebath-gateway-heartbeat")
		ctx := context.Background()
		for {
			select {
			case <-stop:
				return
			default:
			}
			func() {
				defer func() {
					if failure := recover(); failure != nil {
						pkgLog().Debug(fmt.Sprintf("heartbeat refresh loop iteration failed: %s", failureText(failure)))
					}
				}()
				refreshBackendHeartbeat(ctx)
			}()
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(heartbeatRefreshS * float64(time.Second))):
			}
		}
	}()
}

// Schedules the once per process orphan sweep after the reconnect grace unless grace, TTL or config disable it
func scheduleStartupOrphanSweep(ctx context.Context) {
	if wsOrphanReapGraceS() <= 0 || sessionTTLS <= 0 || !sessionOrphanReaperEnabled(ctx) {
		return
	}
	startupOrphanSweepMu.Lock()
	if startupOrphanSweepRan {
		startupOrphanSweepMu.Unlock()
		return
	}
	startupOrphanSweepRan = true
	startupOrphanSweepMu.Unlock()
	reaperDaemonTimer(wsOrphanReapGraceS(), func() { sweepOrphanedSessionRows(context.Background()) }, "startup orphan session sweep failed", "warning")
}
