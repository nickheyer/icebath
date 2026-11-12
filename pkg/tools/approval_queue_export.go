package tools

import "context"

// Runs the gateway approval wait of a session for a case of another package, the choice empty when withdrawn
func AwaitGatewayDecisionForTests(ctx context.Context, env *Env, sessionKey string, notify GatewayNotifyFunc, data map[string]any) (resolved bool, choice, cancelled string) {
	decision := awaitGatewayDecision(ctx, env, sessionKey, notify, data, "gateway")
	return decision.Resolved, decision.Choice, decision.Cancelled
}

// Notifier registered for a session, nil when none is
func GatewayNotifyForTests(sessionKey string) GatewayNotifyFunc { return gatewayNotifyCb(sessionKey) }

// Appends a pending gateway approval entry to a session queue the way a hermes case seeds _gateway_queues
func QueueGatewayApprovalForTests(sessionKey string, data map[string]any) *ApprovalEntry {
	entry := newApprovalEntry(data)
	approvalState.Lock()
	approvalState.gatewayQueues[sessionKey] = append(approvalState.gatewayQueues[sessionKey], entry)
	approvalState.Unlock()
	return entry
}
