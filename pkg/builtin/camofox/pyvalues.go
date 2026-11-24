package camofox

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Python exception raised by this package, its class name and str text
type pyError struct {
	class string
	text  string
}

func (e *pyError) Error() string { return e.text }

// Python exception class name the error stands for
func (e *pyError) PyTypeName() string { return e.class }

// Runs fn as a try block whose except Exception clause receives any error or panic fn raised
func try(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
				return
			}
			err = fmt.Errorf("%v", r)
		}
	}()
	return fn()
}

// dict.get of a decoded JSON value, the AttributeError for a value that is not an object
func get(v any, key string, def any) (any, error) {
	if o, ok := v.(*pyjson.Object); ok && o != nil {
		if val, has := o.Get(key); has {
			return val, nil
		}
		return def, nil
	}
	return nil, &pyError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))}
}

// isinstance check for a decoded JSON object
func isDict(v any) bool {
	o, ok := v.(*pyjson.Object)
	return ok && o != nil
}

// The str(value or "") idiom, empty for a falsy value
func pyOrStr(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Response.json of a body, decoded as requests decodes text with replacement before json.loads
func decodeJSON(body []byte) (any, error) {
	return pyjson.Loads(strings.ToValidUTF8(string(body), "\uFFFD"))
}

// json.dumps of a tool result with ensure_ascii off
func dumps(v any) string { return tools.JSON(v) }

// tool_error with success false, the error text bounded first
func fail(message string) string {
	return tools.JSON(tools.Dict("error", tools.BoundErrorText(message), "success", false))
}

// Task id a call keys its session by, default when empty
func taskOrDefault(taskID string) string {
	if taskID == "" {
		return "default"
	}
	return taskID
}

// Task id as a log line prints it, None when the call carried none
func logTask(taskID string) string {
	if taskID == "" {
		return "None"
	}
	return taskID
}
