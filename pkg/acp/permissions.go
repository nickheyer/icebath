package acp

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var optionIDToIcebath = map[string]string{
	"allow_once": "once", "allow_session": "session", "allow_always": "always", "deny": "deny", "deny_always": "deny",
}

var permissionRequestIDs atomic.Int64

// Client permission request the ACP connection sends
type RequestPermissionFunc func(ctx context.Context, sessionID string, toolCall *pyacp.ToolCallProgress, options []pyacp.PermissionOption) (*pyacp.RequestPermissionResponse, error)

// Reports whether the schema accepts a permission option kind
func permissionOptionSupportsKind(kind string) bool {
	_, err := pyacp.Validate[pyacp.PermissionOption](permissionProbe(kind))
	return err == nil
}

func permissionProbe(kind string) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("optionId", "__probe__")
	o.Set("kind", kind)
	o.Set("name", "probe")
	return o
}

// Permission options matching icebath approval semantics, once and deny only when the gate re-asks
func buildPermissionOptions(allowPermanent, allowSession, smartDenied bool) []pyacp.PermissionOption {
	onceOnly := smartDenied || !allowSession
	options := []pyacp.PermissionOption{{OptionID: "allow_once", Kind: "allow_once", Name: "Allow once"}}
	if !onceOnly {
		options = append(options, pyacp.PermissionOption{OptionID: "allow_session", Kind: "allow_always", Name: "Allow for session"})
		if allowPermanent {
			options = append(options, pyacp.PermissionOption{OptionID: "allow_always", Kind: "allow_always", Name: "Allow always"})
		}
	}
	options = append(options, pyacp.PermissionOption{OptionID: "deny", Kind: "reject_once", Name: "Deny"})
	if !onceOnly && permissionOptionSupportsKind("reject_always") {
		options = append(options, pyacp.PermissionOption{OptionID: "deny_always", Kind: "reject_always", Name: "Deny always"})
	}
	return options
}

// Pending execute tool call update attached to a permission request with a unique perm-check id
func buildPermissionToolCall(command, description string) *pyacp.ToolCallProgress {
	content, title := "$ "+command, command
	if description != "" {
		content = description + "\n$ " + command
		title = description + ": " + command
	}
	raw := pyjson.NewObject()
	raw.Set("command", command)
	raw.Set("description", description)
	return pyacp.UpdateToolCall(fmt.Sprintf("perm-check-%d", permissionRequestIDs.Add(1)), pyacp.ToolCallFields{
		Title: pyacp.Ptr(title), Kind: pyacp.Ptr("execute"), Status: pyacp.Ptr("pending"),
		Content:  []pyacp.ToolCallContent{pyacp.ToolContent(pyacp.TextBlock(content))},
		RawInput: raw,
	})
}

// Reports whether a response selected one of the allow options
func permissionAllowed(resp *pyacp.RequestPermissionResponse, options []pyacp.PermissionOption) bool {
	if resp == nil {
		return false
	}
	sel, ok := resp.Outcome.(*pyacp.AllowedOutcome)
	if !ok {
		return false
	}
	for _, o := range options {
		if o.OptionID == sel.OptionID && strings.HasPrefix(o.Kind, "allow") {
			return true
		}
	}
	return false
}
