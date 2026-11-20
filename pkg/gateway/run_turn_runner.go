package gateway

import "context"

// Duck typed adapter that sends its own exec approval prompt
type execApprovalSender interface {
	SendExecApproval(ctx context.Context, chatID, command, sessionKey string, description *string, metadata map[string]any, allowPermanent, allowSession, smartDenied bool, requestID *string) (SendResult, error)
}

// Whether the adapter renders native approval buttons, a duck type counting when it sends approvals itself
func rendersExecApprovalButtons(adapter any) bool {
	if pa, ok := adapter.(PlatformAdapter); ok {
		return pa.Base().SupportsExecApprovalButtons()
	}
	_, ok := adapter.(execApprovalSender)
	return ok
}
