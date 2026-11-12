package tools

// Appends a pending gateway approval entry to a session queue the way a hermes case seeds _gateway_queues
func QueueGatewayApprovalForTests(sessionKey string, data map[string]any) *ApprovalEntry {
	entry := newApprovalEntry(data)
	approvalState.Lock()
	approvalState.gatewayQueues[sessionKey] = append(approvalState.gatewayQueues[sessionKey], entry)
	approvalState.Unlock()
	return entry
}
