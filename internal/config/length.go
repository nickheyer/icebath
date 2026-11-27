package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Least or most length of time a setting may be
func Bound(length time.Duration) *time.Duration {
	return &length
}

// Unit the setting keeps a length of time in
func (s Setting) unit() time.Duration {
	if s.Unit <= 0 {
		return time.Second
	}
	return s.Unit
}

// Name of the unit a length of time is kept in, as a value is shown with it
func (s Setting) UnitName() string {
	switch s.unit() {
	case time.Millisecond:
		return "ms"
	case time.Minute:
		return "min"
	case time.Hour:
		return "h"
	case 24 * time.Hour:
		return "days"
	}
	return "s"
}

// Default of the setting as it is kept: a length of time as the number of its unit it comes to, nothing for one whose unset value means
// something of its own
func (s Setting) DefaultValue() any {
	length, ok := s.Default.(time.Duration)
	switch {
	case !ok:
		return s.Default
	case s.Null != "":
		return nil
	}
	return s.kept(length)
}

// Number of a value decoded from YAML or JSON, false for anything else
func numeric(value any) (float64, bool) {
	switch v := value.(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float64:
		return v, !math.IsNaN(v)
	}
	return 0, false
}

// Length of time a value spells: a number counting the unit, or text of one or of a length such as 500ms, 30s or 2m; false for any other
// value. A number past the longest length there is comes to that length
func Length(value any, unit time.Duration) (time.Duration, bool) {
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		if length, err := time.ParseDuration(text); err == nil {
			return length, true
		}
		n, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return 0, false
		}
		value = n
	}
	n, ok := numeric(value)
	if !ok {
		return 0, false
	}
	if scaled := n * float64(unit); math.Abs(scaled) < math.MaxInt64 {
		return time.Duration(scaled), true
	}
	if n < 0 {
		return math.MinInt64, true
	}
	return math.MaxInt64, true
}

// Number of the setting's unit a length of time comes to, whole when it divides evenly
func (s Setting) kept(length time.Duration) any {
	if length%s.unit() == 0 {
		return int(length / s.unit())
	}
	return float64(length) / float64(s.unit())
}

// Length of time as the number the setting keeps with its unit
func (s Setting) said(length time.Duration) string {
	return WithUnit(s.kept(length), s.UnitName())
}

// Number of a length of time with the name of its unit, a unit of words in the singular for exactly one
func WithUnit(number any, unit string) string {
	if n, ok := numeric(number); ok && n == 1 && len(unit) > 3 && strings.HasSuffix(unit, "s") {
		unit = strings.TrimSuffix(unit, "s")
	}
	return fmt.Sprintf("%v %s", number, unit)
}

// Length of time a value written for the setting comes to, kept as a number of its unit, and empty text nothing for a setting whose unset
// value means something; refused when it spells none or is outside its bounds
func (s Setting) checkLength(value any) (any, error) {
	if text, ok := value.(string); ok && strings.TrimSpace(text) == "" && s.Null != "" {
		return nil, nil
	}
	length, ok := Length(value, s.unit())
	if !ok {
		return nil, fmt.Errorf("%s takes a length of time such as 30s or 5m", s.Title)
	}
	n, number := numeric(value)
	if text, ok := value.(string); ok {
		parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		n, number = parsed, err == nil
	}
	if number && math.Abs(n*float64(s.unit())) >= math.MaxInt64 {
		return nil, fmt.Errorf("%s is too long, the most it takes is %d days", s.Title, int(time.Duration(math.MaxInt64)/(24*time.Hour)))
	}
	if s.Min != nil && length < *s.Min {
		return nil, fmt.Errorf("%s must be at least %s", s.Title, s.said(*s.Min))
	}
	if s.Max != nil && length > *s.Max {
		return nil, fmt.Errorf("%s must be at most %s", s.Title, s.said(*s.Max))
	}
	return s.kept(length), nil
}

// Length of time a stored value or a variable gives the setting, as a number of its unit and brought within its bounds; false when it spells
// none, or is under the least of a setting whose default stands in for such a length
func (s Setting) readLength(value any) (any, bool) {
	length, ok := Length(value, s.unit())
	if !ok {
		return nil, false
	}
	if s.Min != nil && length < *s.Min {
		if s.DefaultBelow {
			return nil, false
		}
		length = *s.Min
	}
	if s.Max != nil {
		length = min(length, *s.Max)
	}
	return s.kept(length), true
}
