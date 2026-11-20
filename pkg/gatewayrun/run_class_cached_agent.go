package gatewayrun

import "github.com/nickheyer/icebath/pkg/agent"

// Resets a cached agent for a new turn, its activity triple and flush cursor only when fresh
func initCachedAgentForTurn(a *agent.AIAgent, interruptDepth int) {
	if interruptDepth == 0 {
		a.ResetActivityTriple("starting new turn (cached)")
		a.SetLastFlushedDBIdx(0)
	}
	a.SetAPICallCount(0)
}
