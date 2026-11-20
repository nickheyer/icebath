package state

import (
	"reflect"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var contentNonTextPartTypes = map[string]bool{"image": true, "image_url": true, "input_image": true, "audio": true, "input_audio": true}

var contentTextKeys = []string{"text", "content", "input_text", "output_text", "summary_text"}

// Value under key for any mapping, else the same named attribute of a struct, nil when absent
func partField(value any, key string) any {
	switch m := value.(type) {
	case nil:
		return nil
	case map[string]any:
		return m[key]
	case pyObject:
		v, _ := m.get(key)
		return v
	case interface{ Get(string) (any, bool) }:
		v, _ := m.Get(key)
		return v
	case pyrepr.Mapping:
		for k, v := range m.PyItems() {
			if k == any(key) {
				return v
			}
		}
		return nil
	}
	if rv := reflect.ValueOf(value); rv.Kind() == reflect.Map {
		kt := rv.Type().Key()
		var k reflect.Value
		switch {
		case kt.Kind() == reflect.String:
			k = reflect.ValueOf(key).Convert(kt)
		case kt.Kind() == reflect.Interface && reflect.TypeOf(key).Implements(kt):
			k = reflect.ValueOf(key)
		default:
			return nil
		}
		if v := rv.MapIndex(k); v.IsValid() {
			return v.Interface()
		}
		return nil
	}
	return attrOf(value, key)
}

// Struct field whose json name or Go name matches key, else its Extra entry, nil for anything without one
func attrOf(value any, key string) any {
	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	if f, ok := structField(rv, key); ok {
		return f.Interface()
	}
	if ef := rv.FieldByName("Extra"); ef.IsValid() && ef.CanInterface() {
		if extra, ok := ef.Interface().(map[string]any); ok {
			return extra[key]
		}
	}
	return nil
}

// Exported struct field whose json name or Go name matches key
func structField(rv reflect.Value, key string) (reflect.Value, bool) {
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() || f.Name == "Extra" {
			continue
		}
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "-" {
			continue
		}
		if tag == key || (tag == "" && strings.EqualFold(f.Name, strings.ReplaceAll(key, "_", ""))) {
			return rv.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// Elements of a Python list value, false for strings, bytes and non sequences
func listItems(value any) ([]any, bool) {
	switch v := value.(type) {
	case []any:
		return v, true
	case string, []byte, nil:
		return nil, false
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() != reflect.Slice {
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

// Visible text of one content part, empty for media parts and parts without text
func textFromPart(part any) string {
	if part == nil {
		return ""
	}
	if s, ok := part.(string); ok {
		return s
	}
	typ := ""
	if t := partField(part, "type"); pyTruthy(t) {
		typ = pyrepr.Str(t)
	}
	if contentNonTextPartTypes[pystr.Lower(pystr.Strip(typ))] {
		return ""
	}
	for _, key := range contentTextKeys {
		if s, ok := partField(part, key).(string); ok {
			return s
		}
	}
	return ""
}

// Visible text of chat or Responses message content joined with sep, the str of any other value
func FlattenMessageText(content any, sep string) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	if items, ok := listItems(content); ok {
		var chunks []string
		for _, item := range items {
			if chunk := textFromPart(item); chunk != "" {
				chunks = append(chunks, chunk)
			}
		}
		return strings.Join(chunks, sep)
	}
	if text := textFromPart(content); text != "" {
		return text
	}
	return pyrepr.Str(content)
}
