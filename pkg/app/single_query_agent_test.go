package app

import "testing"

func TestSingleQueryAgentIsNilWhenSessionCreateFailed(t *testing.T) {
	prev := currentActiveAgent()
	setActiveAgent(nil)
	t.Cleanup(func() { setActiveAgent(prev) })
	a := singleQueryAgent(&CLI{})
	if a != nil {
		t.Fatalf("no agent was ever built, got %#v", a)
	}
	shutdownAgentMemoryProvider(a)
	active := &GatewayAgent{sessionID: "s1"}
	setActiveAgent(active)
	if got := singleQueryAgent(&CLI{}); got != cliAgent(active) {
		t.Fatalf("the active agent is the fallback, got %#v", got)
	}
}
