package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestMissedSteerRetentionPendingSteerLandsInCompletionEntry(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	parent.model = "test-model"
	scriptedResultChild(t, parent.env, "test-model", 0, 0, map[string]any{
		"final_response": "done", "completed": true, "interrupted": false, "api_calls": 1, "messages": []any{},
		"pending_steer": "focus on pricing instead",
	})
	entry := delegateFirstResult(t, parent, "race test")
	summary := entryText(entry, "summary")
	if entryText(entry, "missed_steer") != "focus on pricing instead" || !strings.Contains(summary, "steer did not land") || !strings.Contains(summary, "focus on pricing instead") || entryText(entry, "status") != "completed" {
		t.Fatalf("entry %v", entry)
	}
}

func TestMissedSteerRetentionNoPendingSteerLeavesEntryUntouched(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	parent.model = "test-model"
	scriptedResultChild(t, parent.env, "test-model", 0, 0, map[string]any{
		"final_response": "done", "completed": true, "interrupted": false, "api_calls": 1, "messages": []any{},
	})
	entry := delegateFirstResult(t, parent, "clean run")
	if entryHas(entry, "missed_steer") || strings.Contains(entryText(entry, "summary"), "steer did not land") {
		t.Fatalf("entry %v", entry)
	}
}

// Waits until another goroutine holds the subagent registry lock
func awaitRegistryLockHeld(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if activeSubagentsMu.TryLock() {
			activeSubagentsMu.Unlock()
			time.Sleep(2 * time.Millisecond)
			continue
		}
		return
	}
	t.Fatal("the steer never entered the registry")
}

func TestMissedSteerRetentionAcceptedSteerRacingCompletionIsDurablyRetained(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	child := livenessChild(env, "race-child", 0, 0)
	child.subagentID, child.pendingSteerLock = "sid-linearized-accept", &sync.Mutex{}
	running, allowReturn := make(chan struct{}), make(chan struct{})
	scriptChildConversation(t, child, func(context.Context) (map[string]any, error) {
		close(running)
		select {
		case <-allowReturn:
		case <-time.After(5 * time.Second):
			t.Error("the run was never allowed to return")
		}
		return map[string]any{"final_response": "done", "completed": true, "interrupted": false, "api_calls": 1, "messages": []any{}}, nil
	})
	parent := &AIAgent{env: env}
	results := make(chan *pyjson.Object, 1)
	go func() {
		results <- runSingleChild(tools.WithToolEnv(context.Background(), env), 0, "race", child, parent, "", nil, nil)
	}()
	waitSignal(t, running, "child start")
	child.pendingSteerLock.Lock()
	accepted := make(chan bool, 1)
	go func() { accepted <- SteerSubagent(child.subagentID, "retain this exact text", nil, nil, nil) }()
	awaitRegistryLockHeld(t)
	close(allowReturn)
	child.pendingSteerLock.Unlock()
	select {
	case ok := <-accepted:
		if !ok {
			t.Fatal("the racing steer was refused")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the steer never returned")
	}
	select {
	case entry := <-results:
		if entryText(entry, "missed_steer") != "retain this exact text" {
			t.Fatalf("entry %v", entry)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run never returned")
	}
}

// Progress callback whose flush blocks until released, the blocked completion callback of the hermes test
type blockingFlushProgress struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingFlushProgress) progressFunc() any {
	return func(event, name string, preview any, args any, extra map[string]any) {}
}

func (p *blockingFlushProgress) Flush() {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
	case <-time.After(5 * time.Second):
	}
}

func TestMissedSteerRetentionSteerAfterRunReturnIsRejectedBeforeCompletionCallback(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	child := livenessChild(env, "late-child", 0, 0)
	child.subagentID, child.pendingSteerLock = "sid-closed-before-callback", &sync.Mutex{}
	probe := &blockingFlushProgress{entered: make(chan struct{}), release: make(chan struct{})}
	child.toolProgressCallback = probe
	scriptChildConversation(t, child, func(context.Context) (map[string]any, error) {
		return map[string]any{"final_response": "done", "completed": true, "interrupted": false, "api_calls": 1, "messages": []any{}}, nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSingleChild(tools.WithToolEnv(context.Background(), env), 0, "late", child, &AIAgent{env: env}, "", nil, nil)
	}()
	waitSignal(t, probe.entered, "completion callback")
	accepted := SteerSubagent(child.subagentID, "too late", nil, nil, nil)
	unlock := icLock(child.pendingSteerLock)
	steered := len(child.steerParts())
	unlock()
	close(probe.release)
	waitSignal(t, done, "run return")
	if accepted || steered != 0 {
		t.Fatalf("late steer accepted %v steered %d", accepted, steered)
	}
}
