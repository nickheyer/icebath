package provider

import (
	"reflect"
	"sort"
	"strconv"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Identity of a map, slice or pointer held on a conversion path
type plainDataID struct {
	kind reflect.Kind
	ptr  uintptr
	size int
}

// Identity of a value that can contain itself, false for every other value
func plainDataIdentity(value any) (plainDataID, bool) {
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Map, reflect.Pointer:
		if !rv.IsNil() {
			return plainDataID{rv.Kind(), rv.Pointer(), 0}, true
		}
	case reflect.Slice:
		if rv.Len() > 0 {
			return plainDataID{rv.Kind(), rv.Pointer(), rv.Len()}, true
		}
	}
	return plainDataID{}, false
}

// Text of a value's plain data: map keys sorted, ordered objects and struct fields in their order, strings quoted inside a container, nil as null, a container met again inside itself as {...} or [...]
func plainDataText(value any, active map[plainDataID]bool, nested bool) string {
	id, tracked := plainDataIdentity(value)
	if tracked && active[id] {
		return plainDataCycleMarker(value)
	}
	if tracked {
		active[id] = true
		defer delete(active, id)
	}
	item := func(v any) string { return plainDataText(v, active, true) }
	switch v := value.(type) {
	case nil:
		return "null"
	case ModelDumper:
		return plainDataText(v.ModelDump(false), active, nested)
	case interface{ ModelDump() any }:
		return plainDataText(v.ModelDump(), active, nested)
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = strconv.Quote(k) + ": " + item(v[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case *jsonutil.Object:
		if v == nil {
			return "null"
		}
		parts := make([]string, 0, v.Len())
		for k, val := range v.All() {
			parts = append(parts, strconv.Quote(k)+": "+item(val))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, len(v))
		for i, val := range v {
			parts[i] = item(val)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []byte:
		return plainDataScalarText(string(v), nested)
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return "null"
		}
		return plainDataText(rv.Elem().Interface(), active, nested)
	case reflect.Map:
		keys := make([]string, 0, rv.Len())
		vals := make(map[string]any, rv.Len())
		for iter := rv.MapRange(); iter.Next(); {
			k := anyutil.String(iter.Key().Interface())
			keys = append(keys, k)
			vals[k] = iter.Value().Interface()
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = strconv.Quote(k) + ": " + item(vals[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
			break
		}
		parts := make([]string, rv.Len())
		for i := range parts {
			parts[i] = item(rv.Index(i).Interface())
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case reflect.Struct:
		var parts []string
		for i := 0; i < rv.NumField(); i++ {
			if name := plainDataFieldName(rv.Type().Field(i)); name != "" {
				parts = append(parts, strconv.Quote(name)+": "+item(rv.Field(i).Interface()))
			}
		}
		if len(parts) > 0 {
			return "{" + strings.Join(parts, ", ") + "}"
		}
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16,
		reflect.Uint32, reflect.Uint64, reflect.Uintptr, reflect.Float32, reflect.Float64:
		return anyutil.String(value)
	}
	return plainDataScalarText(anyutil.String(value), nested)
}

// Marker standing for a container met again inside itself, [...] for a list and {...} for a mapping or object
func plainDataCycleMarker(value any) string {
	rv := reflect.ValueOf(value)
	if rv.Kind() == reflect.Pointer && !rv.IsNil() {
		rv = rv.Elem()
	}
	if k := rv.Kind(); k == reflect.Slice || k == reflect.Array {
		return "[...]"
	}
	return "{...}"
}

// Text of a string-like value, quoted when it sits inside a container
func plainDataScalarText(s string, nested bool) string {
	if nested {
		return strconv.Quote(s)
	}
	return s
}

// Whether toPlainData descends into the value rather than returning it as it is
func plainDataContainer(value any) bool {
	switch v := value.(type) {
	case nil, []byte:
		return false
	case ModelDumper, interface{ ModelDump() any }, map[string]any, []any:
		return true
	case *jsonutil.Object:
		return v != nil
	}
	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer && !rv.IsNil() {
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map, reflect.Slice, reflect.Array:
		return true
	case reflect.Struct:
		for i := 0; i < rv.NumField(); i++ {
			if plainDataFieldName(rv.Type().Field(i)) != "" {
				return true
			}
		}
	}
	return false
}

// Attribute name of a struct field, its JSON name when tagged, empty for a field that is not exported or is private
func plainDataFieldName(field reflect.StructField) string {
	if !field.IsExported() {
		return ""
	}
	name := field.Name
	if tag, _, _ := strings.Cut(field.Tag.Get("json"), ","); tag == "-" {
		return ""
	} else if tag != "" {
		name = tag
	}
	if strings.HasPrefix(name, "_") {
		return ""
	}
	return name
}

// Plain maps, lists and scalars of an SDK object or nested data, a container in a true cycle or past depth 20 becoming its text
func toPlainData(value any, depth int, path map[plainDataID]bool) any {
	if path == nil {
		path = map[plainDataID]bool{}
	}
	id, tracked := plainDataIdentity(value)
	if depth > 20 || (tracked && path[id]) {
		if !plainDataContainer(value) {
			return value
		}
		return plainDataText(value, map[plainDataID]bool{}, false)
	}
	rec := func(v any) any { return toPlainData(v, depth+1, path) }
	if tracked {
		path[id] = true
		defer delete(path, id)
	}
	switch v := value.(type) {
	case ModelDumper:
		return rec(v.ModelDump(false))
	case interface{ ModelDump() any }:
		return rec(v.ModelDump())
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = rec(item)
		}
		return out
	case *jsonutil.Object:
		if v == nil {
			return value
		}
		out := make(map[string]any, v.Len())
		for _, k := range v.Keys() {
			out[k] = rec(v.ValueOr(k, nil))
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = rec(item)
		}
		return out
	case []byte:
		return value
	}
	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer && !rv.IsNil() {
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		out := make(map[string]any, rv.Len())
		for iter := rv.MapRange(); iter.Next(); {
			out[anyutil.String(iter.Key().Interface())] = rec(iter.Value().Interface())
		}
		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = rec(rv.Index(i).Interface())
		}
		return out
	case reflect.Struct:
		out := map[string]any{}
		for i := 0; i < rv.NumField(); i++ {
			if name := plainDataFieldName(rv.Type().Field(i)); name != "" {
				out[name] = rec(rv.Field(i).Interface())
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return value
}
