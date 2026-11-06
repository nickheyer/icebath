package tools

import (
	"context"
	"testing"
	"time"
)

// Asker answering deny while recording the open human wait windows it observed during the prompt
type waitObservingAsker struct {
	env     *Env
	pending int
}

func (a *waitObservingAsker) Ask(context.Context, []Question) (FormResult, error) {
	a.pending, _ = humanWaitPending(resolveWaitKey(a.env, ""))
	return FormResult{Outcome: "submitted", Answers: []Answer{{Status: "answered", Text: "deny"}}}, nil
}

func humanWaitPending(key string) (int, bool) {
	humanWait.Lock()
	defer humanWait.Unlock()
	st, ok := humanWait.states[key]
	if !ok {
		return 0, false
	}
	return st.pending, true
}

func TestAwaitGatewayDecisionRecordsHumanWait(t *testing.T) {
	env := gateEnv(t)
	data := map[string]any{"command": "rm -rf /tmp/x", "description": "test", "pattern_key": "k", "pattern_keys": []any{"k"}}
	notified := make(chan struct{})
	done := make(chan gatewayDecision, 1)
	go func() {
		done <- awaitGatewayDecision(context.Background(), env, gateSession, func(map[string]any) error {
			close(notified)
			return nil
		}, data, "gateway")
	}()
	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway notify never fired")
	}
	time.Sleep(100 * time.Millisecond)
	waited := HumanWaitSeconds(env, gateSession)
	ResolveGatewayApproval(gateSession, "deny", true, "", "")
	var decision gatewayDecision
	select {
	case decision = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway wait did not resolve")
	}
	if waited <= 0 {
		t.Fatalf("gateway poll loop recorded no human wait")
	}
	if !decision.Resolved || decision.Choice != "deny" {
		t.Fatalf("decision %+v", decision)
	}
	if pending, ok := humanWaitPending(gateSession); !ok || pending != 0 {
		t.Fatalf("window still open pending=%d present=%v", pending, ok)
	}
}

func TestPromptDangerousApprovalRecordsHumanWait(t *testing.T) {
	env := gateEnv(t)
	env.Values.Store("session.interactive", true)
	asker := &waitObservingAsker{env: env}
	env.Ask = asker
	choice := promptDangerousApproval(context.Background(), env, promptOptions{command: "rm -rf /tmp/x", description: "test", allowSession: true})
	if choice.value != "deny" {
		t.Fatalf("choice %+v", choice)
	}
	if asker.pending != 1 {
		t.Fatalf("window was not open while the prompt ran pending=%d", asker.pending)
	}
	if pending, ok := humanWaitPending(resolveWaitKey(env, "")); !ok || pending != 0 {
		t.Fatalf("prompt window state pending=%d present=%v", pending, ok)
	}
}
