package tools

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Result key of a call the tool declined before doing anything
const RefusedKey = "refused"

// Error result of a call the tool declined before doing anything, the reason the model and the person read
func RefusedResult(reason string) string {
	b, _ := pyjson.EncodeWTF8Python(Dict("error", reason, RefusedKey, true))
	return string(b)
}

// Verdict of a finished call for a person, refused when the tool declined it, with its reason
func ToolOutcome(ctx context.Context, toolName string, result any) (failed, refused bool, reason string) {
	failed, suffix := DetectToolFailure(ctx, toolName, result)
	data := resultObject(result)
	if declined, _ := data[RefusedKey].(bool); declined {
		return true, true, strings.TrimSpace(pyrepr.Str(data["error"]))
	}
	if !failed {
		return false, false, ""
	}
	if text, ok := data["error"].(string); ok && strings.TrimSpace(text) != "" {
		return true, false, strings.TrimSpace(text)
	}
	return true, false, strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(suffix), "["), "]")
}
