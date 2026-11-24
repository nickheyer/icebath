package acp

import (
	"fmt"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/tools"
	acputil "github.com/nickheyer/icebath/pkg/utils/acp"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

var editPermissionRequestIDs atomic.Int64

// Pending edit tool call update shown with the proposal's diff in a permission request
func BuildACPEditToolCall(proposal *tools.EditProposal) *acputil.ToolCallProgress {
	raw := jsonutil.NewObject()
	raw.Set("tool", proposal.ToolName)
	raw.Set("arguments", proposal.Arguments)
	return acputil.UpdateToolCall(fmt.Sprintf("edit-approval-%d", editPermissionRequestIDs.Add(1)), acputil.ToolCallFields{
		Title: new("Approve edit: " + tools.RedactApprovalDisplay(nil, proposal.Path, false)),
		Kind:  new("edit"), Status: new("pending"),
		Content:  []acputil.ToolCallContent{acputil.ToolDiffContent(proposal.Path, proposal.NewText, proposal.OldText)},
		RawInput: raw,
	})
}
