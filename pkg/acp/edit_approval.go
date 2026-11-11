package acp

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/tools"
)

var editApprovalLog = logging.GetLogger("acp_adapter.edit_approval")

var editPermissionRequestIDs atomic.Int64

// Binds an ACP edit approval requester for calls made under the returned context
func SetEditApprovalRequester(ctx context.Context, requester tools.EditApprovalRequester) context.Context {
	return tools.WithEditApprovalRequester(ctx, requester)
}

// Pending edit tool call update shown with the proposal's diff in a permission request
func BuildACPEditToolCall(proposal *tools.EditProposal) *pyacp.ToolCallProgress {
	raw := pyjson.NewObject()
	raw.Set("tool", proposal.ToolName)
	raw.Set("arguments", proposal.Arguments)
	return pyacp.UpdateToolCall(fmt.Sprintf("edit-approval-%d", editPermissionRequestIDs.Add(1)), pyacp.ToolCallFields{
		Title: pyacp.Ptr("Approve edit: " + tools.RedactApprovalDisplay(nil, proposal.Path, false)),
		Kind:  pyacp.Ptr("edit"), Status: pyacp.Ptr("pending"),
		Content:  []pyacp.ToolCallContent{pyacp.ToolDiffContent(proposal.Path, proposal.NewText, proposal.OldText)},
		RawInput: raw,
	})
}

// Policy and workspace the auto approval check reads per proposal
type AutoApproveGetter func() (policy string, cwd string, err error)

// Edit approval requester asking the ACP client unless the session policy auto approves the edit
func MakeACPEditApprovalRequester(requestPermission RequestPermissionFunc, loop Loop, sessionID string, timeout *float64, autoApprove AutoApproveGetter, sendUpdate SendUpdateFunc) tools.EditApprovalRequester {
	return func(ctx context.Context, env *tools.Env, proposal *tools.EditProposal) (bool, error) {
		if autoApprove != nil {
			policy, cwd, err := autoApprove()
			if err != nil {
				editApprovalLog.Debug("ACP edit auto-approval policy check failed")
			} else if tools.ShouldAutoApproveEdit(proposal, policy, cwd) {
				editApprovalLog.Info(fmt.Sprintf("Auto-approved ACP edit under policy %s: %s", policy, proposal.Path))
				return true, nil
			}
		}
		options := []pyacp.PermissionOption{
			{OptionID: "allow_once", Kind: "allow_once", Name: "Allow edit"},
			{OptionID: "deny", Kind: "reject_once", Name: "Deny"},
		}
		response, _ := AwaitPermission(requestPermission, loop, sessionID, BuildACPEditToolCall(proposal), options,
			ResolvePermissionTimeout(ctx, timeout), "Edit approval request", sendUpdate)
		if response == nil {
			return false, nil
		}
		sel, ok := response.Outcome.(*pyacp.AllowedOutcome)
		return ok && sel.OptionID == "allow_once", nil
	}
}
