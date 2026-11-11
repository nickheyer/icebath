package tools

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func twoTargets(kind string) []*ConnTarget {
	return []*ConnTarget{NewConnTarget("gmail", kind, "connect"), NewConnTarget("notion", kind, "connect")}
}

func textPtr(s string) *string { return &s }

func mustTransition(t *testing.T, o *ConnectionOperation, name string, to TargetState, actor ConnActor, f TransitionFields) *pyjson.Object {
	t.Helper()
	change, err := o.Transition(name, to, actor, f)
	if err != nil {
		t.Fatal(err)
	}
	return change
}

func TestTransitionEnforcesTheContractAndNamesTheActor(t *testing.T) {
	operation := NewConnectionOperation(twoTargets("connector"), "", "", nil)
	mustTransition(t, operation, "gmail", TargetInitiated, ActorBackendWatcher, TransitionFields{})
	if _, err := operation.Transition("gmail", TargetConnected, ActorUser, TransitionFields{}); err == nil {
		t.Fatal("a card claimed connected")
	} else if _, ok := err.(*IllegalTransition); !ok {
		t.Fatalf("error %T", err)
	}
	if _, err := operation.Transition("gmail", TargetPending, ActorBackendWatcher, TransitionFields{}); err == nil {
		t.Fatal("an edge back to pending was allowed")
	} else if _, ok := err.(*IllegalTransition); !ok {
		t.Fatalf("error %T", err)
	}
	mustTransition(t, operation, "gmail", TargetConnected, ActorBackendWatcher, TransitionFields{})
	if operation.Target("gmail").State != TargetConnected {
		t.Fatalf("state %s", operation.Target("gmail").State)
	}
}

func TestTransitionReturnsTheChangeAndWakesTheWaiter(t *testing.T) {
	operation := NewConnectionOperation(twoTargets("connector"), "", "", nil)
	if operation.Wake.IsSet() {
		t.Fatal("wake set before any change")
	}
	change := mustTransition(t, operation, "gmail", TargetInitiated, ActorBackendWatcher, TransitionFields{Detail: textPtr("link minted")})
	want := pyjson.ObjectOf("target", "gmail", "from", "pending", "to", "initiated", "actor", "backend_watcher", "detail", "link minted")
	if !pyjson.Equal(change, want) {
		t.Fatalf("change %v", change)
	}
	if !operation.Wake.IsSet() {
		t.Fatal("wake not set")
	}
}

func TestSameStateTransitionIsANoOpNotAnError(t *testing.T) {
	operation := NewConnectionOperation(twoTargets("connector"), "", "", nil)
	mustTransition(t, operation, "gmail", TargetInitiated, ActorBackendWatcher, TransitionFields{})
	operation.Wake.Clear()
	if change := mustTransition(t, operation, "gmail", TargetInitiated, ActorBackendWatcher, TransitionFields{}); change != nil {
		t.Fatalf("change %v", change)
	}
	if operation.Wake.IsSet() {
		t.Fatal("wake set by a no-op")
	}
}

func TestSettleIsExactlyOnceAndStampsNotConnected(t *testing.T) {
	operation := NewConnectionOperation(twoTargets("connector"), "", "", nil)
	mustTransition(t, operation, "gmail", TargetInitiated, ActorBackendWatcher, TransitionFields{})
	mustTransition(t, operation, "gmail", TargetConnected, ActorBackendWatcher, TransitionFields{})
	if !operation.Settle(SettleContinue) {
		t.Fatal("first settle refused")
	}
	frozen := operation.Result(context.Background(), nil, true)
	if operation.Settle(SettleDeadline) {
		t.Fatal("second settle accepted")
	}
	if !pyjson.Equal(operation.Result(context.Background(), nil, true), frozen) {
		t.Fatal("result changed after settling")
	}
	by := map[string]*pyjson.Object{}
	rows, _ := frozen.Get("targets")
	for _, row := range rows.([]any) {
		obj := row.(*pyjson.Object)
		name, _ := obj.Get("name")
		by[name.(string)] = obj
	}
	if state, _ := by["gmail"].Get("state"); state != "connected" {
		t.Fatalf("gmail state %v", state)
	}
	if state, _ := by["notion"].Get("state"); state != "not_connected" {
		t.Fatalf("notion state %v", state)
	}
	if _, has := by["notion"].Get("detail"); has {
		t.Fatal("settle reason copied into the notion detail")
	}
}

func TestAllResolvedSettlesOnConnectedOrSkippedOnly(t *testing.T) {
	operation := NewConnectionOperation(twoTargets("connector"), "", "", nil)
	mustTransition(t, operation, "gmail", TargetInitiated, ActorBackendWatcher, TransitionFields{})
	mustTransition(t, operation, "gmail", TargetFailed, ActorBackendWatcher, TransitionFields{Detail: textPtr("oauth denied")})
	mustTransition(t, operation, "notion", TargetSkipped, ActorUser, TransitionFields{})
	if operation.SettleIfAllResolved() {
		t.Fatal("a failed target settled the operation")
	}
	mustTransition(t, operation, "gmail", TargetSkipped, ActorUser, TransitionFields{})
	if !operation.SettleIfAllResolved() {
		t.Fatal("all resolved did not settle")
	}
	if operation.SettledBy() != SettleAllResolved {
		t.Fatalf("settled by %s", operation.SettledBy())
	}
}

func TestTargetKeepsTheLinkAndTheMintDetailAcrossTransitions(t *testing.T) {
	operation := NewConnectionOperation(twoTargets("connector"), "", "", nil)
	mustTransition(t, operation, "gmail", TargetInitiated, ActorBackendWatcher, TransitionFields{ConnectURL: textPtr("https://link/gmail"), Detail: textPtr("")})
	mustTransition(t, operation, "gmail", TargetFailed, ActorBackendWatcher, TransitionFields{Detail: textPtr("vendor said no")})
	snap := operation.Target("gmail").snapshot(true)
	if url, _ := snap.Get("connect_url"); url != "https://link/gmail" {
		t.Fatalf("connect_url %v", url)
	}
	if detail, _ := snap.Get("detail"); detail != "vendor said no" {
		t.Fatalf("detail %v", detail)
	}
}

func TestRequestPayloadCarriesTheLiveTargetSnapshot(t *testing.T) {
	target := NewConnTarget("gmail", "connector", "reconnect")
	target.Instructions = "Finish setup"
	operation := NewConnectionOperation([]*ConnTarget{target}, "", "call-1", nil)
	mustTransition(t, operation, "gmail", TargetInitiated, ActorBackendWatcher, TransitionFields{ConnectURL: textPtr("https://l/gmail")})
	payload := operation.RequestPayload()
	rows, _ := payload.Get("targets")
	if len(rows.([]any)) != 1 {
		t.Fatalf("targets %v", rows)
	}
	want := pyjson.ObjectOf("name", "gmail", "kind", "connector", "action", "reconnect", "state", "initiated", "resolved", false,
		"instructions", "Finish setup", "connect_url", "https://l/gmail")
	if !pyjson.Equal(rows.([]any)[0], want) {
		t.Fatalf("target %v", rows.([]any)[0])
	}
	if id, _ := payload.Get("tool_call_id"); id != "call-1" {
		t.Fatalf("tool_call_id %v", id)
	}
	if _, has := payload.Get("reason"); has {
		t.Fatal("payload carries a reason")
	}
}
