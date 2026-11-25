package camofox

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Error raised by this package, the kind label it records under and its text
type kindError struct {
	class string
	text  string
}

func (e *kindError) Error() string { return e.text }

// Kind label the error records under
func (e *kindError) ErrorKind() string { return e.class }

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

// Value at a key of a decoded JSON object, def when absent, an error for a value that is not an object
func get(v any, key string, def any) (any, error) {
	if o, ok := v.(*jsonutil.Object); ok && o != nil {
		if val, has := o.Get(key); has {
			return val, nil
		}
		return def, nil
	}
	return nil, &kindError{"AttributeError", "expected an object, got " + anyutil.Kind(v)}
}

// isinstance check for a decoded JSON object
func isDict(v any) bool {
	o, ok := v.(*jsonutil.Object)
	return ok && o != nil
}

// JSON value of a reply body, invalid UTF-8 replaced before decoding
func decodeJSON(body []byte) (any, error) {
	return jsonutil.Decode(strings.ToValidUTF8(string(body), "\uFFFD"))
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
