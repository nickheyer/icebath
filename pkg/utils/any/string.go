package anyutil

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Text of a value: "" for nil, a string as is, []byte as text, numbers in decimal, true or false, an error's or Stringer's own text, containers as compact JSON
func String(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int16:
		return strconv.FormatInt(int64(x), 10)
	case int8:
		return strconv.FormatInt(int64(x), 10)
	case uint:
		return strconv.FormatUint(uint64(x), 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case uint32:
		return strconv.FormatUint(uint64(x), 10)
	case uint16:
		return strconv.FormatUint(uint64(x), 10)
	case uint8:
		return strconv.FormatUint(uint64(x), 10)
	case float64:
		return formatFloat(x, 64)
	case float32:
		return formatFloat(float64(x), 32)
	case json.Number:
		return string(x)
	case *jsonutil.Object:
		if x == nil {
			return ""
		}
		return x.String()
	case map[string]any:
		if x == nil {
			return "{}"
		}
		return encode(x)
	case []any:
		if x == nil {
			return "[]"
		}
		return encode(x)
	case []string:
		if x == nil {
			return "[]"
		}
		return encode(x)
	case error:
		if nilPointer(v) {
			return ""
		}
		return x.Error()
	case fmt.Stringer:
		if nilPointer(v) {
			return ""
		}
		return x.String()
	}
	return reflectString(reflect.ValueOf(v), v)
}

// A string, a string-kind value or []byte in Go double-quoted form, anything else as String gives it
func Quote(v any) string {
	switch x := v.(type) {
	case string:
		return strconv.Quote(x)
	case []byte:
		return strconv.Quote(string(x))
	case nil, bool, int, int64, float64, json.Number, map[string]any, []any, *jsonutil.Object:
		return String(v)
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.String {
		return strconv.Quote(rv.String())
	} else if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
		return strconv.Quote(string(rv.Bytes()))
	}
	return String(v)
}

// Text of a value outside the common shapes, by its kind
func reflectString(rv reflect.Value, v any) string {
	switch rv.Kind() {
	case reflect.String:
		return rv.String()
	case reflect.Bool:
		return strconv.FormatBool(rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(rv.Uint(), 10)
	case reflect.Float32:
		return formatFloat(rv.Float(), 32)
	case reflect.Float64:
		return formatFloat(rv.Float(), 64)
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return string(rv.Bytes())
		}
		if rv.IsNil() {
			return "[]"
		}
		return encode(v)
	case reflect.Map:
		if rv.IsNil() {
			return "{}"
		}
		return encode(v)
	case reflect.Array:
		return encode(v)
	case reflect.Pointer:
		if rv.IsNil() {
			return ""
		}
		return String(rv.Elem().Interface())
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		if rv.IsNil() {
			return ""
		}
	}
	return fmt.Sprint(v)
}

// Compact JSON text of a container, values JSON has no form for (channels, functions, complex numbers) written as their fmt text, and %!(reason) for a container that cannot be written at all, such as one that contains itself
func encode(v any) string {
	text, err := jsonutil.Encode(v, fmtFallback)
	if err != nil {
		return "%!(" + err.Error() + ")"
	}
	return text
}

var fmtFallback = jsonutil.Convert(func(v any) (any, error) { return fmt.Sprint(v), nil })

// Shortest text that reads back as f, in fixed form between 1e-6 and 1e21 and exponent form outside as encoding/json writes floats
func formatFloat(f float64, bits int) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	}
	format := byte('f')
	if abs := math.Abs(f); abs != 0 {
		if bits == 64 && (abs < 1e-6 || abs >= 1e21) || bits == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21) {
			format = 'e'
		}
	}
	var buf [32]byte
	b := strconv.AppendFloat(buf[:0], f, format, -1, bits)
	if n := len(b); format == 'e' && n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
		b[n-2] = b[n-1]
		b = b[:n-1]
	}
	return string(b)
}

// Whether v is a nil pointer, whose methods may not be callable
func nilPointer(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}
