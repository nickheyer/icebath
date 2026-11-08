package provider

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Normalized tool call with dict arguments serialized and provider fields kept as extra keys
func BuildToolCall(id, name string, arguments any, providerFields map[string]any) state.ToolCall {
	args := ""
	if dict, ok := arguments.(map[string]any); ok {
		args, _ = pyjson.Dumps(dict)
	} else {
		args = pyrepr.Str(arguments)
	}
	tc := state.ToolCall{ID: id, Type: "function", Function: state.ToolCallFunction{Name: name, Arguments: args}}
	if len(providerFields) > 0 {
		tc.Extra = make(map[string]any, len(providerFields))
		for k, v := range providerFields {
			tc.Extra[k] = v
		}
	}
	return tc
}

// Provider stop reason translated through the mapping, stop for an unknown or absent reason
func MapFinishReason(reason any, mapping map[string]string) string {
	text, ok := reason.(string)
	if !ok {
		return "stop"
	}
	if mapped, has := mapping[text]; has {
		return mapped
	}
	return "stop"
}
