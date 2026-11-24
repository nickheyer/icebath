package timeutil

import (
	"fmt"
	"strconv"
	"time"
)

// Calendar date with no time of day, what a bare YAML date such as 2001-12-14 loads to
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// Date in yyyy-mm-dd form
func (d Date) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.Year, int(d.Month), d.Day) }

// Date in yyyy-mm-dd form, so JSON and other text encoders write it as a plain date string
func (d Date) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Instant the date starts at in loc, UTC when loc is nil
func (d Date) Midnight(loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, loc)
}

// Instant time.Date builds from the fields, or a *RangeError naming the first field time.Date would have normalized; nil loc means UTC
func StrictDate(year int, month time.Month, day, hour, minute, second, nsec int, loc *time.Location) (time.Time, error) {
	if err := checkCalendar(year, int(month), day); err != nil {
		return time.Time{}, err
	}
	if err := checkClock(hour, minute, second); err != nil {
		return time.Time{}, err
	}
	if err := checkRange("nanosecond", nsec, 0, 999_999_999); err != nil {
		return time.Time{}, err
	}
	if loc == nil {
		loc = time.UTC
	}
	return time.Date(year, month, day, hour, minute, second, nsec, loc), nil
}

// Calendar or clock field outside its range
type RangeError struct {
	Field    string
	Value    int
	Min, Max int
}

// Field name, its allowed range and the value given
func (e *RangeError) Error() string {
	return e.Field + " must be in " + strconv.Itoa(e.Min) + ".." + strconv.Itoa(e.Max) + ", not " + strconv.Itoa(e.Value)
}

// Text that is not a date or time of the form a parser expects, Err holding the *RangeError when a field is out of range
type ParseError struct {
	Form string
	Text string
	Err  error
}

// Form expected, the text quoted and the out of range field when that is the reason
func (e *ParseError) Error() string {
	msg := "invalid " + e.Form + " " + strconv.Quote(e.Text)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Out of range field error the text failed on, nil for a syntax error
func (e *ParseError) Unwrap() error { return e.Err }

func checkRange(field string, v, lo, hi int) error {
	if v < lo || v > hi {
		return &RangeError{Field: field, Value: v, Min: lo, Max: hi}
	}
	return nil
}

// Month and day check of a proleptic Gregorian date, any year allowed
func checkCalendar(year, month, day int) error {
	if err := checkRange("month", month, 1, 12); err != nil {
		return err
	}
	return checkRange("day", day, 1, daysIn(year, time.Month(month)))
}

func checkClock(hour, minute, second int) error {
	if err := checkRange("hour", hour, 0, 23); err != nil {
		return err
	}
	if err := checkRange("minute", minute, 0, 59); err != nil {
		return err
	}
	return checkRange("second", second, 0, 59)
}

func checkOffset(hour, minute, second int) error {
	if err := checkRange("offset hour", hour, 0, 23); err != nil {
		return err
	}
	if err := checkRange("offset minute", minute, 0, 59); err != nil {
		return err
	}
	return checkRange("offset second", second, 0, 59)
}

// Days in a month of the proleptic Gregorian calendar
func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
