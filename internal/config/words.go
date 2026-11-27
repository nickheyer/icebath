package config

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/spf13/cast"
)

// What a secret shows once one is kept
const Hidden = "••••••••"

// Words for a value that holds nothing and whose setting says nothing of what that means
const (
	NotSet = "Not set"
	None   = "None"
)

// Value of the setting in words, as a row of the settings screen and config show say it: what is in effect rather than the value that
// stands for it, a number with what it counts, a length of time in the largest unit that divides it
func (s Setting) Say(value any) string {
	if s.Secret {
		if text, _ := value.(string); text != "" {
			return Hidden
		}
		return NotSet
	}
	if s.Zero != "" && s.IsZero(value) {
		return s.Zero
	}
	switch s.Kind() {
	case Toggle:
		if cast.ToBool(value) {
			return "On"
		}
		return "Off"
	case Number:
		if n, ok := numeric(value); ok {
			return s.count(n)
		}
	case Duration:
		if value == nil {
			return capital(s.Null, NotSet)
		}
		if length, ok := Length(value, s.unit()); ok {
			return Span(length)
		}
	case List:
		// A list of records is said as how many it holds
		if s.Entry != nil && len(s.Entry.Fields) > 0 {
			records, _ := value.([]any)
			if len(records) == 0 {
				return None
			}
			return counted(float64(len(records)), s.noun())
		}
		items := cast.ToStringSlice(value)
		if len(items) == 0 {
			return None
		}
		titles := make([]string, len(items))
		for i, item := range items {
			titles[i] = s.named(item)
		}
		return strings.Join(titles, ", ")
	case Table:
		entries, _ := value.(map[string]any)
		if len(entries) == 0 {
			return None
		}
		return counted(float64(len(entries)), s.noun())
	}
	text := strings.TrimSpace(cast.ToString(value))
	if titled := s.named(text); titled != "" {
		return titled
	}
	return NotSet
}

// Title of the choice with the value, the value itself when no choice has it or has a title
func (s Setting) named(value string) string {
	if s.Options != nil {
		for _, option := range s.Options() {
			if option.Value == value && option.Title != "" {
				return option.Title
			}
		}
	}
	return value
}

// What one entry of the table is called in the plural, entries for a table that does not say
func (s Setting) noun() string {
	if s.Entry == nil || s.Entry.Noun == "" {
		return "entries"
	}
	return plural(s.Entry.Noun)
}

// Whether the value is the one Zero speaks of: ZeroValue when the setting names one, else the zero value of its kind
func (s Setting) IsZero(value any) bool {
	if s.ZeroValue != nil {
		if want, ok := numeric(s.ZeroValue); ok {
			have, number := numeric(value)
			return number && have == want
		}
		return reflect.DeepEqual(value, s.ZeroValue)
	}
	if value == nil {
		return s.Kind() != Duration || s.Null == ""
	}
	if n, ok := numeric(value); ok {
		return n == 0
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v) == ""
	case bool:
		return !v
	}
	held := reflect.ValueOf(value)
	switch held.Kind() {
	case reflect.Slice, reflect.Map, reflect.Array:
		return held.Len() == 0
	}
	return false
}

// Value Zero speaks of, as the setting keeps it
func (s Setting) Zeroed() any {
	if s.ZeroValue != nil {
		return s.ZeroValue
	}
	switch s.Kind() {
	case Toggle:
		return false
	case Number:
		if _, fraction := s.Default.(float64); fraction {
			return 0.0
		}
		return 0
	case Duration:
		return 0
	case List:
		if _, records := s.Default.([]any); records {
			return []any{}
		}
		return []string{}
	case Table:
		return map[string]any{}
	}
	return ""
}

// Bounds of a number or a length of time in words, such as From 1 to 8 or At least 30 seconds, empty for none
func (s Setting) Bounds() string {
	var least, most string
	switch s.Kind() {
	case Number:
		if s.Least != nil {
			least = s.count(*s.Least)
		}
		if s.Most != nil {
			most = s.count(*s.Most)
		}
		// What is counted is said once, after the higher bound
		if least != "" && most != "" && s.Counts != "" && s.Counts != Bytes && s.Counts != Share && s.Counts != Plain {
			least = digits(*s.Least)
		}
	case Duration:
		if s.Min != nil {
			least = Span(*s.Min)
		}
		if s.Max != nil {
			most = Span(*s.Max)
		}
	}
	switch {
	case least != "" && most != "":
		return "From " + least + " to " + most
	case least != "":
		return "At least " + least
	case most != "":
		return "At most " + most
	}
	return ""
}

// Why a number is outside the bounds of the setting, nil when it is within them or is the value Zero speaks of
func (s Setting) within(number any) error {
	n, ok := numeric(number)
	if !ok || (s.Zero != "" && s.IsZero(number)) {
		return nil
	}
	if (s.Least != nil && n < *s.Least) || (s.Most != nil && n > *s.Most) {
		bounds := s.Bounds()
		return fmt.Errorf("%s must be %s.", s.Title, strings.ToLower(bounds[:1])+bounds[1:])
	}
	if n < 0 && !s.Negative() {
		return fmt.Errorf("%s can't be less than 0.", s.Title)
	}
	return nil
}

// Reports whether the setting takes a number or a length of time under 0. One does only where its setting says so: by a least bound, by a
// default under 0, or by a value under 0 that Zero speaks of
func (s Setting) Negative() bool {
	if s.Least != nil || s.Min != nil {
		return true
	}
	if n, ok := numeric(s.Default); ok && n < 0 {
		return true
	}
	n, ok := numeric(s.ZeroValue)
	return ok && n < 0
}

// Number with what the setting counts: a count of bytes in the largest unit that fits, a share as a percentage, a number that names a
// thing as it is written, any other with the word for what it counts, in the singular for exactly one
func (s Setting) count(n float64) string {
	switch s.Counts {
	case "":
		return digits(n)
	case Bytes:
		return Size(n)
	case Share:
		return digits(math.Round(n*1000)/10) + "%"
	case Plain:
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return counted(n, s.Counts)
}

// Number and the word for what it counts, the word in the singular for exactly one
func counted(n float64, what string) string { return strs.Counted(n, what) }

// Word for several of what the singular names
func plural(word string) string { return strs.Plural(word) }

// Number in digits grouped in thousands, with as many decimals as it has and no more than three
func digits(n float64) string { return strs.Number(n) }

// Count of bytes in the largest unit that fits, as every screen and command says a size
func Size(n float64) string { return strs.Size(n) }

// Length of time in words, in the largest unit that divides it, as every screen and command says one
func Span(length time.Duration) string { return strs.Span(length) }

// Text with a capital first letter, the fallback when it is empty
func capital(text, fallback string) string {
	if text == "" {
		return fallback
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// Line saying what a number counts, as the field a value is typed into and the example of a settings file say it, empty for a number
// that counts nothing
func Counting(counts string) string {
	switch counts {
	case "", Plain:
		return ""
	case Share:
		return "A number from 0 to 1: 0.5 is 50%."
	}
	return "A number of " + counts + "."
}

// Why text does not start the way the setting says it must, nil when it does, says nothing of it, or is empty
func (s Setting) started(text string) error {
	if len(s.Starts) == 0 || strings.TrimSpace(text) == "" {
		return nil
	}
	for _, start := range s.Starts {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), strings.ToLower(start)) {
			return nil
		}
	}
	return fmt.Errorf("%s must start with %s.", s.Title, strings.Join(s.Starts, " or "))
}
