package tuiserver

import (
	"errors"
	"fmt"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
)

var sessionRegistry = &handlerRegistry{}

func init() {
	registerSessionListHandlers()
	sessionRegistry.install()
}

// Whole number of a param, the default when missing or unparsable, a panic with an OverflowError for a value past what a 64 bit integer holds
func intParam(params map[string]any, key string, def int) int {
	raw, present := params[key]
	if !present {
		return def
	}
	n, err := anyutil.ToInt(raw)
	if errors.Is(err, anyutil.ErrRange) {
		panic(&errorutil.Error{Kind: "OverflowError", Msg: fmt.Sprintf("the %s value %s is larger than a 64 bit integer holds", key, anyutil.String(raw))})
	}
	if err != nil {
		return def
	}
	return int(n)
}
