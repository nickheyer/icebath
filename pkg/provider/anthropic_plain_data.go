package provider

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
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

// Text of a value as str renders it, a container met again inside itself shown as an ellipsis
func plainDataText(value any, active map[plainDataID]bool, nested bool) string {
	id, tracked := plainDataIdentity(value)
	enter := func(marker string) (string, bool) {
		if tracked && active[id] {
			return marker, true
		}
		if tracked {
			active[id] = true
		}
		return "", false
	}
	switch v := value.(type) {
	case map[string]any:
		if marker, again := enter("{...}"); again {
			return marker
		}
		defer delete(active, id)
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyrepr.Repr(k) + ": " + plainDataText(v[k], active, true)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		if marker, again := enter("[...]"); again {
			return marker
		}
		defer delete(active, id)
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = plainDataText(item, active, true)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	if rv := reflect.ValueOf(value); rv.Kind() == reflect.Pointer && !rv.IsNil() {
		return fmt.Sprintf("<%s object at %#x>", rv.Elem().Type().String(), rv.Pointer())
	}
	if nested {
		return pyrepr.Repr(value)
	}
	return pyrepr.Str(value)
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

// Plain maps, lists and scalars of an SDK object or nested data, a true cycle or a depth past 20 becoming its text
func toPlainData(value any, depth int, path map[plainDataID]bool) any {
	if path == nil {
		path = map[plainDataID]bool{}
	}
	id, tracked := plainDataIdentity(value)
	if depth > 20 || (tracked && path[id]) {
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
	case *pyjson.Object:
		if v == nil {
			return value
		}
		out := make(map[string]any, len(v.Keys))
		for _, k := range v.Keys {
			out[k] = rec(v.GetOr(k, nil))
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
			out[pyrepr.Str(iter.Key().Interface())] = rec(iter.Value().Interface())
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
