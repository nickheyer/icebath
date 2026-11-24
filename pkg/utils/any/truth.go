package anyutil

import (
	"encoding/json"
	"math/big"
	"reflect"
	"strconv"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Whether a value counts as true the way Go templates test it: false for nil, false, zero numbers, empty strings, slices, maps and objects, nil pointers and anything whose Len is 0
func IsTrue(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case int32:
		return x != 0
	case int16:
		return x != 0
	case int8:
		return x != 0
	case uint:
		return x != 0
	case uint64:
		return x != 0
	case uint32:
		return x != 0
	case uint16:
		return x != 0
	case uint8:
		return x != 0
	case float64:
		return x != 0
	case float32:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case []string:
		return len(x) > 0
	case []byte:
		return len(x) > 0
	case []map[string]any:
		return len(x) > 0
	case *jsonutil.Object:
		return x != nil && x.Len() > 0
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		return x != "" && (err != nil || f != 0)
	case *big.Int:
		return x != nil && x.Sign() != 0
	case interface{ Len() int }:
		return !nilPointer(v) && x.Len() > 0
	}
	return reflectTrue(reflect.ValueOf(v))
}

// First value that is true, else the last one, nil when there are none
func Or(vals ...any) any {
	for _, v := range vals {
		if IsTrue(v) {
			return v
		}
	}
	if len(vals) == 0 {
		return nil
	}
	return vals[len(vals)-1]
}

func reflectTrue(rv reflect.Value) bool {
	switch rv.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return rv.Len() > 0
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return rv.Uint() != 0
	case reflect.Float32, reflect.Float64:
		return rv.Float() != 0
	case reflect.Complex64, reflect.Complex128:
		return rv.Complex() != 0
	case reflect.Chan, reflect.Func, reflect.Pointer, reflect.UnsafePointer, reflect.Interface:
		return !rv.IsNil()
	}
	return true
}
