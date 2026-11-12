package app

import (
	"context"
	"reflect"
	"testing"
)

// Gateway agent that counts how often its LLM clients are released
type releaseCountingAgent struct {
	*GatewayAgent
	releaseCalls int
}

func (a *releaseCountingAgent) ReleaseClients(context.Context) { a.releaseCalls++ }

func TestReasoningCommandReleasesOldAgentClientsBeforeRebuild(t *testing.T) {
	recordPrints(t)
	agent := &releaseCountingAgent{GatewayAgent: &GatewayAgent{ctx: context.Background()}}
	c := bareCLI()
	c.reasoningConfig = map[string]any{"enabled": true, "effort": "medium"}
	c.agent = agent
	c.handleReasoningCommand("/reasoning high")
	if want := map[string]any{"enabled": true, "effort": "high"}; !reflect.DeepEqual(c.reasoningConfig, want) {
		t.Fatalf("reasoningConfig = %#v, want %#v", c.reasoningConfig, want)
	}
	if c.agent != nil {
		t.Fatalf("agent = %#v, want nil", c.agent)
	}
	if agent.releaseCalls != 1 {
		t.Fatalf("release calls = %d, want 1", agent.releaseCalls)
	}
}
