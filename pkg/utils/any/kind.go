package anyutil

import (
	"encoding/json"
	"math/big"
	"reflect"
	"strconv"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Word for the type of a value in messages: "null", "boolean", "integer", "number", "string", "bytes", "array", "object", else the Go type
func Kind(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []byte:
		return "bytes"
	case int, int64, int32, int16, int8, uint, uint64, uint32, uint16, uint8, uintptr:
		return "integer"
	case float64, float32:
		return "number"
	case json.Number:
		if _, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return "integer"
		}
		return "number"
	case *big.Int:
		if x == nil {
			return "null"
		}
		return "integer"
	case []any, []string, []map[string]any:
		return "array"
	case map[string]any, map[string]string:
		return "object"
	case *jsonutil.Object:
		if x == nil {
			return "null"
		}
		return "object"
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool:
		return "boolean"
	case reflect.String:
		return "string"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return "bytes"
		}
		return "array"
	case reflect.Array:
		return "array"
	case reflect.Map:
		return "object"
	case reflect.Pointer:
		if rv.IsNil() {
			return "null"
		}
		if rv.Elem().Kind() == reflect.Struct {
			return rv.Type().Elem().String()
		}
		return Kind(rv.Elem().Interface())
	}
	return rv.Type().String()
}
