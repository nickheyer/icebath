package provider

import (
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

// DeepSeek models sent DeepSeek's own reasoning controls, as the deepseek kind shapes them for the id without its vendor; other families none
func shape(call *provider.Call) (map[string]any, map[string]any) {
	model := strings.TrimSpace(call.Model)
	vendor, bare, ok := strings.Cut(model, "/")
	if !ok || !strings.EqualFold(vendor, "deepseek") || call.Kind == nil {
		return nil, nil
	}
	native, known := call.Kind("deepseek")
	if !known || native.Shape == nil {
		return nil, nil
	}
	inner := *call
	inner.Model = bare
	return native.Shape(&inner)
}
