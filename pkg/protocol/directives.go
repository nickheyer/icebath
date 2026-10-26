package protocol

import "context"

// What a client asks of the endpoint beyond the turn itself
type Directives struct {
	// Keep nothing of the exchange
	Ghost bool
	// Add no system prompt and no tools of the endpoint's own
	Bare bool
}

type directivesKey struct{}

// Context whose requests ask the endpoint for what the function reports each time one is sent
func WithDirectives(ctx context.Context, fn func() Directives) context.Context {
	return context.WithValue(ctx, directivesKey{}, fn)
}

// Directives the context asks for right now, none when it carries no source
func DirectivesFrom(ctx context.Context) Directives {
	if fn, ok := ctx.Value(directivesKey{}).(func() Directives); ok && fn != nil {
		return fn()
	}
	return Directives{}
}
