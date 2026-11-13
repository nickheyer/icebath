package system

import (
	"math"
	"slices"
	"strings"
	"sync"
)

// ActiveWork is the running turn summary one renderer publishes, titles only for titled sessions
type ActiveWork struct {
	Titles []string `json:"titles"`
	Count  int      `json:"count"`
}

// NoActiveWork is the idle summary
var NoActiveWork = ActiveWork{Count: 0, Titles: []string{}}

// Coerces an untrusted renderer payload into an ActiveWork with count never below the title count
func NormalizeActiveWork(payload any) ActiveWork {
	raw, ok := payload.(map[string]any)
	if !ok || raw == nil {
		return ActiveWork{Count: 0, Titles: []string{}}
	}
	titles := []string{}
	if list, ok := raw["titles"].([]any); ok {
		for _, item := range list {
			title, ok := item.(string)
			if !ok {
				continue
			}
			if trimmed := jsTrim(title); trimmed != "" {
				titles = append(titles, trimmed)
			}
		}
	}
	count := 0
	if n, ok := raw["count"].(float64); ok && !math.IsNaN(n) && !math.IsInf(n, 0) {
		count = int(math.Max(0, math.Floor(n)))
	}
	return ActiveWork{Count: max(count, len(titles)), Titles: titles}
}

// Merges every window report into one, titles deduplicated because windows can show the same session
func MergeActiveWork(reports []ActiveWork) ActiveWork {
	titles := []string{}
	count := 0
	for _, report := range reports {
		count = max(count, report.Count)
		for _, title := range report.Titles {
			if !slices.Contains(titles, title) {
				titles = append(titles, title)
			}
		}
	}
	return ActiveWork{Count: max(count, len(titles)), Titles: titles}
}

// String.prototype.trim whitespace and line terminators
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
			return true
		}
		return r >= 0x2000 && r <= 0x200a
	})
}

// ActiveWorkTracker keeps the report of each renderer window and the last summary any renderer sent
type ActiveWorkTracker struct {
	mu       sync.Mutex
	byWindow map[string]ActiveWork
	order    []string
	lastSeen ActiveWork
}

// Empty tracker whose last seen summary is idle
func NewActiveWorkTracker() *ActiveWorkTracker {
	return &ActiveWorkTracker{byWindow: map[string]ActiveWork{}, lastSeen: ActiveWork{Count: 0, Titles: []string{}}}
}

// Records a window report and the last seen summary, true when the window was not tracked before
func (t *ActiveWorkTracker) Set(windowID string, work ActiveWork) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, known := t.byWindow[windowID]
	if !known {
		t.order = append(t.order, windowID)
	}
	t.byWindow[windowID] = work
	t.lastSeen = work
	return !known
}

// Drops the report of a destroyed or crashed window, true when it was tracked
func (t *ActiveWorkTracker) Forget(windowID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.byWindow[windowID]; !ok {
		return false
	}
	delete(t.byWindow, windowID)
	t.order = slices.DeleteFunc(t.order, func(id string) bool { return id == windowID })
	return true
}

// True while the window has a live report
func (t *ActiveWorkTracker) Has(windowID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.byWindow[windowID]
	return ok
}

func (t *ActiveWorkTracker) reports() []ActiveWork {
	out := make([]ActiveWork, 0, len(t.order)+1)
	for _, id := range t.order {
		out = append(out, t.byWindow[id])
	}
	return out
}

// Merged picture of the live window reports
func (t *ActiveWorkTracker) Merged() ActiveWork {
	t.mu.Lock()
	defer t.mu.Unlock()
	return MergeActiveWork(t.reports())
}

// Merged picture of the live reports plus the last summary, which survives a reload mid turn
func (t *ActiveWorkTracker) MergedWithLastSeen() ActiveWork {
	t.mu.Lock()
	defer t.mu.Unlock()
	return MergeActiveWork(append(t.reports(), t.lastSeen))
}

// True while any live window reports a turn in flight
func (t *ActiveWorkTracker) AnyTurnInFlight() bool {
	return t.Merged().Count > 0
}
