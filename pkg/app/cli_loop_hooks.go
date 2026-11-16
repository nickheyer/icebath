package app

import (
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/provider"
)

// cliCommandState holds the slash command, loop and model switch state of the interactive CLI
type cliCommandState struct {
	loopManager          agent.LoopAutomation
	lastLoopTickCheck    float64
	lastGoalBarrierCheck float64
	// Heartbeat manager of the current session and whether its idle watchdog runs
	heartbeatManager         agent.HeartbeatAutomation
	heartbeatWatchdogStarted bool
	// Marks the next process command call as an internal re entry the user's metrics already counted
	slashRedispatch bool
	// Model switch staging of the current confirm and apply sequence
	confirmedSwitchWarning  string
	oneTurnSelectionContext *provider.SelectionContext
	modelSwitchOneTurn      bool
}

// Copy of the queued items in FIFO order
func (q *inputQueue) snapshot() []any {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]any(nil), q.items...)
}

// Loop manager bound to the current session id, the gateway's on a gateway store and none before its session opens
func (c *CLI) getLoopManager() agent.LoopAutomation {
	return automationManager(c, &c.loopManager,
		func(sid string) agent.LoopAutomation { return agent.NewLoopManager(c.ctx, sid) },
		func(sid string, connection *gatewaycli.GatewayConnection) (agent.LoopAutomation, error) {
			return NewGatewayLoopManager(c.ctx, sid, connection)
		})
}

// Whether the session's goal defers this loop's tick, asked of the gateway when the manager is the gateway's
func (c *CLI) goalBlocksLoopTick(mgr agent.LoopAutomation) bool {
	if remote, isRemote := mgr.(interface{ GoalBlocksTick() bool }); isRemote {
		return remote.GoalBlocksTick()
	}
	return agent.GoalBlocksLoopTick(c.ctx, mgr.Session())
}
