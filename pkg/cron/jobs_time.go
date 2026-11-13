package cron

import (
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Bounds of the per job catch up override in minutes, zero meaning never catch up
const (
	MinGraceMinutes = 0
	MaxGraceMinutes = 24 * 60
)

// Warns once per distinct corrupt persisted knob value so a bad record cannot spam every tick
type knobWarner struct {
	mu     sync.Mutex
	warned map[string]bool
	log    *slog.Logger
}

func newKnobWarner(log *slog.Logger) *knobWarner {
	return &knobWarner{warned: map[string]bool{}, log: log}
}

func (w *knobWarner) warnOnce(kind string, value any, detail string) {
	key := kind + ":" + pyrepr.Repr(value)
	w.mu.Lock()
	seen := w.warned[key]
	w.warned[key] = true
	w.mu.Unlock()
	if seen || w.log == nil {
		return
	}
	w.log.Warn(fmt.Sprintf("Job %s %s is invalid (%s); falling back to the profile setting. Fix or clear it with "+
		"`icebath cron edit`.", kind, pyrepr.Repr(value), detail))
}

// Forgets every warned value
func (w *knobWarner) reset() {
	w.mu.Lock()
	w.warned = map[string]bool{}
	w.mu.Unlock()
}

// NormalizeTimezone validates a job timezone and returns its IANA key, nil meaning inherit the profile
func NormalizeTimezone(value any) (*string, error) {
	if value == nil {
		return nil, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("timezone must be an IANA name such as 'Asia/Hong_Kong', not %T.", value)
	}
	name := strings.TrimSpace(text)
	if name == "" {
		return nil, nil
	}
	if _, err := time.LoadLocation(name); err != nil || strings.ContainsAny(name, " \t") || strings.HasPrefix(name, "/") {
		return nil, fmt.Errorf("Unknown timezone %q. Use an IANA key from the system zone database, e.g. 'Asia/Hong_Kong'.", name)
	}
	return &name, nil
}

// NormalizeGraceMinutes validates a grace override, nil meaning use the computed window
func NormalizeGraceMinutes(value any) (*int, error) {
	if value == nil {
		return nil, nil
	}
	var minutes int
	switch v := value.(type) {
	case bool:
		return nil, fmt.Errorf("grace_minutes must be a whole number of minutes (0 = never catch up), not a boolean.")
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return nil, nil
		}
		digits := strings.TrimLeft(s, "+-")
		if digits == "" || strings.Trim(digits, "0123456789") != "" {
			return nil, fmt.Errorf("grace_minutes must be a whole number of minutes, not %q.", v)
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("grace_minutes must be a whole number of minutes, not %q.", v)
		}
		minutes = n
	case int:
		minutes = v
	case int32:
		minutes = int(v)
	case int64:
		minutes = int(v)
	case float32:
		if float64(v) != math.Trunc(float64(v)) {
			return nil, fmt.Errorf("grace_minutes must be a whole number of minutes, not %v.", v)
		}
		minutes = int(v)
	case float64:
		if v != math.Trunc(v) {
			return nil, fmt.Errorf("grace_minutes must be a whole number of minutes, not %v.", v)
		}
		minutes = int(v)
	default:
		return nil, fmt.Errorf("grace_minutes must be a whole number of minutes, not %v.", value)
	}
	if minutes < MinGraceMinutes || minutes > MaxGraceMinutes {
		return nil, fmt.Errorf("grace_minutes must be between %d and %d (0 = never catch up), got %d.", MinGraceMinutes, MaxGraceMinutes, minutes)
	}
	return &minutes, nil
}

// Zone a job schedule is expressed in, nil to inherit the profile, never failing on a corrupt value
func (w *knobWarner) zoneForJob(timezone *string) *time.Location {
	if timezone == nil {
		return nil
	}
	name := strings.TrimSpace(*timezone)
	if name == "" {
		return nil
	}
	loc, err := pydatetime.ZoneInfo(name)
	if err != nil {
		w.warnOnce("timezone", *timezone, err.Error())
		return nil
	}
	return loc
}

// Catch up window in seconds from an explicit override, nil to compute it from cadence
func (w *knobWarner) graceSecondsOverride(grace *int, raw any) *int {
	if grace == nil && raw == nil {
		return nil
	}
	var value any = raw
	if grace != nil {
		value = *grace
	}
	minutes, err := NormalizeGraceMinutes(value)
	if err != nil {
		w.warnOnce("grace_minutes", value, err.Error())
		return nil
	}
	if minutes == nil {
		return nil
	}
	seconds := *minutes * 60
	return &seconds
}

// Drops a cleared timezone key while keeping a zero grace override
func pruneInheritedKnobs(job *Job) {
	if job.Timezone != nil && strings.TrimSpace(*job.Timezone) == "" {
		job.Timezone = nil
	}
}

// Instant of a one shot run_at, an offset less value read in the job zone, zero when unparseable
func (c *clock) oneshotInstant(value string, zone *time.Location) (time.Time, bool) {
	v, err := ParseISO(value)
	if err != nil {
		return time.Time{}, false
	}
	if !v.Aware && zone != nil {
		w := v.Time
		return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), zone), true
	}
	return c.ensureAware(v, zone), true
}
