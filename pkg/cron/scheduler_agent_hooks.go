package cron

import (
	"context"
	"fmt"
	"sync"
)

// Agent package entry points the cron run path calls, installed from the agent package init
var agentHooks struct {
	sync.RWMutex
	factory       func(ctx context.Context, o AgentOptions) (CronAgent, error)
	hardInterrupt func(a CronAgent, reason, toolReason string)
	explain       func(reason string, cause, model *string) string
	cleanup       func()
	maxIteration  func(result any) bool
}

// Python ModuleNotFoundError raised when an agent module the run path imports is absent
type agentModuleMissingError struct{ module string }

func (e *agentModuleMissingError) Error() string {
	return fmt.Sprintf("No module named '%s'", e.module)
}

func (e *agentModuleMissingError) ExceptionName() string { return "ModuleNotFoundError" }

// Installs the run_agent AIAgent constructor cron runs build their agent with
func SetAgentFactory(f func(ctx context.Context, o AgentOptions) (CronAgent, error)) {
	agentHooks.Lock()
	agentHooks.factory = f
	agentHooks.Unlock()
}

// Installs agent.interrupt_compat request_hard_interrupt for the inactivity watchdog
func SetRequestHardInterrupt(f func(a CronAgent, reason, toolReason string)) {
	agentHooks.Lock()
	agentHooks.hardInterrupt = f
	agentHooks.Unlock()
}

// Installs AIAgent._format_turn_completion_explanation used to spot a boilerplate turn summary
func SetFormatTurnCompletionExplanation(f func(reason string, cause, model *string) string) {
	agentHooks.Lock()
	agentHooks.explain = f
	agentHooks.Unlock()
}

// Installs agent.auxiliary_client cleanup_stale_async_clients run after each agent teardown
func SetCleanupStaleAsyncClients(f func()) {
	agentHooks.Lock()
	agentHooks.cleanup = f
	agentHooks.Unlock()
}

// Installs agent.turn_failure_copy is_max_iteration_handoff that tells a budget handoff from a failure
func SetMaxIterationHandoffCheck(f func(result any) bool) {
	agentHooks.Lock()
	agentHooks.maxIteration = f
	agentHooks.Unlock()
}

// Builds the AIAgent through the registered factory, failing like the import when an agent module is absent
var newCronAgent = func(ctx context.Context, o AgentOptions) (CronAgent, error) {
	agentHooks.RLock()
	factory, explain := agentHooks.factory, agentHooks.explain
	interrupt, maxIteration := agentHooks.hardInterrupt, agentHooks.maxIteration
	agentHooks.RUnlock()
	switch {
	case factory == nil || explain == nil:
		return nil, &agentModuleMissingError{"run_agent"}
	case interrupt == nil:
		return nil, &agentModuleMissingError{"agent.interrupt_compat"}
	case maxIteration == nil:
		return nil, &agentModuleMissingError{"agent.turn_failure_copy"}
	}
	return factory(ctx, o)
}

// Hard interrupts a live agent through the registered hook that construction already required
func requestHardInterrupt(a CronAgent, reason, toolReason string) {
	agentHooks.RLock()
	f := agentHooks.hardInterrupt
	agentHooks.RUnlock()
	f(a, reason, toolReason)
}

// Reports whether the agent result is a max iteration handoff through the required hook
func isMaxIterationHandoff(result map[string]any) bool {
	agentHooks.RLock()
	f := agentHooks.maxIteration
	agentHooks.RUnlock()
	return f(result)
}

// Renders the agent's own turn completion explanation through the required hook
func formatTurnCompletionExplanation(reason string, cause, model *string) string {
	agentHooks.RLock()
	f := agentHooks.explain
	agentHooks.RUnlock()
	return f(reason, cause, model)
}

// Reaps cached async clients, an error when agent.auxiliary_client is not registered
func cleanupStaleAsyncClients() error {
	agentHooks.RLock()
	f := agentHooks.cleanup
	agentHooks.RUnlock()
	if f == nil {
		return &agentModuleMissingError{"agent.auxiliary_client"}
	}
	f()
	return nil
}
