package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Tool callback arguments as the owner builds the shared tool event from them, args and result bounded
func ToolFrame(ctx context.Context, callID, name any, args any, result ...any) (*pyjson.Object, error) {
	frame := pyjson.NewObject()
	frame.Set("tool_call_id", pyrepr.Str(toolFrameCallID(callID)))
	toolName := "tool"
	if pyrepr.Truthy(name) {
		toolName = pyrepr.Str(name)
	}
	frame.Set("name", toolName)
	argsDict, isDict := args.(map[string]any)
	if obj, ok := args.(*pyjson.Object); ok {
		argsDict, isDict = pyjson.Plain(obj).(map[string]any), true
	}
	if !isDict {
		argsDict = map[string]any{}
	}
	isError := false
	if len(result) > 0 {
		text, isText := result[0].(string)
		if !isText {
			text = pyrepr.Str(result[0])
		}
		frame.Set("result", text)
		isError, _ = tools.DetectToolFailure(ctx, toolName, result[0])
		frame.Set("is_error", isError)
	}
	frame.Set("args", BoundedToolArgs(argsDict))
	if len(result) > 0 {
		frame.Set("result", BoundedToolResult(result[0], isError))
	}
	text, err := pyjson.Dumps(frame, pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if err != nil {
		return nil, err
	}
	return pyjson.LoadsDict(text)
}

func toolFrameCallID(v any) any {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return v
}
