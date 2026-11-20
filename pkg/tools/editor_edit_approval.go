package tools

import "context"

// Waits on the current session's gateway approval for one editor edit, approved only for a resolved one time grant
func AwaitEditorEditDecision(ctx context.Context, env *Env, data map[string]any) bool {
	route := CurrentSessionKey(env)
	approvalState.Lock()
	notify := approvalState.gatewayNotify[route]
	approvalState.Unlock()
	if notify == nil {
		return false
	}
	decision := awaitGatewayDecision(ctx, env, route, notify, data, "acp")
	return decision.Resolved && decision.Choice == "once"
}
