package acp

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pyacp"
)

var optionIDToIcebath = map[string]string{
	"allow_once": "once", "allow_session": "session", "allow_always": "always", "deny": "deny", "deny_always": "deny",
}

var permissionRequestIDs atomic.Int64

// Client permission request the ACP connection sends
type RequestPermissionFunc func(ctx context.Context, sessionID string, toolCall *acputil.ToolCallProgress, options []acputil.PermissionOption) (*acputil.RequestPermissionResponse, error)

// Reports whether the schema accepts a permission option kind
func permissionOptionSupportsKind(kind string) bool {
	_, err := pyacp.Validate[acputil.PermissionOption](permissionProbe(kind))
	return err == nil
}

func permissionProbe(kind string) *jsonutil.Object {
	o := jsonutil.NewObject()
	o.Set("optionId", "__probe__")
	o.Set("kind", kind)
	o.Set("name", "probe")
	return o
}

// Permission options matching icebath approval semantics, once and deny only when the gate re-asks
func buildPermissionOptions(allowPermanent, allowSession, smartDenied bool) []acputil.PermissionOption {
	onceOnly := smartDenied || !allowSession
	options := []acputil.PermissionOption{{OptionID: "allow_once", Kind: "allow_once", Name: "Allow once"}}
	if !onceOnly {
		options = append(options, acputil.PermissionOption{OptionID: "allow_session", Kind: "allow_always", Name: "Allow for session"})
		if allowPermanent {
			options = append(options, acputil.PermissionOption{OptionID: "allow_always", Kind: "allow_always", Name: "Allow always"})
		}
	}
	options = append(options, acputil.PermissionOption{OptionID: "deny", Kind: "reject_once", Name: "Deny"})
	if !onceOnly && permissionOptionSupportsKind("reject_always") {
		options = append(options, acputil.PermissionOption{OptionID: "deny_always", Kind: "reject_always", Name: "Deny always"})
	}
	return options
}

// Pending execute tool call update attached to a permission request with a unique perm-check id
func buildPermissionToolCall(command, description string) *acputil.ToolCallProgress {
	content, title := "$ "+command, command
	if description != "" {
		content = description + "\n$ " + command
		title = description + ": " + command
	}
	raw := jsonutil.NewObject()
	raw.Set("command", command)
	raw.Set("description", description)
	return acputil.UpdateToolCall(fmt.Sprintf("perm-check-%d", permissionRequestIDs.Add(1)), acputil.ToolCallFields{
		Title: new(title), Kind: new("execute"), Status: new("pending"),
		Content:  []acputil.ToolCallContent{acputil.ToolContent(acputil.TextBlock(content))},
		RawInput: raw,
	})
}

// Reports whether a response selected one of the allow options
func permissionAllowed(resp *acputil.RequestPermissionResponse, options []acputil.PermissionOption) bool {
	if resp == nil {
		return false
	}
	sel, ok := resp.Outcome.(*acputil.AllowedOutcome)
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
