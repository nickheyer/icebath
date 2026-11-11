package gatewayrun

import "context"

// Producer stable identity of a background result as its three element tuple, nil when it carries none
func (r *GatewayRunner) CompletionDeliveryIdentity(_ context.Context, event map[string]any) []any {
	identity, ok := completionDeliveryIdentity(event)
	if !ok {
		return nil
	}
	return []any{identity.evtType, identity.producerID, identity.incarnation}
}

// Verdict, owner and target of the conversation a background result belongs to
func (r *GatewayRunner) ClassifyCompletionOwner(ctx context.Context, parentSessionID, sessionKey string) (string, string, string, error) {
	return r.classifyCompletionOwner(ctx, parentSessionID, sessionKey)
}
