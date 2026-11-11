package gatewayrun

import (
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Process task id and pre turn process baseline a turn stamped on the agent it ran
type turnProcessStamp struct {
	taskID   string
	baseline map[string]bool
}

// Attributes only the turn and agent cache modules set
type runnerTurnState struct {
	agentCacheBoundsCache *gateway.AgentCacheBounds
	turnProcessMu         sync.Mutex
	turnProcessStamps     map[*agent.AIAgent]turnProcessStamp
	// One shot per session notes prepended to the next user message, set by the model and skills reload commands
	pendingModelNotes        map[string]string
	pendingSkillsReloadNotes map[string]string
	// Executor workers that outlived their gateway turn, exposed to drain and interrupt until they exit
	deferredAgentWorkersMu sync.Mutex
	deferredAgentWorkers   map[*gatewayFuture]*agent.AIAgent
	// Waiters that clean an agent up once its still running executor worker finishes
	deferredAgentCleanupTasks map[*gatewayTask]struct{}
	// One shot provider switch notice the last runtime resolution left for the next agent, empty when none
	preAgentFallbackNoticeMu sync.Mutex
	preAgentFallbackNotice   string
}
