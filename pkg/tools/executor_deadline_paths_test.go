package tools

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestRegisteredDeadlineSurvivesSequentialExecutionAndBridge(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	names := []string{"deadline_probe_slow", "deadline_probe_ordinary"}
	var mu sync.Mutex
	var release, finished chan struct{}
	blocked := func(context.Context, *Call) string {
		mu.Lock()
		rel, fin := release, finished
		mu.Unlock()
		timer := time.AfterFunc(2*time.Second, func() {
			mu.Lock()
			defer mu.Unlock()
			select {
			case <-rel:
			default:
				close(rel)
			}
		})
		defer timer.Stop()
		select {
		case <-rel:
		case <-time.After(15 * time.Second):
		}
		close(fin)
		return "released"
	}
	for i, name := range names {
		d := Def{Name: name, Toolset: "deadline-probe", Schema: map[string]any{"name": name, "description": "Wait for the test-owned event.",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}, Handler: blocked}
		if i == 0 {
			d.Timeout = 20 * time.Second
		}
		Default.Register(d)
	}
	defer func() {
		for _, name := range names {
			_ = Default.Deregister(name, "", "")
		}
	}()
	host := &fakeExecutorHost{env: newTestToolEnv(t), enabled: []string{"deadline-probe"}}
	host.env.Config = config.Tree{"timeouts": map[string]any{"tools": map[string]any{"sequential_call": 1}}}
	e := NewExecutor(host, NewDispatcher(Default))
	cases := []struct {
		name     string
		args     string
		succeeds bool
	}{
		{names[0], "{}", true},
		{ToolCallName, `{"calls": [{"name": "deadline_probe_slow", "arguments": {}}]}`, true},
		{names[1], "{}", false},
	}
	for i, c := range cases {
		mu.Lock()
		release, finished = make(chan struct{}), make(chan struct{})
		rel, fin := release, finished
		mu.Unlock()
		var messages []*state.Message
		call := state.ToolCall{ID: "deadline-" + string(rune('0'+i)), Type: "function", Function: state.ToolCallFunction{Name: c.name, Arguments: c.args}}
		err := e.ExecuteToolCallsSequential(context.Background(), []state.ToolCall{call}, &messages, "deadline-probe", false)
		result := ""
		if len(messages) > 0 {
			result = messageText(messages[len(messages)-1])
		}
		mu.Lock()
		select {
		case <-rel:
		default:
			close(rel)
		}
		mu.Unlock()
		select {
		case <-fin:
		case <-time.After(5 * time.Second):
			t.Fatalf("case %d tool never finished", i)
		}
		if err != nil {
			t.Fatalf("case %d %v", i, err)
		}
		if c.succeeds && (!strings.Contains(result, "released") || strings.Contains(result, "timed out")) {
			t.Fatalf("case %d result %q", i, result)
		}
		if !c.succeeds && !strings.Contains(result, "timed out after 1.0s") {
			t.Fatalf("case %d result %q", i, result)
		}
	}
}

func TestExtendedDeadlineIsABarrierForDirectAndBridgedBatches(t *testing.T) {
	env := newTestToolEnv(t)
	ctx := context.Background()
	name := ToolDescribeName
	old := Default.Entry(ctx, env, name)
	Default.Register(Def{Name: name, Toolset: "deadline-probe", Schema: map[string]any{"name": name}, Override: true, Timeout: 1000 * time.Second,
		Handler: func(context.Context, *Call) string { return "ok" }})
	defer func() {
		current := Default.Entry(ctx, env, name)
		if current == nil {
			t.Fatal("override vanished")
		}
		Default.RestoreRegistration(name, current, old, "")
	}()
	for _, c := range []struct{ exposed, args string }{{name, "{}"}, {ToolCallName, `{"calls": [{"name": "tool_describe", "arguments": {}}]}`}} {
		call := state.ToolCall{Function: state.ToolCallFunction{Name: c.exposed, Arguments: c.args}}
		if adm := batchAdmission(ctx, env, call, ""); adm != nil {
			t.Fatalf("%s admitted to a parallel batch %+v", c.exposed, adm)
		}
	}
}

func TestRelativeFileCheckpointUsesTaskWorkspaceThroughExecutor(t *testing.T) {
	f := newCPFixture(t)
	processCwd := filepath.Join(f.tmp, "opt", "icebath")
	workspace := filepath.Join(f.tmp, "opt", "data", "workspace")
	f.write(filepath.Join(processCwd, "pyproject.toml"), "[project]\nname = 'icebath'\n")
	f.write(filepath.Join(workspace, "pyproject.toml"), "[project]\nname = 'workspace'\n")
	f.write(filepath.Join(workspace, "existing.txt"), "before\n")
	f.env.Cwd = processCwd
	t.Setenv("TERMINAL_CWD", workspace)
	m := f.mgr(true, 20, 500, 10)
	e := NewExecutor(&fakeExecutorHost{env: f.env, ckptMgr: m}, NewDispatcher(NewRegistry()))
	e.beginToolExecution(&ToolCallRef{Name: "write_file", Args: map[string]any{"path": "test_permissions2.txt"}, TaskID: "gateway-session", CallID: "c1"}, 0)
	if len(m.ListCheckpoints(workspace)) == 0 || len(m.ListCheckpoints(processCwd)) != 0 {
		t.Fatal("checkpoint taken for the wrong directory")
	}
}

func TestContainerBackendTaskLeavesHostStoreUntouchedThroughExecutor(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	hostFile := filepath.Join(f.tmp, "workspace", "project", "a.txt")
	f.write(hostFile, "host content\n")
	container := "container-checkpoint-task"
	termPool.Lock()
	termPool.active[container] = &fakePathEnv{envType: "docker"}
	termPool.Unlock()
	t.Cleanup(func() {
		termPool.Lock()
		delete(termPool.active, container)
		termPool.Unlock()
	})
	e := NewExecutor(&fakeExecutorHost{env: f.env, ckptMgr: m}, NewDispatcher(NewRegistry()))
	e.beginToolExecution(&ToolCallRef{Name: "write_file", Args: map[string]any{"path": hostFile}, TaskID: container, CallID: "c0"}, 0)
	e.beginToolExecution(&ToolCallRef{Name: "terminal", Args: map[string]any{"command": "rm -rf " + filepath.Dir(hostFile)}, TaskID: container, CallID: "call-1"}, 0)
	m.RecordLandedWrites([]string{hostFile}, container)
	if len(m.ListCheckpoints(filepath.Dir(hostFile))) != 0 || f.exists(f.base) {
		t.Fatal("container task touched the host store")
	}
	e.beginToolExecution(&ToolCallRef{Name: "write_file", Args: map[string]any{"path": hostFile}, TaskID: "local-task", CallID: "c2"}, 0)
	if len(m.ListCheckpoints(filepath.Dir(hostFile))) != 1 {
		t.Fatal("local task not checkpointed")
	}
}

// Executor whose relay runs the icebath pipeline on its own goroutine like relay_tools.execute on a pool thread
func relayGoroutineExecutor(t *testing.T, defs ...Def) (*Executor, *fakeExecutorHost) {
	e, host, _ := registryExecutor(t, defs...)
	e.Dispatcher.Relay = func(rctx context.Context, _ *Env, _ string, args map[string]any, _ ToolHookIDs, pipeline func(context.Context, map[string]any) (string, error)) (string, map[string]any, error) {
		type out struct {
			result string
			err    error
		}
		ch := make(chan out, 1)
		go func() {
			r, err := pipeline(rctx, args)
			ch <- out{r, err}
		}()
		o := <-ch
		return o.result, args, o.err
	}
	return e, host
}

func TestConcurrentToolCallbackHeartbeatReachesHostFromRelayGoroutine(t *testing.T) {
	e, host := relayGoroutineExecutor(t, handlerDef("image_generate", func(ctx context.Context, _ *Call) string {
		if cb := ActivityCallbackOf(ctx); cb != nil {
			cb("executing tool: image_generate")
		}
		return `{"ok": true}`
	}))
	calls := []state.ToolCall{{ID: "call-1", Type: "function", Function: state.ToolCallFunction{Name: "image_generate", Arguments: `{"prompt": "x"}`}}}
	var messages []*state.Message
	if err := e.ExecuteToolCallsConcurrent(context.Background(), calls, &messages, "task-1", true); err != nil {
		t.Fatal(err)
	}
	if n := host.touches("executing tool: image_generate"); n < 2 {
		t.Fatalf("tool callback heartbeat did not reach the host, %d stamps", n)
	}
}

func TestBeginToolExecutionInsideRelayToolStampsActivity(t *testing.T) {
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	e := NewExecutor(host, NewDispatcher(NewRegistry()))
	ref := &ToolCallRef{Name: "image_generate", Args: map[string]any{"prompt": "x"}, TaskID: "task-1", CallID: "call-1"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.beginToolExecution(ref, 1)
	}()
	<-done
	if host.touches("executing tool: image_generate") != 1 {
		t.Fatalf("activity %v", host.activity)
	}
	if len(host.currentTool) != 1 || host.currentTool[0] != "image_generate" {
		t.Fatalf("current tool %v", host.currentTool)
	}
}
