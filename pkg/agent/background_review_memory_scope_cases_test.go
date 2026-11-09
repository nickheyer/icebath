package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Fork whose profile memory flags the whitelist reads
func brReviewAgent(t *testing.T, memoryEnabled, userProfileEnabled bool) *AIAgent {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	return &AIAgent{memoryEnabled: memoryEnabled, userProfileEnabled: userProfileEnabled, env: &tools.Env{Home: h, Values: &sync.Map{}}}
}

func TestReviewToolWhitelistScopeSkillOnlyReviewOmitsMemoryTool(t *testing.T) {
	whitelist, _ := reviewToolWhitelist(context.Background(), brReviewAgent(t, true, false), nil, false)
	if whitelist["memory"] || !whitelist["skill_manage"] {
		t.Fatalf("whitelist %v", whitelist)
	}
}

func TestReviewToolWhitelistScopeMemoryReviewKeepsMemoryTool(t *testing.T) {
	whitelist, _ := reviewToolWhitelist(context.Background(), brReviewAgent(t, true, false), nil, true)
	if !whitelist["memory"] {
		t.Fatalf("whitelist %v", whitelist)
	}
}

func TestReviewToolWhitelistScopeMemoryDisabledProfileStaysMemoryFree(t *testing.T) {
	whitelist, _ := reviewToolWhitelist(context.Background(), brReviewAgent(t, false, false), nil, true)
	if whitelist["memory"] {
		t.Fatalf("whitelist %v", whitelist)
	}
}

func TestReviewToolWhitelistScopeDefaultScopeIsMemorylessFailClosed(t *testing.T) {
	whitelist, _ := reviewToolWhitelist(context.Background(), brReviewAgent(t, true, false), nil, false)
	if whitelist["memory"] {
		t.Fatalf("whitelist %v", whitelist)
	}
}

// Replaces the review worker with one recording the scope flags it was handed
func brCaptureWorker(t *testing.T) *map[string]bool {
	t.Helper()
	captured := map[string]bool{}
	prev := runReviewInThread
	runReviewInThread = func(_ context.Context, _ *AIAgent, _ []map[string]any, _ string, _ map[string]any, _ *backgroundReviewRun, reviewMemory, explicit bool) {
		captured["review_memory"], captured["explicit"] = reviewMemory, explicit
	}
	t.Cleanup(func() { runReviewInThread = prev })
	return &captured
}

func TestSpawnForwardsScopeTargetPassesReviewMemoryToWorker(t *testing.T) {
	captured := brCaptureWorker(t)
	agent := brBareAgent(t)
	for _, tc := range []struct{ memory, skills bool }{{false, true}, {true, false}} {
		target, _, err := spawnBackgroundReviewThread(context.Background(), agent, backgroundReviewKwargs{
			messagesSnapshot: []map[string]any{}, reviewMemory: tc.memory, reviewSkills: tc.skills}, nil)
		if err != nil {
			t.Fatal(err)
		}
		target(context.Background())
		if (*captured)["review_memory"] != tc.memory {
			t.Fatalf("review_memory %v for %+v", (*captured)["review_memory"], tc)
		}
	}
}

func TestExplicitRefineOriginTargetPassesExplicitToWorker(t *testing.T) {
	captured := brCaptureWorker(t)
	target, _, err := spawnBackgroundReviewThread(context.Background(), brBareAgent(t), backgroundReviewKwargs{
		messagesSnapshot: []map[string]any{}, reviewMemory: true, explicit: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	target(context.Background())
	if !(*captured)["explicit"] {
		t.Fatal("explicit not forwarded")
	}
}

func TestExplicitRefineOriginForkKeepsBackgroundReviewOriginAndCarriesAttendedness(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		rec := recordForks(t, func(fork *AIAgent) { fork.env = &tools.Env{Values: &sync.Map{}}; fork.memoryEnabled = true })
		prev := runReviewForkConversation
		runReviewForkConversation = func(context.Context, *AIAgent, string, []map[string]any) error { return nil }
		t.Cleanup(func() { runReviewForkConversation = prev })
		parent := reviewParentAgent()
		parent.activeChildrenLock = &sync.Mutex{}
		st := &reviewForkState{reviewUsage: map[string]any{}}
		if err := runReviewFork(context.Background(), parent, []map[string]any{}, "p", nil, nil, st, true, explicit); err != nil {
			t.Fatalf("explicit %v: %v", explicit, err)
		}
		if len(rec.forks) != 1 {
			t.Fatalf("explicit %v: %d forks", explicit, len(rec.forks))
		}
		fork := rec.forks[0]
		if fork.memoryWriteOrigin != backgroundReviewOrigin || fork.reviewAttended != explicit {
			t.Fatalf("explicit %v: origin %q attended %v", explicit, fork.memoryWriteOrigin, fork.reviewAttended)
		}
	}
}

// Memory store of the hermes _store fixture inside an isolated home with the review write origin bound
func brConsolidationStore(t *testing.T) (*memory.Store, *tools.Env) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root+"/home")
	store := memory.NewStore(root, 500, 300, true, true)
	if err := store.LoadFromDisk(); err != nil {
		t.Fatal(err)
	}
	env := &tools.Env{Home: root + "/home", Values: &sync.Map{}}
	return store, env
}

// Parsed memory tool answer
func brMemoryCall(t *testing.T, env *tools.Env, store *memory.Store, action, content, oldText string) (string, map[string]any) {
	t.Helper()
	raw := memory.MemoryTool(context.Background(), env, store, action, "memory", content, nilIfEmpty(oldText), nil, nil)
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("non JSON result %q", raw)
	}
	return raw, out
}

// nil for an empty string, the absent keyword of the hermes call
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Assistant tool call row of the review transcript
func brToolCall(id string, args map[string]any) map[string]any {
	raw, _ := json.Marshal(args)
	return map[string]any{"id": id, "function": map[string]any{"name": "memory", "arguments": string(raw)}}
}

// Reports a review action mentioning the staged proposal
func brStagedSurfaced(actions []string) bool {
	for _, a := range actions {
		if strings.Contains(a, "staged for your approval") {
			return true
		}
	}
	return false
}

func TestConsolidationProposalSurfacesStagedProposalSurfacesInSummary(t *testing.T) {
	store, env := brConsolidationStore(t)
	if r, err := store.Add("memory", "standing rule entry"); err != nil || !r.Success() {
		t.Fatalf("add %v %v", r, err)
	}
	skills.SetCurrentWriteOrigin(env, backgroundReviewOrigin)
	raw, result := brMemoryCall(t, env, store, "patch", "consolidated entry", "standing rule")
	if result["staged"] != true || result["proposal_staged"] != true {
		t.Fatalf("result %v", result)
	}
	reviewMessages := []map[string]any{
		{"role": "assistant", "tool_calls": []any{brToolCall("c1", map[string]any{"action": "patch", "old_text": "standing rule", "content": "consolidated entry"})}},
		{"role": "tool", "tool_call_id": "c1", "content": raw},
	}
	actions, err := summarizeBackgroundReviewActions(context.Background(), reviewMessages, []map[string]any{}, nil)
	if err != nil || !brStagedSurfaced(actions) {
		t.Fatalf("actions %v %v", actions, err)
	}
}

func TestConsolidationProposalSurfacesNearLimitDenialEndToEnd(t *testing.T) {
	store, env := brConsolidationStore(t)
	if r, err := store.Add("memory", "seed entry one"); err != nil || !r.Success() {
		t.Fatalf("seed %v %v", r, err)
	}
	if r, err := store.Add("memory", strings.Repeat("x", 600)); err != nil || r.Success() {
		t.Fatalf("near limit add accepted %v %v", r, err)
	}
	skills.SetCurrentWriteOrigin(env, backgroundReviewOrigin)
	addRaw, addResult := brMemoryCall(t, env, store, "add", strings.Repeat("y", 600), "")
	replaceRaw, replaceResult := brMemoryCall(t, env, store, "patch", "merged entry", "seed entry one")
	if addResult["success"] != false {
		t.Fatalf("add %v", addResult)
	}
	if replaceResult["staged"] != true || replaceResult["proposal_staged"] != true {
		t.Fatalf("replace %v", replaceResult)
	}
	entries := store.Entries("memory")
	if !containsEntry(entries, "seed entry one") || containsEntry(entries, "merged entry") {
		t.Fatalf("entries %v", entries)
	}
	reviewMessages := []map[string]any{
		{"role": "assistant", "tool_calls": []any{
			brToolCall("c1", map[string]any{"action": "add", "content": strings.Repeat("y", 600)}),
			brToolCall("c2", map[string]any{"action": "patch", "old_text": "seed entry one", "content": "merged entry"}),
		}},
		{"role": "tool", "tool_call_id": "c1", "content": addRaw},
		{"role": "tool", "tool_call_id": "c2", "content": replaceRaw},
	}
	actions, err := summarizeBackgroundReviewActions(context.Background(), reviewMessages, []map[string]any{}, nil)
	if err != nil || !brStagedSurfaced(actions) {
		t.Fatalf("actions %v %v", actions, err)
	}
}

// Reports whether an entry list holds the text
func containsEntry(entries []string, text string) bool {
	for _, e := range entries {
		if e == text {
			return true
		}
	}
	return false
}
