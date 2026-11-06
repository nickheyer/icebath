package pyjson

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
