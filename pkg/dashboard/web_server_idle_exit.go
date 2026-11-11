package dashboard

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Seconds a client-less SSH isolated backend waits before retiring
const DefaultIdleGraceS = 900.0

// Websocket ping cadence for a client at the far end of a tunnel
const (
	TunnelWSPingIntervalS = 60.0
	TunnelWSPingTimeoutS  = 600.0
)

func idleExitLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.web_server_idle_exit")
}

var monotonicEpoch = time.Now()

// Seconds on the monotonic clock
func monotonic() float64 { return time.Since(monotonicEpoch).Seconds() }

// Live accepted websocket count and the moment the last client left
type IdleClientTracker struct {
	now          func() float64
	mu           sync.Mutex
	live         int
	lastClientAt float64
}

// Tracker reading the given clock, the monotonic clock when nil
func NewIdleClientTracker(now func() float64) *IdleClientTracker {
	if now == nil {
		now = monotonic
	}
	return &IdleClientTracker{now: now, lastClientAt: now()}
}

// Counts an accepted client
func (t *IdleClientTracker) OnOpen() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.live++
	t.lastClientAt = t.now()
}

// Uncounts a client that left, restarting the idle clock
func (t *IdleClientTracker) OnClose() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.live = max(0, t.live-1)
	t.lastClientAt = t.now()
}

// Number of connected clients
func (t *IdleClientTracker) LiveCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.live
}

// Seconds since the last client left, zero while one is connected
func (t *IdleClientTracker) IdleFor() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.live > 0 {
		return 0
	}
	return t.now() - t.lastClientAt
}

// App counting websocket sessions from their accept until the scope ends
func WrapASGIWithWSTracking(app ASGIApp, tracker *IdleClientTracker) ASGIApp {
	return func(ctx context.Context, scope *Scope, receive Receive, send Send) error {
		if scope.Type != "websocket" {
			return app(ctx, scope, receive, send)
		}
		var accepted atomic.Bool
		defer func() {
			if accepted.Load() {
				tracker.OnClose()
			}
		}()
		return app(ctx, scope, receive, func(ctx context.Context, m Message) error {
			if m.Type == "websocket.accept" && accepted.CompareAndSwap(false, true) {
				tracker.OnOpen()
			}
			return send(ctx, m)
		})
	}
}

// Worker handle a session slot holds, live while its goroutine runs
type AliveThread interface{ IsAlive() bool }

// Whether a session record holds a running turn, a queued prompt or a live worker
func sessionWorkInFlight(s *tuigateway.Session) (bool, error) {
	for _, key := range []string{"running", "queued_prompt", "queued_prompts", "_auto_continue_scheduled"} {
		if pyrepr.Truthy(s.Get(key)) {
			return true, nil
		}
	}
	for _, key := range []string{"_run_thread", "_agent_build_thread"} {
		v := s.Get(key)
		if v == nil {
			continue
		}
		t, ok := v.(AliveThread)
		if !ok {
			return false, fmt.Errorf("'%s' object has no attribute 'is_alive'", pyrepr.TypeName(v))
		}
		if t.IsAlive() {
			return true, nil
		}
	}
	return false, nil
}

var probeFailureLogged atomic.Bool

// Ledger holding work by name, empty when all are empty, not ok when one cannot be read
func BusyLedger() (string, bool) {
	if provider.Retirement.ActiveCount() > 0 {
		return "retirement_admission", true
	}
	var busy []string
	for _, live := range tuigateway.LiveSessions() {
		inFlight, err := sessionWorkInFlight(live.Session)
		if err != nil {
			if probeFailureLogged.CompareAndSwap(false, true) {
				idleExitLog().Warn("idle-exit turn probe unavailable; this backend will not self-retire", "error", err)
			}
			return "", false
		}
		if inFlight {
			busy = append(busy, live.SID)
		}
	}
	if len(busy) > 0 {
		return "session:" + strings.Join(busy, ","), true
	}
	if tools.AsyncDelegationActiveCount() > 0 {
		return "delegation", true
	}
	jobs := cron.GetRunningJobIDs()
	if len(jobs) == 0 {
		return "", true
	}
	ids := make([]string, 0, len(jobs))
	for id := range jobs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return "cron:" + strings.Join(ids, ","), true
}

// Whether a turn is in flight, not ok when the ledgers cannot be read
func TurnInFlight() (bool, bool) {
	ledger, ok := BusyLedger()
	return ledger != "", ok
}

// Whether no client has been connected for the grace window and no turn provably runs
func ShouldExitIdle(tracker *IdleClientTracker, graceS float64, probe func() (bool, bool)) bool {
	if probe == nil {
		probe = TurnInFlight
	}
	if tracker.IdleFor() < graceS {
		return false
	}
	busy, ok := probe()
	return ok && !busy
}

// Goroutine setting the exit flag once the backend has been idle past the grace window
func StartIdleWatchdog(shouldExit *atomic.Bool, tracker *IdleClientTracker, graceS, pollS float64, probe func() (bool, bool)) <-chan struct{} {
	pollS = min(pollS, max(0.5, graceS/4))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !shouldExit.Load() {
			if ShouldExitIdle(tracker, graceS, probe) {
				idleExitLog().Warn(fmt.Sprintf("SSH-isolated backend idle for %.0fs with no client and no running turn; exiting.", tracker.IdleFor()))
				shouldExit.Store(true)
				return
			}
			time.Sleep(time.Duration(pollS * float64(time.Second)))
		}
	}()
	return done
}
