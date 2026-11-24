package gateway

import (
	"unicode/utf8"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Viewer payload ceiling of one tool event field, the full input and output stay in canonical history
const maxToolEventChars = 64 * 1024

// ASCII JSON text of a value with unknown objects rendered by their str text
func toolEventJSON(value any) string {
	text, err := jsonutil.Encode(value, jsonutil.ASCII(), jsonutil.Convert(func(v any) (any, error) { return anyutil.String(v), nil }))
	if err != nil {
		return anyutil.String(value)
	}
	return text
}

// Tool arguments within the viewer ceiling, large ones cut to bounded scalar texts per key
func boundedArgs(args any) any {
	if len(toolEventJSON(args)) <= maxToolEventChars {
		return args
	}
	result := jsonutil.NewObject()
	remaining := maxToolEventChars / 2
	object, _ := jsonutil.AsObject(args)
	if object == nil {
		return result
	}
	for _, key := range object.Keys() {
		if remaining <= 0 || result.Len() >= 64 {
			break
		}
		value := object.Value(key)
		name := textutil.First(key, 256)
		text, isText := value.(string)
		if !isText {
			text = toolEventJSON(value)
		}
		text = textutil.First(text, min(4096, remaining))
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
	if utf8.RuneCountInString(text) <= maxToolEventChars {
		return text
	}
	key := "output"
	if isError {
		key = "error"
	}
	return toolEventJSON(jsonutil.ObjectOf(key, textutil.First(text, maxToolEventChars/2), "truncated", true))
}

// Tool arguments within the viewer ceiling, for the managed worker child
func BoundedToolArgs(args any) any { return boundedArgs(args) }

// Tool result text within the viewer ceiling, for the managed worker child
func BoundedToolResult(result any, isError bool) string { return boundedResult(result, isError) }
