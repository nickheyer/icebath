package pyjson

import "math/big"

// Decoded JSON with every Object turned into a plain map and every map and list copied, scalars unchanged
func Plain(v any) any {
	switch t := v.(type) {
	case *Object:
		if t == nil {
			return nil
		}
		out := make(map[string]any, len(t.Vals))
		for k, val := range t.Vals {
			out[k] = Plain(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = Plain(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = Plain(val)
		}
		return out
	}
	return v
}

// Python decoded value in the DecodeWTF8 shapes, dicts as plain maps and every int or float as float64
func WTF8Shape(v any) any {
	switch t := v.(type) {
	case *Object:
		if t == nil {
			return nil
		}
		out := make(map[string]any, len(t.Vals))
		for k, val := range t.Vals {
			out[k] = WTF8Shape(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = WTF8Shape(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = WTF8Shape(val)
		}
		return out
	case int64:
		return float64(t)
	case *big.Int:
		if t == nil {
			return nil
		}
		f, _ := new(big.Float).SetInt(t).Float64()
		return f
	}
	return v
}
