package tuiserver

import (
	"context"
	"io"
)

// Setup side effects that take the parameters of the walk, each printing to out and reporting success
var setupParamActions = map[string]func(ctx context.Context, h string, params map[string]string, out io.Writer) bool{}

// Action of a name bound to the text parameters of the request, false for an unknown name
func setupActionFor(name string, params map[string]string) (func(ctx context.Context, h string, out io.Writer) bool, bool) {
	action, known := setupParamActions[name]
	if !known {
		return nil, false
	}
	return func(ctx context.Context, h string, out io.Writer) bool { return action(ctx, h, params, out) }, true
}
