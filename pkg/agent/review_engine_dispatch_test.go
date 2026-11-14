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

// Always open retirement fence standing in for the provider wiring
type reviewRetirementDouble struct{}

func (reviewRetirementDouble) Work() (bool, func()) { return true, func() {} }
func (reviewRetirementDouble) Acquire() bool        { return true }
func (reviewRetirementDouble) Release()             {}

// Review dispatch fixture, the fake parent of the hermes tests over a temp home with the child seams swapped
func reviewDispatchParent(t *testing.T, built *childBuildSpec, summary string) (*tools.Env, *AIAgent) {
	t.Helper()
	if tools.AsyncDelegationRetirement == nil {
		tools.AsyncDelegationRetirement = reviewRetirementDouble{}
		t.Cleanup(func() { tools.AsyncDelegationRetirement = nil })
	}
	tools.ResetAsyncDelegationForTests()
	for {
		if _, ok := tools.DefaultProcessRegistry().CompletionQueue.GetNowait(); !ok {
			break
		}
	}
	env := &tools.Env{Home: t.TempDir(), Values: &sync.Map{}}
	t.Setenv("ICEBATH_HOME", env.Home)
	parent := &AIAgent{sessionID: "review-parent-sess", env: env, quietMode: true, activeChildrenLock: &sync.Mutex{}}
	prevRun, prevBuild, prevCreds := runSingleChildFn, buildChildAgentFn, resolveDelegationCredentialsFn
	t.Cleanup(func() {
		runSingleChildFn, buildChildAgentFn, resolveDelegationCredentialsFn = prevRun, prevBuild, prevCreds
		deadline := time.Now().Add(2 * time.Second)
		for tools.AsyncDelegationActiveCount() > 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		tools.ResetAsyncDelegationForTests()
	})
	runSingleChildFn = func(_ context.Context, taskIndex int, _ string, _, _ *AIAgent, _ string, _, _ any) *pyjson.Object {
		return pyjson.ObjectOf("task_index", taskIndex, "status", "completed", "summary", summary, "api_calls", 2,
			"duration_seconds", 0.1, "model", "m", "exit_reason", "completed")
	}
	buildChildAgentFn = func(_ context.Context, spec childBuildSpec) (*AIAgent, error) {
		*built = spec
		return &AIAgent{delegateRole: "leaf"}, nil
	}
	resolveDelegationCredentialsFn = func(context.Context, map[string]any, *AIAgent) (map[string]any, error) {
		return map[string]any{"model": "m", "provider": nil, "base_url": nil, "api_key": nil, "api_mode": nil, "command": nil, "args": nil}, nil
	}
	return env, parent
}

func awaitReviewCompletion(t *testing.T) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if evt, ok := tools.DefaultProcessRegistry().CompletionQueue.GetNowait(); ok {
			if evt["type"] == "async_delegation" {
				return evt
			}
			continue
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no async_delegation completion reached the shared queue")
	return nil
}

func firstResultSummary(t *testing.T, evt map[string]any) string {
	t.Helper()
	switch results := evt["results"].(type) {
	case []any:
		if m, ok := results[0].(map[string]any); ok {
			return pyjsonStr(m["summary"])
		}
		if o, ok := results[0].(*pyjson.Object); ok {
			v, _ := o.Get("summary")
			return pyjsonStr(v)
		}
	case []map[string]any:
		return pyjsonStr(results[0]["summary"])
	case []*pyjson.Object:
		v, _ := results[0].Get("summary")
		return pyjsonStr(v)
	}
	t.Fatalf("results %T %v", evt["results"], evt["results"])
	return ""
}

func pyjsonStr(v any) string {
	s, _ := v.(string)
	return s
}

func TestStartReviewDispatchesBackgroundAndCompletes(t *testing.T) {
	var built childBuildSpec
	env, parent := reviewDispatchParent(t, &built, "REVIEW: looks good")
	msgs := []map[string]any{
		{"role": "user", "content": "open a PR for the fix"},
		{"role": "assistant", "content": "PR #77 opened: https://x/pull/77"},
	}
	result, err := tools.StartReview(context.Background(), env, parent, msgs, "check the tests")
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := result.Get("status"); status != "dispatched" {
		t.Fatalf("result %v", result)
	}
	contextText, _ := built.Context.(string)
	goal, _ := built.Goal.(string)
	if !strings.Contains(contextText, "PR #77 opened") || !strings.Contains(contextText, "check the tests") || !strings.HasPrefix(goal, "Review: ") || !strings.Contains(contextText, tools.ReviewGoal) {
		t.Fatalf("goal %q context %q", goal, contextText)
	}
	evt := awaitReviewCompletion(t)
	if got := firstResultSummary(t, evt); got != "REVIEW: looks good" {
		t.Fatalf("summary %q in %v", got, evt)
	}
}

func TestStartReviewThreadsLoadedSkillsIntoContext(t *testing.T) {
	var built childBuildSpec
	env, parent := reviewDispatchParent(t, &built, "ok")
	parent.ephemeralSystemPrompt = `session with the "hermes-agent-dev" skill preloaded.`
	msgs := []map[string]any{
		{"role": "user", "content": "open a PR"},
		{"role": "assistant", "content": "PR #9 opened"},
	}
	result, err := tools.StartReview(context.Background(), env, parent, msgs, "")
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := result.Get("status"); status != "dispatched" {
		t.Fatalf("result %v", result)
	}
	contextText, _ := built.Context.(string)
	if !strings.Contains(contextText, "hermes-agent-dev") || !strings.Contains(contextText, "skill_view") {
		t.Fatalf("context %q", contextText)
	}
	awaitReviewCompletion(t)
}
