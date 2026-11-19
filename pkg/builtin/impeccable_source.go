package builtin

import (
	"strconv"
	"strings"
)

// ECMAScript String() of a frontmatter value
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "undefined"
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			if item != nil {
				parts[i] = jsString(item)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}
