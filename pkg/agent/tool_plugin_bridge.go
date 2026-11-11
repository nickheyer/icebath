package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Puts the plugin hooks and middleware on the process wide tool dispatcher every agent executes through
func init() { installToolPluginBridge(tools.DefaultDispatcher) }

// Appends the pre, post and transform hooks and both middleware chains of the plugin layer to a dispatcher
func installToolPluginBridge(d *tools.Dispatcher) {
	d.PreToolCall = append(d.PreToolCall, pluginPreToolCall)
	d.PostToolCall = append(d.PostToolCall, pluginPostToolCall)
	d.TransformToolResult = append(d.TransformToolResult, pluginTransformToolResult)
	d.ToolRequestMiddleware = append(d.ToolRequestMiddleware, pluginToolRequestMiddleware)
	d.ToolExecutionMiddleware = append(d.ToolExecutionMiddleware, pluginToolExecutionMiddleware)
}

// Identity keywords every tool hook and middleware call carries, absent ids as empty strings
func toolHookIDKwargs(ids tools.ToolHookIDs) map[string]any {
	return map[string]any{"task_id": ids.TaskID, "session_id": ids.SessionID, "tool_call_id": ids.ToolCallID,
		"turn_id": ids.TurnID, "api_request_id": ids.APIRequestID}
}

// One pass over the pre_tool_call hooks as a single directive carrying the merged modify args
func pluginPreToolCall(ctx context.Context, _ *tools.Env, name string, args map[string]any, ids tools.ToolHookIDs, trace []map[string]any) tools.PreToolDirective {
	details, err := plugins.PreToolCallDirectiveDetails(ctx, name, args, plugins.PreToolCallIDs{TaskID: ids.TaskID, SessionID: ids.SessionID,
		ToolCallID: ids.ToolCallID, TurnID: ids.TurnID, APIRequestID: ids.APIRequestID, MiddlewareTrace: trace})
	if err != nil {
		slog.Default().Debug("pre_tool_call hook error: " + err.Error())
		return tools.PreToolDirective{}
	}
	action := details.Action
	if action == "" && details.ModifiedArgs != nil {
		action = "modify"
	}
	return tools.PreToolDirective{Action: action, Message: details.Message, RuleKey: details.RuleKey, Args: details.ModifiedArgs}
}

// Error message keyword of a finished call, None unless the call raised and an empty text for an empty exception
func toolEventErrorMessage(ev tools.PostToolCallEvent) any {
	if ev.ErrorMessage != "" {
		return ev.ErrorMessage
	}
	switch ev.ErrorType {
	case "", "tool_error", "plugin_block", "edit_approval_denied", "edit_approval_error":
		return nil
	}
	return ""
}

// Keywords of the post_tool_call and transform_tool_result hooks for one finished call
func toolEventKwargs(ev tools.PostToolCallEvent) map[string]any {
	kwargs := toolHookIDKwargs(ev.IDs)
	args := ev.Args
	if args == nil {
		args = map[string]any{}
	}
	kwargs["tool_name"], kwargs["args"], kwargs["result"] = ev.ToolName, args, ev.Result
	kwargs["duration_ms"], kwargs["status"] = ev.DurationMS, ev.Status
	kwargs["error_type"], kwargs["error_message"] = nil, toolEventErrorMessage(ev)
	if ev.ErrorType != "" {
		kwargs["error_type"] = ev.ErrorType
	}
	return kwargs
}

// Fires the post_tool_call observers and plugin hooks when anything consumes them
func pluginPostToolCall(ctx context.Context, _ *tools.Env, ev tools.PostToolCallEvent) {
	if !lifecycleHasHook(ctx, "post_tool_call") {
		return
	}
	kwargs := toolEventKwargs(ev)
	trace := make([]any, 0, len(ev.MiddlewareTrace))
	for _, entry := range ev.MiddlewareTrace {
		trace = append(trace, entry)
	}
	kwargs["middleware_trace"] = trace
	lifecycleInvokeHook(ctx, "post_tool_call", kwargs)
}

// First string a transform_tool_result hook returns, false when none replaces the result
func pluginTransformToolResult(ctx context.Context, _ *tools.Env, ev tools.PostToolCallEvent) (string, bool) {
	if !lifecycleHasHook(ctx, "transform_tool_result") {
		return "", false
	}
	for _, result := range lifecycleInvokeHook(ctx, "transform_tool_result", toolEventKwargs(ev)) {
		if text, ok := result.(string); ok {
			return text, true
		}
	}
	return "", false
}

// Runs the plugin tool_request chain after the dispatcher's own relay pass, its trace entries kept as given
func pluginToolRequestMiddleware(ctx context.Context, _ *tools.Env, name string, args, originalArgs map[string]any, ids tools.ToolHookIDs) tools.RequestMiddlewareDirective {
	kwargs := toolHookIDKwargs(ids)
	kwargs["skip_relay"] = true
	result, err := plugins.ApplyToolRequestMiddleware(ctx, name, args, originalArgs, kwargs)
	if err != nil {
		slog.Default().Debug("tool_request middleware error: " + err.Error())
		return tools.RequestMiddlewareDirective{}
	}
	payload, isMap := result.Payload.(map[string]any)
	if !result.Changed || !isMap {
		return tools.RequestMiddlewareDirective{}
	}
	return tools.RequestMiddlewareDirective{Args: payload, Trace: result.Trace}
}

// Text of a middleware result, a string as given and any other value as JSON
func toolMiddlewareResultText(result any) string {
	if text, ok := result.(string); ok {
		return text
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(result); err != nil {
		return config.PyStr(result)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Runs the downstream execution through the plugin tool_execution chain
func pluginToolExecutionMiddleware(ctx context.Context, _ *tools.Env, name string, args, originalArgs map[string]any, ids tools.ToolHookIDs, next func(map[string]any) (string, error)) (string, error) {
	kwargs := toolHookIDKwargs(ids)
	if originalArgs != nil {
		kwargs["original_args"] = originalArgs
	}
	result, err := plugins.RunToolExecutionMiddleware(ctx, name, args, func(payload any) (any, error) {
		nextArgs, _ := payload.(map[string]any)
		return next(nextArgs)
	}, kwargs)
	if err != nil {
		return "", err
	}
	return toolMiddlewareResultText(result), nil
}
