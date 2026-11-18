package pyhttpx

import (
	"encoding"
	"encoding/json"
	"reflect"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Body bytes the Python SDKs and httpx write for a json request, dict order kept, compact, non ASCII raw and NaN refused
func JSONBody(v any) ([]byte, error) {
	value, err := wireValue(v)
	if err != nil {
		return nil, err
	}
	text, err := pyjson.Dumps(value, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"), pyjson.AllowNaN(false),
		pyjson.Default(wireDefault))
	if err != nil {
		return nil, err
	}
	return []byte(text), nil
}

// Value with every raw JSON fragment and Go marshaler replaced by the ordered value json.loads reads from its text
func wireValue(v any) (any, error) {
	switch t := v.(type) {
	case nil, string, bool, float64, int, int64, json.Number:
		return v, nil
	case json.RawMessage:
		if len(t) == 0 {
			return nil, nil
		}
		return pyjson.LoadsBytes(t)
	case *pyjson.Object:
		if t == nil {
			return v, nil
		}
		var out *pyjson.Object
		for i, k := range t.Keys {
			item, err := wireValue(t.Vals[k])
			if err != nil {
				return nil, err
			}
			if out == nil && !sameValue(item, t.Vals[k]) {
				out = &pyjson.Object{Keys: append([]string(nil), t.Keys...), Vals: make(map[string]any, len(t.Vals))}
				for _, prev := range t.Keys[:i] {
					out.Vals[prev] = t.Vals[prev]
				}
			}
			if out != nil {
				out.Vals[k] = item
			}
		}
		if out == nil {
			return t, nil
		}
		return out, nil
	case map[string]any:
		var out map[string]any
		for k, val := range t {
			item, err := wireValue(val)
			if err != nil {
				return nil, err
			}
			if out == nil && !sameValue(item, val) {
				out = make(map[string]any, len(t))
				for pk, pv := range t {
					out[pk] = pv
				}
			}
			if out != nil {
				out[k] = item
			}
		}
		if out == nil {
			return t, nil
		}
		return out, nil
	case []any:
		var out []any
		for i, val := range t {
			item, err := wireValue(val)
			if err != nil {
				return nil, err
			}
			if out == nil && !sameValue(item, val) {
				out = append([]any(nil), t...)
			}
			if out != nil {
				out[i] = item
			}
		}
		if out == nil {
			return t, nil
		}
		return out, nil
	case []map[string]any:
		out := make([]any, len(t))
		for i, val := range t {
			item, err := wireValue(val)
			if err != nil {
				return nil, err
			}
			out[i] = item
		}
		return out, nil
	}
	return v, nil
}

// Identity of a value before and after the walk, containers by their backing storage
func sameValue(a, b any) bool {
	ra, rb := reflect.ValueOf(a), reflect.ValueOf(b)
	if ra.Kind() != rb.Kind() || ra.Type() != rb.Type() {
		return false
	}
	switch ra.Kind() {
	case reflect.Map, reflect.Pointer:
		return ra.Pointer() == rb.Pointer()
	case reflect.Slice:
		return ra.Len() == rb.Len() && (ra.Len() == 0 || ra.Pointer() == rb.Pointer())
	}
	return true
}

// Ordered value of a Go marshaler or struct the SDK encoder would have dumped from its model fields
func wireDefault(v any) (any, error) {
	var raw []byte
	var err error
	switch m := v.(type) {
	case json.Marshaler:
		raw, err = m.MarshalJSON()
	case encoding.TextMarshaler:
		var text []byte
		if text, err = m.MarshalText(); err == nil {
			return string(text), nil
		}
	default:
		raw, err = json.Marshal(v)
	}
	if err != nil {
		return nil, &pyjson.TypeError{Msg: "Object of type " + reflect.TypeOf(v).String() + " is not JSON serializable"}
	}
	return pyjson.LoadsBytes(raw)
}
