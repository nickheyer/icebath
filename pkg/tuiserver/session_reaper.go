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

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Seconds of a dashboard orphan knob, the env override winning when set and the default on a bad value
func wsOrphanSetting(ctx context.Context, envVar, cfgKey string, def float64) float64 {
	var raw any
	if value, set := os.LookupEnv(envVar); set && strings.TrimSpace(value) != "" {
		raw = value
	} else {
		func() {
			defer func() { _ = recover() }()
			cfg, err := loadConfig(ctx)
			if err != nil {
				return
			}
			if dashboard, isDict := cfg["dashboard"].(map[string]any); isDict {
				raw = dashboard[cfgKey]
			}
		}()
	}
	if raw == nil {
		return math.Max(0, def)
	}
	value, err := pyint.Float(raw)
	if err != nil {
		return math.Max(0, def)
	}
	return math.Max(0, value)
}

// Grace before an orphaned WebSocket session is interrupted and reaped, zero parking it forever
var wsOrphanReapGraceS = sync.OnceValue(func() float64 {
	return wsOrphanSetting(context.Background(), "ICEBATH_TUI_WS_ORPHAN_REAP_GRACE_S", "ws_orphan_reap_grace_s", 20.0)
})

// Hours scale last resort idle window of a session, zero disabling the TTL sweeps
var sessionTTLS = math.Max(0, envFloat("ICEBATH_TUI_SESSION_TTL_S", float64(6*3600)))

// Session sources the startup sweep may close
var orphanSweepSources = []string{"tui", "desktop", "subagent", "unknown"}

var (
	startupOrphanSweepMu  sync.Mutex
	startupOrphanSweepRan bool
)

// Timer and config seams of the reaper, the real functions swapped only where a hermes case installs a fake
var (
	reaperAfterFunc            = time.AfterFunc
	reaperLoadCfg              = loadCfg
	sessionOrphanReaperEnabled = orphanReaperEnabled
)

// Runs fn once after delay on its own goroutine, logging a panic at the given level instead of raising
func reaperDaemonTimer(delay float64, fn func(), failLog string, level string) {
	reaperAfterFunc(time.Duration(delay*float64(time.Second)), func() {
		defer func() {
			if failure := recover(); failure != nil {
				text := fmt.Sprintf("%s: %s", failLog, failureText(failure))
				if level == "warning" {
					logger().Warn(text)
				} else {
					logger().Debug(text)
				}
			}
		}()
		fn()
	})
}

// Whether the startup orphan sweep is on, failing open when the config is unreadable or silent
func orphanReaperEnabled(ctx context.Context) (enabled bool) {
	defer func() {
		if recover() != nil {
			enabled = true
		}
	}()
	cfg := reaperLoadCfg(ctx)
	if dashboard, isDict := cfg["dashboard"].(map[string]any); isDict {
		if value, present := dashboard["startup_orphan_sweep"]; present {
			if value == nil {
				return true
			}
			return isTruthyValue(value)
		}
	}
	return true
}

// Ends the tui, desktop and subagent rows a dead process left open, sparing every id this process holds
func sweepOrphanedSessionRows(ctx context.Context) []string {
	db := reaperDB(ctx)
	if db == nil || sessionTTLS <= 0 {
		return []string{}
	}
	liveIDs := map[string]bool{}
	for _, live := range tuigateway.LiveSessions() {
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
		logger().Info(fmt.Sprintf("Closed %d orphaned session row(s) from a previous gateway process (startup_orphan_reap): %s",
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

// Heartbeat seams, the real functions swapped only where a hermes case installs a fake
var (
	reaperDB                = getDB
	backendIDForThisProcess = backendIDOfProcess
	refreshBackendHeartbeat = refreshHeartbeatRow
)

// Heartbeat row identity of this process, its pid plus a startup nonce so a reused pid never inherits a row
func backendIDOfProcess(ctx context.Context) string {
	return fmt.Sprintf("%s@%s:%d:%s", currentProfileName(ctx), reaperHostname(), os.Getpid(), backendNonce)
}

// Refreshes the heartbeat row of this backend, nothing when the store is unavailable
func refreshHeartbeatRow(ctx context.Context) {
	db := reaperDB(ctx)
	if db == nil {
		return
	}
	err := db.RegisterBackendHeartbeat(ctx, state.BackendHeartbeat{BackendID: backendIDForThisProcess(ctx), PID: os.Getpid(),
		StartedAt: gatewayStartedAtOnce(), Profile: currentProfileName(ctx), Host: reaperHostname()})
	if err != nil {
		logger().Debug(fmt.Sprintf("backend heartbeat refresh failed: %s", err))
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
				logger().Debug(fmt.Sprintf("initial backend heartbeat write failed: %s", failureText(failure)))
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
			if db := reaperDB(exitCtx); db != nil {
				_, _ = db.ClearBackendHeartbeat(exitCtx, backendIDForThisProcess(exitCtx))
			}
		}()
	})
	go func() {
		defer threadExceptHook("hermes-gateway-heartbeat")
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
						logger().Debug(fmt.Sprintf("heartbeat refresh loop iteration failed: %s", failureText(failure)))
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
