package gatewayrun

// Main gateway controller that manages adapter lifecycles and routes messages to and from the agent
type GatewayRunner struct {
	runnerCoreState
	runnerAdapterState
	runnerInboundState
	runnerTurnState
	runnerStartupState
	runnerReconcileState
	runnerRuntimeState
}
