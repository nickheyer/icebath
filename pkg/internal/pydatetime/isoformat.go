package pydatetime

import (
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Error text of an ISO string fromisoformat refuses
func invalidISOFormat(s string) error {
	return fmt.Errorf("Invalid isoformat string: %s", pyrepr.Repr(s))
}

// Parses an ISO 8601 string with the rules of CPython datetime fromisoformat, naive results in UTC
func FromISOFormat(s string) (time.Time, error) {
	t, _, err := ParseISOFormat(s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// Parses like fromisoformat keeping the parsed UTC offset as the zone and reporting whether one was present
func ParseISOFormat(s string) (t time.Time, aware bool, err error) {
	rs := []rune(s)
	if len(rs) < 7 {
		return time.Time{}, false, invalidISOFormat(s)
	}
	sep, ok := isoDatetimeSeparator(rs)
	if !ok {
		return time.Time{}, false, invalidISOFormat(s)
	}
	year, month, day, ok := isoParseDate(string(rs[:min(sep, len(rs))]))
	if !ok {
		return time.Time{}, false, invalidISOFormat(s)
	}
	hour, minute, second, micro := 0, 0, 0, 0
	offset := time.Duration(0)
	if len(rs) > sep {
		var offSeconds, offMicro int
		hour, minute, second, micro, offSeconds, offMicro, aware, ok = isoParseTime(string(rs[sep+1:]))
		if !ok {
			return time.Time{}, false, invalidISOFormat(s)
		}
		if aware {
			offset = time.Duration(offSeconds)*time.Second + time.Duration(offMicro)*time.Microsecond
			if offset <= -24*time.Hour || offset >= 24*time.Hour {
				return time.Time{}, false, fmt.Errorf("offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24), not %s", TimedeltaRepr(offset))
			}
		}
	}
	if hour == 24 {
		if minute != 0 || second != 0 || micro != 0 {
			return time.Time{}, false, fmt.Errorf("minute, second, and microsecond must be 0 when hour is 24")
		}
		hour = 0
		day++
		if month >= 1 && month <= 12 && year >= 1 && day > daysInMonth(year, month) {
			day = 1
			month++
			if month > 12 {
				month = 1
				year++
			}
		}
	}
	if year < 1 || year > 9999 {
		return time.Time{}, false, fmt.Errorf("year %d is out of range", year)
	}
	if month < 1 || month > 12 {
		return time.Time{}, false, fmt.Errorf("month must be in 1..12")
	}
	if day < 1 || day > daysInMonth(year, month) {
		return time.Time{}, false, fmt.Errorf("day %d must be in range 1..%d for month %d in year %d", day, daysInMonth(year, month), month, year)
	}
	if hour > 23 {
		return time.Time{}, false, fmt.Errorf("hour must be in 0..23")
	}
	if minute > 59 {
		return time.Time{}, false, fmt.Errorf("minute must be in 0..59")
	}
	if second > 59 {
		return time.Time{}, false, fmt.Errorf("second must be in 0..59")
	}
	instant := time.Date(year, time.Month(month), day, hour, minute, second, micro*1000, time.UTC).Add(-offset)
	if !aware {
		return instant, false, nil
	}
	return instant.In(time.FixedZone("", int(offset/time.Second))), true, nil
}

// Python repr of a timedelta offset for the out of range message
func TimedeltaRepr(d time.Duration) string {
	micros := int64(d / time.Microsecond)
	days := micros / 86_400_000_000
	rem := micros % 86_400_000_000
	if rem < 0 {
		days--
		rem += 86_400_000_000
	}
	seconds, us := rem/1_000_000, rem%1_000_000
	var parts []string
	if days != 0 {
		parts = append(parts, fmt.Sprintf("days=%d", days))
	}
	if seconds != 0 {
		parts = append(parts, fmt.Sprintf("seconds=%d", seconds))
	}
	if us != 0 {
		parts = append(parts, fmt.Sprintf("microseconds=%d", us))
	}
	if len(parts) == 0 {
		return "datetime.timedelta(0)"
	}
	return "datetime.timedelta(" + strings.Join(parts, ", ") + ")"
}

// Days in a Gregorian month
func daysInMonth(year, month int) int {
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// Byte at i or the NUL a C string would read past its end
func cAt(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

// Code point index of the date and time separator under the CPython disambiguation rules
func isoDatetimeSeparator(rs []rune) (int, bool) {
	n := len(rs)
	at := func(i int) rune {
		if i < n {
			return rs[i]
		}
		return 0
	}
	digit := func(r rune) bool { return r >= '0' && r <= '9' }
	if n == 7 {
		return 7, true
	}
	if at(4) == '-' {
		if at(5) == 'W' {
			if n < 8 {
				return 0, false
			}
			if n > 8 && rs[8] == '-' {
				if n == 9 {
					return 0, false
				}
				if n > 10 && digit(rs[10]) {
					return 8, true
				}
				return 10, true
			}
			return 8, true
		}
		return 10, true
	}
	if at(4) == 'W' {
		idx := 7
		for idx < n && digit(rs[idx]) {
			idx++
		}
		if idx < 9 {
			return idx, true
		}
		if idx%2 == 0 {
			return 7, true
		}
		return 8, true
	}
	return 8, true
}

// Reads n ASCII digits at p, false when any is missing
func isoDigits(s string, p, n int) (int, int, bool) {
	v := 0
	for i := 0; i < n; i++ {
		c := cAt(s, p+i)
		if !isASCIIDigit(c) {
			return 0, p, false
		}
		v = v*10 + int(c-'0')
	}
	return v, p + n, true
}

// Calendar date of a YYYY-MM-DD, YYYYMMDD, or ISO week string
func isoParseDate(d string) (int, int, int, bool) {
	year, p, ok := isoDigits(d, 0, 4)
	if !ok {
		return 0, 0, 0, false
	}
	sep := cAt(d, p) == '-'
	if sep {
		p++
	}
	if cAt(d, p) == 'W' {
		p++
		week, np, ok := isoDigits(d, p, 2)
		if !ok {
			return 0, 0, 0, false
		}
		p = np
		weekday := 1
		if p < len(d) {
			if sep {
				if cAt(d, p) != '-' {
					return 0, 0, 0, false
				}
				p++
			}
			if weekday, _, ok = isoDigits(d, p, 1); !ok {
				return 0, 0, 0, false
			}
		}
		return isoWeekToGregorian(year, week, weekday)
	}
	month, p, ok := isoDigits(d, p, 2)
	if !ok {
		return 0, 0, 0, false
	}
	if sep {
		if cAt(d, p) != '-' {
			return 0, 0, 0, false
		}
		p++
	}
	day, _, ok := isoDigits(d, p, 2)
	if !ok {
		return 0, 0, 0, false
	}
	return year, month, day, true
}

// Gregorian date of an ISO year, week, and weekday, false outside the calendar
func isoWeekToGregorian(year, week, weekday int) (int, int, int, bool) {
	if year < 1 || year > 9999 {
		return 0, 0, 0, false
	}
	if week < 1 || week > 52 {
		first := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Weekday()
		leap := year%4 == 0 && (year%100 != 0 || year%400 == 0)
		if week != 53 || !(first == time.Thursday || (first == time.Wednesday && leap)) {
			return 0, 0, 0, false
		}
	}
	if weekday < 1 || weekday > 7 {
		return 0, 0, 0, false
	}
	jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
	monday := jan4.AddDate(0, 0, -((int(jan4.Weekday()) + 6) % 7))
	d := monday.AddDate(0, 0, (week-1)*7+weekday-1)
	return d.Year(), int(d.Month()), d.Day(), true
}

// Clock fields of an HH MM SS ffffff extended or compact time string ending at end, status 1 when text trails
func isoClock(s string, end int) (h, m, sec, micro, status int) {
	vals := [3]int{}
	p := 0
	hasSep := true
	for i := 0; i < 3; i++ {
		v, np, ok := isoDigits(s, p, 2)
		if !ok {
			return 0, 0, 0, 0, -3
		}
		vals[i] = v
		p = np
		c := cAt(s, p)
		p++
		if i == 0 {
			hasSep = c == ':'
		}
		if c == '.' || c == ',' {
			if i < 2 {
				return 0, 0, 0, 0, -3
			}
			break
		}
		if p >= end {
			if c != 0 {
				return vals[0], vals[1], vals[2], 0, 1
			}
			return vals[0], vals[1], vals[2], 0, 0
		}
		if hasSep && c == ':' {
			continue
		}
		if !hasSep {
			p--
			continue
		}
		return 0, 0, 0, 0, -4
	}
	toParse := min(end-p, 6)
	if toParse <= 0 {
		return 0, 0, 0, 0, -3
	}
	frac, np, ok := isoDigits(s, p, toParse)
	if !ok {
		return 0, 0, 0, 0, -3
	}
	p = np
	for i := toParse; i < 6; i++ {
		frac *= 10
	}
	for isASCIIDigit(cAt(s, p)) {
		p++
	}
	if cAt(s, p) != 0 {
		return vals[0], vals[1], vals[2], frac, 1
	}
	return vals[0], vals[1], vals[2], frac, 0
}

// Time and optional offset after the separator, aware when an offset or Z is present
func isoParseTime(t string) (h, m, s, micro, offset, offsetMicro int, aware, ok bool) {
	tz := 0
	for tz < len(t) && t[tz] != 'Z' && t[tz] != '+' && t[tz] != '-' {
		tz++
	}
	h, m, s, micro, rv := isoClock(t, tz)
	if rv < 0 {
		return 0, 0, 0, 0, 0, 0, false, false
	}
	if tz == len(t) {
		return h, m, s, micro, 0, 0, false, rv == 0
	}
	if t[tz] == 'Z' {
		return h, m, s, micro, 0, 0, true, tz+1 == len(t)
	}
	sign := 1
	if t[tz] == '-' {
		sign = -1
	}
	rest := t[tz+1:]
	th, tm, ts, tmicro, trv := isoClock(rest, len(rest))
	if trv != 0 {
		return 0, 0, 0, 0, 0, 0, false, false
	}
	return h, m, s, micro, sign * (th*3600 + tm*60 + ts), sign * tmicro, true, true
}

// Python datetime isoformat of an instant in UTC with a zero offset suffix and microseconds only when nonzero
func ISOFormatUTC(t time.Time) string {
	t = t.UTC()
	if micro := t.Nanosecond() / 1000; micro != 0 {
		return t.Format("2006-01-02T15:04:05") + fmt.Sprintf(".%06d+00:00", micro)
	}
	return t.Format("2006-01-02T15:04:05") + "+00:00"
}

// Python datetime isoformat of an aware value in its own zone, microseconds and offset seconds only when nonzero
func ISOFormat(t time.Time) string {
	base := t.Format("2006-01-02T15:04:05")
	if micro := t.Nanosecond() / 1000; micro != 0 {
		base += fmt.Sprintf(".%06d", micro)
	}
	_, off := t.Zone()
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	base += fmt.Sprintf("%s%02d:%02d", sign, off/3600, off%3600/60)
	if off%60 != 0 {
		base += fmt.Sprintf(":%02d", off%60)
	}
	return base
}
