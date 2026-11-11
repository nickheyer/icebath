package pydantic

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// How closely an input matched a validator, ordered lax then strict then exact as pydantic-core ranks union members
type exactness int

const (
	lax exactness = iota
	strict
	exact
)

// Mutable state one validation threads through its validators, the exactness and fields_set_count of pydantic-core
type state struct {
	exactness exactness
	fieldsSet int
}

func (s *state) floor(e exactness) {
	if e < s.exactness {
		s.exactness = e
	}
}

// One validator of the schema a field annotation compiles to
type Type interface {
	validate(st *state, v any) (any, []ErrorDetails)
	// Name pydantic-core gives the validator, the location tag of a union member
	Label() string
}

// Dict view of a mapping input, maps with string keys entering in sorted key order
func dictOf(v any) (*pyjson.Object, bool) {
	if o, ok := pyjson.AsObject(v); ok {
		return o, true
	}
	if v == nil {
		return nil, false
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Map || rv.Type().Key().Kind() != reflect.String {
		return nil, false
	}
	keys := make([]string, 0, rv.Len())
	for _, k := range rv.MapKeys() {
		keys = append(keys, k.String())
	}
	sort.Strings(keys)
	o := pyjson.NewObject()
	for _, k := range keys {
		o.Set(k, rv.MapIndex(reflect.ValueOf(k).Convert(rv.Type().Key())).Interface())
	}
	return o, true
}

// Items of a list input, a tuple counting as a lax match
func listOf(v any) ([]any, exactness, bool) {
	switch x := v.(type) {
	case []any:
		return x, exact, true
	case pyrepr.Tuple:
		return []any(x), lax, true
	case []byte, string, nil:
		return nil, lax, false
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, lax, false
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, exact, true
}

// Float value of a Go float or a json.Number written as a float
func floatOf(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case json.Number:
		if !strings.ContainsAny(string(x), ".eE") {
			return 0, false
		}
		f, err := strconv.ParseFloat(string(x), 64)
		return f, err == nil || isRange(err)
	}
	return 0, false
}

func isRange(err error) bool {
	ne, ok := err.(*strconv.NumError)
	return ok && ne.Err == strconv.ErrRange
}

// An int64 when the value fits, the big integer otherwise
func intValue(n *big.Int) any {
	if n.IsInt64() {
		return n.Int64()
	}
	return new(big.Int).Set(n)
}

type strType struct {
	min, max    int
	pattern     *regexp.Regexp
	patternText string
}

// The str annotation
var Str Type = &strType{min: -1, max: -1}

// A constraint of Field or Annotated metadata on a str or list annotation
type Constraint func(*strType)

// Field(min_length=n)
func MinLen(n int) Constraint { return func(t *strType) { t.min = n } }

// Field(max_length=n)
func MaxLen(n int) Constraint { return func(t *strType) { t.max = n } }

// Field(pattern=expr)
func Pattern(expr string) Constraint {
	return func(t *strType) { t.pattern, t.patternText = regexp.MustCompile(expr), expr }
}

// A str annotation carrying length or pattern constraints
func ConStr(cs ...Constraint) Type {
	t := &strType{min: -1, max: -1}
	for _, c := range cs {
		c(t)
	}
	return t
}

func (t *strType) Label() string {
	if t.min >= 0 || t.max >= 0 || t.pattern != nil {
		return "constrained-str"
	}
	return "str"
}

func (t *strType) validate(st *state, v any) (any, []ErrorDetails) {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case []byte:
		if !utf8.Valid(x) {
			return nil, fail("string_unicode", "Input should be a valid string, unable to parse raw data as a unicode string", v)
		}
		st.floor(lax)
		s = string(x)
	default:
		return nil, fail("string_type", "Input should be a valid string", v)
	}
	n := pystr.Len(s)
	if t.min >= 0 && n < t.min {
		return nil, fail("string_too_short", fmt.Sprintf("String should have at least %d character%s", t.min, plural(t.min)), v)
	}
	if t.max >= 0 && n > t.max {
		return nil, fail("string_too_long", fmt.Sprintf("String should have at most %d character%s", t.max, plural(t.max)), v)
	}
	if t.pattern != nil && !t.pattern.MatchString(s) {
		return nil, fail("string_pattern_mismatch", fmt.Sprintf("String should match pattern '%s'", t.patternText), v)
	}
	return s, nil
}

type intType struct{}

// The int annotation
var Int Type = intType{}

func (intType) Label() string { return "int" }

func (intType) validate(st *state, v any) (any, []ErrorDetails) {
	switch x := v.(type) {
	case bool:
		st.floor(lax)
		if x {
			return int64(1), nil
		}
		return int64(0), nil
	case string:
		st.floor(lax)
		return strAsInt(x, v)
	case []byte:
		st.floor(lax)
		return strAsInt(string(x), v)
	}
	if n, ok := pyrepr.Int(v); ok {
		return intValue(n), nil
	}
	if f, ok := floatOf(v); ok {
		st.floor(lax)
		return floatAsInt(f, v)
	}
	return nil, fail("int_type", "Input should be a valid integer", v)
}

// Integer of a whole float, refusing infinities, fractions and values past the i64 range
func floatAsInt(f float64, input any) (any, []ErrorDetails) {
	switch {
	case math.IsInf(f, 0) || math.IsNaN(f):
		return nil, fail("finite_number", "Input should be a finite number", input)
	case math.Mod(f, 1) != 0:
		return nil, fail("int_from_float", "Input should be a valid integer, got a number with a fractional part", input)
	case float64(math.MinInt64) < f && f < float64(math.MaxInt64):
		return int64(f), nil
	}
	return nil, fail("int_parsing_size", "Unable to parse input string as an integer, exceeded maximum size", input)
}

var intBody = regexp.MustCompile(`^[+-]?[0-9]+(?:_[0-9]+)*$`)

// Integer text with an optional sign and underscores between digits
func parseIntBody(s string) (*big.Int, bool) {
	if !intBody.MatchString(s) {
		return nil, false
	}
	return new(big.Int).SetString(strings.TrimPrefix(strings.ReplaceAll(s, "_", ""), "+"), 10)
}

// Integer parsed from trimmed text, a whole decimal like 1.000 accepted
func strAsInt(text string, input any) (any, []ErrorDetails) {
	s := strings.TrimSpace(text)
	if len(s) > 4300 {
		return nil, fail("int_parsing_size", "Unable to parse input string as an integer, exceeded maximum size", input)
	}
	if n, ok := parseIntBody(s); ok {
		return intValue(n), nil
	}
	if i := strings.IndexByte(s, '.'); i >= 0 && i+1 < len(s) && strings.Trim(s[i+1:], "0") == "" {
		if n, ok := parseIntBody(s[:i]); ok {
			return intValue(n), nil
		}
	}
	return nil, fail("int_parsing", "Input should be a valid integer, unable to parse string as an integer", input)
}

type floatType struct{}

// The float annotation
var Float Type = floatType{}

func (floatType) Label() string { return "float" }

func (floatType) validate(st *state, v any) (any, []ErrorDetails) {
	switch x := v.(type) {
	case bool:
		st.floor(lax)
		if x {
			return 1.0, nil
		}
		return 0.0, nil
	case string:
		st.floor(lax)
		return strAsFloat(x, v)
	case []byte:
		st.floor(lax)
		return strAsFloat(string(x), v)
	}
	if f, ok := floatOf(v); ok {
		return f, nil
	}
	if n, ok := pyrepr.Int(v); ok {
		st.floor(strict)
		f, _ := new(big.Float).SetInt(n).Float64()
		return f, nil
	}
	return nil, fail("float_type", "Input should be a valid number", v)
}

var rustFloat = regexp.MustCompile(`^[+-]?(?:(?i:inf|infinity|nan)|(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?)$`)

// Float of text in the grammar Rust parses, overflow giving an infinity
func parseRustFloat(s string) (float64, bool) {
	if !rustFloat.MatchString(s) {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil || isRange(err)
}

// Float parsed from trimmed text, or from the untrimmed text with its digit underscores removed
func strAsFloat(text string, input any) (any, []ErrorDetails) {
	if f, ok := parseRustFloat(strings.TrimSpace(text)); ok {
		return f, nil
	}
	if !strings.HasPrefix(text, "_") && !strings.HasSuffix(text, "_") && !strings.Contains(text, "__") {
		if f, ok := parseRustFloat(strings.ReplaceAll(text, "_", "")); ok {
			return f, nil
		}
	}
	return nil, fail("float_parsing", "Input should be a valid number, unable to parse string as a number", input)
}

type boolType struct{}

// The bool annotation
var Bool Type = boolType{}

func (boolType) Label() string { return "bool" }

func (boolType) validate(st *state, v any) (any, []ErrorDetails) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		st.floor(lax)
		return strAsBool(x, v)
	case []byte:
		st.floor(lax)
		return strAsBool(string(x), v)
	}
	if n, ok := pyrepr.Int(v); ok {
		st.floor(lax)
		return intAsBool(n.Sign() == 0, n.Cmp(big.NewInt(1)) == 0, v)
	}
	if f, ok := floatOf(v); ok {
		st.floor(lax)
		whole, errs := floatAsInt(f, v)
		if errs != nil {
			return nil, fail("bool_type", "Input should be a valid boolean", v)
		}
		return intAsBool(whole.(int64) == 0, whole.(int64) == 1, v)
	}
	return nil, fail("bool_type", "Input should be a valid boolean", v)
}

func intAsBool(zero, one bool, input any) (any, []ErrorDetails) {
	switch {
	case zero:
		return false, nil
	case one:
		return true, nil
	}
	return nil, fail("bool_parsing", "Input should be a valid boolean, unable to interpret input", input)
}

// Bool of the words pydantic-core reads, matched without case and without trimming
func strAsBool(s string, input any) (any, []ErrorDetails) {
	for _, w := range []string{"0", "f", "n", "no", "off", "false"} {
		if strings.EqualFold(s, w) {
			return false, nil
		}
	}
	for _, w := range []string{"1", "t", "y", "on", "yes", "true"} {
		if strings.EqualFold(s, w) {
			return true, nil
		}
	}
	return nil, fail("bool_parsing", "Input should be a valid boolean, unable to interpret input", input)
}

type anyType struct{}

// The Any annotation, JsonValue of the contracts
var Any Type = anyType{}

func (anyType) Label() string                                  { return "any" }
func (anyType) validate(_ *state, v any) (any, []ErrorDetails) { return v, nil }

type noneType struct{}

// The None annotation
var NoneT Type = noneType{}

func (noneType) Label() string { return "none" }

func (noneType) validate(_ *state, v any) (any, []ErrorDetails) {
	if v == nil {
		return nil, nil
	}
	return nil, fail("none_required", "Input should be None", v)
}

type nullableType struct{ inner Type }

// An annotation joined with None, X | None
func Nullable(t Type) Type { return nullableType{t} }

func (t nullableType) Label() string { return "nullable[" + t.inner.Label() + "]" }

func (t nullableType) validate(st *state, v any) (any, []ErrorDetails) {
	if v == nil {
		return nil, nil
	}
	return t.inner.validate(st, v)
}

type listType struct {
	item     Type
	min, max int
}

// list[item] with optional MinLen and MaxLen constraints
func List(item Type, cs ...Constraint) Type {
	bounds := &strType{min: -1, max: -1}
	for _, c := range cs {
		c(bounds)
	}
	return listType{item, bounds.min, bounds.max}
}

func (t listType) Label() string { return "list[" + t.item.Label() + "]" }

func (t listType) validate(st *state, v any) (any, []ErrorDetails) {
	items, ex, ok := listOf(v)
	if !ok {
		return nil, fail("list_type", "Input should be a valid list", v)
	}
	st.floor(ex)
	if t.max >= 0 && len(items) > t.max {
		return nil, fail("too_long", fmt.Sprintf("List should have at most %d item%s after validation, not %d", t.max, plural(t.max), len(items)), v)
	}
	out := make([]any, 0, len(items))
	var errs []ErrorDetails
	for i, item := range items {
		val, e := t.item.validate(st, item)
		if e != nil {
			errs = append(errs, under(i, e)...)
			continue
		}
		out = append(out, val)
	}
	if errs != nil {
		return nil, errs
	}
	if t.min >= 0 && len(out) < t.min {
		return nil, fail("too_short", fmt.Sprintf("List should have at least %d item%s after validation, not %d", t.min, plural(t.min), len(out)), v)
	}
	return out, nil
}

type dictType struct{ value Type }

// dict[str, value]
func Dict(value Type) Type { return dictType{value} }

func (t dictType) Label() string { return "dict[str," + t.value.Label() + "]" }

func (t dictType) validate(st *state, v any) (any, []ErrorDetails) {
	d, ok := dictOf(v)
	if !ok {
		return nil, fail("dict_type", "Input should be a valid dictionary", v)
	}
	out := pyjson.NewObject()
	var errs []ErrorDetails
	for _, k := range d.Keys {
		val, e := t.value.validate(st, d.Vals[k])
		if e != nil {
			errs = append(errs, under(k, e)...)
			continue
		}
		out.Set(k, val)
	}
	if errs != nil {
		return nil, errs
	}
	return out, nil
}

type literalType struct {
	values []any
	reprs  []string
}

// Literal[values...] of str, bool, int or None values
func Literal(values ...any) Type {
	reprs := make([]string, len(values))
	for i, v := range values {
		values[i] = pyInt(v)
		reprs[i] = pyrepr.Repr(v)
	}
	return literalType{values, reprs}
}

// The int64 a validated int holds for a Go int, any other value unchanged
func pyInt(v any) any {
	if n, ok := v.(int); ok {
		return int64(n)
	}
	return v
}

func (t literalType) Label() string { return "literal[" + strings.Join(t.reprs, ",") + "]" }

func (t literalType) validate(_ *state, v any) (any, []ErrorDetails) {
	for _, want := range t.values {
		if literalMatches(want, v) {
			return want, nil
		}
	}
	return nil, fail("literal_error", "Input should be "+expectedText(t.reprs), v)
}

// Match of an input against one literal value under Python equality, a bool equal to the int it counts as
func literalMatches(want, v any) bool {
	switch w := want.(type) {
	case nil:
		return v == nil
	case string:
		s, ok := v.(string)
		return ok && s == w
	}
	wn, wok := numberOf(want)
	vn, vok := numberOf(v)
	return wok && vok && wn.Cmp(vn) == 0
}

// Exact numeric value of a bool, int or float input
func numberOf(v any) (*big.Float, bool) {
	if b, ok := v.(bool); ok {
		if b {
			return big.NewFloat(1), true
		}
		return big.NewFloat(0), true
	}
	if n, ok := pyrepr.Int(v); ok {
		return new(big.Float).SetInt(n), true
	}
	if f, ok := floatOf(v); ok && !math.IsNaN(f) {
		return big.NewFloat(f), true
	}
	return nil, false
}

// A WireEnum, a StrEnum whose members validate from their values
type EnumType struct {
	Name   string
	names  []string
	values []string
}

// StrEnum class from alternating member names and values in declaration order
func Enum(name string, pairs ...string) *EnumType {
	e := &EnumType{Name: name}
	for i := 0; i+1 < len(pairs); i += 2 {
		e.names = append(e.names, pairs[i])
		e.values = append(e.values, pairs[i+1])
	}
	return e
}

// Value of the member with the given name, panicking on a name the enum lacks
func (e *EnumType) Member(name string) string {
	for i, n := range e.names {
		if n == name {
			return e.values[i]
		}
	}
	panic("AttributeError: " + name)
}

func (e *EnumType) Label() string { return "str-enum[" + e.Name + "]" }

func (e *EnumType) validate(st *state, v any) (any, []ErrorDetails) {
	if s, ok := v.(string); ok {
		for _, val := range e.values {
			if s == val {
				st.floor(lax)
				return val, nil
			}
		}
	}
	reprs := make([]string, len(e.values))
	for i, val := range e.values {
		reprs[i] = pyrepr.Repr(val)
	}
	return nil, fail("enum", "Input should be "+expectedText(reprs), v)
}

type unionType struct{ choices []Type }

// A | B | ... validated in smart mode, the best exactness winning and every member error kept on failure
func Union(choices ...Type) Type { return unionType{choices} }

func (t unionType) Label() string {
	labels := make([]string, len(t.choices))
	for i, c := range t.choices {
		labels[i] = c.Label()
	}
	return "union[" + strings.Join(labels, ",") + "]"
}

func (t unionType) validate(st *state, v any) (any, []ErrorDetails) {
	oldExact, oldFields := st.exactness, st.fieldsSet
	var best any
	bestExact, bestFields, found := lax, -1, false
	var errs []ErrorDetails
	for _, c := range t.choices {
		st.exactness, st.fieldsSet = exact, -1
		out, e := c.validate(st, v)
		if e != nil {
			errs = append(errs, under(c.Label(), e)...)
			continue
		}
		if st.exactness == exact && st.fieldsSet < 0 {
			st.exactness, st.fieldsSet = oldExact, oldFields
			return out, nil
		}
		better := !found
		if found {
			if bestFields >= 0 && st.fieldsSet >= 0 && bestFields != st.fieldsSet {
				better = bestFields < st.fieldsSet
			} else {
				better = bestExact < st.exactness
			}
		}
		if better {
			best, bestExact, bestFields, found = out, st.exactness, st.fieldsSet, true
		}
	}
	st.exactness, st.fieldsSet = oldExact, oldFields
	if !found {
		return nil, errs
	}
	st.floor(bestExact)
	if bestFields >= 0 {
		st.fieldsSet = bestFields
	}
	return best, nil
}
