package types

import (
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type numberKind uint8

const (
	numberPosInt numberKind = iota + 1
	numberNegInt
	numberFloat
)

// jsonNumber is a JSON number classified the way serde_json stores it
type jsonNumber struct {
	kind numberKind
	u    uint64
	i    int64
	f    float64
}

// Classifies a Go value as a serde_json number, reporting false for non-numbers
func classifyNumber(v any) (jsonNumber, bool) {
	switch n := v.(type) {
	case int:
		return signedNumber(int64(n)), true
	case int8:
		return signedNumber(int64(n)), true
	case int16:
		return signedNumber(int64(n)), true
	case int32:
		return signedNumber(int64(n)), true
	case int64:
		return signedNumber(n), true
	case uint:
		return jsonNumber{kind: numberPosInt, u: uint64(n)}, true
	case uint8:
		return jsonNumber{kind: numberPosInt, u: uint64(n)}, true
	case uint16:
		return jsonNumber{kind: numberPosInt, u: uint64(n)}, true
	case uint32:
		return jsonNumber{kind: numberPosInt, u: uint64(n)}, true
	case uint64:
		return jsonNumber{kind: numberPosInt, u: n}, true
	case float32:
		return jsonNumber{kind: numberFloat, f: float64(n)}, true
	case float64:
		return jsonNumber{kind: numberFloat, f: n}, true
	case json.Number:
		return parseNumberText(string(n))
	}
	return jsonNumber{}, false
}

func signedNumber(i int64) jsonNumber {
	if i >= 0 {
		return jsonNumber{kind: numberPosInt, u: uint64(i)}
	}
	return jsonNumber{kind: numberNegInt, i: i}
}

// Parses number text into the serde_json number kind it would produce
func parseNumberText(s string) (jsonNumber, bool) {
	if s == "" {
		return jsonNumber{}, false
	}
	integer := !strings.ContainsAny(s, ".eE")
	if integer {
		if s[0] == '-' {
			if i, err := strconv.ParseInt(s, 10, 64); err == nil && i != 0 {
				return jsonNumber{kind: numberNegInt, i: i}, true
			}
		} else if u, err := strconv.ParseUint(s, 10, 64); err == nil {
			return jsonNumber{kind: numberPosInt, u: u}, true
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !isRangeError(err) {
		return jsonNumber{}, false
	}
	return jsonNumber{kind: numberFloat, f: f}, true
}

func isRangeError(err error) bool {
	ne, ok := err.(*strconv.NumError)
	return ok && ne.Err == strconv.ErrRange
}

func (n jsonNumber) asU64() (uint64, bool) {
	if n.kind == numberPosInt {
		return n.u, true
	}
	return 0, false
}

func (n jsonNumber) asI64() (int64, bool) {
	switch n.kind {
	case numberPosInt:
		if n.u <= math.MaxInt64 {
			return int64(n.u), true
		}
	case numberNegInt:
		return n.i, true
	}
	return 0, false
}

func (n jsonNumber) asF64() float64 {
	switch n.kind {
	case numberPosInt:
		return float64(n.u)
	case numberNegInt:
		return float64(n.i)
	}
	return n.f
}

func (n jsonNumber) equal(o jsonNumber) bool {
	if n.kind != o.kind {
		return false
	}
	switch n.kind {
	case numberPosInt:
		return n.u == o.u
	case numberNegInt:
		return n.i == o.i
	}
	return n.f == o.f
}

// Formats the number as serde_json Number Display writes it
func (n jsonNumber) String() string {
	switch n.kind {
	case numberPosInt:
		return strconv.FormatUint(n.u, 10)
	case numberNegInt:
		return strconv.FormatInt(n.i, 10)
	}
	return FormatF64(n.f)
}

// JSONUint64 reads a JSON number as serde_json as_u64 does
func JSONUint64(v any) (uint64, bool) {
	n, ok := classifyNumber(v)
	if !ok {
		return 0, false
	}
	return n.asU64()
}

// JSONInt64 reads a JSON number as serde_json as_i64 does
func JSONInt64(v any) (int64, bool) {
	n, ok := classifyNumber(v)
	if !ok {
		return 0, false
	}
	return n.asI64()
}

// JSONFloat64 reads any JSON number as serde_json as_f64 does
func JSONFloat64(v any) (float64, bool) {
	n, ok := classifyNumber(v)
	if !ok {
		return 0, false
	}
	return n.asF64(), true
}

// IsJSONInteger reports whether a JSON number is stored as an integer by serde_json
func IsJSONInteger(v any) bool {
	n, ok := classifyNumber(v)
	return ok && n.kind != numberFloat
}

// IsJSONNumber reports whether a value is any JSON number
func IsJSONNumber(v any) bool {
	_, ok := classifyNumber(v)
	return ok
}

// IsJSONFloat reports whether a JSON number is stored as a float by serde_json
func IsJSONFloat(v any) bool {
	n, ok := classifyNumber(v)
	return ok && n.kind == numberFloat
}

// JSONObject returns the value as a JSON object map
func JSONObject(v any) (map[string]any, bool) {
	m, ok := normalizeJSON(v).(map[string]any)
	return m, ok
}

// JSONArray returns the value as a JSON array
func JSONArray(v any) ([]any, bool) {
	a, ok := normalizeJSON(v).([]any)
	return a, ok
}

// JSONString returns the value as a JSON string
func JSONString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// JSONBool returns the value as a JSON boolean
func JSONBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// JSONGet looks up a key of a JSON object, matching serde_json Value get with a string index
func JSONGet(v any, key string) (any, bool) {
	m, ok := JSONObject(v)
	if !ok {
		return nil, false
	}
	value, ok := m[key]
	return value, ok
}

// JSONIndex looks up an element of a JSON array
func JSONIndex(v any, index int) (any, bool) {
	a, ok := JSONArray(v)
	if !ok || index < 0 || index >= len(a) {
		return nil, false
	}
	return a[index], true
}

// Converts foreign Go values inside a JSON tree into plain decoded JSON
func normalizeJSON(v any) any {
	switch v.(type) {
	case nil, bool, string, map[string]any, []any, json.Number,
		int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return v
	}
	out, err := ToJSONValue(v)
	if err != nil {
		return v
	}
	return out
}

// JSONEqual compares two JSON values with serde_json Value equality
func JSONEqual(a, b any) bool {
	a, b = normalizeJSON(a), normalizeJSON(b)
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !JSONEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !JSONEqual(xv, yv) {
				return false
			}
		}
		return true
	}
	xn, ok := classifyNumber(a)
	if !ok {
		return reflect.DeepEqual(a, b)
	}
	yn, ok := classifyNumber(b)
	return ok && xn.equal(yn)
}

// CloneJSON deep copies a JSON value so mutation of the copy never reaches the source
func CloneJSON(v any) any {
	switch x := normalizeJSON(v).(type) {
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = CloneJSON(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = CloneJSON(e)
		}
		return out
	default:
		return x
	}
}

// CloneJSONObject deep copies a JSON object map, keeping nil as nil
func CloneJSONObject(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, e := range m {
		out[k] = CloneJSON(e)
	}
	return out
}

// MergeJSON shallow merges two optional JSON values with the second winning, nil meaning absent
func MergeJSON(a, b any) any {
	ma, aObj := JSONObject(a)
	mb, bObj := JSONObject(b)
	if a != nil && b != nil && aObj && bObj {
		out := make(map[string]any, len(ma)+len(mb))
		for k, v := range ma {
			out[k] = v
		}
		for k, v := range mb {
			out[k] = v
		}
		return out
	}
	if b != nil {
		return b
	}
	return a
}

// IsJSONNull reports whether a value is JSON null
func IsJSONNull(v any) bool {
	return v == nil
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// FormatF64 writes a finite float the way serde_json does, NaN and infinities as null
func FormatF64(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	return formatShortestFloat(f, -5, 15, true, true)
}

// Formats shortest round-trip digits in fixed notation inside the exponent window else exponential
func formatShortestFloat(f float64, minFixed, maxFixed int, integralSuffix, plusExponent bool) string {
	var b strings.Builder
	if math.Signbit(f) {
		b.WriteByte('-')
		f = -f
	}
	if f == 0 {
		b.WriteString("0.0")
		if !integralSuffix {
			return strings.TrimSuffix(b.String(), ".0")
		}
		return b.String()
	}
	digits, exp := shortestDigits(f)
	if exp >= minFixed && exp <= maxFixed {
		switch {
		case exp >= len(digits)-1:
			b.WriteString(digits)
			b.WriteString(strings.Repeat("0", exp-(len(digits)-1)))
			if integralSuffix {
				b.WriteString(".0")
			}
		case exp >= 0:
			b.WriteString(digits[:exp+1])
			b.WriteByte('.')
			b.WriteString(digits[exp+1:])
		default:
			b.WriteString("0.")
			b.WriteString(strings.Repeat("0", -exp-1))
			b.WriteString(digits)
		}
		return b.String()
	}
	b.WriteByte(digits[0])
	if len(digits) > 1 {
		b.WriteByte('.')
		b.WriteString(digits[1:])
	}
	b.WriteByte('e')
	if exp < 0 {
		b.WriteByte('-')
		exp = -exp
	} else if plusExponent {
		b.WriteByte('+')
	}
	b.WriteString(strconv.Itoa(exp))
	return b.String()
}

// Returns the shortest round-trip decimal digits and the decimal exponent of the first digit
func shortestDigits(f float64) (string, int) {
	s := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expText, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.Replace(mant, ".", "", 1)
	return digits, exp
}

// RustF64Debug formats a float as Rust Debug output for f64
func RustF64Debug(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	abs := math.Abs(f)
	if abs != 0 && (abs < 1e-4 || abs >= 1e16) {
		return formatShortestFloat(f, 1, 0, false, false)
	}
	return formatShortestFloat(f, -400, 400, true, false)
}

// RustF64Display formats a float as Rust Display output for f64
func RustF64Display(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return formatShortestFloat(f, -400, 400, false, false)
}

// RustStrDebug quotes a string as Rust Debug output for str
func RustStrDebug(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case '\n':
			b.WriteString(`\n`)
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case 0:
			b.WriteString(`\0`)
		default:
			if rustNeedsUnicodeEscape(r) {
				b.WriteString(`\u{`)
				b.WriteString(strconv.FormatInt(int64(r), 16))
				b.WriteByte('}')
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Reports whether Rust escape_debug writes the rune as a unicode escape
func rustNeedsUnicodeEscape(r rune) bool {
	if r == ' ' {
		return false
	}
	if !unicode.IsPrint(r) {
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Other_Grapheme_Extend)
}
