package gateway

// Runner methods the messaging chat picks, the model and compress mutations and the policy tier call
type RunnerModelContract interface {
	messagingRouteRunner
	agentRuntimeResolver
	reasoningOverrideRunner
	chatModelRunner
	sessionStatePeekRunner
}
