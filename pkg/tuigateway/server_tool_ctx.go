package tuigateway

import "github.com/nickheyer/icebath/pkg/tools"

// Argument preview of a tool row capped at 80 chars, empty when the preview cannot be built
func ToolCtx(name string, args map[string]any) (out string) {
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()
	limit := 80
	return tools.BuildToolPreview(nil, name, args, &limit)
}
