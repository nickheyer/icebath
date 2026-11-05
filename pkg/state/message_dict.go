package state

import (
	"bytes"
	"encoding/json"

	"reflect"
	"strings"
)

// Extra value recording that a key the struct always renders was absent from the source dict
type absentKey struct{}

// Dict keys of the Message fields that carry no json name
var messageBookkeepingKeys = map[string]string{"RowID": "id", "Active": "active", "Compacted": "compacted", "Persisted": DBPersistedMarker}

// Lossless dict form of a message, struct fields first and every Extra key on top
func MessageToDict(m *Message) map[string]any {
	if m == nil {
		return nil
	}
	return structToDict(reflect.ValueOf(m).Elem(), messageBookkeepingKeys)
}

// Message holding every dict key, keys whose value does not fit a field kept in Extra
func MessageFromDict(d map[string]any) *Message {
	m := &Message{}
	dictToStruct(d, reflect.ValueOf(m).Elem(), messageBookkeepingKeys)
	return m
}

// Lossless dict form of a tool call struct
func ToolCallToDict(tc ToolCall) map[string]any {
	return structToDict(reflect.ValueOf(tc), nil)
}

// Tool call struct from a dict entry, unfit keys kept in Extra
func ToolCallFromDict(d map[string]any) ToolCall {
	var tc ToolCall
	dictToStruct(d, reflect.ValueOf(&tc).Elem(), nil)
	return tc
}

// Dict key and omitempty flag of a struct field, empty key for fields with no dict form
func fieldKey(f reflect.StructField, bookkeeping map[string]string) (string, bool) {
	if f.Name == "Extra" || !f.IsExported() {
		return "", false
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return bookkeeping[f.Name], true
	}
	name, opts, _ := strings.Cut(tag, ",")
	return name, strings.Contains(opts, "omitempty")
}

// Renders struct fields under their dict keys then applies Extra, dropping keys marked absent
func structToDict(rv reflect.Value, bookkeeping map[string]string) map[string]any {
	out := map[string]any{}
	rt := rv.Type()
	for i := range rt.NumField() {
		key, omitEmpty := fieldKey(rt.Field(i), bookkeeping)
		if key == "" {
			continue
		}
		fv := rv.Field(i)
		if omitEmpty && fv.IsZero() {
			continue
		}
		out[key] = fieldToDictValue(fv)
	}
	if extra, ok := rv.FieldByName("Extra").Interface().(map[string]any); ok {
		for k, v := range extra {
			if _, absent := v.(absentKey); absent {
				delete(out, k)
				continue
			}
			out[k] = v
		}
	}
	return out
}

// Python shaped value of one struct field
func fieldToDictValue(fv reflect.Value) any {
	switch v := fv.Interface().(type) {
	case json.RawMessage:
		if len(v) == 0 {
			return nil
		}
		dec := json.NewDecoder(bytes.NewReader(v))
		dec.UseNumber()
		var out any
		if dec.Decode(&out) != nil {
			return string(v)
		}
		return out
	case []ToolCall:
		out := make([]any, len(v))
		for i, tc := range v {
			out[i] = ToolCallToDict(tc)
		}
		return out
	case ToolCallFunction:
		return structToDict(fv, nil)
	case []string:
		out := make([]any, len(v))
		for i, s := range v {
			out[i] = s
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(v))
		for k, s := range v {
			out[k] = s
		}
		return out
	}
	return fv.Interface()
}

// Fills struct fields from their dict keys, sending unfit values, zero valued omitempty keys and absences to Extra
func dictToStruct(d map[string]any, rv reflect.Value, bookkeeping map[string]string) {
	extra := map[string]any{}
	known := map[string]bool{}
	rt := rv.Type()
	for i := range rt.NumField() {
		key, omitEmpty := fieldKey(rt.Field(i), bookkeeping)
		if key == "" {
			continue
		}
		known[key] = true
		value, present := d[key]
		if !present {
			if !omitEmpty {
				extra[key] = absentKey{}
			}
			continue
		}
		fv := rv.Field(i)
		if !assignDictValue(fv, value) {
			extra[key] = value
			continue
		}
		if omitEmpty && fv.IsZero() {
			extra[key] = value
		}
	}
	for k, v := range d {
		if !known[k] {
			extra[k] = v
		}
	}
	if len(extra) > 0 {
		rv.FieldByName("Extra").Set(reflect.ValueOf(extra))
	}
}

// Stores a dict value into a field when the Python value has the field's shape exactly
func assignDictValue(fv reflect.Value, value any) bool {
	switch fv.Interface().(type) {
	case json.RawMessage:
		if value == nil {
			return true
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return false
		}
		candidate := reflect.ValueOf(json.RawMessage(raw))
		if !reflect.DeepEqual(fieldToDictValue(candidate), value) {
			return false
		}
		fv.Set(candidate)
		return true
	case []ToolCall:
		items, ok := value.([]any)
		if !ok {
			return false
		}
		calls := make([]ToolCall, len(items))
		for i, item := range items {
			dict, ok := item.(map[string]any)
			if !ok {
				return false
			}
			calls[i] = ToolCallFromDict(dict)
		}
		fv.Set(reflect.ValueOf(calls))
		return true
	case ToolCallFunction:
		dict, ok := value.(map[string]any)
		if !ok {
			return false
		}
		var fn ToolCallFunction
		dictToStruct(dict, reflect.ValueOf(&fn).Elem(), nil)
		fv.Set(reflect.ValueOf(fn))
		return true
	case []string:
		items, ok := value.([]any)
		if !ok {
			return false
		}
		strs := make([]string, len(items))
		for i, item := range items {
			if strs[i], ok = item.(string); !ok {
				return false
			}
		}
		fv.Set(reflect.ValueOf(strs))
		return true
	case map[string]string:
		dict, ok := value.(map[string]any)
		if !ok {
			return false
		}
		out := make(map[string]string, len(dict))
		for k, item := range dict {
			s, ok := item.(string)
			if !ok {
				return false
			}
			out[k] = s
		}
		fv.Set(reflect.ValueOf(out))
		return true
	case int64:
		n, ok := exactInt(value)
		if ok {
			fv.SetInt(n)
		}
		return ok
	case int:
		n, ok := exactInt(value)
		if ok {
			fv.SetInt(n)
		}
		return ok
	case float64:
		f, ok := value.(float64)
		if ok {
			fv.SetFloat(f)
		}
		return ok
	}
	if value == nil {
		return fv.Kind() == reflect.Interface || fv.Kind() == reflect.Map && fv.IsNil()
	}
	val := reflect.ValueOf(value)
	if val.Type() != fv.Type() && fv.Kind() != reflect.Interface {
		return false
	}
	fv.Set(val)
	return true
}

// Integer of a Python int value, bool excluded since it renders as True and False
func exactInt(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil && !strings.ContainsAny(v.String(), ".eE")
	}
	return 0, false
}
