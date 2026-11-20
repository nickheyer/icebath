package agent

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestDelegationViewKeepsToolListIdentityUntilTheAgentSwapsIt(t *testing.T) {
	a := &AIAgent{tools: []any{map[string]any{"type": "function", "function": map[string]any{"name": "delegate_task"}}}}
	v := newDelegationAgentView(a)
	first := v.Tools()
	if second := v.Tools(); &second[0] != &first[0] {
		t.Fatal("unchanged carried tools gave a new list")
	}
	a.SetToolSnapshot([]map[string]any{{"type": "function", "function": map[string]any{"name": "delegate_task"}}}, nil)
	if after := v.Tools(); &after[0] == &first[0] {
		t.Fatal("swapped carried tools kept the old list")
	}
	if _, set := v.DelegationBackend(); set {
		t.Fatal("backend reported set before any record")
	}
	v.SetDelegationBackend("acp")
	if backend, set := v.DelegationBackend(); backend != "acp" || !set || a.DelegationBackend() != "acp" {
		t.Fatalf("backend %q set %v", backend, set)
	}
	if v.Unwrap() != a {
		t.Fatal("view did not unwrap to its agent")
	}
}

func TestInlineDelegationFreezesAtTheFirstBuildScope(t *testing.T) {
	a := &AIAgent{}
	if !tools.AgentJoinsDelegation(tools.GatewayDelegationInlineScope(t.Context(), true), a) {
		t.Fatal("inline gateway scope did not join delegation")
	}
	if !tools.AgentJoinsDelegation(t.Context(), a) {
		t.Fatal("frozen inline decision was read again from a later scope")
	}
	if inline, set := a.DelegationInline(); !inline || !set {
		t.Fatalf("inline %v set %v", inline, set)
	}
}

func TestRequestedToolRebuildSkipsForksSubagentsAndUnrequestedSessions(t *testing.T) {
	fork := &AIAgent{sessionID: "s-fork"}
	fork.turnOrigin = "background_review"
	child := &AIAgent{sessionID: "s-child", delegateDepth: 1}
	plain := &AIAgent{sessionID: "s-plain", env: &tools.Env{}}
	for _, a := range []*AIAgent{fork, child, plain} {
		if applyRequestedToolRebuild(t.Context(), a) {
			t.Fatalf("agent %s rebuilt without a request", a.sessionID)
		}
	}
}
