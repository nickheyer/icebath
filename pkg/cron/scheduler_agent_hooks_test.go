package cron

import (
	"context"
	"testing"
)

// Agent package doubles every test starts with, the factory stays unregistered until a test installs one
func init() {
	SetRequestHardInterrupt(func(a CronAgent, reason, toolReason string) {
		if hard, ok := a.(interface {
			HardInterrupt(reason, toolReason string)
		}); ok {
			hard.HardInterrupt(reason, toolReason)
			return
		}
		a.Interrupt(reason)
	})
	SetFormatTurnCompletionExplanation(func(_ context.Context, reason string, cause, model *string) string { return "" })
	SetCleanupStaleAsyncClients(func() {})
	SetMaxIterationHandoffCheck(func(result any) bool {
		m, _ := result.(map[string]any)
		return m["turn_exit_reason"] == "max_iterations_reached" && m["completed"] == true
	})
}

// Registers an agent factory for one test and restores the previous one afterwards
func installFakeAgent(t *testing.T, f func(ctx context.Context, o AgentOptions) (CronAgent, error)) {
	t.Helper()
	agentHooks.RLock()
	prev := agentHooks.factory
	agentHooks.RUnlock()
	SetAgentFactory(f)
	t.Cleanup(func() { SetAgentFactory(prev) })
}

func TestNewCronAgentWithoutFactoryFailsLikeTheRunAgentImport(t *testing.T) {
	installFakeAgent(t, nil)
	_, err := newCronAgent(context.Background(), AgentOptions{})
	if err == nil || err.Error() != "No module named 'run_agent'" || cronExceptionName(err) != "ModuleNotFoundError" {
		t.Fatalf("err = %v (%s)", err, cronExceptionName(err))
	}
}

func TestNewCronAgentNamesEachMissingAgentModule(t *testing.T) {
	fake := &fakeCronAgent{}
	installFakeAgent(t, func(ctx context.Context, o AgentOptions) (CronAgent, error) { return fake, nil })
	agentHooks.Lock()
	interrupt, maxIteration := agentHooks.hardInterrupt, agentHooks.maxIteration
	agentHooks.hardInterrupt = nil
	agentHooks.Unlock()
	t.Cleanup(func() { SetRequestHardInterrupt(interrupt); SetMaxIterationHandoffCheck(maxIteration) })
	if _, err := newCronAgent(context.Background(), AgentOptions{}); err == nil || err.Error() != "No module named 'agent.interrupt_compat'" {
		t.Fatalf("interrupt err = %v", err)
	}
	SetRequestHardInterrupt(interrupt)
	SetMaxIterationHandoffCheck(nil)
	if _, err := newCronAgent(context.Background(), AgentOptions{}); err == nil || err.Error() != "No module named 'agent.turn_failure_copy'" {
		t.Fatalf("handoff err = %v", err)
	}
	SetMaxIterationHandoffCheck(maxIteration)
	if a, err := newCronAgent(context.Background(), AgentOptions{}); err != nil || a != fake {
		t.Fatalf("registered = %v %v", a, err)
	}
}

func TestCleanupStaleAsyncClientsReportsTheMissingAuxiliaryModule(t *testing.T) {
	agentHooks.Lock()
	prev := agentHooks.cleanup
	agentHooks.cleanup = nil
	agentHooks.Unlock()
	t.Cleanup(func() { SetCleanupStaleAsyncClients(prev) })
	if err := cleanupStaleAsyncClients(); err == nil || err.Error() != "No module named 'agent.auxiliary_client'" {
		t.Fatalf("err = %v", err)
	}
	calls := 0
	SetCleanupStaleAsyncClients(func() { calls++ })
	if err := cleanupStaleAsyncClients(); err != nil || calls != 1 {
		t.Fatalf("err = %v calls = %d", err, calls)
	}
}
