package agent

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestFormatBatchTagOrdinalsPerParentSession(t *testing.T) {
	a, b := &AIAgent{sessionID: "tag-a"}, &AIAgent{sessionID: "tag-b"}
	if got := formatBatchTag("d1", a); got != "set 1" {
		t.Fatalf("first = %q", got)
	}
	if got := formatBatchTag("d2", a); got != "set 2" {
		t.Fatalf("second = %q", got)
	}
	if got := formatBatchTag("d1", a); got != "set 1" {
		t.Fatalf("repeat = %q", got)
	}
	if got := formatBatchTag("d9", b); got != "set 1" {
		t.Fatalf("other scope = %q", got)
	}
	if got := formatBatchTag("", a); got != "" {
		t.Fatalf("empty id = %q", got)
	}
	if got := batchPrefix("d2", 2, 9, a); got != "[set 2 · 3/9] " {
		t.Fatalf("prefix = %q", got)
	}
	if got := batchPrefix("", 0, 1, a); got != "" {
		t.Fatalf("lone unknown = %q", got)
	}
}

func TestMergeRequestOverridesExplicitWinsAndExtraBodyMergesOneLevel(t *testing.T) {
	runtime := map[string]any{"temperature": 0.2, "extra_body": map[string]any{"thinking": map[string]any{"type": "disabled"}, "a": 1}}
	explicit := map[string]any{"temperature": 0.7, "extra_body": map[string]any{"a": 2}}
	got := mergeRequestOverrides(runtime, explicit)
	extra := got["extra_body"].(map[string]any)
	if got["temperature"] != 0.7 || extra["a"] != 2 || extra["thinking"] == nil {
		t.Fatalf("merged = %v", got)
	}
	if runtime["extra_body"].(map[string]any)["a"] != 1 {
		t.Fatal("runtime overrides mutated")
	}
	if mergeRequestOverrides(nil, map[string]any{}) != nil {
		t.Fatal("empty merge is not None")
	}
}

func TestResolveDelegationCredentialsPureInheritMergesOverParent(t *testing.T) {
	parent := &AIAgent{requestOverrides: map[string]any{"x": 1}}
	got, err := resolveDelegationCredentials(t.Context(), map[string]any{"model": " m ", "request_overrides": map[string]any{"y": 2}}, parent)
	if err != nil {
		t.Fatal(err)
	}
	ro := got["request_overrides"].(map[string]any)
	if got["model"] != "m" || got["provider"] != nil || ro["x"] != 1 || ro["y"] != 2 {
		t.Fatalf("bundle = %v", got)
	}
}

func TestFabricatedEntryCarriesRoleAndRoute(t *testing.T) {
	route := pyjson.NewObject()
	route.Set("backend", "b")
	child := &AIAgent{delegateRole: "leaf", delegateRoute: route, provider: "p", model: "m"}
	e := fabricatedEntry(3, "error", "boom", child, 1.5)
	if strings.Join(e.Keys, ",") != "task_index,status,summary,error,api_calls,duration_seconds,_child_role,route" {
		t.Fatalf("keys = %v", e.Keys)
	}
	if v, _ := e.Get("_child_role"); v != "leaf" {
		t.Fatalf("role = %v", v)
	}
}

func TestAttachAndDetachChildTrackActiveChildren(t *testing.T) {
	parent, child := &AIAgent{}, &AIAgent{}
	attachChild(parent, child)
	if len(parent.activeChildren) != 1 {
		t.Fatal("not attached")
	}
	detachChild(parent, child)
	detachChild(parent, child)
	if len(parent.activeChildren) != 0 {
		t.Fatal("not detached")
	}
}

func TestChildSystemPromptOrchestratorDepthNote(t *testing.T) {
	got := buildChildSystemPrompt(t.Context(), nil, "ctx", "", "orchestrator", 3, 2)
	if !strings.Contains(got, "\nCONTEXT:\nctx") || !strings.Contains(got, childLeafChildrenNote) || !strings.Contains(got, "depth 2") {
		t.Fatalf("prompt = %q", got)
	}
	if strings.Contains(buildChildSystemPrompt(t.Context(), nil, "", "", "leaf", 3, 1), "Orchestrator Role") {
		t.Fatal("leaf got the orchestrator block")
	}
}

func TestWorktreeContextNoteNamesPathAndBranch(t *testing.T) {
	note := buildWorktreeContextNote(map[string]string{"path": "/r/.worktrees/subagent-x", "branch": "icebath-subagent/subagent-x"})
	if !strings.Contains(note, "at: /r/.worktrees/subagent-x\n") || !strings.Contains(note, "branch is: icebath-subagent/subagent-x\n") {
		t.Fatalf("note = %q", note)
	}
}
