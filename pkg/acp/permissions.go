package acp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/tools"
)

var permissionsLog = logging.GetLogger("acp_adapter.permissions")

var optionIDToHermes = map[string]string{
	"allow_once": "once", "allow_session": "session", "allow_always": "always", "deny": "deny", "deny_always": "deny",
}

var permissionRequestIDs atomic.Int64

// Client permission request the ACP connection sends
type RequestPermissionFunc func(ctx context.Context, sessionID string, toolCall *pyacp.ToolCallProgress, options []pyacp.PermissionOption) (*pyacp.RequestPermissionResponse, error)

// Sink of session updates a bridge sends after the answer
type SendUpdateFunc func(update pyacp.SessionUpdate)

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

// Permission options matching Hermes approval semantics, once and deny only when the gate re-asks
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

// Hermes approval string for an outcome, deny for anything but an offered selected option
func mapOutcomeToHermes(outcome pyacp.PermissionOutcome, allowed map[string]bool) string {
	sel, ok := outcome.(*pyacp.AllowedOutcome)
	if !ok {
		return "deny"
	}
	if !allowed[sel.OptionID] {
		permissionsLog.Warning("Permission request returned unknown option_id: " + sel.OptionID)
		return "deny"
	}
	if v, ok := optionIDToHermes[sel.OptionID]; ok {
		return v
	}
	return "deny"
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

// Schedules the permission request on loop and blocks for the answer, closing the bubble when sendUpdate is given
func AwaitPermission(requestPermission RequestPermissionFunc, loop Loop, sessionID string, toolCall *pyacp.ToolCallProgress, options []pyacp.PermissionOption, timeout float64, what string, sendUpdate SendUpdateFunc) (*pyacp.RequestPermissionResponse, bool) {
	future := safeScheduleThreadsafe(func(ctx context.Context) (any, error) {
		return requestPermission(ctx, sessionID, toolCall, options)
	}, loop, permissionsLog, what+": failed to schedule on loop")
	if future == nil {
		return nil, false
	}
	var response *pyacp.RequestPermissionResponse
	timedOut := false
	res, err := future.Result(time.Duration(timeout * float64(time.Second)))
	switch {
	case errors.Is(err, ErrFutureTimeout):
		future.Cancel()
		permissionsLog.Warning(fmt.Sprintf("%s timed out after %ss", what, pyrepr.Float(timeout)))
		timedOut = true
	case err != nil:
		future.Cancel()
		permissionsLog.Warning(fmt.Sprintf("%s failed: %s", what, err))
	default:
		response, _ = res.(*pyacp.RequestPermissionResponse)
	}
	if sendUpdate != nil {
		status := "failed"
		if permissionAllowed(response, options) {
			status = "completed"
		}
		sendUpdate(pyacp.UpdateToolCall(toolCall.ToolCallID, pyacp.ToolCallFields{Status: pyacp.Ptr(status)}))
	}
	return response, timedOut
}

// Explicit timeout in seconds, the configured approvals timeout when nil
func ResolvePermissionTimeout(ctx context.Context, timeout *float64) float64 {
	if timeout != nil {
		return *timeout
	}
	env := &tools.Env{}
	if cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil); err == nil {
		env.Config = cfg
	}
	return float64(tools.GetApprovalTimeout(env))
}

// Hermes approval callback bridging to the client's request_permission, timeout when nobody answered
type ApprovalCallback func(ctx context.Context, command, description string, allowPermanent, allowSession, smartDenied bool) string

// Approval callback asking the ACP client through loop, auto denying after the timeout
func MakeApprovalCallback(requestPermission RequestPermissionFunc, loop Loop, sessionID string, timeout *float64, sendUpdate SendUpdateFunc) ApprovalCallback {
	return func(ctx context.Context, command, description string, allowPermanent, allowSession, smartDenied bool) string {
		options := buildPermissionOptions(allowPermanent, allowSession, smartDenied)
		response, timedOut := AwaitPermission(requestPermission, loop, sessionID, buildPermissionToolCall(command, description),
			options, ResolvePermissionTimeout(ctx, timeout), "Permission request", sendUpdate)
		if timedOut {
			return "timeout"
		}
		if response == nil {
			return "deny"
		}
		allowed := map[string]bool{}
		for _, o := range options {
			allowed[o.OptionID] = true
		}
		return mapOutcomeToHermes(response.Outcome, allowed)
	}
}

// Asker form of an approval callback, the surface the dangerous command gate asks through an ACP turn
type ApprovalAsker struct{ Callback ApprovalCallback }

// Answers the approval gate with the callback's own answer
func (a ApprovalAsker) ApprovalCallback(ctx context.Context, command, description string, o tools.ApprovalCallbackOptions) (string, error) {
	return a.Callback(ctx, command, description, o.AllowPermanent, o.AllowSession, o.SmartDenied), nil
}

// Answers each menu question through the callback, session and always offered when the menu lists them
func (a ApprovalAsker) Ask(ctx context.Context, questions []tools.Question) (tools.FormResult, error) {
	answers := make([]tools.Answer, 0, len(questions))
	for _, q := range questions {
		description := strings.TrimPrefix(q.Header, "⚠️  DANGEROUS COMMAND: ")
		choice := a.Callback(ctx, q.Text, description, slices.Contains(q.Choices, "always"), slices.Contains(q.Choices, "session"), false)
		answers = append(answers, tools.Answer{Status: "answered", Selected: []string{choice}})
	}
	return tools.FormResult{Outcome: "answered", Answers: answers}, nil
}
