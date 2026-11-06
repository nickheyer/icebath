package observability

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

const (
	BusyArrivalMetric = "icebath.gateway.busy_arrival.count"
	BusyPeakField     = "gateway_peak_busy_chats_bucket"
)

var (
	BusyArrivalDispositions = Set("queued", "steered", "interrupted", "merged", "dropped", "other")
	BusyWaitBuckets         = Set("lt_5s", "5s_to_30s", "30s_to_2m", "2m_to_10m", "gte_10m", "never")
)

const peakDays = 8

var (
	peakMu        sync.Mutex
	runningChats  = map[string]string{}
	runningPeaks  = map[string]map[string]int{}
	resolvedHomes = map[string]string{}
	busyNow       = pyTime
)

// ProfileHomeResolver resolves the profile home a message source's turn runs under, implemented by the gateway runner
type ProfileHomeResolver interface {
	ResolveProfileHomeForSource(source any) (string, error)
}

func busyDay(epoch float64) string {
	return time.Unix(0, int64(epoch*1e9)).UTC().Format(time.DateOnly)
}

func homeKey(raw string) string {
	if resolved, ok := resolvedHomes[raw]; ok {
		return resolved
	}
	resolved, err := pypath.Resolve(raw)
	if err != nil {
		resolved = raw
	}
	resolvedHomes[raw] = resolved
	return resolved
}

// Counts a chat claimed outside its profile's scope toward the profile owning the session
func NoteRunningChatOwner(runner ProfileHomeResolver, sessionKey string, source any) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Running-chat owner not resolved", "error", fmt.Sprint(r))
		}
	}()
	owner, err := runner.ResolveProfileHomeForSource(source)
	if err != nil {
		slog.Debug("Running-chat owner not resolved", "error", err)
		return
	}
	peakMu.Lock()
	defer peakMu.Unlock()
	runningChats[sessionKey] = homeKey(owner)
}

// Updates each profile's daily peak of chats running at once from the runner's running set
func ObserveRunningChats(ctx context.Context, sessionKeys []string) {
	keys := make(map[string]bool, len(sessionKeys))
	for _, key := range sessionKeys {
		keys[key] = true
	}
	peakMu.Lock()
	defer peakMu.Unlock()
	for key := range runningChats {
		if !keys[key] {
			delete(runningChats, key)
		}
	}
	owner := ""
	for key := range keys {
		if _, ok := runningChats[key]; !ok {
			if owner == "" {
				owner = homeKey(home.Dir(ctx))
			}
			runningChats[key] = owner
		}
	}
	counts := map[string]int{}
	for _, h := range runningChats {
		counts[h]++
	}
	today := busyDay(busyNow())
	for h, count := range counts {
		days := runningPeaks[h]
		if days == nil {
			days = map[string]int{}
			runningPeaks[h] = days
		}
		days[today] = max(days[today], count)
		ordered := make([]string, 0, len(days))
		for day := range days {
			ordered = append(ordered, day)
		}
		sort.Strings(ordered)
		for _, stale := range ordered[:max(0, len(ordered)-peakDays)] {
			delete(days, stale)
		}
	}
}

// Most chats of a profile running at once on a UTC day as this process saw them
func DayPeak(profileHome, day string) int {
	peakMu.Lock()
	owner := homeKey(profileHome)
	running := 0
	for _, h := range runningChats {
		if h == owner {
			running++
		}
	}
	days := maps.Clone(runningPeaks[owner])
	peakMu.Unlock()
	peak := days[day]
	latest := ""
	for d := range days {
		latest = max(latest, d)
	}
	if len(days) == 0 || day >= latest {
		return max(peak, running)
	}
	return peak
}
