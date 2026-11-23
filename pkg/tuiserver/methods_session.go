package tuiserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var sessionRegistry = &handlerRegistry{}

func init() {
	registerSessionListHandlers()
	sessionRegistry.install()
}

// Handler body of a session scoped method, given the resolved live session
type sessionHandler func(ctx context.Context, rid any, params map[string]any, session *tuihost.Session) map[string]any

// Python int of a param, the default when missing or unparsable and the overflow of an infinite value raised
func intParam(params map[string]any, key string, def int) int {
	raw, present := params[key]
	if !present {
		return def
	}
	n, err := pyint.Of(raw)
	if err != nil {
		var pyErr *pyint.Error
		if errors.As(err, &pyErr) && pyErr.Kind == "OverflowError" {
			panic(err)
		}
		return def
	}
	if !n.IsInt64() {
		panic(&pyint.Error{Kind: "OverflowError", Msg: fmt.Sprintf("the %s value %s is larger than a 64 bit integer holds", key, n.String())})
	}
	return int(n.Int64())
}
