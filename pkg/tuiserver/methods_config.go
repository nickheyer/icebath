package tuiserver

import (
	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

// int of a param value or its default when falsy, raising like int() on a bad value
func intParamOr(v any, def int) int {
	if !truthy(v) {
		return def
	}
	n, err := pyint.Int64(v)
	if err != nil {
		panic(err)
	}
	return int(n)
}
