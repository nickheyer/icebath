package gateway

import (
	"fmt"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Refuses an editor policy outside an ACP launch or with an unknown key, a non ask policy or bad servers
func validateEditor(source string, editor any) error {
	if editor == nil {
		return nil
	}
	editor = pyjson.Plain(editor)
	if source != "acp" {
		return invalidParams("editor", "an editor policy is only accepted from an ACP launch, not "+source)
	}
	fields, ok := editor.(map[string]any)
	if !ok {
		return invalidParams("editor", "must be a mapping, got "+pyrepr.TypeName(editor))
	}
	var unknown []string
	for key := range fields {
		if key != "mcp_servers" && key != "edit_approval_policy" {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return invalidParams("editor."+unknown[0], "unknown editor key")
	}
	if policy, has := fields["edit_approval_policy"]; has && policy != "ask" {
		return invalidParams("editor.edit_approval_policy", "must be 'ask': every edit is approved one at a time")
	}
	servers, has := fields["mcp_servers"]
	if !has {
		servers = []any{}
	}
	if _, isList := servers.([]any); !isList {
		return invalidParams("editor.mcp_servers", fmt.Sprintf("must be a list, got %s", pyrepr.TypeName(servers)))
	}
	return validateServers(servers)
}

// RuntimeStoreError invalid_params naming the field it read and why
func invalidParams(field, detail string) error {
	return state.NewRuntimeStoreError("invalid_params", &field, &detail)
}
