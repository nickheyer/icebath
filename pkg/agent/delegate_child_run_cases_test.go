package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Child double that accepts a hard stop, the attributes AIAgent.hard_interrupt needs
func interruptibleChild() *AIAgent {
	return &AIAgent{hardInterruptRequested: &pythread.Event{}, pendingRedirectLock: &sync.Mutex{},
		toolWorkerThreads: map[int64]bool{}, toolWorkerThreadsLock: &sync.Mutex{}, activeChildrenLock: &sync.Mutex{},
		quietMode: true, apiMode: "test"}
}

func holdsChild(parent, child *AIAgent) bool {
	for _, c := range parent.activeChildren {
		if c == child {
			return true
		}
	}
	return false
}

func TestInterruptedChildEntryCarriesItsPartialOutput(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "kickoff"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "Audited 3 of 7 modules; two findings so far."}},
			"tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "terminal", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "[Command interrupted]"},
		map[string]any{"role": "assistant", "content": "Operation interrupted."},
	}
	result := map[string]any{"final_response": "Operation interrupted.", "messages": messages, "api_calls": 2, "completed": false, "interrupted": true}
	child := &AIAgent{model: "m", sessionCostStatus: "unknown", sessionPromptTokens: 1, sessionCompletionTokens: 1, delegateRole: "leaf"}
	entry := buildResultEntry(child, result, 0, 12.5, schemaOutcome{})
	if entryValue(entry, "status") != "interrupted" || entryValue(entry, "exit_reason") != "interrupted" {
		t.Fatalf("entry %v", entry)
	}
	if entryValue(entry, "summary") != "Audited 3 of 7 modules; two findings so far." || entryValue(entry, "error") != "Operation interrupted." {
		t.Fatalf("entry %v", entry)
	}
}

func TestChildAttachedAfterParentStopIsStoppedToo(t *testing.T) {
	parent := &AIAgent{}
	parent.interruptRequested.Store(true)
	parent.interruptMessage = "stop"
	late := interruptibleChild()
	attachChild(parent, late)
	if !holdsChild(parent, late) {
		t.Fatal("late child not attached")
	}
	if !late.interruptRequested.Load() || !late.hardInterruptRequested.IsSet() || late.interruptMessage != "stop" {
		t.Fatalf("late child stops: requested %v hard %v message %q", late.interruptRequested.Load(), late.hardInterruptRequested.IsSet(), late.interruptMessage)
	}
}

func TestChildAttachedToRunningParentIsLeftAlone(t *testing.T) {
	parent := &AIAgent{}
	child := interruptibleChild()
	attachChild(parent, child)
	if !holdsChild(parent, child) || child.interruptRequested.Load() || child.hardInterruptRequested.IsSet() {
		t.Fatalf("child attached %v requested %v", holdsChild(parent, child), child.interruptRequested.Load())
	}
}

func TestRealSpawnPathChildStartsInterrupted(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("delegation:\n  max_spawn_depth: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	parent, err := NewAIAgent(ctx, AIAgentOptions{SessionID: "p", APIKey: "k", BaseURL: "https://openrouter.ai/api/v1", Provider: "openrouter",
		APIMode: "chat_completions", Model: "anthropic/claude-sonnet-4.6", Platform: "cli", QuietMode: true,
		SkipContextFiles: true, SkipMemory: true, SaveTrajectories: false, EnabledToolsets: []string{"file"}})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	mid, err := buildChildAgent(ctx, childBuildSpec{TaskIndex: 0, Goal: "mid", MaxIterations: 4, TaskCount: 1, Parent: parent})
	if err != nil {
		t.Fatal(err)
	}
	defer mid.Close()
	mid.HardInterrupt("stop", "")
	grandchild, err := buildChildAgent(ctx, childBuildSpec{TaskIndex: 0, Goal: "gc", MaxIterations: 4, TaskCount: 1, Parent: mid})
	if err != nil {
		t.Fatal(err)
	}
	defer grandchild.Close()
	if !grandchild.interruptRequested.Load() {
		t.Fatal("grandchild built under a stopped orchestrator did not start interrupted")
	}
}
