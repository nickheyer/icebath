package pyacp

import (
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Version of pydantic the schema validates under, used in error urls
const pydanticMajorMinor = "2.14"

type fieldInfo struct {
	index    int
	alias    string
	py       string
	req      bool
	def      string
	lit      string
	enum     []string
	enumType bool
	union    []string
}

var fieldCache sync.Map

// Parsed acp tags of a schema struct in pydantic field order
func modelFields(t reflect.Type) []fieldInfo {
	if v, ok := fieldCache.Load(t); ok {
		return v.([]fieldInfo)
	}
	var out []fieldInfo
	for i := 0; i < t.NumField(); i++ {
		tag, ok := t.Field(i).Tag.Lookup("acp")
		if !ok {
			continue
		}
		parts := strings.Split(tag, ",")
		fi := fieldInfo{index: i, alias: parts[0], py: parts[0]}
		for _, p := range parts[1:] {
			k, v, _ := strings.Cut(p, "=")
			switch k {
			case "py":
				fi.py = v
			case "req":
				fi.req = true
			case "def":
				fi.def = v
			case "lit":
				fi.lit = v
			case "enum":
				fi.enum = strings.Split(v, "|")
			case "enumtype":
				fi.enumType = true
			case "union":
				fi.union = strings.Split(v, "|")
			}
		}
		out = append(out, fi)
	}
	fieldCache.Store(t, out)
	return out
}

// Reports whether v is a schema model value or pointer to one
func IsModel(v any) bool {
	if v == nil {
		return false
	}
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct && len(modelFields(t)) > 0
}

// Reports whether v is nil or a typed nil pointer, slice, map or interface
func IsNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface, reflect.Func:
		return rv.IsNil()
	}
	return false
}

// Flags of pydantic model_dump
type DumpOptions struct {
	ByAlias         bool
	ExcludeNone     bool
	ExcludeUnset    bool
	ExcludeDefaults bool
}

// Options of serialize_params, by alias excluding None and defaults
var SerializeParams = DumpOptions{ByAlias: true, ExcludeNone: true, ExcludeDefaults: true}

// Options the connection uses for a model returned as a request result
var ResultDump = DumpOptions{ByAlias: true, ExcludeNone: true, ExcludeUnset: true}

// Python model_dump of a schema model as ordered JSON values, other values converted recursively
func Dump(v any, o DumpOptions) any {
	if v == nil {
		return nil
	}
	return dumpValue(reflect.ValueOf(v), o)
}

func dumpValue(rv reflect.Value, o DumpOptions) any {
	switch rv.Kind() {
	case reflect.Invalid:
		return nil
	case reflect.Interface, reflect.Pointer:
		if rv.IsNil() {
			return nil
		}
		if obj, ok := rv.Interface().(*pyjson.Object); ok {
			out := pyjson.NewObject()
			for _, k := range obj.Keys {
				out.Set(k, Dump(obj.Vals[k], o))
			}
			return out
		}
		if b, ok := rv.Interface().(*big.Int); ok {
			return b
		}
		return dumpValue(rv.Elem(), o)
	case reflect.Struct:
		if len(modelFields(rv.Type())) > 0 {
			return dumpModel(rv, o)
		}
		return rv.Interface()
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return []any{}
		}
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = dumpValue(rv.Index(i), o)
		}
		return out
	case reflect.Map:
		keys := rv.MapKeys()
		names := make([]string, 0, len(keys))
		for _, k := range keys {
			names = append(names, fmt.Sprint(k.Interface()))
		}
		sort.Strings(names)
		out := pyjson.NewObject()
		for _, n := range names {
			out.Set(n, dumpValue(rv.MapIndex(reflect.ValueOf(n).Convert(rv.Type().Key())), o))
		}
		return out
	case reflect.String:
		return rv.String()
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(rv.Uint())
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	}
	return rv.Interface()
}

func nillable(k reflect.Kind) bool {
	switch k {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		return true
	}
	return false
}

// Default value of a field as a dumped JSON value
func defaultDump(f fieldInfo, ft reflect.Type, o DumpOptions) any {
	switch f.def {
	case "false":
		return false
	case "true":
		return true
	case "[]":
		return []any{}
	case "model":
		et := ft
		if et.Kind() == reflect.Pointer {
			et = et.Elem()
		}
		return dumpModel(reflect.New(et).Elem(), o)
	case "dict":
		obj := pyjson.NewObject()
		obj.Set("terminal", false)
		return obj
	}
	return nil
}

// Reports whether a set field equals its declared default under Python equality
func equalsDefault(f fieldInfo, fv reflect.Value) bool {
	switch f.def {
	case "false", "true":
		if fv.Kind() == reflect.Pointer {
			fv = fv.Elem()
		}
		return fv.Kind() == reflect.Bool && fv.Bool() == (f.def == "true")
	case "[]":
		return fv.Kind() == reflect.Slice && fv.Len() == 0
	case "model":
		full := DumpOptions{}
		return pyjson.Equal(dumpValue(fv, full), defaultDump(f, fv.Type(), full))
	}
	return false
}

func dumpModel(rv reflect.Value, o DumpOptions) *pyjson.Object {
	out := pyjson.NewObject()
	for _, f := range modelFields(rv.Type()) {
		key := f.py
		if o.ByAlias {
			key = f.alias
		}
		if f.lit != "" {
			out.Set(key, f.lit)
			continue
		}
		fv := rv.Field(f.index)
		if nillable(fv.Kind()) && fv.IsNil() && !(f.req && fv.Kind() == reflect.Slice) {
			if o.ExcludeUnset && !f.req {
				continue
			}
			if f.def == "" {
				if o.ExcludeNone {
					continue
				}
				out.Set(key, nil)
				continue
			}
			if o.ExcludeDefaults {
				continue
			}
			out.Set(key, defaultDump(f, fv.Type(), o))
			continue
		}
		if o.ExcludeDefaults && f.def != "" && equalsDefault(f, fv) {
			continue
		}
		out.Set(key, dumpValue(fv, o))
	}
	return out
}

// Pydantic ValidationError carrying the errors() list of a failed model_validate
type ValidationError struct {
	Title string
	Errs  []*pyjson.Object
}

// Pydantic str form listing each error with its location, message and input
func (e *ValidationError) Error() string {
	var b strings.Builder
	plural := "s"
	if len(e.Errs) == 1 {
		plural = ""
	}
	fmt.Fprintf(&b, "%d validation error%s for %s", len(e.Errs), plural, e.Title)
	for _, er := range e.Errs {
		loc, _ := er.Get("loc")
		parts := []string{}
		for _, l := range loc.([]any) {
			parts = append(parts, fmt.Sprint(l))
		}
		msg, _ := er.Get("msg")
		typ, _ := er.Get("type")
		input, _ := er.Get("input")
		url, _ := er.Get("url")
		inputRepr := pyrepr.Repr(input)
		if r := []rune(inputRepr); len(r) > 50 {
			inputRepr = string(r[:25]) + "..." + string(r[len(r)-24:])
		}
		fmt.Fprintf(&b, "\n%s\n  %s [type=%s, input_value=%s, input_type=%s]\n    For further information visit %s", strings.Join(parts, "."), msg, typ, inputRepr, pyrepr.TypeName(input), url)
	}
	return b.String()
}

// The errors() list as JSON values
func (e *ValidationError) Errors() []any {
	out := make([]any, len(e.Errs))
	for i, er := range e.Errs {
		out[i] = er
	}
	return out
}

func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case string:
		return "str"
	case bool:
		return "bool"
	case int, int64, *big.Int:
		return "int"
	case float64:
		return "float"
	case []any:
		return "list"
	case *pyjson.Object:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

type validator struct {
	errs []*pyjson.Object
}

func (va *validator) fail(typ string, loc []any, msg string, input any, ctx *pyjson.Object) {
	er := pyjson.NewObject()
	er.Set("type", typ)
	er.Set("loc", append([]any{}, loc...))
	er.Set("msg", msg)
	er.Set("input", input)
	if ctx != nil {
		er.Set("ctx", ctx)
	}
	er.Set("url", "https://errors.pydantic.dev/"+pydanticMajorMinor+"/v/"+typ)
	va.errs = append(va.errs, er)
}

// Model_validate of decoded JSON into a new schema model of type T
func Validate[T any](input any) (*T, error) {
	t := reflect.TypeFor[T]()
	va := &validator{}
	rv, _ := va.model(t, input, nil)
	if len(va.errs) > 0 {
		return nil, &ValidationError{Title: t.Name(), Errs: va.errs}
	}
	out := rv.Addr().Interface().(*T)
	return out, nil
}

// Model_validate of decoded JSON into the schema type of the given name
func ValidateNamed(name string, input any) (any, error) {
	t, ok := schemaTypes[name]
	if !ok {
		return nil, fmt.Errorf("unknown schema model %s", name)
	}
	va := &validator{}
	rv, _ := va.model(t, input, nil)
	if len(va.errs) > 0 {
		return nil, &ValidationError{Title: t.Name(), Errs: va.errs}
	}
	return rv.Addr().Interface(), nil
}

func quoteList(vals []string) string {
	q := make([]string, len(vals))
	for i, v := range vals {
		q[i] = "'" + v + "'"
	}
	if len(q) == 1 {
		return q[0]
	}
	return strings.Join(q[:len(q)-1], ", ") + " or " + q[len(q)-1]
}

func ctxOf(k string, v any) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set(k, v)
	return o
}

// Validated struct value and the number of fields the input set
func (va *validator) model(t reflect.Type, input any, loc []any) (reflect.Value, int) {
	out := reflect.New(t).Elem()
	if inst := reflect.ValueOf(input); input != nil && inst.Kind() == reflect.Pointer && inst.Type().Elem() == t {
		return inst.Elem(), len(modelFields(t))
	}
	obj, ok := input.(*pyjson.Object)
	if !ok {
		va.fail("model_type", loc, "Input should be a valid dictionary or instance of "+t.Name(), input, ctxOf("class_name", t.Name()))
		return out, 0
	}
	set := 0
	for _, f := range modelFields(t) {
		val, present := obj.Get(f.alias)
		if !present && f.py != f.alias {
			val, present = obj.Get(f.py)
		}
		floc := append(append([]any{}, loc...), f.alias)
		if !present {
			if f.req || f.lit != "" {
				va.fail("missing", floc, "Field required", input, nil)
			}
			continue
		}
		set++
		if f.lit != "" {
			if s, ok := val.(string); !ok || s != f.lit {
				va.fail("literal_error", floc, "Input should be "+quoteList([]string{f.lit}), val, ctxOf("expected", quoteList([]string{f.lit})))
			}
			continue
		}
		fv := out.Field(f.index)
		va.value(fv, f, val, floc)
	}
	return out, set
}

// Validates one input value into fv following pydantic lax python mode
func (va *validator) value(fv reflect.Value, f fieldInfo, val any, loc []any) {
	ft := fv.Type()
	if val == nil && nillable(ft.Kind()) {
		required := f.req && ((ft.Kind() == reflect.Interface && len(f.union) > 0) || ft.Kind() == reflect.Slice)
		if !required {
			return
		}
	}
	if ft.Kind() == reflect.Pointer && ft != reflect.TypeFor[*pyjson.Object]() {
		inner := reflect.New(ft.Elem())
		va.value(inner.Elem(), fieldInfo{enum: f.enum, enumType: f.enumType, union: f.union}, val, loc)
		fv.Set(inner)
		return
	}
	switch {
	case ft == reflect.TypeFor[*pyjson.Object]():
		obj, ok := val.(*pyjson.Object)
		if !ok {
			va.fail("dict_type", loc, "Input should be a valid dictionary", val, nil)
			return
		}
		fv.Set(reflect.ValueOf(obj))
	case ft.Kind() == reflect.String:
		s, ok := val.(string)
		if !ok {
			if len(f.enum) > 0 {
				va.enumFail(f, loc, val)
				return
			}
			va.fail("string_type", loc, "Input should be a valid string", val, nil)
			return
		}
		if len(f.enum) > 0 && !contains(f.enum, s) {
			va.enumFail(f, loc, val)
			return
		}
		fv.SetString(s)
	case ft.Kind() == reflect.Bool:
		b, ok := laxBool(val)
		if !ok {
			if _, isStr := val.(string); isStr {
				va.fail("bool_parsing", loc, "Input should be a valid boolean, unable to interpret input", val, nil)
			} else {
				va.fail("bool_type", loc, "Input should be a valid boolean", val, nil)
			}
			return
		}
		fv.SetBool(b)
	case ft.Kind() == reflect.Int:
		va.intValue(fv, val, loc)
	case ft.Kind() == reflect.Float64:
		switch x := val.(type) {
		case float64:
			fv.SetFloat(x)
		case int64:
			fv.SetFloat(float64(x))
		case bool:
			fv.SetFloat(map[bool]float64{true: 1, false: 0}[x])
		case string:
			p, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
			if err != nil {
				va.fail("float_parsing", loc, "Input should be a valid number, unable to parse string as a number", val, nil)
				return
			}
			fv.SetFloat(p)
		default:
			va.fail("float_type", loc, "Input should be a valid number", val, nil)
		}
	case ft.Kind() == reflect.Slice:
		items, ok := val.([]any)
		if !ok {
			va.fail("list_type", loc, "Input should be a valid list", val, nil)
			return
		}
		sl := reflect.MakeSlice(ft, len(items), len(items))
		for i, it := range items {
			va.value(sl.Index(i), fieldInfo{req: true, enum: f.enum, enumType: f.enumType, union: f.union}, it, append(append([]any{}, loc...), int64(i)))
		}
		fv.Set(sl)
	case ft.Kind() == reflect.Struct:
		rv, _ := va.model(ft, val, loc)
		fv.Set(rv)
	case ft.Kind() == reflect.Interface && len(f.union) > 0:
		va.union(fv, f.union, val, loc)
	default:
		if val != nil {
			fv.Set(reflect.ValueOf(val))
		}
	}
}

func (va *validator) enumFail(f fieldInfo, loc []any, val any) {
	typ := "literal_error"
	if f.enumType {
		typ = "enum"
	}
	exp := quoteList(f.enum)
	va.fail(typ, loc, "Input should be "+exp, val, ctxOf("expected", exp))
}

func (va *validator) intValue(fv reflect.Value, val any, loc []any) {
	switch x := val.(type) {
	case int64:
		fv.SetInt(x)
	case bool:
		if x {
			fv.SetInt(1)
		} else {
			fv.SetInt(0)
		}
	case *big.Int:
		if x.IsInt64() {
			fv.SetInt(x.Int64())
			return
		}
		fv.SetInt(x.Int64())
	case float64:
		if x != math.Trunc(x) || math.IsInf(x, 0) || math.IsNaN(x) {
			va.fail("int_from_float", loc, "Input should be a valid integer, got a number with a fractional part", val, nil)
			return
		}
		fv.SetInt(int64(x))
	case string:
		s := strings.ReplaceAll(strings.TrimSpace(x), "_", "")
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			fv.SetInt(n)
			return
		}
		if fl, err := strconv.ParseFloat(s, 64); err == nil && fl == math.Trunc(fl) && !strings.ContainsAny(s, "eEn") {
			fv.SetInt(int64(fl))
			return
		}
		va.fail("int_parsing", loc, "Input should be a valid integer, unable to parse string as an integer", val, nil)
	default:
		va.fail("int_type", loc, "Input should be a valid integer", val, nil)
	}
}

// Smart mode union keeping the successful member with the most fields set, first on ties
func (va *validator) union(fv reflect.Value, members []string, val any, loc []any) {
	var best reflect.Value
	bestSet := -1
	var failed []*pyjson.Object
	for _, m := range members {
		sub := &validator{}
		mloc := append(append([]any{}, loc...), m)
		var rv reflect.Value
		set := 0
		if name, ok := strings.CutPrefix(m, "list:"); ok {
			mloc[len(mloc)-1] = "list[" + name + "]"
			lt := reflect.SliceOf(schemaTypes[name])
			holder := reflect.New(lt).Elem()
			sub.value(holder, fieldInfo{req: true}, val, mloc)
			rv = holder
			if items, ok := val.([]any); ok {
				set = len(items)
			}
		} else {
			mt := schemaTypes[m]
			var sv reflect.Value
			sv, set = sub.model(mt, val, mloc)
			rv = sv.Addr()
		}
		if len(sub.errs) > 0 {
			failed = append(failed, sub.errs...)
			continue
		}
		if set > bestSet {
			best, bestSet = rv, set
		}
	}
	if bestSet < 0 {
		va.errs = append(va.errs, failed...)
		return
	}
	fv.Set(best)
}

func laxBool(val any) (bool, bool) {
	switch x := val.(type) {
	case bool:
		return x, true
	case int64:
		if x == 0 || x == 1 {
			return x == 1, true
		}
	case float64:
		if x == 0 || x == 1 {
			return x == 1, true
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "0", "off", "f", "false", "n", "no":
			return false, true
		case "1", "on", "t", "true", "y", "yes":
			return true, true
		}
	}
	return false, false
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
