package memory

import (
	"context"
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

const (
	// Even forced trims honor this floor so a burst of agent closes coalesces into one collection
	trimForceFloor = 5 * time.Second
	// A process turn free this long runs the one full collection the busy path trims skip
	IdleCollectAfter = 300 * time.Second
)

var (
	trimMu        sync.Mutex
	lastTrim      time.Time
	lastAllocTrim time.Time
	trimCallCount int

	// Seams the trim tests replace
	trimNow        = time.Now
	trimCollect    = runtime.GC
	trimRelease    = releaseOSMemory
	readProcStatus = readLinuxProcStatus
)

// Returns freed heap pages to the OS, one when the released page count grew and zero otherwise
func releaseOSMemory() int {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	debug.FreeOSMemory()
	runtime.ReadMemStats(&after)
	if after.HeapReleased > before.HeapReleased {
		return 1
	}
	return 0
}

// Trim settings of the home in effect: enabled, cooldown seconds, log cadence and minimum logged delta
func trimSettings(ctx context.Context, env *tools.Env) (bool, float64, int, float64) {
	sctx := registrySettings(ctx, env)
	return settings.ContextMemoryTrimEnabled.Get(sctx), max(0, settings.ContextMemoryTrimCooldownSeconds.Get(sctx).Seconds()),
		max(1, settings.ContextMemoryTrimLogEveryN.Get(sctx)), max(0, settings.ContextMemoryTrimInfoLogMinDeltaMb.Get(sctx))
}

// Linux process status text, false off Linux or when unreadable
func readLinuxProcStatus() (string, bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "", false
	}
	return string(b), true
}

// Process memory telemetry for trim logs, resident sizes on Linux only
type MemorySnapshot struct {
	RSSKiB       *int
	RSSAnonKiB   *int
	ThreadCount  int
	HistoryBytes *int
}

// Lightweight process memory telemetry, history bytes recorded when given and not negative
func CollectMemorySnapshot(historyBytes *int) MemorySnapshot {
	snap := MemorySnapshot{ThreadCount: runtime.NumGoroutine()}
	if status, ok := readProcStatus(); ok && status != "" {
		for _, line := range strings.Split(universalNewlines(status), "\n") {
			key, raw, found := strings.Cut(line, ":")
			if !found || (key != "VmRSS" && key != "RssAnon") {
				continue
			}
			fields := strings.Fields(raw)
			if len(fields) == 0 || !isASCIIDigits(fields[0]) {
				continue
			}
			n, err := strconv.Atoi(fields[0])
			if err != nil {
				continue
			}
			if key == "VmRSS" {
				snap.RSSKiB = &n
			} else {
				snap.RSSAnonKiB = &n
			}
		}
	}
	if historyBytes != nil && *historyBytes >= 0 {
		h := *historyBytes
		snap.HistoryBytes = &h
	}
	return snap
}

func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func optInt(p *int) string {
	if p == nil {
		return "None"
	}
	return strconv.Itoa(*p)
}

// Whether a successful trim is logged, forced trims always and periodic ones by cadence and resident delta
func shouldLogTrim(force bool, logEveryN, callCount int, before, after MemorySnapshot, infoLogMinDeltaMB float64) bool {
	if force {
		return true
	}
	if callCount%logEveryN != 0 {
		return false
	}
	if before.RSSKiB == nil || after.RSSKiB == nil {
		return true
	}
	return math.Abs(float64(*after.RSSKiB-*before.RSSKiB)) >= infoLogMinDeltaMB*1024
}

// Tunes one trim, AllocatorOnly skipping the explicit full collection and Cooldown overriding the configured one
type TrimOptions struct {
	Force         bool
	Reason        string
	Cooldown      *float64
	AllocatorOnly bool
}

// Collects and releases free heap pages, true only when pages were released, cooldown and kill switch returning false
func TrimMemory(ctx context.Context, env *tools.Env, opts TrimOptions) (released bool) {
	enabled, configuredCooldown, logEveryN, minDelta := trimSettings(ctx, env)
	if !enabled {
		return false
	}
	collect := !opts.AllocatorOnly
	trimMu.Lock()
	defer trimMu.Unlock()
	now := trimNow()
	cooldown := configuredCooldown
	if opts.Cooldown != nil {
		cooldown = max(0, *opts.Cooldown)
	}
	last := lastTrim
	if !collect && lastAllocTrim.After(last) {
		last = lastAllocTrim
	}
	window := cooldown
	if opts.Force {
		window = trimForceFloor.Seconds()
	}
	if !last.IsZero() && now.Sub(last).Seconds() < window {
		return false
	}
	lastAllocTrim = now
	if collect {
		lastTrim = now
	}
	reason := opts.Reason
	if reason == "" {
		reason = "cleanup"
	}
	defer func() {
		if rec := recover(); rec != nil {
			envLog(env).Warn(fmt.Sprintf("memory trim failed after %s: %T: %v", reason, rec, rec))
			released = false
		}
	}()
	before := CollectMemorySnapshot(nil)
	started := time.Now()
	if collect {
		trimCollect()
	}
	result := trimRelease()
	released = result != 0
	after := CollectMemorySnapshot(nil)
	duration := float64(time.Since(started).Microseconds()) / 1000
	trimCallCount++
	if released && shouldLogTrim(opts.Force, logEveryN, trimCallCount, before, after, minDelta) {
		envLog(env).Info(fmt.Sprintf("memory trim: reason=%s malloc_trim=%d collected=%s rss_kib=%s->%s rss_anon_kib=%s->%s "+
			"threads=%d duration_ms=%.1f", reason, result, anyutil.String(collect), optInt(before.RSSKiB), optInt(after.RSSKiB),
			optInt(before.RSSAnonKiB), optInt(after.RSSAnonKiB), after.ThreadCount, duration))
	}
	return released
}

// Process wide quiet clock of the review idle queue, ok false while a turn runs or before the first turn ends
type QuietClock interface {
	QuietPeriod() (quietFor time.Duration, quietSince time.Time, ok bool)
}

var (
	idleMu                      sync.Mutex
	idleCollectedFor            time.Time
	idleCollectedSet            bool
	agentClosedSinceIdleCollect bool
)

// Re-arms the idle collection after an agent closed, even within one quiet period
func NoteAgentClosed() {
	idleMu.Lock()
	agentClosedSinceIdleCollect = true
	idleMu.Unlock()
}

// Timer lane trim, allocator release only plus one full collection per quiet period past the idle threshold
func TrimMemoryWhenIdle(ctx context.Context, env *tools.Env, reason string, clock QuietClock) bool {
	quietFor, quietSince, ok := clock.QuietPeriod()
	idleMu.Lock()
	due := ok && quietFor >= IdleCollectAfter && (!idleCollectedSet || !quietSince.Equal(idleCollectedFor) || agentClosedSinceIdleCollect)
	if due {
		idleCollectedFor, idleCollectedSet, agentClosedSinceIdleCollect = quietSince, true, false
	}
	idleMu.Unlock()
	if enabled, _, _, _ := trimSettings(ctx, env); !due || !enabled {
		return TrimMemory(ctx, env, TrimOptions{Reason: reason, AllocatorOnly: true})
	}
	started := time.Now()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	freesBefore := stats.Frees
	trimCollect()
	runtime.ReadMemStats(&stats)
	envLog(env).Info(fmt.Sprintf("idle full collection: reason=%s quiet_s=%d unreachable=%d duration_ms=%.1f",
		reason, int(quietFor.Seconds()), stats.Frees-freesBefore, float64(time.Since(started).Microseconds())/1000))
	return TrimMemory(ctx, env, TrimOptions{Force: true, Reason: reason + " after idle collection", AllocatorOnly: true})
}
