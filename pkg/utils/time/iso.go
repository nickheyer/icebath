package timeutil

import (
	"strings"
	"time"
)

// Layout of RFC 3339 text with exactly six fractional digits and Z for a zero offset, fixed width so stamps of one offset sort as text in time order
const RFC3339Micro = "2006-01-02T15:04:05.000000Z07:00"

const isoForm = "ISO 8601 time"

// Instant of ISO 8601 text, a calendar, week or ordinal date alone or with a time of day, read as wall time in loc when the text carries no UTC offset and in UTC when loc is nil too; 24:00 is the next midnight and a leap second :60 the first instant of the next minute
func ParseISO(s string, loc *time.Location) (time.Time, error) {
	t, _, err := ParseISOZone(s, loc)
	return t, err
}

// Instant of ISO 8601 text as ParseISO reads it, also reporting whether the text carried Z or a UTC offset
func ParseISOZone(s string, loc *time.Location) (time.Time, bool, error) {
	p := isoParser{s: strings.TrimSpace(s)}
	f, ok := p.parse()
	if !ok {
		return time.Time{}, false, &ParseError{Form: isoForm, Text: s}
	}
	if err := f.check(); err != nil {
		return time.Time{}, false, &ParseError{Form: isoForm, Text: s, Err: err}
	}
	zone := loc
	switch {
	case f.zoned && f.offset == 0:
		zone = time.UTC
	case f.zoned:
		zone = time.FixedZone("", f.offset)
	case zone == nil:
		zone = time.UTC
	}
	if f.hour == 24 {
		return time.Date(f.year, time.Month(f.month), f.day+1, 0, 0, 0, 0, zone), f.zoned, nil
	}
	return time.Date(f.year, time.Month(f.month), f.day, f.hour, f.minute, f.second, f.nsec, zone), f.zoned, nil
}

// Fields of parsed ISO text before range checks
type isoFields struct {
	year, month, day           int
	week, weekday, yearDay     int
	hour, minute, second, nsec int
	offHour, offMinute, offSec int
	offset                     int
	zoned, weekDate, ordinal   bool
}

// Cursor over ISO text, every read failing softly at the end of the text
type isoParser struct {
	s string
	i int
}

func (p *isoParser) peek() byte {
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

func (p *isoParser) isDigit() bool { c := p.peek(); return '0' <= c && c <= '9' }

// Value of exactly n ASCII digits at the cursor, false when fewer are there
func (p *isoParser) digits(n int) (int, bool) {
	if p.i+n > len(p.s) {
		return 0, false
	}
	v := 0
	for j := p.i; j < p.i+n; j++ {
		c := p.s[j]
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int(c-'0')
	}
	p.i += n
	return v, true
}

// Steps over c when it is next, reporting whether it was
func (p *isoParser) skip(c byte) bool {
	if p.peek() == c {
		p.i++
		return true
	}
	return false
}

func (p *isoParser) parse() (isoFields, bool) {
	var f isoFields
	if !p.date(&f) {
		return f, false
	}
	if p.i == len(p.s) {
		return f, true
	}
	switch p.peek() {
	case 'T', 't', ' ':
		p.i++
	default:
		return f, false
	}
	if !p.clock(&f) {
		return f, false
	}
	for p.peek() == ' ' || p.peek() == '\t' {
		p.i++
	}
	if p.i == len(p.s) {
		return f, true
	}
	if !p.zone(&f) {
		return f, false
	}
	return f, p.i == len(p.s)
}

// Number of ASCII digits in the run at the cursor
func (p *isoParser) digitRun() int {
	n := 0
	for p.i+n < len(p.s) && '0' <= p.s[p.i+n] && p.s[p.i+n] <= '9' {
		n++
	}
	return n
}

// Calendar date, ISO week date or ordinal date, each in extended or basic form
func (p *isoParser) date(f *isoFields) bool {
	var ok bool
	if f.year, ok = p.digits(4); !ok {
		return false
	}
	extended := p.skip('-')
	if p.skip('W') {
		f.weekDate, f.weekday = true, 1
		if f.week, ok = p.digits(2); !ok {
			return false
		}
		if extended {
			if !p.skip('-') {
				return true
			}
		} else if !p.isDigit() {
			return true
		}
		f.weekday, ok = p.digits(1)
		return ok
	}
	if p.digitRun() == 3 {
		f.ordinal = true
		f.yearDay, ok = p.digits(3)
		return ok
	}
	if f.month, ok = p.digits(2); !ok {
		return false
	}
	if extended && !p.skip('-') {
		return false
	}
	f.day, ok = p.digits(2)
	return ok
}

// Hours with optional minutes and seconds, colon separated or run together, a decimal fraction allowed on the last of them
func (p *isoParser) clock(f *isoFields) bool {
	var ok bool
	if f.hour, ok = p.digits(2); !ok {
		return false
	}
	extended := p.skip(':')
	if !extended && !p.isDigit() {
		return p.fraction(f, 3600)
	}
	if f.minute, ok = p.digits(2); !ok {
		return false
	}
	if extended {
		if !p.skip(':') {
			return p.fraction(f, 60)
		}
	} else if !p.isDigit() {
		return p.fraction(f, 60)
	}
	if f.second, ok = p.digits(2); !ok {
		return false
	}
	return p.fraction(f, 1)
}

// Optional . or , fraction of the last clock field, a unit seconds long, spread over the smaller fields and cut to the nanosecond; false for a separator with no digit after it
func (p *isoParser) fraction(f *isoFields, unit int64) bool {
	if c := p.peek(); c != '.' && c != ',' {
		return true
	}
	p.i++
	if !p.isDigit() {
		return false
	}
	num, den := int64(0), int64(1)
	for ; p.isDigit(); p.i++ {
		if den < 10_000_000_000_000 {
			num, den = num*10+int64(p.s[p.i]-'0'), den*10
		}
	}
	ns := num * unit
	if den <= 1_000_000_000 {
		ns *= 1_000_000_000 / den
	} else {
		ns /= den / 1_000_000_000
	}
	f.minute += int(ns / 60_000_000_000)
	f.second += int(ns / 1_000_000_000 % 60)
	f.nsec = int(ns % 1_000_000_000)
	return true
}

// Z or a signed UTC offset of hours with optional minutes and seconds, colon separated or run together
func (p *isoParser) zone(f *isoFields) bool {
	f.zoned = true
	switch p.peek() {
	case 'Z', 'z':
		p.i++
		return true
	case '+', '-':
	default:
		return false
	}
	sign := 1
	if p.s[p.i] == '-' {
		sign = -1
	}
	p.i++
	var ok bool
	if f.offHour, ok = p.digits(2); !ok {
		return false
	}
	extended := p.skip(':')
	if extended || p.isDigit() {
		if f.offMinute, ok = p.digits(2); !ok {
			return false
		}
		if (extended && p.skip(':')) || (!extended && p.isDigit()) {
			if f.offSec, ok = p.digits(2); !ok {
				return false
			}
		}
	}
	f.offset = sign * (f.offHour*3600 + f.offMinute*60 + f.offSec)
	return true
}

// Range error of the first field outside the calendar or the clock, resolving a week or ordinal date to its calendar day; second 60 passes as a leap second
func (f *isoFields) check() error {
	switch {
	case f.weekDate:
		if err := checkRange("week", f.week, 1, isoWeeksIn(f.year)); err != nil {
			return err
		}
		if err := checkRange("weekday", f.weekday, 1, 7); err != nil {
			return err
		}
		jan4 := time.Date(f.year, time.January, 4, 0, 0, 0, 0, time.UTC)
		day := jan4.AddDate(0, 0, (f.week-1)*7+f.weekday-1-(int(jan4.Weekday())+6)%7)
		f.year, f.month, f.day = day.Year(), int(day.Month()), day.Day()
	case f.ordinal:
		if err := checkRange("day of year", f.yearDay, 1, time.Date(f.year, time.December, 31, 0, 0, 0, 0, time.UTC).YearDay()); err != nil {
			return err
		}
		day := time.Date(f.year, time.January, f.yearDay, 0, 0, 0, 0, time.UTC)
		f.month, f.day = int(day.Month()), day.Day()
	}
	if err := checkCalendar(f.year, f.month, f.day); err != nil {
		return err
	}
	if f.hour != 24 || f.minute != 0 || f.second != 0 || f.nsec != 0 {
		if err := checkRange("hour", f.hour, 0, 23); err != nil {
			return err
		}
		if err := checkRange("minute", f.minute, 0, 59); err != nil {
			return err
		}
		if err := checkRange("second", f.second, 0, 60); err != nil {
			return err
		}
	}
	return checkOffset(f.offHour, f.offMinute, f.offSec)
}

// Number of ISO weeks in a year, 53 when it starts on a Thursday or is a leap year starting on a Wednesday
func isoWeeksIn(year int) int {
	switch time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC).Weekday() {
	case time.Thursday:
		return 53
	case time.Wednesday:
		if daysIn(year, time.February) == 29 {
			return 53
		}
	}
	return 52
}
