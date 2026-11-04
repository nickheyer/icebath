package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
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

// FormatISO renders t the way Python isoformat does, microseconds only when present
func FormatISO(t time.Time) string {
	base := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		base += fmt.Sprintf(".%06d", us)
	}
	_, off := t.Zone()
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	return fmt.Sprintf("%s%s%02d:%02d", base, sign, off/3600, (off%3600)/60)
}

// FormatISOUTC renders an instant in UTC with a +00:00 suffix
func FormatISOUTC(t time.Time) string { return FormatISO(t.UTC()) }

// ErrNotISO reports a value that is not an ISO 8601 timestamp
var ErrNotISO = errors.New("not an ISO 8601 timestamp")

// ParseISO accepts the forms Python fromisoformat accepts, naive values kept as UTC wall fields
func ParseISO(value string) (ISOTime, error) {
	s := strings.TrimSpace(value)
	if len(s) < 10 {
		return ISOTime{}, ErrNotISO
	}
	year, err1 := strconv.Atoi(s[0:4])
	month, err2 := strconv.Atoi(s[5:7])
	day, err3 := strconv.Atoi(s[8:10])
	if err1 != nil || err2 != nil || err3 != nil || s[4] != '-' || s[7] != '-' {
		return ISOTime{}, ErrNotISO
	}
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return ISOTime{}, ErrNotISO
	}
	rest := s[10:]
	if rest == "" {
		return ISOTime{Time: time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)}, nil
	}
	if rest[0] != 'T' && rest[0] != 't' && rest[0] != ' ' {
		return ISOTime{}, ErrNotISO
	}
	rest = rest[1:]
	loc := time.UTC
	aware := false
	if idx := strings.IndexAny(rest, "+-Zz"); idx >= 0 {
		offset := rest[idx:]
		rest = rest[:idx]
		off, err := parseOffset(offset)
		if err != nil {
			return ISOTime{}, err
		}
		loc = time.FixedZone("", off)
		if off == 0 {
			loc = time.UTC
		}
		aware = true
	}
	hour, minute, second, nanos, err := parseClock(rest)
	if err != nil {
		return ISOTime{}, err
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, nanos, loc)
	if t.Day() != day || t.Month() != time.Month(month) {
		return ISOTime{}, ErrNotISO
	}
	return ISOTime{Time: t, Aware: aware}, nil
}

// Parses HH:MM[:SS[.fraction]] clock text
func parseClock(s string) (int, int, int, int, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, 0, 0, 0, ErrNotISO
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 || len(parts[0]) != 2 {
		return 0, 0, 0, 0, ErrNotISO
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 || len(parts[1]) != 2 {
		return 0, 0, 0, 0, ErrNotISO
	}
	second, nanos := 0, 0
	if len(parts) == 3 {
		sec := parts[2]
		frac := ""
		if dot := strings.IndexAny(sec, ".,"); dot >= 0 {
			frac = sec[dot+1:]
			sec = sec[:dot]
		}
		second, err = strconv.Atoi(sec)
		if err != nil || second < 0 || second > 59 || len(sec) != 2 {
			return 0, 0, 0, 0, ErrNotISO
		}
		if frac != "" {
			if len(frac) > 9 {
				frac = frac[:9]
			}
			n, err := strconv.Atoi(frac)
			if err != nil {
				return 0, 0, 0, 0, ErrNotISO
			}
			for i := len(frac); i < 9; i++ {
				n *= 10
			}
			nanos = n
		}
	}
	return hour, minute, second, nanos, nil
}

// Parses Z, +HH, +HHMM or +HH:MM into seconds east of UTC
func parseOffset(s string) (int, error) {
	if s == "Z" || s == "z" {
		return 0, nil
	}
	sign := 1
	switch s[0] {
	case '+':
	case '-':
		sign = -1
	default:
		return 0, ErrNotISO
	}
	body := strings.ReplaceAll(s[1:], ":", "")
	if len(body) != 2 && len(body) != 4 && len(body) != 6 {
		return 0, ErrNotISO
	}
	h, err := strconv.Atoi(body[:2])
	if err != nil || h > 23 {
		return 0, ErrNotISO
	}
	m, sec := 0, 0
	if len(body) >= 4 {
		if m, err = strconv.Atoi(body[2:4]); err != nil || m > 59 {
			return 0, ErrNotISO
		}
	}
	if len(body) == 6 {
		if sec, err = strconv.Atoi(body[4:6]); err != nil || sec > 59 {
			return 0, ErrNotISO
		}
	}
	return sign * (h*3600 + m*60 + sec), nil
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
