package gateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Viewer payload ceiling of one tool event field, the full input and output stay in canonical history
const maxToolEventChars = 64 * 1024

// ASCII JSON text of a value with unknown objects rendered by their str text
func toolEventJSON(value any) string {
	text, err := pyjson.Dumps(value, pyjson.EnsureASCII(true), pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if err != nil {
		return pyrepr.Str(value)
	}
	return text
}

// Tool arguments within the viewer ceiling, large ones cut to bounded scalar texts per key
func boundedArgs(args any) any {
	if len(toolEventJSON(args)) <= maxToolEventChars {
		return args
	}
	result := pyjson.NewObject()
	remaining := maxToolEventChars / 2
	object, _ := pyjson.AsObject(args)
	if object == nil {
		return result
	}
	for _, key := range object.Keys {
		if remaining <= 0 || result.Len() >= 64 {
			break
		}
		value := object.Vals[key]
		name := pystr.Slice(key, 0, 256)
		text, isText := value.(string)
		if !isText {
			text = toolEventJSON(value)
		}
		text = pystr.Slice(text, 0, min(4096, remaining))
		result.Set(name, text)
		remaining -= len(toolEventJSON([]any{name, text}))
	}
	return result
}

// Tool result text within the viewer ceiling, a cut one kept as valid JSON naming error or output
func boundedResult(result any, isError bool) string {
	text, isText := result.(string)
	if !isText {
		text = toolEventJSON(result)
	}
	if pystr.Len(text) <= maxToolEventChars {
		return text
	}
	key := "output"
	if isError {
		key = "error"
	}
	return toolEventJSON(pyjson.ObjectOf(key, pystr.Slice(text, 0, maxToolEventChars/2), "truncated", true))
}

// Tool arguments within the viewer ceiling, for the managed worker child
func BoundedToolArgs(args any) any { return boundedArgs(args) }

// Tool result text within the viewer ceiling, for the managed worker child
func BoundedToolResult(result any, isError bool) string { return boundedResult(result, isError) }
