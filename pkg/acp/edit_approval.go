package acp

import (
	"fmt"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

var editPermissionRequestIDs atomic.Int64

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
