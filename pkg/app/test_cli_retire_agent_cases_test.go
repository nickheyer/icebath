package app

import (
	"context"
	"testing"
)

// Gateway agent that counts how often its LLM clients are released
type releaseCountingAgent struct {
	*GatewayAgent
	releaseCalls int
}

func (a *releaseCountingAgent) releaseClients() { a.releaseCalls++ }

func TestRetireAgentReleasesOldAgentClientsBeforeRebuild(t *testing.T) {
	recordPrints(t)
	agent := &releaseCountingAgent{GatewayAgent: &GatewayAgent{ctx: context.Background()}}
	c := bareCLI()
	c.setAgent(agent)
	retireAgent(c)
	if c.agentRef() != nil {
		t.Fatalf("agent = %#v, want nil", c.agentRef())
	}
	if agent.releaseCalls != 1 {
		t.Fatalf("release calls = %d, want 1", agent.releaseCalls)
	}
}
