package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Tool callback arguments as the owner builds the shared tool event from them, args and result bounded
func ToolFrame(ctx context.Context, callID, name any, args any, result ...any) (*jsonutil.Object, error) {
	frame := jsonutil.NewObject()
	frame.Set("tool_call_id", anyutil.String(toolFrameCallID(callID)))
	toolName := "tool"
	if anyutil.IsTrue(name) {
		toolName = anyutil.String(name)
	}
	frame.Set("name", toolName)
	argsDict, isDict := args.(map[string]any)
	if obj, ok := args.(*jsonutil.Object); ok {
		argsDict, isDict = jsonutil.Plain(obj).(map[string]any), true
	}
	if !isDict {
		argsDict = map[string]any{}
	}
	isError := false
	if len(result) > 0 {
		text, isText := result[0].(string)
		if !isText {
			text = anyutil.String(result[0])
		}
		frame.Set("result", text)
		isError, _ = tools.DetectToolFailure(ctx, toolName, result[0])
		frame.Set("is_error", isError)
	}
	frame.Set("args", BoundedToolArgs(argsDict))
	if len(result) > 0 {
		frame.Set("result", BoundedToolResult(result[0], isError))
	}
	text, err := jsonutil.Encode(frame, jsonutil.Convert(func(v any) (any, error) { return anyutil.String(v), nil }), jsonutil.Spaced(), jsonutil.ASCII())
	if err != nil {
		return nil, err
	}
	return jsonutil.DecodeObject(text)
}

func toolFrameCallID(v any) any {
	if !anyutil.IsTrue(v) {
		return ""
	}
	return v
}
