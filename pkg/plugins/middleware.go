package plugins

import (
	"context"
	"fmt"
	"reflect"

	"github.com/nickheyer/icebath/pkg/config"
)

const (
	ObserverSchemaVersion   = "hermes.observer.v1"
	MiddlewareSchemaVersion = "hermes.middleware.v1"
	ToolRequestMiddleware   = "tool_request"
	ToolExecutionMiddleware = "tool_execution"
	LLMRequestMiddleware    = "llm_request"
	LLMExecutionMiddleware  = "llm_execution"
)

var validMiddleware = map[string]bool{
	ToolRequestMiddleware: true, ToolExecutionMiddleware: true, LLMRequestMiddleware: true, LLMExecutionMiddleware: true,
}

// Middleware callback receiving keyword payload fields, returning a rewrite or the wrapped result
type MiddlewareFunc func(kwargs map[string]any) (any, error)

// Downstream continuation an execution middleware calls at most once, nil payload reusing the current one
type NextCall func(payload any) (any, error)

// One registered middleware callback with its identity and display name
type middlewareEntry struct {
	fn   MiddlewareFunc
	name string
}

// Result of running request middleware over a payload
type RequestMiddlewareResult struct {
	Payload         any
	OriginalPayload any
	Changed         bool
	Trace           []map[string]any
}

// Observer payload stamped with the telemetry schema version unless already set
func ObserverPayload(kwargs map[string]any) map[string]any {
	if _, ok := kwargs["telemetry_schema_version"]; !ok {
		kwargs["telemetry_schema_version"] = ObserverSchemaVersion
	}
	return kwargs
}

// Middleware payload stamped with the telemetry and middleware schema versions unless already set
func MiddlewarePayload(kwargs map[string]any) map[string]any {
	ObserverPayload(kwargs)
	if _, ok := kwargs["middleware_schema_version"]; !ok {
		kwargs["middleware_schema_version"] = MiddlewareSchemaVersion
	}
	return kwargs
}

// Deep copy of nested maps and lists, other members shared by reference
func safeCopy(payload any) any {
	switch v := payload.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = safeCopy(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = safeCopy(item)
		}
		return out
	}
	return payload
}

// Shallow copy of keyword fields
func copyKwargs(kwargs map[string]any) map[string]any {
	out := make(map[string]any, len(kwargs)+4)
	for k, v := range kwargs {
		out[k] = v
	}
	return out
}

// Feeds the payload through every middleware of a kind, each mapping rewrite under the key replacing it
func (m *Manager) applyRequestChain(kind, payloadKey string, trace []map[string]any, original any, kwargs map[string]any) RequestMiddlewareResult {
	current := kwargs[payloadKey]
	for _, result := range m.InvokeMiddleware(kind, payloadKey, MiddlewarePayload(kwargs)) {
		rewrite, ok := result.(map[string]any)
		if !ok {
			continue
		}
		next, ok := rewrite[payloadKey].(map[string]any)
		if !ok {
			continue
		}
		current = safeCopy(next)
		entry := map[string]any{}
		for _, key := range []string{"source", "reason", "name"} {
			if s, ok := rewrite[key].(string); ok && s != "" {
				entry[key] = s
			}
		}
		if len(entry) == 0 {
			entry = map[string]any{"source": "plugin"}
		}
		trace = append(trace, entry)
	}
	return RequestMiddlewareResult{Payload: current, OriginalPayload: original, Changed: len(trace) > 0, Trace: trace}
}

// Runs LLM request middleware where a returned request mapping replaces the provider kwargs
func (m *Manager) ApplyLLMRequestMiddleware(request map[string]any, context map[string]any) RequestMiddlewareResult {
	if !m.HasMiddleware(LLMRequestMiddleware) {
		return RequestMiddlewareResult{Payload: request, OriginalPayload: request}
	}
	original := safeCopy(request)
	kwargs := copyKwargs(context)
	kwargs["request"] = safeCopy(original)
	kwargs["original_request"] = original
	return m.applyRequestChain(LLMRequestMiddleware, "request", nil, original, kwargs)
}

// Runs relay intercepts then tool request middleware where a returned args mapping replaces the tool arguments
func (m *Manager) ApplyToolRequestMiddleware(goctx context.Context, toolName string, args, originalArgs map[string]any, context map[string]any) (RequestMiddlewareResult, error) {
	if originalArgs == nil {
		originalArgs = args
	}
	original := safeCopy(originalArgs)
	current := safeCopy(args)
	var trace []map[string]any
	ctx := copyKwargs(context)
	sessionID := ""
	if jsonTruthy(ctx["session_id"]) {
		sessionID = config.PyStr(ctx["session_id"])
	}
	skipRelay := config.PyTruthy(ctx["skip_relay"])
	delete(ctx, "skip_relay")
	if sessionID != "" && !skipRelay {
		currentArgs, _ := current.(map[string]any)
		relayArgs, err := relayApplyToolRequestIntercepts(goctx, sessionID, toolName, currentArgs)
		if err != nil {
			return RequestMiddlewareResult{}, err
		}
		if !reflect.DeepEqual(relayArgs, currentArgs) {
			current = safeCopy(relayArgs)
			trace = append(trace, map[string]any{"source": "nemo_relay"})
		}
	}
	if !m.HasMiddleware(ToolRequestMiddleware) {
		payload := any(args)
		if len(trace) > 0 {
			payload = current
		}
		return RequestMiddlewareResult{Payload: payload, OriginalPayload: args, Changed: len(trace) > 0, Trace: trace}, nil
	}
	ctx["tool_name"] = toolName
	ctx["args"] = current
	ctx["original_args"] = original
	return m.applyRequestChain(ToolRequestMiddleware, "args", trace, original, ctx), nil
}

// Runs provider execution through the registered LLM execution middleware
func (m *Manager) RunLLMExecutionMiddleware(request map[string]any, next NextCall, context map[string]any) (any, error) {
	kwargs := copyKwargs(context)
	original, ok := kwargs["original_request"]
	delete(kwargs, "original_request")
	if !ok {
		original = request
	}
	kwargs["request"] = request
	kwargs["original_request"] = original
	return m.runExecutionChain(LLMExecutionMiddleware, next, kwargs)
}

// Runs tool execution through the registered tool execution middleware
func (m *Manager) RunToolExecutionMiddleware(toolName string, args map[string]any, next NextCall, context map[string]any) (any, error) {
	kwargs := copyKwargs(context)
	original, ok := kwargs["original_args"]
	delete(kwargs, "original_args")
	if !ok {
		original = args
	}
	kwargs["tool_name"] = toolName
	kwargs["args"] = args
	kwargs["original_args"] = original
	return m.runExecutionChain(ToolExecutionMiddleware, next, kwargs)
}

// Marks an error raised below a middleware frame so that frame does not swallow it
type downstreamExecutionError struct{ original error }

func (e *downstreamExecutionError) Error() string { return e.original.Error() }

// Calls each execution middleware around the terminal call, a failing frame skipped unless downstream ran
func (m *Manager) runExecutionChain(kind string, terminal NextCall, kwargs map[string]any) (any, error) {
	payloadKey := "args"
	if _, ok := kwargs["request"]; ok {
		payloadKey = "request"
	}
	m.mu.RLock()
	callbacks := append([]*middlewareEntry(nil), m.middleware[kind]...)
	m.mu.RUnlock()
	if len(callbacks) == 0 {
		return terminal(kwargs[payloadKey])
	}
	var callAt func(index int, payload any) (any, error)
	callAt = func(index int, payload any) (any, error) {
		if index >= len(callbacks) {
			return terminal(payload)
		}
		cb := callbacks[index]
		nextCalled, nextSucceeded := false, false
		var nextResult any
		next := NextCall(func(nextPayload any) (any, error) {
			if nextCalled {
				return nil, fmt.Errorf("Middleware '%s' callback %s called next_call() more than once; downstream execution is single-use", kind, cb.name)
			}
			nextCalled = true
			if nextPayload == nil {
				nextPayload = payload
			}
			result, err := callAt(index+1, nextPayload)
			if err != nil {
				return nil, &downstreamExecutionError{original: err}
			}
			nextResult, nextSucceeded = result, true
			return result, nil
		})
		callKwargs := MiddlewarePayload(copyKwargs(kwargs))
		callKwargs[payloadKey] = payload
		callKwargs["next_call"] = next
		var result any
		err := callGuarded(func() error {
			var cbErr error
			result, cbErr = cb.fn(callKwargs)
			return cbErr
		})
		if err == nil {
			return result, nil
		}
		if down, ok := err.(*downstreamExecutionError); ok {
			return nil, down.original
		}
		m.reportHookFailure(kind, cb.name, callKwargs, err, "Middleware")
		if nextSucceeded {
			return nextResult, nil
		}
		if nextCalled {
			return nil, err
		}
		return callAt(index+1, payload)
	}
	return callAt(0, kwargs[payloadKey])
}

// Reports whether any middleware of a kind is registered outside a safe worker
func (m *Manager) HasMiddleware(kind string) bool {
	if config.SafeWorkerEnabled() {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.middleware[kind]) > 0
}

// Calls every middleware of a kind in isolation, request kinds composing rewrites under the payload key
func (m *Manager) InvokeMiddleware(kind, payloadKey string, kwargs map[string]any) []any {
	if config.SafeWorkerEnabled() {
		return []any{}
	}
	m.mu.RLock()
	callbacks := append([]*middlewareEntry(nil), m.middleware[kind]...)
	m.mu.RUnlock()
	results := []any{}
	for _, cb := range callbacks {
		callKwargs := kwargs
		if payloadKey != "" {
			originalKey := "original_" + payloadKey
			callKwargs = copyKwargs(kwargs)
			callKwargs[payloadKey] = safeCopy(kwargs[payloadKey])
			callKwargs[originalKey] = safeCopy(kwargs[originalKey])
		}
		var ret any
		err := callGuarded(func() error {
			var cbErr error
			ret, cbErr = cb.fn(callKwargs)
			return cbErr
		})
		if isUserInterrupt(context.Background(), err) {
			raiseUserInterrupt(err)
		}
		if err != nil {
			m.reportHookFailure(kind, cb.name, kwargs, err, "Middleware")
			continue
		}
		if ret != nil {
			results = append(results, ret)
			if rewrite, ok := ret.(map[string]any); payloadKey != "" && ok {
				if next, ok := rewrite[payloadKey].(map[string]any); ok {
					kwargs[payloadKey] = next
				}
			}
		}
	}
	return results
}
