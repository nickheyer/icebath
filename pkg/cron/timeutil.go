package cron

import (
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
)

// ISOTime is a parsed ISO 8601 stamp that remembers whether it carried a UTC offset
type ISOTime struct {
	Time  time.Time
	Aware bool
}

// Zone that stands for the system local clock at the current moment as a fixed offset
func systemFixedZone(now time.Time) *time.Location {
	name, off := now.In(time.Local).Zone()
	return time.FixedZone(name, off)
}

// FormatISO renders t the way Python isoformat does
func FormatISO(t time.Time) string {
	return pydatetime.ISOFormat(t)
}

// FormatISOUTC renders an instant in UTC with a +00:00 suffix
func FormatISOUTC(t time.Time) string { return FormatISO(t.UTC()) }

// ParseISO parses like Python datetime fromisoformat, naive values kept as UTC wall fields
func ParseISO(value string) (ISOTime, error) {
	t, aware, err := pydatetime.ParseISOFormat(value)
	if err != nil {
		return ISOTime{}, err
	}
	return ISOTime{Time: t, Aware: aware}, nil
}

// Aware instant in target, a naive value read as system local wall time of the current offset
func (c *clock) ensureAware(v ISOTime, target *time.Location) time.Time {
	if target == nil {
		target = c.zone()
	}
	if !v.Aware {
		w := v.Time
		fixed := systemFixedZone(c.now())
		return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), fixed).In(target)
	}
	return v.Time.In(target)
}

// Parsed and normalized stamp, zero and false when value is not ISO text
func (c *clock) parseAware(value string, target *time.Location) (time.Time, bool) {
	v, err := ParseISO(value)
	if err != nil {
		return time.Time{}, false
	}
	return c.ensureAware(v, target), true
}

// Elapsed seconds between two instants independent of wall clock
func elapsedSeconds(later, earlier time.Time) float64 {
	return later.Sub(earlier).Seconds()
}

// Instant seconds later than t, kept in the location of t
func secondsAfter(t time.Time, seconds float64) time.Time {
	return t.Add(time.Duration(seconds * float64(time.Second))).In(t.Location())
}

// Wall clock of t expressed as the same clock fields in UTC
func wallOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// Both resolutions of a wall clock in loc, fold zero using the offset before a transition
func wallInZone(wall time.Time, loc *time.Location) (time.Time, time.Time) {
	g := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), wall.Nanosecond(), loc)
	_, og := g.Zone()
	w := wall.Unix()
	ns := int64(wall.Nanosecond())
	make := func(off int) time.Time { return time.Unix(w-int64(off), ns).In(loc) }
	start, end := g.ZoneBounds()
	if !end.IsZero() {
		_, after := end.Zone()
		before := og
		lo, hi := end.Unix()+int64(minInt(before, after)), end.Unix()+int64(maxInt(before, after))
		if w >= lo && w < hi {
			return make(before), make(after)
		}
	}
	if !start.IsZero() {
		_, before := start.Add(-time.Second).Zone()
		after := og
		lo, hi := start.Unix()+int64(minInt(before, after)), start.Unix()+int64(maxInt(before, after))
		if w >= lo && w < hi {
			return make(before), make(after)
		}
	}
	return make(og), make(og)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Reports whether two aware stamps carry different UTC offsets
func offsetMismatch(stored, current time.Time) bool {
	_, a := stored.Zone()
	_, b := current.Zone()
	return a != b
}

// Reports whether the stored wall clock is later than the current wall clock
func storedWallClockIsFuture(stored, current time.Time) bool {
	return wallOf(stored).After(wallOf(current))
}

// Compact lateness text such as 31m, 2h 30m or 45s
func FormatLateness(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	total := int64(seconds)
	if total < 60 {
		return fmt.Sprintf("%ds", total)
	}
	minutes := total / 60
	hours := minutes / 60
	minutes %= 60
	days := hours / 24
	hours %= 24
	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 && days == 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if len(parts) == 0 {
		return "0m"
	}
	return strings.Join(parts, " ")
}
