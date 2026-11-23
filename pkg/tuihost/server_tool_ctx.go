package tuihost

import "github.com/nickheyer/icebath/pkg/tools"

// What a tool call asked for, whole, as its live and its stored row show it, empty when the preview cannot be built
func ToolCtx(name string, args map[string]any) (out string) {
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()
	whole := 0
	return tools.BuildToolPreview(nil, name, args, &whole)
}
