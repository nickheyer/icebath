package gatewayrun

import (
	"context"
	"fmt"
	"reflect"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Runtime selection of an API adapter that maps an admission's settings onto the resolved runtime kwargs
type apiTurnRuntimeSelector interface {
	PrepareTurnRuntime(ctx context.Context, model string, runtimeKwargs map[string]any, settings any) (string, error)
}

// Model and runtime kwargs the executing API admission selects, unchanged outside one
func prepareAPIRuntime(ctx context.Context, model string, runtimeKwargs map[string]any) (string, map[string]any, error) {
	current := gateway.APIExecution(ctx)
	if current == nil {
		return model, runtimeKwargs, nil
	}
	settings, _ := current.Get("settings")
	adapterValue, _ := current.Get("adapter")
	selector, ok := adapterValue.(apiTurnRuntimeSelector)
	if !ok {
		return "", nil, &pyAttributeError{fmt.Sprintf("'%T' object has no attribute '_select_agent_runtime'", adapterValue)}
	}
	selected, err := selector.PrepareTurnRuntime(ctx, model, runtimeKwargs, settings)
	if err != nil {
		return "", nil, err
	}
	return selected, runtimeKwargs, nil
}

// Calls a callback of any function shape with the arguments converted to its parameter types
func callAnyCallback(cb any, args ...any) {
	if cb == nil {
		return
	}
	fn := reflect.ValueOf(cb)
	if fn.Kind() != reflect.Func || fn.IsNil() {
		return
	}
	ft := fn.Type()
	in := make([]reflect.Value, len(args))
	for i, arg := range args {
		var pt reflect.Type
		if ft.IsVariadic() && i >= ft.NumIn()-1 {
			pt = ft.In(ft.NumIn() - 1).Elem()
		} else {
			pt = ft.In(i)
		}
		if arg == nil {
			in[i] = reflect.Zero(pt)
			continue
		}
		v := reflect.ValueOf(arg)
		if !v.Type().AssignableTo(pt) && v.Type().ConvertibleTo(pt) {
			v = v.Convert(pt)
		}
		in[i] = v
	}
	fn.Call(in)
}

// Hands the admission's API observers the agent's reasoning, status, commentary and tool progress fenced to the running claim
func wireAPIObservers(ctx context.Context, a *agent.AIAgent, owner *turnApprovalOwner, wantInterim bool) {
	publish := func(key string, args []any, kwargs map[string]any) bool {
		accepted, err := gateway.PublishAPIObserverCallback(ctx, owner.authority, owner.sessionID, owner.generation, key, args, kwargs)
		if err != nil {
			runLog().Debug(fmt.Sprintf("API observer %s publish failed: %s", key, err))
		}
		return accepted
	}
	status, interim := a.StatusCallback(), a.InterimAssistantCallback()
	progress := a.RawToolProgressCallback()
	a.SetReasoningCallback(func(text string) { publish("reasoning_callback", []any{text}, nil) })
	a.SetStatusCallback(func(kind string, message any) {
		publish("status_callback", []any{kind, message}, nil)
		callAnyCallback(status, kind, message)
	})
	a.SetToolProgressCallback(func(eventType, name, preview string, args map[string]any, extra map[string]any) {
		if eventType == "tool.started" || eventType == "tool.completed" {
			if err := gateway.PublishAPIToolProgress(ctx, owner.authority, owner.sessionID, owner.generation,
				[]any{eventType, name, preview, args}, extra); err != nil {
				runLog().Debug(fmt.Sprintf("API tool progress publish failed: %s", err))
			}
		}
		callAnyCallback(progress, eventType, name, preview, args, extra)
	})
	if wantInterim {
		a.SetInterimAssistantCallback(func(text string, alreadyStreamed bool) {
			publish("interim_assistant_callback", []any{text}, map[string]any{"already_streamed": alreadyStreamed})
			callAnyCallback(interim, text, alreadyStreamed)
		})
	}
}
