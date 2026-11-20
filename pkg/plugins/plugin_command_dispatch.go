package plugins

import (
	"context"
	"sync/atomic"
)

type commandCall struct {
	context map[string]any
	claimed atomic.Bool
}

type commandCallKey struct{}

// Calls a slash command handler once with the raw args plus the session context keywords it declares
func InvokePluginCommand(ctx context.Context, handler PluginCommandHandler, rawArgs string, context map[string]any) (any, error) {
	if context == nil {
		context = map[string]any{}
	}
	return handler(contextWithCommandCall(ctx, &commandCall{context: context}), rawArgs)
}

func contextWithCommandCall(ctx context.Context, call *commandCall) context.Context {
	return context.WithValue(ctx, commandCallKey{}, call)
}

// Session context keywords the dispatching surface passed, read by an in process handler taking every keyword
func PluginCommandContext(ctx context.Context) map[string]any {
	call, _ := ctx.Value(commandCallKey{}).(*commandCall)
	if call == nil || call.claimed.Load() {
		return map[string]any{}
	}
	out := make(map[string]any, len(call.context))
	for k, v := range call.context {
		out[k] = v
	}
	return out
}

// Takes the pending command context once so a handler nested inside gets only what it is passed
func claimCommandContext(ctx context.Context) (context.Context, map[string]any) {
	call, _ := ctx.Value(commandCallKey{}).(*commandCall)
	if call == nil || !call.claimed.CompareAndSwap(false, true) {
		return ctx, map[string]any{}
	}
	return contextWithCommandCall(ctx, nil), call.context
}

// Positional and keyword arguments a peer signature accepts, the single raw argument for opaque signatures
func commandCallArgs(sig []wireParam, rawArgs string, context map[string]any) ([]any, map[string]any) {
	args := []any{rawArgs}
	if sig == nil {
		return args, nil
	}
	params := sig
	kwargs := make(map[string]any, len(context))
	for k, v := range context {
		kwargs[k] = v
	}
	if len(params) > 0 && params[0].Kind != paramKeywordOnly {
		if params[0].Kind == paramPositionalOrKeyword {
			delete(kwargs, params[0].Name)
		}
		params = params[1:]
	}
	for _, p := range params {
		if p.Kind == paramVarKeyword {
			return args, kwargs
		}
	}
	accepted := map[string]any{}
	for _, p := range params {
		if p.Kind == paramPositionalOrKeyword || p.Kind == paramKeywordOnly {
			if v, ok := kwargs[p.Name]; ok {
				accepted[p.Name] = v
			}
		}
	}
	return args, accepted
}

// Slash command handler over a hosted callable passing the context keywords its signature declares
func hostedCommandHandler(handler *HostedCallable) PluginCommandHandler {
	return func(ctx context.Context, raw string) (any, error) {
		ctx, context := claimCommandContext(ctx)
		args, kwargs := commandCallArgs(handler.Sig, raw, context)
		return handler.Call(ctx, args, kwargs)
	}
}
