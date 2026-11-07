package tools

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Bounds a str result, keeps the multimodal envelope as JSON and turns any other value into a contract error
func NormalizeHandlerResult(env *Env, name string, result any) string {
	switch v := result.(type) {
	case string:
		return boundJSONErrorResult(env, v)
	case map[string]any:
		if IsMultimodalToolResult(v) {
			return ToolResult(v)
		}
	case *pyjson.Object:
		flag, _ := v.Vals["_multimodal"].(bool)
		if _, isList := v.Vals["content"].([]any); flag && isList {
			return ToolResult(v)
		}
	}
	resultType := handlerResultTypeName(result)
	toolsLogger(env).Error(fmt.Sprintf("Tool %s handler returned unsupported result type: %s", name, resultType))
	body := pyjson.NewObject()
	body.Set("error", boundModelErrorText(env, "Tool handler returned unsupported result type: "+resultType))
	body.Set("error_type", "tool_result_contract")
	body.Set("tool", name)
	body.Set("result_type", resultType)
	return ToolResult(body)
}

// Python type name of a handler result, an ordered JSON object reading as dict
func handlerResultTypeName(result any) string {
	if _, ok := result.(*pyjson.Object); ok {
		return "dict"
	}
	return pyrepr.TypeName(result)
}
