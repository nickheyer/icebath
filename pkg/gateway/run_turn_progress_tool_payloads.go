package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Plain mapping of tool arguments, empty for anything that is not a dict
func toolArgsMap(args any) map[string]any {
	switch typed := args.(type) {
	case *pyjson.Object:
		if typed == nil {
			return map[string]any{}
		}
		plain, _ := pyjson.Plain(typed).(map[string]any)
		return plain
	case map[string]any:
		return typed
	}
	return map[string]any{}
}

// Text of a call id or tool name, the fallback when it is falsy
func toolText(value any, fallback string) string {
	if !pyrepr.Truthy(value) {
		return fallback
	}
	return pyrepr.Str(value)
}

// Tool start contract every client reads plus the call id, name and bounded args the gateway consumers use
func toolLifecyclePayload(callID, toolName, args any) *pyjson.Object {
	name := toolText(toolName, "tool")
	if _, isObject := pyjson.AsObject(args); !isObject {
		args = pyjson.NewObject()
	}
	limit := 80
	plain := toolArgsMap(args)
	payload := pyjson.ObjectOf("tool_id", toolText(callID, ""), "name", name,
		"context", tools.BuildToolPreview(nil, name, plain, &limit),
		"tool_call_id", toolText(callID, ""), "tool_name", name, "args", boundedArgs(args))
	var labels []any
	for _, label := range tools.ToolLabelsForCall(nil, name, plain) {
		entry := pyjson.NewObject()
		for key, value := range label.AsPayload() {
			entry.Set(key, value)
		}
		labels = append(labels, entry)
	}
	if len(labels) > 0 {
		payload.Set("labels", labels)
	}
	return payload
}

// Tool complete event as every viewer reads it, the start contract plus the verdict, result and verbose text
func toolCompletePayload(ctx context.Context, callID, toolName, args, result any, isError *bool, verbose bool) *pyjson.Object {
	failed := false
	if isError == nil {
		failed, _ = tools.DetectToolFailure(ctx, pyrepr.Str(toolName), result)
	} else {
		failed = *isError
	}
	payload := toolLifecyclePayload(callID, toolName, args)
	payload.Set("is_error", failed)
	payload.Set("result", boundedResult(result, failed))
	if verbose {
		payload.Set("result_text", tuigateway.ToolResultText(result))
	}
	return payload
}

// Exact key set of a frame
func frameKeysAre(frame *pyjson.Object, keys ...string) bool {
	if frame == nil || frame.Len() != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := frame.Get(key); !ok {
			return false
		}
	}
	return true
}

// Publishes a managed worker's tool frame as an in process turn publishes its own, false for any other frame
func publishWorkerToolEvent(ctx context.Context, a *SessionAuthority, sessionID string, generation int64, frame *pyjson.Object) (bool, error) {
	kind, _ := frame.Get("type")
	callID, _ := frame.Get("tool_call_id")
	name, _ := frame.Get("name")
	args, _ := frame.Get("args")
	result, _ := frame.Get("result")
	flag, _ := frame.Get("is_error")
	_, callText := callID.(string)
	_, nameText := name.(string)
	_, argsDict := args.(*pyjson.Object)
	switch kind {
	case "tool.start":
		if !frameKeysAre(frame, "type", "tool_call_id", "name", "args") || !callText || !nameText || !argsDict {
			return false, nil
		}
	case "tool.complete":
		_, resultText := result.(string)
		_, flagBool := flag.(bool)
		if !frameKeysAre(frame, "type", "tool_call_id", "name", "args", "result", "is_error") || !callText || !nameText || !argsDict || !resultText || !flagBool {
			return false, nil
		}
	default:
		return false, nil
	}
	var payload *pyjson.Object
	if kind == "tool.start" {
		payload = toolLifecyclePayload(callID, name, args)
	} else {
		live, ok := a.live(sessionID)
		if !ok {
			return false, &pyKeyError{sessionID}
		}
		isError := flag.(bool)
		payload = toolCompletePayload(ctx, callID, name, args, result, &isError, live.ToolProgressMode == "verbose")
	}
	if _, err := a.PublishExecution(sessionID, generation, kind.(string), payload); err != nil {
		return false, err
	}
	if err := PublishAPIToolEvent(ctx, a, sessionID, generation, kind.(string), callID.(string), name.(string), args, result); err != nil {
		return false, err
	}
	if kind == "tool.start" {
		var preview any
		if text := tools.BuildToolPreview(nil, name.(string), toolArgsMap(args), nil); text != "" {
			preview = text
		}
		return true, PublishAPIToolProgress(ctx, a, sessionID, generation, []any{"tool.started", name, preview, args}, nil)
	}
	return true, PublishAPIToolProgress(ctx, a, sessionID, generation, []any{"tool.completed", name, nil, nil},
		map[string]any{"is_error": flag, "result": result})
}
