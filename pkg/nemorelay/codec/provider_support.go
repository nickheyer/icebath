package codec

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

// Number kinds of a decoded JSON value as the Rust JSON model distinguishes them
const (
	serdeNumNone = iota
	serdeNumPosInt
	serdeNumNegInt
	serdeNumFloat
)

// Decoded JSON number split into the Rust model kinds
type serdeNumber struct {
	kind int
	u    uint64
	i    int64
	f    float64
}

// Classifies a JSON value as a positive integer, negative integer or float
func serdeNumberOf(v any) serdeNumber {
	if u, ok := types.JSONUint64(v); ok {
		return serdeNumber{kind: serdeNumPosInt, u: u, f: float64(u)}
	}
	if i, ok := types.JSONInt64(v); ok {
		return serdeNumber{kind: serdeNumNegInt, i: i, f: float64(i)}
	}
	if f, ok := types.JSONFloat64(v); ok {
		return serdeNumber{kind: serdeNumFloat, f: f}
	}
	return serdeNumber{}
}

// Formats a finite float exactly as the Rust JSON serializer prints it
func serdeFloatText(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "inf"
	}
	if math.IsInf(f, -1) {
		return "-inf"
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	neg := strings.HasPrefix(s, "-")
	mantissa, expText, _ := strings.Cut(strings.TrimPrefix(s, "-"), "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.TrimRight(strings.Replace(mantissa, ".", "", 1), "0")
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	length := len(digits)
	if exp >= -5 && exp <= 15 {
		switch {
		case length-1 <= exp:
			b.WriteString(digits)
			b.WriteString(strings.Repeat("0", exp+1-length))
			b.WriteString(".0")
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
	b.WriteString(digits[:1])
	if length > 1 {
		b.WriteByte('.')
		b.WriteString(digits[1:])
	}
	b.WriteByte('e')
	if exp >= 0 {
		b.WriteByte('+')
	}
	b.WriteString(strconv.Itoa(exp))
	return b.String()
}

// Quotes a string the way the Rust Debug formatter prints a str
func rustStrDebug(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		switch r {
		case 0:
			b.WriteString(`\0`)
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
		default:
			if unicode.In(r, unicode.Mn, unicode.Me, unicode.Other_Grapheme_Extend) || !unicode.IsPrint(r) {
				fmt.Fprintf(&b, `\u{%x}`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Returns the keys of a JSON object in sorted order
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Describes an unexpected JSON value the way Rust JSON deserialization errors do
func serdeUnexpected(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return fmt.Sprintf("boolean `%t`", t)
	case string:
		return "string " + rustStrDebug(t)
	case []any:
		return "sequence"
	case map[string]any:
		return "map"
	}
	n := serdeNumberOf(v)
	switch n.kind {
	case serdeNumPosInt:
		return fmt.Sprintf("integer `%d`", n.u)
	case serdeNumNegInt:
		return fmt.Sprintf("integer `%d`", n.i)
	case serdeNumFloat:
		return fmt.Sprintf("floating point `%s`", serdeFloatText(n.f))
	}
	return "map"
}

// Builds the invalid type error of Rust JSON deserialization
func serdeInvalidType(v any, expected string) error {
	return fmt.Errorf("invalid type: %s, expected %s", serdeUnexpected(v), expected)
}

// Decodes a JSON string with Rust deserialization errors
func serdeString(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", serdeInvalidType(v, "a string")
}

// Decodes a JSON boolean with Rust deserialization errors
func serdeBool(v any) (bool, error) {
	if b, ok := v.(bool); ok {
		return b, nil
	}
	return false, serdeInvalidType(v, "a boolean")
}

// Decodes an unsigned 64-bit integer with Rust deserialization errors
func serdeU64(v any) (uint64, error) {
	n := serdeNumberOf(v)
	switch n.kind {
	case serdeNumPosInt:
		return n.u, nil
	case serdeNumNegInt:
		return 0, fmt.Errorf("invalid value: integer `%d`, expected u64", n.i)
	}
	return 0, serdeInvalidType(v, "u64")
}

// Decodes a 64-bit float from any JSON number with Rust deserialization errors
func serdeF64(v any) (float64, error) {
	n := serdeNumberOf(v)
	if n.kind == serdeNumNone {
		return 0, serdeInvalidType(v, "f64")
	}
	return n.f, nil
}

// Decodes a JSON array element by element with Rust deserialization errors
func serdeSeq(v any, item func(any) error) error {
	items, ok := v.([]any)
	if !ok {
		return serdeInvalidType(v, "a sequence")
	}
	for _, value := range items {
		if err := item(value); err != nil {
			return err
		}
	}
	return nil
}

// Decodes a JSON array of strings with Rust deserialization errors
func serdeStrings(v any) ([]string, error) {
	out := []string{}
	err := serdeSeq(v, func(item any) error {
		s, err := serdeString(item)
		if err != nil {
			return err
		}
		out = append(out, s)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Decodes a JSON array keeping every element as a raw value
func serdeValues(v any) ([]any, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, serdeInvalidType(v, "a sequence")
	}
	return append(make([]any, 0, len(items)), items...), nil
}

// Decodes an optional string, null meaning none
func serdeOptString(v any) (*string, error) {
	if v == nil {
		return nil, nil
	}
	s, err := serdeString(v)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Decodes an optional unsigned integer, null meaning none
func serdeOptU64(v any) (*uint64, error) {
	if v == nil {
		return nil, nil
	}
	n, err := serdeU64(v)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// Decodes an optional float, null meaning none
func serdeOptF64(v any) (*float64, error) {
	if v == nil {
		return nil, nil
	}
	n, err := serdeF64(v)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// One named field of a struct decoded with Rust derive semantics
type serdeField struct {
	key       string
	optional  bool
	defaulted bool
	set       func(v any) error
}

// Decodes a JSON object or positional array into fields with Rust derive semantics, collecting unknown keys when flattening
func serdeStruct(v any, name string, fields []serdeField, flatten bool, extra map[string]any) error {
	switch t := v.(type) {
	case map[string]any:
		seen := make([]bool, len(fields))
		for _, key := range sortedKeys(t) {
			index := -1
			for i, field := range fields {
				if field.key == key {
					index = i
					break
				}
			}
			if index < 0 {
				if flatten && extra != nil {
					extra[key] = t[key]
				}
				continue
			}
			seen[index] = true
			value := t[key]
			if fields[index].optional && value == nil {
				continue
			}
			if err := fields[index].set(value); err != nil {
				return err
			}
		}
		for i, field := range fields {
			if !seen[i] && !field.optional {
				return fmt.Errorf("missing field `%s`", field.key)
			}
		}
		return nil
	case []any:
		if flatten {
			return serdeInvalidType(v, "struct "+name)
		}
		for i, field := range fields {
			if i >= len(t) {
				if field.defaulted {
					continue
				}
				return fmt.Errorf("invalid length %d, expected struct %s with %d elements", i, name, len(fields))
			}
			if field.optional && t[i] == nil {
				continue
			}
			if err := field.set(t[i]); err != nil {
				return err
			}
		}
		if len(t) > len(fields) {
			return fmt.Errorf("invalid length %d, expected fewer elements in array", len(t))
		}
		return nil
	}
	return serdeInvalidType(v, "struct "+name)
}

// Parses tool call arguments as JSON, keeping the raw text as a string when it is not JSON
func parseArguments(arguments string) any {
	if value, err := types.DecodeJSON([]byte(arguments)); err == nil {
		return value
	}
	return arguments
}

// Converts a float to a JSON number, mapping non-finite values to null
func jsonF64(v float64) any {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return v
}

// Converts an optional float to a present flag and JSON number
func optF64JSON(v *float64) (any, bool) {
	if v == nil {
		return nil, false
	}
	return jsonF64(*v), true
}

// Converts an optional unsigned integer to a present flag and JSON number
func optU64JSON(v *uint64) (any, bool) {
	if v == nil {
		return nil, false
	}
	return *v, true
}

// Converts an optional string to a present flag and JSON string
func optStringJSON(v *string) (any, bool) {
	if v == nil {
		return nil, false
	}
	return *v, true
}

// Converts an optional boolean to a present flag and JSON boolean
func optBoolJSON(v *bool) (any, bool) {
	if v == nil {
		return nil, false
	}
	return *v, true
}

// Converts an optional string list to a present flag and JSON array
func optStringsJSON(v []string) (any, bool) {
	if v == nil {
		return nil, false
	}
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out, true
}

// Removes keys dropped from extra and writes keys whose extra value changed
func patchExtraFields(obj, baseline, edited map[string]any) {
	for key := range baseline {
		if _, ok := edited[key]; !ok {
			delete(obj, key)
		}
	}
	for key, value := range edited {
		if before, ok := baseline[key]; !ok || !types.JSONEqual(before, value) {
			obj[key] = value
		}
	}
}

// Writes a key when the value is present and removes it otherwise
func setOrRemoveJSON(obj map[string]any, key string, value any, present bool) {
	if present {
		obj[key] = value
		return
	}
	delete(obj, key)
}

// Deep copies the entries of an object except the listed keys
func objectWithout(obj map[string]any, skip ...string) map[string]any {
	out := make(map[string]any, len(obj))
	for key, value := range obj {
		if !containsString(skip, key) {
			out[key] = cloneValue(value)
		}
	}
	return out
}

// Copies only the entries of an object whose keys are absent from a list
func objectWithoutList(obj map[string]any, skip []string) map[string]any {
	return objectWithout(obj, skip...)
}

// Reports whether every key of an object is one of the allowed keys
func onlyKeys(obj map[string]any, allowed ...string) bool {
	for key := range obj {
		if !containsString(allowed, key) {
			return false
		}
	}
	return true
}

// Reports whether a list holds a string
func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// Returns the string at a key of a JSON object value
func getString(v any, key string) (string, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return "", false
	}
	s, ok := obj[key].(string)
	return s, ok
}

// Returns the value at a key of a JSON object value
func getValue(v any, key string) (any, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	value, ok := obj[key]
	return value, ok
}

// Returns the non-negative integer at a key of a JSON object value
func getU64(v any, key string) (uint64, bool) {
	value, ok := getValue(v, key)
	if !ok {
		return 0, false
	}
	return types.JSONUint64(value)
}

// Returns a pointer to a copy of a string
func strPtr(s string) *string {
	return &s
}

// Returns a pointer to a copy of an unsigned integer
func u64Ptr(n uint64) *uint64 {
	return &n
}

// Returns a pointer to a copy of a boolean
func boolPtr(b bool) *bool {
	return &b
}

// Reports whether two optional strings are equal
func eqStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Reports whether two optional unsigned integers are equal
func eqU64Ptr(a, b *uint64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Reports whether two optional signed integers are equal
func eqI64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Reports whether two optional floats are equal with float comparison
func eqF64Ptr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Reports whether two optional booleans are equal
func eqBoolPtr(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Reports whether two optional JSON values with present flags are equal
func eqOptJSON(a any, aPresent bool, b any, bPresent bool) bool {
	aSome := aPresent || a != nil
	bSome := bPresent || b != nil
	if aSome != bSome {
		return false
	}
	return !aSome || types.JSONEqual(a, b)
}

// Reports whether two optional string lists are equal
func eqStrings(a, b []string) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Saturating addition of unsigned integers
func saturatingAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

// Deep copies a decoded JSON value
func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for key, value := range t {
			out[key] = cloneValue(value)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, value := range t {
			out[i] = cloneValue(value)
		}
		return out
	}
	return v
}

// Deep copies an object map, returning an empty map for nil
func cloneObject(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+2)
	for key, value := range m {
		out[key] = cloneValue(value)
	}
	return out
}

// Deep copies a list of JSON values, keeping nil as nil
func cloneSlice(s []any) []any {
	if s == nil {
		return nil
	}
	out := make([]any, len(s))
	for i, value := range s {
		out[i] = cloneValue(value)
	}
	return out
}

// Reports whether two lists hold equal items, treating nil and empty alike
func eqSlice[T any](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !types.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Reports whether two optional lists are equal, nil meaning none
func eqOptSlice[T any](a, b []T) bool {
	return (a == nil) == (b == nil) && eqSlice(a, b)
}

// Reports whether two optional lists of JSON values are equal, nil meaning none
func eqOptJSONSlice(a, b []any) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	for i := range a {
		if !types.JSONEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Reports whether two optional generation parameter sets are equal
func eqParams(a, b *types.GenerationParams) bool {
	if a == nil || b == nil {
		return a == b
	}
	return eqF64Ptr(a.Temperature, b.Temperature) && eqU64Ptr(a.MaxTokens, b.MaxTokens) &&
		eqF64Ptr(a.TopP, b.TopP) && eqStrings(a.Stop, b.Stop)
}

// Returns the temperature of optional parameters
func paramTemperature(p *types.GenerationParams) *float64 {
	if p == nil {
		return nil
	}
	return p.Temperature
}

// Returns the top_p of optional parameters
func paramTopP(p *types.GenerationParams) *float64 {
	if p == nil {
		return nil
	}
	return p.TopP
}

// Returns the token limit of optional parameters
func paramMaxTokens(p *types.GenerationParams) *uint64 {
	if p == nil {
		return nil
	}
	return p.MaxTokens
}

// Returns the stop sequences of optional parameters
func paramStop(p *types.GenerationParams) []string {
	if p == nil {
		return nil
	}
	return p.Stop
}

// Writes temperature and top_p when they changed between two parameter sets
func patchParamFloats(obj map[string]any, edited, before *types.GenerationParams) {
	for _, field := range []struct {
		key        string
		value, old *float64
	}{
		{"temperature", paramTemperature(edited), paramTemperature(before)},
		{"top_p", paramTopP(edited), paramTopP(before)},
	} {
		if !eqF64Ptr(field.value, field.old) {
			value, present := optF64JSON(field.value)
			setOrRemoveJSON(obj, field.key, value, present)
		}
	}
}
