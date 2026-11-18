package pydatetime

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var (
	yaml11TimestampResolver = regexp.MustCompile(`^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$`)
	yaml11TimestampFields   = regexp.MustCompile(`^([0-9][0-9][0-9][0-9])-([0-9][0-9]?)-([0-9][0-9]?)(?:(?:[Tt]|[ \t]+)([0-9][0-9]?):([0-9][0-9]):([0-9][0-9])(?:\.([0-9]*))?(?:[ \t]*(Z|([-+])([0-9][0-9]?)(?::([0-9][0-9]))?))?)?$`)
)

// Python datetime.date value
type Date struct{ Year, Month, Day int }

// Python datetime.timezone holding a fixed offset and the name it was built with
type FixedOffset struct {
	Offset time.Duration
	Name   string
}

// Python datetime.datetime value, naive when Zone is nil
type DateTime struct {
	Year, Month, Day, Hour, Minute, Second, Microsecond int
	Zone                                                *FixedOffset
}

func (d Date) PyRepr() string {
	return fmt.Sprintf("datetime.date(%d, %d, %d)", d.Year, d.Month, d.Day)
}
func (d Date) PyStr() string           { return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day) }
func (Date) PyTypeName() string        { return "date" }
func (z FixedOffset) PyStr() string    { return z.Name }
func (FixedOffset) PyTypeName() string { return "timezone" }
func (DateTime) PyTypeName() string    { return "datetime" }

// Repr naming the offset timedelta and the zone name
func (z FixedOffset) PyRepr() string {
	return "datetime.timezone(" + TimedeltaRepr(z.Offset) + ", " + pyrepr.StrRepr(z.Name) + ")"
}

// Repr dropping a zero microsecond and then a zero second, with the tzinfo of an aware value
func (d DateTime) PyRepr() string {
	s := fmt.Sprintf("datetime.datetime(%d, %d, %d, %d, %d", d.Year, d.Month, d.Day, d.Hour, d.Minute)
	if d.Microsecond != 0 {
		s += fmt.Sprintf(", %d, %d", d.Second, d.Microsecond)
	} else if d.Second != 0 {
		s += fmt.Sprintf(", %d", d.Second)
	}
	if d.Zone != nil {
		s += ", tzinfo=" + d.Zone.PyRepr()
	}
	return s + ")"
}

// Isoformat with a space separator, microseconds when set and the UTC offset of an aware value
func (d DateTime) PyStr() string {
	s := fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", d.Year, d.Month, d.Day, d.Hour, d.Minute, d.Second)
	if d.Microsecond != 0 {
		s += fmt.Sprintf(".%06d", d.Microsecond)
	}
	if d.Zone != nil {
		s += utcOffsetText(d.Zone.Offset)
	}
	return s
}

// Sign, hours and minutes of an offset with seconds and microseconds when present
func utcOffsetText(off time.Duration) string {
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	micros := int64(off / time.Microsecond)
	h, m, sec, us := micros/3_600_000_000, micros/60_000_000%60, micros/1_000_000%60, micros%1_000_000
	s := fmt.Sprintf("%s%02d:%02d", sign, h, m)
	if sec != 0 || us != 0 {
		s += fmt.Sprintf(":%02d", sec)
	}
	if us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s
}

// Date or DateTime a YAML 1.1 safe loader builds from a timestamp scalar, ok false when text is no timestamp
func YAMLTimestamp(text string) (value any, ok bool, err error) {
	if !yaml11TimestampResolver.MatchString(text) {
		return nil, false, nil
	}
	m := yaml11TimestampFields.FindStringSubmatch(text)
	num := func(i int) int { n, _ := strconv.Atoi(m[i]); return n }
	year, month, day := num(1), num(2), num(3)
	if m[4] == "" {
		if err := checkDate(year, month, day); err != nil {
			return nil, true, err
		}
		return Date{Year: year, Month: month, Day: day}, true, nil
	}
	fraction := 0
	if f := m[7]; f != "" {
		digits := (f + "000000")[:6]
		fraction, _ = strconv.Atoi(digits)
		if len(f) > 6 && f[6] > '4' {
			fraction++
		}
		if fraction > 999999 {
			fraction = 0
		}
	}
	var zone *FixedOffset
	if m[9] != "" {
		minutes := num(10) * 60
		if m[11] != "" {
			minutes += num(11)
		}
		if m[9] == "-" {
			minutes = -minutes
		}
		off := time.Duration(minutes) * time.Minute
		if off <= -24*time.Hour || off >= 24*time.Hour {
			return nil, true, valueError("offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24), not %s.", TimedeltaRepr(off))
		}
		zone = &FixedOffset{Offset: off, Name: m[8]}
	} else if m[8] == "Z" {
		zone = &FixedOffset{Name: "Z"}
	}
	hour, minute, second := num(4), num(5), num(6)
	if err := checkDate(year, month, day); err != nil {
		return nil, true, err
	}
	if msg := TimeArgsError(hour, minute, second); msg != "" {
		return nil, true, valueError("%s", msg)
	}
	return DateTime{Year: year, Month: month, Day: day, Hour: hour, Minute: minute, Second: second, Microsecond: fraction, Zone: zone}, true, nil
}

// ValueError the Python date constructor raises for an out of range field
func checkDate(year, month, day int) error {
	if msg := DateArgsError(year, month, day); msg != "" {
		return valueError("%s", msg)
	}
	return nil
}

// Text of the ValueError the Python date constructor raises for an out of range field, empty for a valid date
func DateArgsError(year, month, day int) string {
	switch {
	case year < 1 || year > 9999:
		return fmt.Sprintf("year must be in 1..9999, not %d", year)
	case month < 1 || month > 12:
		return fmt.Sprintf("month must be in 1..12, not %d", month)
	case day < 1 || day > daysInMonth(year, month):
		return fmt.Sprintf("day %d must be in range 1..%d for month %d in year %d", day, daysInMonth(year, month), month, year)
	}
	return ""
}

// Text of the ValueError the Python time fields check raises for an out of range field, empty for a valid time
func TimeArgsError(hour, minute, second int) string {
	switch {
	case hour < 0 || hour > 23:
		return fmt.Sprintf("hour must be in 0..23, not %d", hour)
	case minute < 0 || minute > 59:
		return fmt.Sprintf("minute must be in 0..59, not %d", minute)
	case second < 0 || second > 59:
		return fmt.Sprintf("second must be in 0..59, not %d", second)
	}
	return ""
}
