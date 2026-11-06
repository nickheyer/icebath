package observability

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// RelaunchedPidEnv carries the pid an in place exec keeps so the relaunched program is not a startup
const RelaunchedPidEnv = "ICEBATH_RELAUNCHED_PID"

const (
	maxStartupRecorded = 1024
	launchIDMaxLen     = 64
)

var startupThresholds = []threshold{
	{500, "lt_500ms"}, {1_000, "500ms_to_1s"}, {2_000, "1s_to_2s"}, {5_000, "2s_to_5s"}, {10_000, "5s_to_10s"},
}

type startupKey struct {
	pid      int
	surface  string
	launchID string
}

var (
	startupMu        sync.Mutex
	startupRecorded  = map[startupKey]bool{}
	processStartedAt = ProcessStartedAt
	startupPid       = os.Getpid
	startupGo        = func(run func()) { go run() }
)

// Buckets a non negative duration in milliseconds, false for anything else
func LatencyBucket(elapsedMS any) (string, bool) {
	value, ok := nonNegativeNumber(elapsedMS)
	if !ok {
		return "", false
	}
	return bucket(value, startupThresholds, "gte_10s"), true
}

// Bounded latency row for a known startup surface, nil for an unknown surface or bad duration
func StartupLatencyFields(surface, elapsedMS any) map[string]string {
	b, ok := LatencyBucket(elapsedMS)
	name, isText := surface.(string)
	if !isText || !StartupSurfaces.Has(name) || !ok {
		return nil
	}
	return map[string]string{"latency_bucket": b, "surface": name}
}

// Records one measured startup when shared metrics are on and never fails the caller
func RecordStartupLatency(ctx context.Context, surface string, elapsedMS any) {
	emit(ctx, StartupLatencyMark, func() map[string]string { return StartupLatencyFields(surface, elapsedMS) })
}

// Epoch seconds at which the OS created this process, false when unreadable
func ProcessStartedAt() (float64, bool) {
	started, err := proc.Birth(os.Getpid())
	if err != nil {
		return 0, false
	}
	return started, true
}

// Stamps this pid into the environment right before an in place exec inherits both
func MarkInPlaceRelaunch() {
	os.Setenv(RelaunchedPidEnv, strconv.Itoa(startupPid()))
}

// Reports whether this process is the program an in place exec replaced, keeping its pid
func relaunchedInPlace() bool {
	v, ok := os.LookupEnv(RelaunchedPidEnv)
	return ok && v == strconv.Itoa(startupPid())
}

// Latches one surface and client launch per process, refusing once the latch set is full
func startupClaim(surface, launchID string) bool {
	key := startupKey{startupPid(), surface, launchID}
	startupMu.Lock()
	defer startupMu.Unlock()
	if startupRecorded[key] || len(startupRecorded) >= maxStartupRecorded {
		return false
	}
	startupRecorded[key] = true
	return true
}

// Records process creation to the ready instant, nothing when the creation time is unreadable
func recordSinceProcessStart(ctx context.Context, surface string, readyAt float64) {
	started, ok := processStartedAt()
	if ok {
		RecordStartupLatency(ctx, surface, max(0.0, readyAt-started)*1000)
	}
}

// Records process start to now once per process and surface, optionally off the caller's goroutine
func RecordProcessReady(ctx context.Context, surface string, background bool) {
	readyAt := float64(time.Now().UnixNano()) / 1e9
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("Startup latency for %s not recorded", surface), "error", r)
		}
	}()
	if relaunchedInPlace() || !startupClaim(surface, "") || !relayEnabled(ctx) {
		return
	}
	if !background {
		recordSinceProcessStart(ctx, surface, readyAt)
		return
	}
	bound := context.WithoutCancel(ctx)
	startupGo(func() { recordSinceProcessStart(bound, surface, readyAt) })
}

// Returns an after render handler that records the first rendered CLI prompt once
func CliPromptReadyHandler(ctx context.Context) func(app any) {
	return func(any) { RecordProcessReady(ctx, "cli", true) }
}

// Records a one shot query run as ready, kanban worker dispatches staying out of CLI startup
func RecordCliOneShotReady(ctx context.Context) {
	if os.Getenv("ICEBATH_KANBAN_TASK") == "" {
		RecordProcessReady(ctx, "cli", false)
	}
}

// Client launch id used only as a local latch key, empty when absent or too long
func launchKey(launchID any) string {
	if s, ok := launchID.(string); ok && len([]rune(s)) <= launchIDMaxLen {
		return s
	}
	return ""
}

// Records a TUI or Desktop client's own launch to ready measurement once per surface and launch
func RecordRpcStartupLatency(ctx context.Context, clientSurface, elapsedMS, launchID any) {
	name, _ := clientSurface.(string)
	surface, known := map[string]string{"desktop": "desktop_attach", "desktop_attach": "desktop_attach", "tui": "tui"}[name]
	if !known {
		return
	}
	if _, ok := LatencyBucket(elapsedMS); ok && startupClaim(surface, launchKey(launchID)) {
		RecordStartupLatency(ctx, surface, elapsedMS)
	}
}
