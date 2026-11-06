package codeexec

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

var configStamp atomic.Int64

// Writes the code_execution section over strict mode and a 30 second timeout, busting the load cache
func writeKernelConfig(t *testing.T, env *tools.Env, overrides map[string]any) {
	t.Helper()
	cfg := map[string]any{"mode": "strict", "timeout": 30}
	for k, v := range overrides {
		cfg[k] = v
	}
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("terminal:\n  backend: local\ncode_execution:\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %v\n", k, cfg[k])
	}
	path := filepath.Join(env.Home, "config.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(time.Duration(configStamp.Add(1)) * time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

// Local backend env with the host python as runtime and a clean kernel registry around the test
func kernelEnv(t *testing.T, overrides map[string]any) *tools.Env {
	t.Helper()
	env := testEnv(t)
	hostRuntime(t)
	t.Setenv("TERMINAL_ENV", "local")
	writeKernelConfig(t, env, overrides)
	ShutdownAllKernels()
	t.Cleanup(ShutdownAllKernels)
	return env
}

func decodeResult(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("result is not JSON: %v: %s", err, out)
	}
	return m
}

func runCode(t *testing.T, ctx context.Context, env *tools.Env, code string, reset bool) map[string]any {
	t.Helper()
	return decodeResult(t, executeCode(ctx, env, code, "kernel-test", nil, reset))
}

func runAs(t *testing.T, ctx context.Context, env *tools.Env, sessionKey, code, taskID string) map[string]any {
	t.Helper()
	tools.SetCurrentSessionKey(env, sessionKey)
	defer tools.ResetCurrentSessionKey(env)
	return decodeResult(t, executeCode(ctx, env, code, taskID, nil, false))
}

// Delegated child scope with its own session values and delegation session id
func childScope(env *tools.Env, childID string) (context.Context, *tools.Env) {
	child := *env
	child.Values = &sync.Map{}
	tools.BindSessionContext(&child, map[string]string{"ICEBATH_SESSION_ID": childID})
	return tools.WithDelegatedChildContext(context.Background(), true), &child
}

func liveKernels() []*sessionKernel {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	out := make([]*sessionKernel, 0, len(registry.kernels))
	for _, k := range registry.kernels {
		out = append(out, k)
	}
	return out
}

func registered(k *sessionKernel) bool {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return registry.kernels[k.key] == k
}

func waitKernelExit(t *testing.T, k *sessionKernel) {
	t.Helper()
	p := k.proc.Load()
	if p == nil {
		t.Fatal("kernel never spawned")
	}
	select {
	case <-p.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("kernel process did not exit")
	}
	if k.alive() {
		t.Fatal("kernel still alive")
	}
}

func outputOf(m map[string]any) string { s, _ := m["output"].(string); return s }
func errorOf(m map[string]any) string  { s, _ := m["error"].(string); return s }
func kernelField(m map[string]any, key string) any {
	info, _ := m["kernel"].(map[string]any)
	return info[key]
}

func wantStatus(t *testing.T, m map[string]any, status string) {
	t.Helper()
	if m["status"] != status {
		t.Fatalf("status %v, want %s: %v", m["status"], status, m)
	}
}

func TestStatePersistsAcrossCells(t *testing.T) {
	env := kernelEnv(t, nil)
	ctx := context.Background()
	first := runCode(t, ctx, env, "x = 41", false)
	wantStatus(t, first, "success")
	if kernelField(first, "reused") != false {
		t.Fatalf("first cell reused a kernel: %v", first)
	}
	second := runCode(t, ctx, env, "print(x + 1)", false)
	wantStatus(t, second, "success")
	if !strings.Contains(outputOf(second), "42") || kernelField(second, "reused") != true || kernelField(second, "execution_count") != 2.0 {
		t.Fatalf("state did not persist: %v", second)
	}
}

func TestResetDiscardsState(t *testing.T) {
	env := kernelEnv(t, nil)
	ctx := context.Background()
	runCode(t, ctx, env, "x = 41", false)
	second := runCode(t, ctx, env, "print(x + 1)", true)
	wantStatus(t, second, "error")
	if !strings.Contains(errorOf(second), "NameError") || kernelField(second, "state_reset") != true {
		t.Fatalf("reset kept state: %v", second)
	}
}

func TestExceptionKeepsTheKernelAlive(t *testing.T) {
	env := kernelEnv(t, nil)
	ctx := context.Background()
	runCode(t, ctx, env, "a = 7", false)
	boom := runCode(t, ctx, env, "1 / 0", false)
	wantStatus(t, boom, "error")
	if !strings.Contains(errorOf(boom), "ZeroDivisionError") {
		t.Fatalf("missing traceback: %v", boom)
	}
	after := runCode(t, ctx, env, "print(a)", false)
	wantStatus(t, after, "success")
	if !strings.Contains(outputOf(after), "7") || kernelField(after, "reused") != true {
		t.Fatalf("kernel died with the exception: %v", after)
	}
}

func TestImportsPersist(t *testing.T) {
	env := kernelEnv(t, nil)
	ctx := context.Background()
	runCode(t, ctx, env, "import json as _j", false)
	second := runCode(t, ctx, env, "print(_j.dumps({'k': 1}))", false)
	if !strings.Contains(outputOf(second), `{"k": 1}`) {
		t.Fatalf("import did not persist: %v", second)
	}
}

// Child mode host that spawns a kernel mid cell, prints its pid and waits to be killed
func kernelHostEntry() {
	if os.Getenv("CODEEXEC_KERNEL_HOST") != "1" {
		return
	}
	homeDir := os.Getenv("ICEBATH_HOME")
	rt := interpreter{Python: os.Getenv("CODEEXEC_KERNEL_PYTHON"), Prefix: os.Getenv("CODEEXEC_KERNEL_PREFIX")}
	runtimeInterpreter = func(context.Context, *tools.Env) (interpreter, error) { return rt, nil }
	env := &tools.Env{Home: homeDir, Config: config.Tree{}, Values: &sync.Map{}, Cwd: homeDir}
	k := newSessionKernel(kernelKey{owner: "parent-death"})
	if err := k.spawn(context.Background(), env, rt.Python, "", nil, 1); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cell, _ := json.Marshal(map[string]string{"id": "x", "code": "import os, time\n" +
		"assert 'ICEBATH_KERNEL_PARENT_PROCESS_HANDLE' not in os.environ\n" +
		"assert 'ICEBATH_KERNEL_PARENT_DEATH_FD' not in os.environ\n" +
		"time.sleep(300)"})
	_, _ = k.stdin.Write(append(cell, '\n'))
	fmt.Println(k.proc.Load().p.Pid)
	time.Sleep(600 * time.Second)
	os.Exit(0)
}

func TestKernelExitsWhenItsBackendParentDies(t *testing.T) {
	env := testEnv(t)
	rt := hostRuntime(t)
	host := exec.Command(os.Args[0], "-test.run=^$")
	host.Env = append(os.Environ(), "CODEEXEC_KERNEL_HOST=1", "ICEBATH_HOME="+env.Home,
		"CODEEXEC_KERNEL_PYTHON="+rt.Python, "CODEEXEC_KERNEL_PREFIX="+rt.Prefix)
	out, err := host.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = host.Process.Kill(); _ = host.Wait() }()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("host printed no kernel pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if !pidAlive(pid) {
		t.Fatal("kernel never came up")
	}
	_ = host.Process.Kill()
	_ = host.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for pidAlive(pid) {
		if time.Now().After(deadline) {
			killPid(pid)
			t.Fatal("session kernel survived its backend parent")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestTimeoutKillsTheKernelAndReportsStateLoss(t *testing.T) {
	env := kernelEnv(t, map[string]any{"timeout": 1})
	ctx := context.Background()
	slow := runCode(t, ctx, env, "import time\ntime.sleep(30)", false)
	wantStatus(t, slow, "timeout")
	if !strings.Contains(errorOf(slow), "state was lost") || len(liveKernels()) != 0 {
		t.Fatalf("timeout kept the kernel: %v", slow)
	}
	writeKernelConfig(t, env, nil)
	fresh := runCode(t, ctx, env, "print('alive')", false)
	wantStatus(t, fresh, "success")
	if kernelField(fresh, "reused") != false || !strings.Contains(outputOf(fresh), "alive") {
		t.Fatalf("no fresh kernel after timeout: %v", fresh)
	}
}

func TestSysExitEndsTheKernel(t *testing.T) {
	env := kernelEnv(t, nil)
	ctx := context.Background()
	done := runCode(t, ctx, env, "import sys\nsys.exit(0)", false)
	if kernelField(done, "ended") != true || len(liveKernels()) != 0 {
		t.Fatalf("sys.exit did not end the kernel: %v", done)
	}
	fresh := runCode(t, ctx, env, "print('respawned')", false)
	if kernelField(fresh, "reused") != false || !strings.Contains(outputOf(fresh), "respawned") {
		t.Fatalf("no respawn after exit: %v", fresh)
	}
}

func TestSubprocessFdOutputReachesTheResult(t *testing.T) {
	env := kernelEnv(t, nil)
	result := runCode(t, context.Background(), env, "import subprocess, sys\n"+
		"subprocess.run([sys.executable, '-c', \"print('raw-passthrough')\"])\n", false)
	wantStatus(t, result, "success")
	if !strings.Contains(outputOf(result), "raw-passthrough") {
		t.Fatalf("raw fd output lost: %v", result)
	}
}

func TestResetIsReachableFromAModelCallDespiteStaleKernelMode(t *testing.T) {
	env := kernelEnv(t, map[string]any{"kernel_mode": "per-call"})
	ctx := context.Background()
	schema := buildExecuteCodeSchema(env, nil, "strict")
	reset := schema["parameters"].(map[string]any)["properties"].(map[string]any)["reset"].(map[string]any)
	if reset["type"] != "boolean" {
		t.Fatalf("reset missing from schema: %v", reset)
	}
	call := func(args map[string]any) map[string]any {
		return decodeResult(t, executeCodeHandler(ctx, &tools.Call{Name: "execute_code", Args: args, TaskID: "kernel-test", Env: env}))
	}
	call(map[string]any{"code": "x = 41"})
	kept := call(map[string]any{"code": "print(x + 1)"})
	if !strings.Contains(outputOf(kept), "42") {
		t.Fatalf("state lost between handler calls: %v", kept)
	}
	wiped := call(map[string]any{"code": "print(x + 1)", "reset": true})
	wantStatus(t, wiped, "error")
	if !strings.Contains(errorOf(wiped), "NameError") {
		t.Fatalf("reset kept state: %v", wiped)
	}
}

func TestStateSurvivesAcrossTurnsOfOneConversation(t *testing.T) {
	env := kernelEnv(t, nil)
	ctx := context.Background()
	wantStatus(t, runAs(t, ctx, env, "conv-a", "x = 41", "turn-1"), "success")
	second := runAs(t, ctx, env, "conv-a", "print(x + 1)", "turn-2")
	wantStatus(t, second, "success")
	if !strings.Contains(outputOf(second), "42") || kernelField(second, "reused") != true {
		t.Fatalf("state lost across turns: %v", second)
	}
}

func TestSessionsAreIsolatedFromEachOther(t *testing.T) {
	env := kernelEnv(t, nil)
	ctx := context.Background()
	runAs(t, ctx, env, "conv-a", "x = 41", "turn-1")
	other := runAs(t, ctx, env, "conv-b", "print(x + 1)", "turn-1")
	wantStatus(t, other, "error")
	if !strings.Contains(errorOf(other), "NameError") {
		t.Fatalf("sessions shared a kernel: %v", other)
	}
}

func TestDelegatedChildrenGetTheirOwnKernels(t *testing.T) {
	env := kernelEnv(t, nil)
	ctx := context.Background()
	runAs(t, ctx, env, "conv-a", "parent_secret = 'p'", "turn-1")
	childCtx, child := childScope(env, "child-1")
	leak := runAs(t, childCtx, child, "conv-a", "print(globals().get('parent_secret', 'ISOLATED'))", "child-task")
	runAs(t, childCtx, child, "conv-a", "child_secret = 'c'", "child-task")
	back := runAs(t, ctx, env, "conv-a", "print(globals().get('child_secret', 'ISOLATED'))", "turn-2")
	if !strings.Contains(outputOf(leak), "ISOLATED") || !strings.Contains(outputOf(back), "ISOLATED") {
		t.Fatalf("state crossed the delegation boundary: %v %v", leak, back)
	}
}

func TestTwoDelegatedChildrenAreIsolatedFromEachOther(t *testing.T) {
	env := kernelEnv(t, nil)
	ctxA, childA := childScope(env, "child-A")
	runAs(t, ctxA, childA, "conv-a", "sibling_secret = 'A'", "t")
	ctxB, childB := childScope(env, "child-B")
	peek := runAs(t, ctxB, childB, "conv-a", "print(globals().get('sibling_secret', 'ISOLATED'))", "t")
	if !strings.Contains(outputOf(peek), "ISOLATED") {
		t.Fatalf("siblings shared a kernel: %v", peek)
	}
}

func TestLiveChildrenKeepTheirKernelsPastTheLRUCap(t *testing.T) {
	env := kernelEnv(t, map[string]any{"max_session_kernels": 2})
	type scope struct {
		ctx context.Context
		env *tools.Env
	}
	var scopes []scope
	for i := range 5 {
		ctx, child := childScope(env, fmt.Sprintf("child-%d", i))
		runAs(t, ctx, child, "conv", fmt.Sprintf("v = %d", i), fmt.Sprintf("child-%d", i))
		scopes = append(scopes, scope{ctx, child})
	}
	for i, s := range scopes {
		outcome := runAs(t, s.ctx, s.env, "conv", "print(v)", fmt.Sprintf("child-%d", i))
		wantStatus(t, outcome, "success")
		if kernelField(outcome, "reused") != true || !strings.Contains(outputOf(outcome), strconv.Itoa(i)) {
			t.Fatalf("live child %d lost its kernel: %v", i, outcome)
		}
	}
}

func TestFinishedChildrenReleaseTheirKernels(t *testing.T) {
	env := kernelEnv(t, nil)
	doneCtx, done := childScope(env, "child-done")
	runAs(t, doneCtx, done, "conv", "v = 1", "child-done")
	liveCtx, live := childScope(env, "child-live")
	runAs(t, liveCtx, live, "conv", "v = 2", "child-live")
	var doomed []*sessionKernel
	for _, k := range liveKernels() {
		if strings.HasSuffix(k.owner, "::child::child-done") {
			doomed = append(doomed, k)
		}
	}
	if len(doomed) != 1 {
		t.Fatalf("want one child-done kernel, got %d", len(doomed))
	}
	ShutdownKernelsForDelegatedChild("child-done")
	for _, k := range liveKernels() {
		if strings.HasSuffix(k.owner, "::child::child-done") {
			t.Fatal("finished child kernel still registered")
		}
	}
	waitKernelExit(t, doomed[0])
	still := runAs(t, liveCtx, live, "conv", "print(v)", "child-live")
	if !strings.Contains(outputOf(still), "2") {
		t.Fatalf("live child lost its kernel: %v", still)
	}
}

func TestSessionClearDisposesTheOwnersKernels(t *testing.T) {
	env := kernelEnv(t, nil)
	ctx := context.Background()
	runAs(t, ctx, env, "conv-a", "x = 41", "turn-1")
	kernels := liveKernels()
	if len(kernels) != 1 || !kernels[0].alive() {
		t.Fatalf("want one live kernel, got %d", len(kernels))
	}
	tools.ClearSession("conv-a")
	if len(liveKernels()) != 0 {
		t.Fatal("session clear kept the kernel")
	}
	waitKernelExit(t, kernels[0])
	after := runAs(t, ctx, env, "conv-a", "print('x' in dir())", "turn-2")
	wantStatus(t, after, "success")
	if !strings.Contains(outputOf(after), "False") {
		t.Fatalf("state survived the session clear: %v", after)
	}
}

func TestLiveKernelsAreCappedLRUAcrossOwners(t *testing.T) {
	env := kernelEnv(t, map[string]any{"max_session_kernels": 2})
	ctx := context.Background()
	seen := map[*sessionKernel]bool{}
	for i := range 4 {
		runAs(t, ctx, env, fmt.Sprintf("conv-%d", i), "x = 1", fmt.Sprintf("turn-%d", i))
		for _, k := range liveKernels() {
			seen[k] = true
		}
	}
	owners := map[string]bool{}
	for _, k := range liveKernels() {
		owners[k.owner] = true
	}
	if len(owners) != 2 || !owners["conv-2"] || !owners["conv-3"] {
		t.Fatalf("live owners %v, want conv-2 and conv-3", owners)
	}
	for k := range seen {
		if !registered(k) {
			waitKernelExit(t, k)
		}
	}
}

func TestIdleKernelsAreReaped(t *testing.T) {
	env := kernelEnv(t, map[string]any{"kernel_idle_timeout": 1})
	ctx := context.Background()
	runAs(t, ctx, env, "conv-a", "x = 41", "turn-1")
	stale := liveKernels()[0]
	time.Sleep(1200 * time.Millisecond)
	runAs(t, ctx, env, "conv-b", "y = 1", "turn-2")
	if registered(stale) {
		t.Fatal("idle kernel was not reaped")
	}
	waitKernelExit(t, stale)
}

func TestParallelCellsShareOneKernelProcess(t *testing.T) {
	env := kernelEnv(t, nil)
	var mu sync.Mutex
	var runners []*exec.Cmd
	prev := spawnOwned
	spawnOwned = func(cmd *exec.Cmd, purpose string) error {
		err := prev(cmd, purpose)
		if len(cmd.Args) == 2 && filepath.Base(cmd.Args[1]) == "icebath_kernel_runner.py" {
			mu.Lock()
			runners = append(runners, cmd)
			mu.Unlock()
		}
		return err
	}
	t.Cleanup(func() { spawnOwned = prev })
	results := make([]map[string]any, 6)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			tools.SetCurrentSessionKey(env, "conv-a")
			results[i] = decodeResult(t, executeCode(context.Background(), env, "import time; time.sleep(0.3)", "t", nil, false))
		})
	}
	wg.Wait()
	for _, r := range results {
		wantStatus(t, r, "success")
	}
	kernels := liveKernels()
	if len(kernels) != 1 || len(runners) != 1 {
		t.Fatalf("parallel cells spawned an unowned kernel: %d kernels, %d runners", len(kernels), len(runners))
	}
	if kernels[0].proc.Load().p.Pid != runners[0].Process.Pid || !kernels[0].alive() {
		t.Fatal("registered kernel is not the spawned runner")
	}
	ShutdownAllKernels()
	waitKernelExit(t, kernels[0])
}

// Records each registry dispatch with the task id and the approval asker it ran under
type dispatchRecord struct {
	tool, taskID string
	ask          tools.Asker
}

type namedAsker string

func (namedAsker) Ask(context.Context, []tools.Question) (tools.FormResult, error) {
	return tools.FormResult{}, nil
}

func recordDispatch(t *testing.T, reply func(name string) string) *[]dispatchRecord {
	t.Helper()
	var mu sync.Mutex
	seen := &[]dispatchRecord{}
	prev := handleFunctionCall
	handleFunctionCall = func(_ context.Context, call *tools.Call) string {
		mu.Lock()
		*seen = append(*seen, dispatchRecord{tool: call.Name, taskID: call.TaskID, ask: call.Env.Ask})
		mu.Unlock()
		return reply(call.Name)
	}
	t.Cleanup(func() { handleFunctionCall = prev })
	return seen
}

func TestIgnoredHelperErrorIsReportedForThatCellOnly(t *testing.T) {
	env := kernelEnv(t, nil)
	recordDispatch(t, func(name string) string {
		if name == "write_file" {
			return `{"error": "Refusing to overwrite a.py: never read"}`
		}
		return `{"ok": true}`
	})
	ctx := context.Background()
	failed := runCode(t, ctx, env, "import icebath_tools\nicebath_tools.write_file('a.py', 'x')\nprint('done')\n", false)
	clean := runCode(t, ctx, env, "import icebath_tools\nicebath_tools.web_search(query='q')\n", false)
	wantStatus(t, failed, "success")
	got, _ := json.Marshal(failed["tool_errors"])
	if string(got) != `[{"error":"Refusing to overwrite a.py: never read","tool":"write_file"}]` {
		t.Fatalf("tool_errors %s", got)
	}
	if _, ok := clean["tool_errors"]; ok {
		t.Fatalf("tool error leaked into the next cell: %v", clean)
	}
}

func TestALaterCellsRPCRunsUnderThatCellsAuthority(t *testing.T) {
	env := kernelEnv(t, nil)
	seen := recordDispatch(t, func(string) string { return `{"ok": true}` })
	cell := "import icebath_tools\nicebath_tools.web_search(query='q')\n"
	ctx := context.Background()
	env.Ask = namedAsker("one")
	first := runCode(t, ctx, env, cell, false)
	env.Ask = namedAsker("two")
	second := runCode(t, ctx, env, cell, false)
	wantStatus(t, first, "success")
	wantStatus(t, second, "success")
	if len(*seen) != 2 || (*seen)[0].ask != namedAsker("one") || (*seen)[1].ask != namedAsker("two") || (*seen)[0].taskID != "kernel-test" {
		t.Fatalf("dispatches %+v", *seen)
	}
}

func TestCrossCellAliasDispatchesUnderTheCurrentCell(t *testing.T) {
	env := kernelEnv(t, nil)
	seen := recordDispatch(t, func(string) string { return `{"ok": true}` })
	ctx := context.Background()
	env.Ask = namedAsker("one")
	first := runCode(t, ctx, env, "import icebath_tools\nalias = icebath_tools.web_search\n", false)
	env.Ask = namedAsker("two")
	second := runCode(t, ctx, env, "alias(query='q')\n", false)
	wantStatus(t, first, "success")
	wantStatus(t, second, "success")
	if len(*seen) != 1 || (*seen)[0].ask != namedAsker("two") {
		t.Fatalf("dispatches %+v", *seen)
	}
}

func TestASettledCellsAuthorityRefusesDispatch(t *testing.T) {
	authority := newCellAuthority(context.Background(), testEnv(t), "turn-1")
	authority.retire()
	if result := authority.dispatch("web_search", map[string]any{"query": "q"}); !strings.Contains(result, "No active execute_code cell") {
		t.Fatalf("retired authority dispatched: %s", result)
	}
}

func TestEachCellInstallsAFreshAuthority(t *testing.T) {
	env := kernelEnv(t, nil)
	ctx := context.Background()
	runCode(t, ctx, env, "x = 1", false)
	kernel := liveKernels()[0]
	first := kernel.authority.Load()
	if first.active.Load() {
		t.Fatal("settled cell authority still active")
	}
	runCode(t, ctx, env, "y = 2", false)
	if kernel.authority.Load() == first || kernel.authority.Load().active.Load() {
		t.Fatal("cell did not install and retire a fresh authority")
	}
}

func TestReapOnceSweepsIdleKernelsWithoutANewAcquire(t *testing.T) {
	env := kernelEnv(t, map[string]any{"kernel_idle_timeout": 1})
	runAs(t, context.Background(), env, "conv-a", "x = 41", "turn-1")
	stale := liveKernels()[0]
	time.Sleep(1200 * time.Millisecond)
	if err := reapOnce(env); err != nil {
		t.Fatal(err)
	}
	if registered(stale) {
		t.Fatal("reaper kept the idle kernel")
	}
	waitKernelExit(t, stale)
}

func TestReapOnceSparesAttachedAndFreshKernels(t *testing.T) {
	env := kernelEnv(t, map[string]any{"kernel_idle_timeout": 1})
	fresh := runAs(t, context.Background(), env, "conv-fresh", "x = 1", "turn-1")
	wantStatus(t, fresh, "success")
	kernel := liveKernels()[0]
	registry.mu.Lock()
	kernel.attached++
	registry.mu.Unlock()
	defer func() {
		registry.mu.Lock()
		kernel.attached--
		registry.mu.Unlock()
	}()
	if err := reapOnce(env); err != nil {
		t.Fatal(err)
	}
	if !registered(kernel) || !kernel.alive() {
		t.Fatal("reaper took an attached kernel")
	}
}

func TestWeekOldKernelDirsGoAndFreshOnesStay(t *testing.T) {
	tmp := t.TempDir()
	prev := gettempdir
	gettempdir = func() (string, error) { return tmp, nil }
	t.Cleanup(func() { gettempdir = prev })
	old, young, bystander := filepath.Join(tmp, "icebath_kernel_old"), filepath.Join(tmp, "icebath_kernel_young"), filepath.Join(tmp, "unrelated_dir")
	for _, p := range []string{old, young, bystander} {
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	weekAndABit := time.Now().Add(-8 * 86400 * time.Second)
	if err := os.Chtimes(old, weekAndABit, weekAndABit); err != nil {
		t.Fatal(err)
	}
	if removed, err := sweepStaleStagingDirs(time.Now()); err != nil || removed != 1 {
		t.Fatalf("removed %d: %v", removed, err)
	}
	for p, want := range map[string]bool{old: false, young: true, bystander: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Fatalf("%s exists=%v", p, err == nil)
		}
	}
}
