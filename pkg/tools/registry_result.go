package tools

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/errchain"
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
	case *jsonutil.Object:
		flag, _ := v.Value("_multimodal").(bool)
		if _, isList := v.Value("content").([]any); flag && isList {
			return ToolResult(v)
		}
	}
	resultType := errchain.Kind(result)
	envLog(env).Error(fmt.Sprintf("Tool %s handler returned an unsupported result: %s", name, resultType))
	body := jsonutil.NewObject()
	body.Set("error", boundModelErrorText(env, "Tool handler returned an unsupported result: "+resultType))
	body.Set("error_type", "tool_result_contract")
	body.Set("tool", name)
	body.Set("result_type", resultType)
	return ToolResult(body)
}
