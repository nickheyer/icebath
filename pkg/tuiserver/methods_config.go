package tuiserver

import (
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// int of a param value or its default when falsy, raising like int() on a bad value
func intParamOr(v any, def int) int {
	if !anyutil.IsTrue(v) {
		return def
	}
	n, err := anyutil.ToInt(v)
	if err != nil {
		panic(err)
	}
	return int(n)
}
