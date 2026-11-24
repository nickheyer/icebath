package provider

import (
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Non string tool call arguments become their compact JSON text, keys in wire order
func spliceNonStringToolArguments(data []byte, payload map[string]any) {
	var ordered any
	choices, _ := payload["choices"].([]any)
	for ci, c := range choices {
		choice, _ := c.(map[string]any)
		msg, _ := choice["message"].(map[string]any)
		calls, _ := msg["tool_calls"].([]any)
		for ti, t := range calls {
			call, _ := t.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			args, has := fn["arguments"]
			if _, isStr := args.(string); !has || args == nil || isStr {
				continue
			}
			if ordered == nil {
				if ordered, _ = jsonutil.Decode(data); ordered == nil {
					return
				}
			}
			text, err := jsonutil.Append(nil, orderedAt(ordered, "choices", ci, "message", "tool_calls", ti, "function", "arguments"))
			if err == nil {
				fn["arguments"] = string(text)
			}
		}
	}
}

// Value at a path of object keys and list indexes in an ordered decode, nil when absent
func orderedAt(v any, path ...any) any {
	for _, step := range path {
		switch key := step.(type) {
		case string:
			obj, ok := v.(*jsonutil.Object)
			if !ok {
				return nil
			}
			v, _ = obj.Get(key)
		case int:
			list, ok := v.([]any)
			if !ok || key >= len(list) {
				return nil
			}
			v = list[key]
		}
	}
	return v
}
