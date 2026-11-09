package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	asyncOwnerDeathModeEnv   = "ICEBATH_TEST_ASYNC_OWNER_DEATH_MODE"
	asyncOwnerDeathMarkerEnv = "ICEBATH_TEST_ASYNC_OWNER_DEATH_MARKER"
)

// Parent of the test_async_delegation.py MagicMock parents, depth zero with the given session id
func asyncTestParent(env *tools.Env, sessionID string) *AIAgent {
	return &AIAgent{sessionID: sessionID, env: env}
}

// Installs the retirement double a detached dispatch needs and empties the completion queue around the test
func asyncDispatchFixture(t *testing.T) *tools.Env {
	t.Helper()
	env := delegationTestEnv(t, 0, false)
	if tools.AsyncDelegationRetirement == nil {
		tools.AsyncDelegationRetirement = reviewRetirementDouble{}
		t.Cleanup(func() { tools.AsyncDelegationRetirement = nil })
	}
	drain := func() {
		for {
			if _, ok := tools.DefaultProcessRegistry().CompletionQueue.GetNowait(); !ok {
				return
			}
		}
	}
	drain()
	t.Cleanup(drain)
	return env
}

// Credentials of model m with every other field None, the creds dict of the hermes tests
func asyncFakeCreds(t *testing.T) {
	t.Helper()
	prev := resolveDelegationCredentialsFn
	resolveDelegationCredentialsFn = func(context.Context, map[string]any, *AIAgent) (map[string]any, error) {
		return map[string]any{"model": "m", "provider": nil, "base_url": nil, "api_key": nil, "api_mode": nil, "command": nil, "args": nil}, nil
	}
	t.Cleanup(func() { resolveDelegationCredentialsFn = prev })
}

// Leaf children named s<index>, the build fake of the hermes tests
func asyncFakeChildren(t *testing.T) {
	t.Helper()
	prev := buildChildPreservingParentToolsFn
	buildChildPreservingParentToolsFn = func(_ context.Context, spec childBuildSpec) (*AIAgent, error) {
		return &AIAgent{delegateRole: "leaf", subagentID: fmt.Sprintf("s%d", spec.TaskIndex)}, nil
	}
	t.Cleanup(func() { buildChildPreservingParentToolsFn = prev })
}

func asyncCompletedEntry(i int, goal string) *pyjson.Object {
	return pyjson.ObjectOf("task_index", i, "status", "completed", "summary", "done: "+goal, "api_calls", 1,
		"duration_seconds", 0.1, "model", "m", "exit_reason", "completed")
}

// Child runs that wait on their gate before returning a completed entry
func asyncGatedChildren(t *testing.T, gates []chan struct{}) {
	t.Helper()
	prev := runSingleChildFn
	runSingleChildFn = func(_ context.Context, i int, goal string, _, _ *AIAgent, _ string, _, _ any) *pyjson.Object {
		select {
		case <-gates[i]:
		case <-time.After(60 * time.Second):
		}
		return asyncCompletedEntry(i, goal)
	}
	t.Cleanup(func() {
		for _, g := range gates {
			select {
			case <-g:
			default:
				close(g)
			}
		}
		runSingleChildFn = prev
	})
}

func makeGates(n int) []chan struct{} {
	gates := make([]chan struct{}, n)
	for i := range gates {
		gates[i] = make(chan struct{})
	}
	return gates
}

// Next completion event within timeout, nil when none arrives
func drainOneCompletion(timeout time.Duration) map[string]any {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if evt, ok := tools.DefaultProcessRegistry().CompletionQueue.GetNowait(); ok {
			return evt
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

func resultField(v any, key string) any {
	switch m := v.(type) {
	case map[string]any:
		return m[key]
	case *pyjson.Object:
		val, _ := m.Get(key)
		return val
	}
	return nil
}

func resultIndexes(t *testing.T, evt map[string]any) []int {
	t.Helper()
	raw, _ := evt["results"].([]any)
	var out []int
	for _, r := range raw {
		idx, ok := resultField(r, "task_index").(int)
		if !ok {
			t.Fatalf("task_index %v", resultField(r, "task_index"))
		}
		out = append(out, idx)
	}
	return out
}

func asyncDelegate(t *testing.T, env *tools.Env, parent *AIAgent, kwargs map[string]any) *pyjson.Object {
	t.Helper()
	out, err := delegateTask(tools.WithToolEnv(context.Background(), env), parent, kwargs)
	return dispatchPayload(t, out, err)
}

func TestDelegateTaskBackgroundRoutesAsyncAndDoesNotBlock(t *testing.T) {
	env := asyncDispatchFixture(t)
	asyncFakeCreds(t)
	asyncFakeChildren(t)
	gates := makeGates(1)
	asyncGatedChildren(t, gates)
	parsed := asyncDelegate(t, env, asyncTestParent(env, "sess"), map[string]any{"goal": "the real task", "context": "ctx", "background": true})
	if entryText(parsed, "status") != "dispatched" || entryText(parsed, "mode") != "background" || !strings.HasPrefix(entryText(parsed, "delegation_id"), "deleg_") {
		t.Fatalf("handle %v", parsed)
	}
	if !tools.DefaultProcessRegistry().CompletionQueue.Empty() || tools.AsyncDelegationActiveCount() != 1 {
		t.Fatalf("a completion exists already or active %d", tools.AsyncDelegationActiveCount())
	}
	close(gates[0])
	evt := drainOneCompletion(5 * time.Second)
	if evt == nil || evt["type"] != "async_delegation" || evt["is_batch"] != true {
		t.Fatalf("event %v", evt)
	}
	results, _ := evt["results"].([]any)
	if len(results) != 1 || resultField(results[0], "summary") != "done: the real task" {
		t.Fatalf("results %v", results)
	}
	if text := tools.FormatProcessNotification(evt); !strings.Contains(text, "the real task") {
		t.Fatalf("notification %q", text)
	}
}

func TestDelegateTaskBackgroundUsesLiveTuiAgentSessionId(t *testing.T) {
	env := asyncDispatchFixture(t)
	asyncFakeCreds(t)
	asyncFakeChildren(t)
	prev := runSingleChildFn
	runSingleChildFn = func(_ context.Context, i int, _ string, _, _ *AIAgent, _ string, _, _ any) *pyjson.Object {
		return pyjson.ObjectOf("task_index", i, "status", "completed", "summary", "done", "api_calls", 1, "duration_seconds", 0.1, "model", "m", "exit_reason", "completed")
	}
	t.Cleanup(func() { runSingleChildFn = prev })
	tools.SetCurrentSessionKey(env, "pre-compress-parent")
	tools.SetSessionVars(env, tools.SessionVars{Source: "tui", SessionKey: "pre-compress-parent", UISessionID: "origin-tab"})
	t.Cleanup(func() {
		tools.ResetCurrentSessionKey(env)
		tools.ClearSessionVars(env)
	})
	parsed := asyncDelegate(t, env, asyncTestParent(env, "post-compress-tip"), map[string]any{"goal": "bg task", "background": true})
	if entryText(parsed, "status") != "dispatched" {
		t.Fatalf("handle %v", parsed)
	}
	evt := drainOneCompletion(5 * time.Second)
	if evt == nil || evt["type"] != "async_delegation" || evt["session_key"] != "post-compress-tip" || evt["origin_ui_session_id"] != "origin-tab" {
		t.Fatalf("event %v", evt)
	}
}

// The _grouped_fanout helper, a gated background fan out returning the parsed handle
func groupedFanout(t *testing.T, env *tools.Env, tasks []any, gates []chan struct{}) *pyjson.Object {
	t.Helper()
	asyncFakeCreds(t)
	asyncFakeChildren(t)
	asyncGatedChildren(t, gates)
	return asyncDelegate(t, env, asyncTestParent(env, "sess"), map[string]any{"tasks": tasks, "background": true})
}

func TestUngroupedTaskCompletesAloneAndGroupCompletesTogether(t *testing.T) {
	env := asyncDispatchFixture(t)
	writeDelegationBlock(t, env.Home, "  independent_completions: true\n")
	gates := makeGates(4)
	tasks := []any{
		map[string]any{"goal": "review PR 1 thoroughly and report"},
		map[string]any{"goal": "compare approach A in detail", "group": "cmp"},
		map[string]any{"goal": "compare approach B in detail", "group": "cmp"},
		map[string]any{"goal": "review PR 2 thoroughly and report"},
	}
	handle := groupedFanout(t, env, tasks, gates)
	if entryText(handle, "status") != "dispatched" {
		t.Fatalf("handle %v", handle)
	}
	rawUnits, _ := handle.Get("units")
	units, _ := rawUnits.([]any)
	byGroup := map[string]any{}
	for _, u := range units {
		idx, _ := resultField(u, "task_indexes").([]any)
		var parts []string
		for _, i := range idx {
			parts = append(parts, fmt.Sprint(i))
		}
		byGroup[strings.Join(parts, ",")] = resultField(u, "group")
	}
	if len(byGroup) != 3 || byGroup["0"] != nil || byGroup["1,2"] != "cmp" || byGroup["3"] != nil {
		t.Fatalf("units %v", units)
	}
	close(gates[3])
	evt := drainOneCompletion(5 * time.Second)
	if evt == nil || fmt.Sprint(resultIndexes(t, evt)) != "[3]" || !strings.Contains(tools.FormatProcessNotification(evt), "review PR 2") {
		t.Fatalf("event %v", evt)
	}
	close(gates[1])
	if evt := drainOneCompletion(500 * time.Millisecond); evt != nil {
		t.Fatalf("half a group completed %v", evt)
	}
	close(gates[2])
	evt = drainOneCompletion(5 * time.Second)
	if evt == nil || evt["group"] != "cmp" || fmt.Sprint(resultIndexes(t, evt)) != "[1 2]" {
		t.Fatalf("event %v", evt)
	}
	close(gates[0])
	evt = drainOneCompletion(5 * time.Second)
	if evt == nil || fmt.Sprint(resultIndexes(t, evt)) != "[0]" {
		t.Fatalf("event %v", evt)
	}
}

func TestMultiTaskCallIsOneCompletionUnlessIndependentCompletions(t *testing.T) {
	env := asyncDispatchFixture(t)
	gates := makeGates(3)
	tasks := []any{
		map[string]any{"goal": "review PR 1 thoroughly and report"},
		map[string]any{"goal": "review PR 2 thoroughly and report", "group": "g"},
		map[string]any{"goal": "review PR 3 thoroughly and report", "group": "g"},
	}
	handle := groupedFanout(t, env, tasks, gates)
	if entryText(handle, "status") != "dispatched" || entryHas(handle, "units") {
		t.Fatalf("handle %v", handle)
	}
	close(gates[0])
	close(gates[1])
	if evt := drainOneCompletion(500 * time.Millisecond); evt != nil {
		t.Fatalf("two of three completed %v", evt)
	}
	close(gates[2])
	evt := drainOneCompletion(5 * time.Second)
	if evt == nil {
		t.Fatal("no completion")
	}
	idx := resultIndexes(t, evt)
	sort.Ints(idx)
	if fmt.Sprint(idx) != "[0 1 2]" {
		t.Fatalf("results %v", idx)
	}
}

// Producer children of the owner death tests, a group whose slow member outlives the owner or a one child unit held in finalize
func dispatchAsyncOwnerDeathChild() {
	mode := os.Getenv(asyncOwnerDeathModeEnv)
	if mode == "" {
		return
	}
	h := os.Getenv("ICEBATH_HOME")
	env := &tools.Env{Home: h, Config: config.Defaults(), Secrets: config.NewSecrets(h), Values: &sync.Map{}}
	if tools.AsyncDelegationRetirement == nil {
		tools.AsyncDelegationRetirement = reviewRetirementDouble{}
	}
	parent := &AIAgent{sessionID: "sess", env: env}
	buildChildPreservingParentToolsFn = func(_ context.Context, spec childBuildSpec) (*AIAgent, error) {
		return &AIAgent{delegateRole: "leaf", subagentID: fmt.Sprintf("s%d", spec.TaskIndex)}, nil
	}
	resolveDelegationCredentialsFn = func(context.Context, map[string]any, *AIAgent) (map[string]any, error) {
		return map[string]any{"model": "m", "provider": nil, "base_url": nil, "api_key": nil, "api_mode": nil, "command": nil, "args": nil}, nil
	}
	runSingleChildFn = func(_ context.Context, i int, goal string, _, _ *AIAgent, _ string, _, _ any) *pyjson.Object {
		if mode == "crash_group" && i == 1 {
			time.Sleep(600 * time.Second)
		}
		return asyncCompletedEntry(i, goal)
	}
	ctx := tools.WithToolEnv(context.Background(), env)
	switch mode {
	case "crash_group":
		_, _ = delegateTask(ctx, parent, map[string]any{"tasks": []any{
			map[string]any{"goal": "fast member of the group task", "group": "g"},
			map[string]any{"goal": "slow member of the group task", "group": "g"},
		}, "background": true})
		time.Sleep(2 * time.Second)
		os.Exit(1)
	case "held_finalize":
		finalizeChildResultsFn = func(context.Context, []*pyjson.Object, []map[string]any, []delegateChild, *AIAgent) {
			_ = os.WriteFile(os.Getenv(asyncOwnerDeathMarkerEnv), []byte("child-returned"), 0o600)
			time.Sleep(600 * time.Second)
		}
		_, _ = delegateTask(ctx, parent, map[string]any{"tasks": []any{map[string]any{"goal": "single background subagent"}}, "background": true})
		time.Sleep(600 * time.Second)
	}
	os.Exit(0)
}

func ownerDeathProducer(t *testing.T, env *tools.Env, mode string, extra ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(append(os.Environ(), "ICEBATH_HOME="+env.Home, asyncOwnerDeathModeEnv+"="+mode), extra...)
	return cmd
}

func restoreOneCompletion(t *testing.T, env *tools.Env) map[string]any {
	t.Helper()
	q := tools.NewEventQueue()
	if _, err := tools.RestoreUndeliveredCompletions(env, q); err != nil {
		t.Fatal(err)
	}
	evt, ok := q.GetNowait()
	if !ok {
		t.Fatal("no completion restored")
	}
	return evt
}

func TestChildFinishedBeforeCrashIsRecoveredWithItsResult(t *testing.T) {
	env := asyncDispatchFixture(t)
	producer := ownerDeathProducer(t, env, "crash_group")
	out, err := producer.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("producer %v\n%s", err, out)
	}
	evt := restoreOneCompletion(t, env)
	raw, _ := evt["results"].([]any)
	byIndex := map[int]any{}
	for _, r := range raw {
		idx, _ := resultField(r, "task_index").(int)
		byIndex[idx] = r
	}
	if resultField(byIndex[0], "status") != "completed" || resultField(byIndex[0], "summary") != "done: fast member of the group task" || resultField(byIndex[1], "status") != "unknown" {
		t.Fatalf("results %v", raw)
	}
	errText, _ := evt["error"].(string)
	if !strings.Contains(errText, "1/2 child results were recorded") || !strings.Contains(tools.FormatProcessNotification(evt), "done: fast member") {
		t.Fatalf("event %v", evt)
	}
}

func TestOneChildUnitKeepsItsFinishedChildWhenTheOwnerDies(t *testing.T) {
	env := asyncDispatchFixture(t)
	marker := filepath.Join(t.TempDir(), "child-returned.flag")
	producer := ownerDeathProducer(t, env, "held_finalize", asyncOwnerDeathMarkerEnv+"="+marker)
	if err := producer.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- producer.Wait() }()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case <-exited:
			t.Fatal("the producer exited before its child returned")
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		_ = producer.Process.Kill()
		<-exited
		t.Fatal("the child never returned, so the death window was never reached")
	}
	_ = producer.Process.Kill()
	<-exited
	time.Sleep(300 * time.Millisecond)
	evt := restoreOneCompletion(t, env)
	raw, _ := evt["results"].([]any)
	if len(raw) != 1 || resultField(raw[0], "status") != "completed" || resultField(raw[0], "summary") != "done: single background subagent" {
		t.Fatalf("results %v", raw)
	}
	if errText, _ := evt["error"].(string); !strings.Contains(errText, "1/1 child results were recorded") {
		t.Fatalf("event %v", evt)
	}
}
