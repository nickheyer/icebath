package dashboard

import (
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// One pydantic validation error, the errors(include_url=False) entry
type ErrorDetails struct {
	Type  string
	Loc   []any
	Msg   string
	Input any
	Ctx   *pyjson.Object
}

// Ordered JSON object of name and value pairs
func jsonObj(pairs ...any) *pyjson.Object { return pyjson.ObjectOf(pairs...) }

// Error entries as the JSON list FastAPI returns, keys in pydantic order
func errorDetailsJSON(errs []ErrorDetails) []any {
	out := make([]any, 0, len(errs))
	for _, e := range errs {
		loc := make([]any, len(e.Loc))
		copy(loc, e.Loc)
		o := jsonObj("type", e.Type, "loc", loc, "msg", e.Msg, "input", jsonableEncoder(e.Input))
		if e.Ctx != nil {
			o.Set("ctx", jsonableEncoder(e.Ctx))
		}
		out = append(out, o)
	}
	return out
}

// Errors with a location prefix prepended to each loc
func prefixLoc(prefix []any, errs []ErrorDetails) []ErrorDetails {
	for i := range errs {
		errs[i].Loc = append(slices.Clone(prefix), errs[i].Loc...)
	}
	return errs
}

func valErr(typ, msg string, input any) []ErrorDetails {
	return []ErrorDetails{{Type: typ, Loc: []any{}, Msg: msg, Input: input}}
}

// Secret string a model holds, masked when encoded
type SecretStr struct{ Value string }

// Value of a validated JSON-able tree with secrets masked, uuids as text and maps ordered by key
func jsonableEncoder(v any) any {
	switch t := v.(type) {
	case SecretStr:
		if t.Value == "" {
			return ""
		}
		return "**********"
	case uuid.UUID:
		return t.String()
	case *pyjson.Object:
		if t == nil {
			return nil
		}
		o := pyjson.NewObject()
		for _, k := range t.Keys {
			o.Set(k, jsonableEncoder(t.Vals[k]))
		}
		return o
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		o := pyjson.NewObject()
		for _, k := range keys {
			o.Set(k, jsonableEncoder(t[k]))
		}
		return o
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = jsonableEncoder(x)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = x
		}
		return out
	}
	return v
}

// Pydantic type of a field or parameter, validating a Python value in lax mode
type FieldType interface {
	Validate(v any) (any, []ErrorDetails)
	isSequence() bool
}

type strType struct{}
type intType struct {
	strict bool
	ge, le *int64
}
type floatType struct{}
type boolType struct{ strict bool }
type anyType struct{}
type secretStrType struct{}
type optionalType struct{ inner FieldType }
type listType struct{ item FieldType }
type dictType struct{ value FieldType }
type literalType struct{ values []any }

var (
	Str        FieldType = strType{}
	Int        FieldType = intType{}
	StrictInt  FieldType = intType{strict: true}
	Float      FieldType = floatType{}
	Bool       FieldType = boolType{}
	StrictBool FieldType = boolType{strict: true}
	Any        FieldType = anyType{}
	Secret     FieldType = secretStrType{}
)

// Integer constrained to the inclusive bounds, a nil bound absent
func IntRange(ge, le *int64) FieldType { return intType{ge: ge, le: le} }

// Value of an int bound
func Bound(n int64) *int64 { return &n }

// Bound errors of a parsed integer, less_than_equal checked before greater_than_equal
func (t intType) checkBounds(n any, input any) []ErrorDetails {
	v, ok := n.(int64)
	if !ok {
		b := n.(*big.Int)
		if t.le != nil && b.Sign() > 0 {
			return t.leErr(input)
		}
		if t.ge != nil && b.Sign() < 0 {
			return t.geErr(input)
		}
		return nil
	}
	if t.le != nil && v > *t.le {
		return t.leErr(input)
	}
	if t.ge != nil && v < *t.ge {
		return t.geErr(input)
	}
	return nil
}

func (t intType) leErr(input any) []ErrorDetails {
	return []ErrorDetails{{Type: "less_than_equal", Loc: []any{}, Msg: "Input should be less than or equal to " + strconv.FormatInt(*t.le, 10),
		Input: input, Ctx: jsonObj("le", *t.le)}}
}

func (t intType) geErr(input any) []ErrorDetails {
	return []ErrorDetails{{Type: "greater_than_equal", Loc: []any{}, Msg: "Input should be greater than or equal to " + strconv.FormatInt(*t.ge, 10),
		Input: input, Ctx: jsonObj("ge", *t.ge)}}
}

// Optional of a type, None passing through
func Optional(t FieldType) FieldType { return optionalType{t} }

// List of an item type
func ListOf(t FieldType) FieldType { return listType{t} }

// String keyed dict of a value type
func DictOf(t FieldType) FieldType { return dictType{t} }

// Literal of the allowed values
func Literal(values ...any) FieldType { return literalType{values} }

func (strType) isSequence() bool       { return false }
func (intType) isSequence() bool       { return false }
func (floatType) isSequence() bool     { return false }
func (boolType) isSequence() bool      { return false }
func (anyType) isSequence() bool       { return false }
func (secretStrType) isSequence() bool { return false }
func (o optionalType) isSequence() bool {
	return o.inner.isSequence()
}
func (listType) isSequence() bool    { return true }
func (dictType) isSequence() bool    { return false }
func (literalType) isSequence() bool { return false }

func (strType) Validate(v any) (any, []ErrorDetails) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return nil, valErr("string_type", "Input should be a valid string", v)
}

func (secretStrType) Validate(v any) (any, []ErrorDetails) {
	if s, ok := v.(string); ok {
		return SecretStr{s}, nil
	}
	return nil, valErr("string_type", "Input should be a valid string", v)
}

func (anyType) Validate(v any) (any, []ErrorDetails) { return v, nil }

// Text with underscores removed when every underscore sits between two digits, false otherwise
func stripUnderscores(s string) (string, bool) {
	if !strings.Contains(s, "_") {
		return "", false
	}
	b := []byte(s)
	for i, c := range b {
		if c == '_' && (i == 0 || i == len(b)-1 || !isDigitByte(b[i-1]) || !isDigitByte(b[i+1])) {
			return "", false
		}
	}
	return strings.ReplaceAll(s, "_", ""), true
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

// Integer of a signed ASCII decimal literal, the Rust integer parse
func parseRustInt(s string) (any, bool) {
	body := strings.TrimPrefix(strings.TrimPrefix(s, "+"), "-")
	if body == "" || strings.HasPrefix(body, "+") || strings.HasPrefix(body, "-") {
		return nil, false
	}
	for i := 0; i < len(body); i++ {
		if !isDigitByte(body[i]) {
			return nil, false
		}
	}
	n, ok := new(big.Int).SetString(strings.TrimPrefix(s, "+"), 10)
	if !ok {
		return nil, false
	}
	if n.IsInt64() {
		return n.Int64(), true
	}
	return n, true
}

// Integer of a lax string, whitespace trimmed, trailing decimal zeros and digit underscores allowed
func strAsInt(raw string) (any, bool) {
	s := strings.TrimFunc(raw, unicode.IsSpace)
	if n, ok := parseRustInt(s); ok {
		return n, true
	}
	if i := strings.IndexByte(s, '.'); i >= 0 && strings.Trim(s[i+1:], "0") == "" {
		if n, ok := parseRustInt(s[:i]); ok {
			return n, true
		}
	}
	if stripped, ok := stripUnderscores(s); ok {
		return parseRustInt(stripped)
	}
	return nil, false
}

func (t intType) Validate(v any) (any, []ErrorDetails) {
	n, errs := t.parse(v)
	if len(errs) > 0 {
		return nil, errs
	}
	if e := t.checkBounds(n, v); len(e) > 0 {
		return nil, e
	}
	return n, nil
}

func (t intType) parse(v any) (any, []ErrorDetails) {
	switch x := v.(type) {
	case bool:
		if t.strict {
			break
		}
		if x {
			return int64(1), nil
		}
		return int64(0), nil
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	case *big.Int:
		return x, nil
	case float64:
		if t.strict {
			break
		}
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return nil, valErr("finite_number", "Input should be a finite number", v)
		}
		if x != math.Trunc(x) {
			return nil, valErr("int_from_float", "Input should be a valid integer, got a number with a fractional part", v)
		}
		if math.Abs(x) < 1<<63 {
			return int64(x), nil
		}
		n, _ := big.NewFloat(x).Int(nil)
		return n, nil
	case string:
		if t.strict {
			break
		}
		if n, ok := strAsInt(x); ok {
			return n, nil
		}
		return nil, valErr("int_parsing", "Input should be a valid integer, unable to parse string as an integer", v)
	}
	return nil, valErr("int_type", "Input should be a valid integer", v)
}

func (floatType) Validate(v any) (any, []ErrorDetails) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case bool:
		if x {
			return 1.0, nil
		}
		return 0.0, nil
	case int64:
		return float64(x), nil
	case int:
		return float64(x), nil
	case *big.Int:
		f, _ := new(big.Float).SetInt(x).Float64()
		return f, nil
	case string:
		s := strings.TrimFunc(x, unicode.IsSpace)
		if f, ok := parseRustFloat(s); ok {
			return f, nil
		}
		if stripped, ok := stripUnderscores(s); ok {
			if f, ok := parseRustFloat(stripped); ok {
				return f, nil
			}
		}
		return nil, valErr("float_parsing", "Input should be a valid number, unable to parse string as a number", v)
	}
	return nil, valErr("float_type", "Input should be a valid number", v)
}

// Float of the Rust f64 grammar, decimal or exponent forms and the inf, infinity and nan words
func parseRustFloat(s string) (float64, bool) {
	body := strings.TrimPrefix(strings.TrimPrefix(s, "+"), "-")
	switch strings.ToLower(body) {
	case "inf", "infinity":
		if strings.HasPrefix(s, "-") {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	case "nan":
		return math.NaN(), true
	}
	if body == "" {
		return 0, false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		if !(isDigitByte(c) || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-') {
			return 0, false
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		var ne *strconv.NumError
		if ok := asNumError(err, &ne); ok && ne.Err == strconv.ErrRange {
			return f, true
		}
		return 0, false
	}
	return f, true
}

func asNumError(err error, out **strconv.NumError) bool {
	ne, ok := err.(*strconv.NumError)
	*out = ne
	return ok
}

func (t boolType) Validate(v any) (any, []ErrorDetails) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case int64, int, *big.Int:
		if t.strict {
			break
		}
		switch pyrepr.Str(x) {
		case "0":
			return false, nil
		case "1":
			return true, nil
		}
		return nil, valErr("bool_parsing", "Input should be a valid boolean, unable to interpret input", v)
	case float64:
		if t.strict {
			break
		}
		if x == 0 {
			return false, nil
		}
		if x == 1 {
			return true, nil
		}
		return nil, valErr("bool_parsing", "Input should be a valid boolean, unable to interpret input", v)
	case string:
		if t.strict {
			break
		}
		switch strings.ToLower(x) {
		case "0", "off", "f", "false", "n", "no":
			return false, nil
		case "1", "on", "t", "true", "y", "yes":
			return true, nil
		}
		return nil, valErr("bool_parsing", "Input should be a valid boolean, unable to interpret input", v)
	}
	return nil, valErr("bool_type", "Input should be a valid boolean", v)
}

func (o optionalType) Validate(v any) (any, []ErrorDetails) {
	if v == nil {
		return nil, nil
	}
	return o.inner.Validate(v)
}

func (l listType) Validate(v any) (any, []ErrorDetails) {
	var items []any
	switch x := v.(type) {
	case []any:
		items = x
	case []string:
		for _, s := range x {
			items = append(items, s)
		}
	default:
		return nil, valErr("list_type", "Input should be a valid list", v)
	}
	out := make([]any, 0, len(items))
	var errs []ErrorDetails
	for i, item := range items {
		val, e := l.item.Validate(item)
		if len(e) > 0 {
			errs = append(errs, prefixLoc([]any{int64(i)}, e)...)
			continue
		}
		out = append(out, val)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

func (d dictType) Validate(v any) (any, []ErrorDetails) {
	in, ok := v.(*pyjson.Object)
	if !ok {
		return nil, valErr("dict_type", "Input should be a valid dictionary", v)
	}
	out := pyjson.NewObject()
	var errs []ErrorDetails
	for _, k := range in.Keys {
		val, e := d.value.Validate(in.Vals[k])
		if len(e) > 0 {
			errs = append(errs, prefixLoc([]any{k}, e)...)
			continue
		}
		out.Set(k, val)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

// Expected text of a literal error, the values repr joined by commas and a final or
func literalExpected(values []any) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = pyrepr.Repr(v)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " or " + parts[len(parts)-1]
}

func (l literalType) Validate(v any) (any, []ErrorDetails) {
	for _, want := range l.values {
		if literalEqual(want, v) {
			return want, nil
		}
	}
	expected := literalExpected(l.values)
	return nil, []ErrorDetails{{Type: "literal_error", Loc: []any{}, Msg: "Input should be " + expected, Input: v,
		Ctx: jsonObj("expected", expected)}}
}

// Whether an input equals a literal value of the same Python type
func literalEqual(want, v any) bool {
	switch w := want.(type) {
	case string:
		s, ok := v.(string)
		return ok && s == w
	case bool:
		b, ok := v.(bool)
		return ok && b == w
	case int64:
		n, ok := v.(int64)
		return ok && n == w
	}
	return want == v
}

// Field of a pydantic model with its alias, type and default
type ModelField struct {
	Name     string
	Alias    string
	Type     FieldType
	Required bool
	Default  any
	Factory  func() any
}

// Required field of a type
func Field(name string, t FieldType) ModelField {
	return ModelField{Name: name, Type: t, Required: true}
}

// Field of a type with a default value
func FieldDefault(name string, t FieldType, def any) ModelField {
	return ModelField{Name: name, Type: t, Default: def}
}

// Field of a type whose default comes from a factory
func FieldFactory(name string, t FieldType, factory func() any) ModelField {
	return ModelField{Name: name, Type: t, Factory: factory}
}

// Key the field validates from, its alias when set
func (f ModelField) validationAlias() string {
	if f.Alias != "" {
		return f.Alias
	}
	return f.Name
}

// Default of an optional field, a fresh copy each time
func (f ModelField) defaultValue() any {
	if f.Factory != nil {
		return f.Factory()
	}
	return pyjson.Clone(f.Default)
}

// Pydantic model of named fields, extra keys ignored
type Model struct {
	ClassName string
	Doc       string
	Fields    []ModelField
}

func (*Model) isSequence() bool { return false }

// Validated field values in declaration order, model_type for a non-dict and missing for an absent required field
func (m *Model) Validate(v any) (any, []ErrorDetails) {
	in, ok := v.(*pyjson.Object)
	if !ok {
		return nil, []ErrorDetails{{Type: "model_type", Loc: []any{},
			Msg:   "Input should be a valid dictionary or instance of " + m.ClassName,
			Input: v, Ctx: jsonObj("class_name", m.ClassName)}}
	}
	out := pyjson.NewObject()
	var errs []ErrorDetails
	for _, f := range m.Fields {
		alias := f.validationAlias()
		raw, present := in.Get(alias)
		if !present {
			if f.Required {
				errs = append(errs, ErrorDetails{Type: "missing", Loc: []any{alias}, Msg: "Field required", Input: in})
				continue
			}
			out.Set(f.Name, f.defaultValue())
			continue
		}
		val, e := f.Type.Validate(raw)
		if len(e) > 0 {
			errs = append(errs, prefixLoc([]any{alias}, e)...)
			continue
		}
		out.Set(f.Name, val)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}
