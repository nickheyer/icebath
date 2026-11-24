package anyutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"
)

var (
	// Conversion failure for a value with no numeric form: a type that is not a number, text that does not parse, NaN asked for as an integer
	ErrNotNumber = errors.New("not a number")
	// Conversion failure for a number the target cannot hold: infinity or beyond the int64 range for an integer, beyond float64 for a float; the strconv sentinel
	ErrRange = strconv.ErrRange
)

const (
	minInt64Float = -9223372036854775808.0
	maxInt64Float = 9223372036854775808.0
)

// Integer a value holds when its type is a Go integer kind, an integral json.Number or a *big.Int within int64
func Int(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case int32:
		return int64(x), true
	case int16:
		return int64(x), true
	case int8:
		return int64(x), true
	case uint:
		if uint64(x) > math.MaxInt64 {
			return 0, false
		}
		return int64(x), true
	case uint64:
		if x > math.MaxInt64 {
			return 0, false
		}
		return int64(x), true
	case uint32:
		return int64(x), true
	case uint16:
		return int64(x), true
	case uint8:
		return int64(x), true
	case json.Number:
		n, err := strconv.ParseInt(string(x), 10, 64)
		return n, err == nil
	case *big.Int:
		if x == nil || !x.IsInt64() {
			return 0, false
		}
		return x.Int64(), true
	case nil, bool, string, float64, float32, []any, map[string]any, []byte:
		return 0, false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if u := rv.Uint(); u <= math.MaxInt64 {
			return int64(u), true
		}
	}
	return 0, false
}

// Number a value holds when its type is a Go integer or float kind, a json.Number or a *big.Int
func Float(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case int16:
		return float64(x), true
	case int8:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint64:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint8:
		return float64(x), true
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		return f, err == nil || errors.Is(err, strconv.ErrRange)
	case *big.Int:
		if x == nil {
			return 0, false
		}
		f, _ := new(big.Float).SetInt(x).Float64()
		return f, true
	case nil, bool, string, []any, map[string]any, []byte:
		return 0, false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	}
	return 0, false
}

// Integer of a number, floats cut toward zero, true and false as 1 and 0, or decimal text around which space is ignored; errors wrap ErrNotNumber (result 0) or ErrRange (result math.MaxInt64 or math.MinInt64 by the sign of the value, as strconv.ParseInt gives it)
func ToInt(v any) (int64, error) {
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		return parseInt(x)
	case []byte:
		return parseInt(string(x))
	case json.Number:
		return parseInt(string(x))
	case float64:
		return floatToInt(x)
	case float32:
		return floatToInt(float64(x))
	case uint:
		if uint64(x) > math.MaxInt64 {
			return math.MaxInt64, conversionError(v, "an integer", ErrRange)
		}
		return int64(x), nil
	case uint64:
		if x > math.MaxInt64 {
			return math.MaxInt64, conversionError(v, "an integer", ErrRange)
		}
		return int64(x), nil
	case *big.Int:
		if x == nil {
			return 0, conversionError(v, "an integer", ErrNotNumber)
		}
		if !x.IsInt64() {
			return bound(x.Sign() < 0), conversionError(v, "an integer", ErrRange)
		}
		return x.Int64(), nil
	}
	if n, ok := Int(v); ok {
		return n, nil
	}
	if v != nil {
		switch rv := reflect.ValueOf(v); rv.Kind() {
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			return math.MaxInt64, conversionError(v, "an integer", ErrRange)
		case reflect.Float32, reflect.Float64:
			return floatToInt(rv.Float())
		case reflect.String:
			return parseInt(rv.String())
		case reflect.Bool:
			if rv.Bool() {
				return 1, nil
			}
			return 0, nil
		}
	}
	return 0, conversionError(v, "an integer", ErrNotNumber)
}

// Float of a number, true and false as 1 and 0, or numeric text around which space is ignored; errors wrap ErrNotNumber (result 0) or ErrRange (result the infinity of the value's sign, as strconv.ParseFloat gives it)
func ToFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		return parseFloat(x)
	case []byte:
		return parseFloat(string(x))
	case json.Number:
		return parseFloat(string(x))
	case *big.Int:
		if x == nil {
			return 0, conversionError(v, "a number", ErrNotNumber)
		}
		f, _ := new(big.Float).SetInt(x).Float64()
		if math.IsInf(f, 0) {
			return f, conversionError(v, "a number", ErrRange)
		}
		return f, nil
	}
	if f, ok := Float(v); ok {
		return f, nil
	}
	if v != nil {
		switch rv := reflect.ValueOf(v); rv.Kind() {
		case reflect.String:
			return parseFloat(rv.String())
		case reflect.Bool:
			if rv.Bool() {
				return 1, nil
			}
			return 0, nil
		}
	}
	return 0, conversionError(v, "a number", ErrNotNumber)
}

// Integer spelled by text: a decimal fraction cut toward zero digit for digit, other float forms (exponent, hexadecimal, underscores) through float64
func parseInt(s string) (int64, error) {
	text := strings.TrimSpace(s)
	n, err := strconv.ParseInt(text, 10, 64)
	if err == nil {
		return n, nil
	}
	if errors.Is(err, strconv.ErrRange) {
		return n, conversionError(s, "an integer", ErrRange)
	}
	if whole, frac, ok := strings.Cut(text, "."); ok && strings.Trim(frac, "0123456789") == "" {
		switch whole {
		case "", "+", "-":
			if frac != "" {
				return 0, nil
			}
		default:
			n, err := strconv.ParseInt(whole, 10, 64)
			if err == nil {
				return n, nil
			}
			if errors.Is(err, strconv.ErrRange) {
				return n, conversionError(s, "an integer", ErrRange)
			}
		}
	}
	f, ferr := strconv.ParseFloat(text, 64)
	switch {
	case ferr == nil && math.IsNaN(f):
		return 0, conversionError(s, "an integer", ErrNotNumber)
	case ferr == nil && !math.IsInf(f, 0) && f >= minInt64Float && f < maxInt64Float:
		return int64(f), nil
	case ferr == nil || errors.Is(ferr, strconv.ErrRange):
		return bound(f < 0), conversionError(s, "an integer", ErrRange)
	}
	return 0, conversionError(s, "an integer", ErrNotNumber)
}

// Number spelled by text, including inf and nan spellings and hexadecimal floats
func parseFloat(s string) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err == nil {
		return f, nil
	}
	if errors.Is(err, strconv.ErrRange) {
		return f, conversionError(s, "a number", ErrRange)
	}
	return 0, conversionError(s, "a number", ErrNotNumber)
}

// Integer part of a float, NaN and values beyond int64 refused
func floatToInt(f float64) (int64, error) {
	switch {
	case math.IsNaN(f):
		return 0, conversionError(f, "an integer", ErrNotNumber)
	case math.IsInf(f, 0) || f < minInt64Float || f >= maxInt64Float:
		return bound(f < 0), conversionError(f, "an integer", ErrRange)
	}
	return int64(f), nil
}

// Largest int64, or the smallest for a negative value: the result of a conversion beyond the range
func bound(negative bool) int64 {
	if negative {
		return math.MinInt64
	}
	return math.MaxInt64
}

// Error naming the value and the target it could not become, wrapping the reason
func conversionError(v any, target string, reason error) error {
	return fmt.Errorf("cannot convert %s to %s: %w", describe(v), target, reason)
}

// Short description of a value for an error message: text quoted, numbers as written, containers by kind
func describe(v any) string {
	switch x := v.(type) {
	case string:
		return strconv.Quote(x)
	case []byte:
		return strconv.Quote(string(x))
	case json.Number:
		return string(x)
	}
	if _, ok := Float(v); ok {
		return String(v)
	}
	return Kind(v)
}
