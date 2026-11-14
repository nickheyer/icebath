package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func intPtr(n int) *int { return &n }

// test_terminal_foreground_timeout_cap.py
func TestForegroundTimeoutAboveMaxIsPromotedToTrackedBackground(t *testing.T) {
	dir := t.TempDir()
	env := newTerminalTestEnv(t, dir)
	spawner := &fakeSpawner{}
	terminalRegistry = func() processSpawner { return spawner }
	cacheFakeEnv("default", newFakeTermEnv(dir, nil))
	r := runTerminal(t, env, TerminalRequest{Command: "echo x >> marker", Timeout: intPtr(9999)})
	if r["error"] != nil || r["output"] != "Background process started" || !strings.HasPrefix(resultString(r, "session_id"), "proc_") {
		t.Fatalf("%v", r)
	}
	if r["notify_on_complete"] != true {
		t.Fatalf("notify %v", r)
	}
	note := resultString(r, "promoted_from_foreground")
	mustContain(t, note, "9999")
	mustContain(t, note, fmt.Sprint(ForegroundMaxTimeout()))
	if len(spawner.commands) != 1 {
		t.Fatalf("spawned %v", spawner.commands)
	}
}

func TestZeroAndNegativeTimeoutRejected(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	for _, n := range []int{0, -1} {
		r := runTerminal(t, env, TerminalRequest{Command: "echo hi", Timeout: intPtr(n)})
		mustContain(t, resultString(r, "error"), "positive")
	}
}

func TestForegroundAllowsHelpVariantForServerCommand(t *testing.T) {
	dir := t.TempDir()
	env := newTerminalTestEnv(t, dir)
	e := newFakeTermEnv(dir, func(context.Context, string, ExecOptions) (*ExecResult, error) {
		return &ExecResult{Output: "usage"}, nil
	})
	cacheFakeEnv("default", e)
	r := runTerminal(t, env, TerminalRequest{Command: "pnpm dev --help"})
	if r["error"] != nil || e.calls[0].command != "pnpm dev --help" {
		t.Fatalf("%v %v", r, e.calls)
	}
}

func TestConfigDefaultAboveCapNotRejectedAndExactMaxRuns(t *testing.T) {
	dir := t.TempDir()
	env := newTerminalTestEnv(t, dir)
	cfg := localTestConfig(dir, map[string]any{"timeout": 900})
	envConfigFn = func(*Env) (map[string]any, error) { return cfg, nil }
	e := newFakeTermEnv(dir, nil)
	cacheFakeEnv("default", e)
	r := runTerminal(t, env, TerminalRequest{Command: "make build"})
	if r["error"] != nil || e.calls[0].opts.Timeout != 900 {
		t.Fatalf("%v %v", r, e.calls)
	}
	r = runTerminal(t, env, TerminalRequest{Command: "echo hello", Timeout: intPtr(ForegroundMaxTimeout())})
	if r["error"] != nil || e.calls[1].opts.Timeout != ForegroundMaxTimeout() {
		t.Fatalf("%v %v", r, e.calls)
	}
}

func TestOverCapTimeoutWithShellBackgroundingIsStillRefused(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	r := runTerminal(t, env, TerminalRequest{Command: "sleep 5 & echo started", Timeout: intPtr(9999)})
	mustContain(t, resultString(r, "error"), "'&' backgrounding")
	r2 := runTerminal(t, env, TerminalRequest{Command: "nohup make test", Timeout: intPtr(9999)})
	mustContain(t, resultString(r2, "error"), "nohup")
}

// test_terminal_none_command_guard.py
func TestNoneCommandReturnsCleanError(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	r := runTerminal(t, env, TerminalRequest{Command: nil})
	if r["exit_code"] != float64(-1) || r["status"] != "error" || resultString(r, "error") == "" {
		t.Fatalf("%v", r)
	}
}

// test_terminal_tool_exception_redaction.py
func TestGenericExceptionResultRedactsErrorAndTraceback(t *testing.T) {
	secret := "sk-proj-AbCdEf1234567890SecretValue999"
	env := newTerminalTestEnv(t, "")
	env.Secrets.Set("ICEBATH_REDACT_SECRETS", "1")
	envConfigFn = func(*Env) (map[string]any, error) { return nil, errors.New("connect failed OPENAI_API_KEY=" + secret) }
	r := runTerminal(t, env, TerminalRequest{Command: "echo hi"})
	if r["status"] != "error" || strings.Contains(resultString(r, "error"), secret) || strings.Contains(resultString(r, "traceback"), secret) {
		t.Fatalf("%v", r)
	}
	mustContain(t, resultString(r, "error"), "OPENAI_API_KEY=")
}

func TestDegradedFailModeResultRedactsErrorAndTraceback(t *testing.T) {
	secret := "sk-proj-AbCdEf1234567890SecretValue999"
	env := newTerminalTestEnv(t, "")
	env.Secrets.Set("ICEBATH_REDACT_SECRETS", "1")
	t.Setenv("TERMINAL_DEGRADED_MODE", "fail")
	ResetTerminalScopeCache()
	envConfigFn = func(*Env) (map[string]any, error) {
		return nil, NewEnvironmentConnectionError("ssh auth failed TOKEN="+secret, "retry later")
	}
	r := runTerminal(t, env, TerminalRequest{Command: "echo hi"})
	if r["status"] != "error" || strings.Contains(resultString(r, "error"), secret) || strings.Contains(resultString(r, "traceback"), secret) {
		t.Fatalf("%v", r)
	}
}

// test_terminal_degraded_mode.py with a backend that cannot connect
type unreachableBackend struct {
	localBackend
	name string
}

func (b unreachableBackend) Name() string { return b.name }
func (b unreachableBackend) CreateEnvironment(context.Context, *Env, CreateOptions) (Environment, error) {
	return nil, NewEnvironmentConnectionError("SSH connection failed: Connection refused", "")
}

func degradedEnv(t *testing.T) *Env {
	t.Helper()
	env := newTerminalTestEnv(t, "")
	if err := RegisterTerminalBackend(unreachableBackend{name: "deadbox"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RestoreTerminalBackend("deadbox", SnapshotTerminalBackend("deadbox"), nil) })
	cfg := localTestConfig(t.TempDir(), map[string]any{"env_type": "deadbox"})
	envConfigFn = func(*Env) (map[string]any, error) { return cfg, nil }
	return env
}

func TestUnreachableBackendReturnsDegradedResult(t *testing.T) {
	env := degradedEnv(t)
	r := runTerminal(t, env, TerminalRequest{Command: "echo hi", TaskID: "t-degraded"})
	if r["status"] != "degraded" || r["exit_code"] != float64(-1) || resultString(r, "reason") == "" || resultString(r, "retry_hint") == "" || resultHas(r, "traceback") {
		t.Fatalf("%v", r)
	}
	termPool.Lock()
	n := len(termPool.active)
	termPool.Unlock()
	if n != 0 {
		t.Fatal("degraded env cached")
	}
}

func TestRecoveryAfterDegraded(t *testing.T) {
	env := degradedEnv(t)
	if r := runTerminal(t, env, TerminalRequest{Command: "echo hi", TaskID: "t-recover"}); r["status"] != "degraded" {
		t.Fatalf("%v", r)
	}
	dir := t.TempDir()
	cfg := localTestConfig(dir, nil)
	envConfigFn = func(*Env) (map[string]any, error) { return cfg, nil }
	r := runTerminal(t, env, TerminalRequest{Command: "echo back", TaskID: "t-recover"})
	if r["exit_code"] != float64(0) || !strings.Contains(resultString(r, "output"), "back") {
		t.Fatalf("%v", r)
	}
	CleanupVM(env, "default", false)
}

func TestNonzeroExitAndCommandNotFoundAreNotDegraded(t *testing.T) {
	env := newTerminalTestEnv(t, t.TempDir())
	defer CleanupVM(env, "default", false)
	r := runTerminal(t, env, TerminalRequest{Command: "exit 3", TaskID: "t-exit3"})
	if r["exit_code"] != float64(3) || r["status"] == "degraded" {
		t.Fatalf("%v", r)
	}
	r = runTerminal(t, env, TerminalRequest{Command: "definitely_not_a_real_command_zzz_42", TaskID: "t-notfound"})
	if r["exit_code"] == float64(0) || r["status"] == "degraded" {
		t.Fatalf("%v", r)
	}
}

func TestFailModeReturnsErrorWithTraceback(t *testing.T) {
	env := degradedEnv(t)
	t.Setenv("TERMINAL_DEGRADED_MODE", "fail")
	ResetTerminalScopeCache()
	r := runTerminal(t, env, TerminalRequest{Command: "echo hi", TaskID: "t-fail"})
	if r["status"] != "error" || !resultHas(r, "traceback") {
		t.Fatalf("%v", r)
	}
	mustContain(t, resultString(r, "error"), "SSH connection failed")
}

func TestInvalidDegradedModeFallsBackToWarn(t *testing.T) {
	env := degradedEnv(t)
	t.Setenv("TERMINAL_DEGRADED_MODE", "bogus-value")
	ResetTerminalScopeCache()
	if r := runTerminal(t, env, TerminalRequest{Command: "echo hi", TaskID: "t-bogus"}); r["status"] != "degraded" {
		t.Fatalf("%v", r)
	}
}

// test_terminal_pre_guard_deadline.py
func stubPipeline(t *testing.T) *[]string {
	t.Helper()
	calls := &[]string{}
	preExecGuardMinTimeout = 0
	planExecutionFn = func(*Env, any, string, *int, bool, bool) (*execPlan, error) {
		return &execPlan{config: map[string]any{}, envType: "local", effectiveTaskID: "pre-guard-deadline-test", cwd: "/tmp", effectiveTimeout: 1}, nil
	}
	acquireEnvFn = func(context.Context, *Env, *execPlan, string) (Environment, error) {
		return newFakeTermEnv("/tmp", nil), nil
	}
	runApprovalGuardsFn = func(context.Context, *Env, string, string, map[string]any, bool) (approvalVerdict, error) {
		return approvalVerdict{}, nil
	}
	runForegroundFn = func(context.Context, *Env, string, Environment, *execPlan, string, string, string, string, string, bool, bool) (string, error) {
		*calls = append(*calls, "foreground")
		return "foreground-ran", nil
	}
	return calls
}

func TestWedgedPreExecutionGuardReturnsBoundedErrorWithoutRunning(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	calls := stubPipeline(t)
	preExecGuardMinTimeout = 50 * time.Millisecond
	planExecutionFn = func(*Env, any, string, *int, bool, bool) (*execPlan, error) {
		return &execPlan{config: map[string]any{}, envType: "local", effectiveTaskID: "pre-guard-deadline-test", cwd: "/tmp", effectiveTimeout: 0}, nil
	}
	preExecBlockFn = func(context.Context, *Env, string, Environment, string, string, string, string) error {
		time.Sleep(time.Second)
		return nil
	}
	start := time.Now()
	r := runTerminal(t, env, TerminalRequest{Command: "echo ok"})
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("guard wedged the tool")
	}
	if r["status"] != "error" || !strings.Contains(resultString(r, "error"), "did not finish") || len(*calls) != 0 {
		t.Fatalf("%v %v", r, *calls)
	}
}

func TestCompletedPreExecutionGuardVerdictsPassThrough(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	calls := stubPipeline(t)
	preExecBlockFn = func(context.Context, *Env, string, Environment, string, string, string, string) error { return nil }
	if out := TerminalTool(context.Background(), env, TerminalRequest{Command: "echo ok"}); out != "foreground-ran" {
		t.Fatal(out)
	}
	preExecBlockFn = func(context.Context, *Env, string, Environment, string, string, string, string) error {
		return &terminalRejected{`{"status":"blocked"}`}
	}
	if out := TerminalTool(context.Background(), env, TerminalRequest{Command: "echo ok"}); out != `{"status":"blocked"}` {
		t.Fatal(out)
	}
	if len(*calls) != 1 {
		t.Fatalf("%v", *calls)
	}
}

func TestPreExecutionGuardOnTheDeadlineWorkerSeesTheToolInterrupt(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	stubPipeline(t)
	var seen []bool
	preExecBlockFn = func(ctx context.Context, _ *Env, _ string, _ Environment, _, _, _, _ string) error {
		seen = append(seen, IsInterrupted(ctx))
		return nil
	}
	id := NewWorkerID()
	ctx := WithWorker(context.Background(), id)
	SetInterrupt(id, true, "")
	TerminalTool(ctx, env, TerminalRequest{Command: "echo ok"})
	SetInterrupt(id, false, "")
	if len(seen) != 1 || !seen[0] {
		t.Fatalf("guard blind to the interrupt: %v", seen)
	}
}

// test_terminal_task_cwd.py
func TestForegroundCommandUsesRegisteredTaskCwdForExistingEnvironment(t *testing.T) {
	env := newTerminalTestEnv(t, "/default")
	taskID := "acp-session-1"
	e := newFakeTermEnv("/default", nil)
	cacheFakeEnv(taskID, e)
	taskEnvOverrides.m[taskID] = map[string]any{"cwd": "/workspace/acp"}
	r := runTerminal(t, env, TerminalRequest{Command: "pwd", TaskID: taskID})
	if r["exit_code"] != float64(0) || len(e.calls) != 1 || e.calls[0].command != "pwd" || e.calls[0].opts.Cwd != "/workspace/acp" || e.calls[0].opts.Timeout != 180 || !e.calls[0].opts.BoundedCapture {
		t.Fatalf("%v %+v", r, e.calls)
	}
	r = runTerminal(t, env, TerminalRequest{Command: "pwd", TaskID: taskID, Workdir: "/explicit/workdir"})
	if r["exit_code"] != float64(0) || e.calls[1].opts.Cwd != "/explicit/workdir" {
		t.Fatalf("%v %+v", r, e.calls)
	}
	if cwd, _ := GetSessionCwd(env, taskID); cwd == "/explicit/workdir" {
		t.Fatal("transient workdir recorded")
	}
}

func TestBackgroundCommandPrefersRecordedSessionCwd(t *testing.T) {
	env := newTerminalTestEnv(t, "/workspace/init")
	taskID := "session-live-cwd-bg"
	cacheFakeEnv(taskID, newFakeTermEnv("/workspace/live", nil))
	taskEnvOverrides.m[taskID] = map[string]any{"cwd": "/workspace/init"}
	spawner := &fakeSpawner{}
	terminalRegistry = func() processSpawner { return spawner }
	RecordSessionCwd(env, taskID, "/workspace/live")
	r := runTerminal(t, env, TerminalRequest{Command: "sleep 1", TaskID: taskID, Background: true})
	if r["exit_code"] != float64(0) || len(spawner.local) != 1 || spawner.local[0].Cwd != "/workspace/live" || spawner.local[0].SessionKey != taskID {
		t.Fatalf("%v %+v", r, spawner.local)
	}
}

func TestHostLocalBackgroundCommandBypassesConfiguredBackend(t *testing.T) {
	dir := t.TempDir()
	env := newTerminalTestEnv(t, dir)
	cfg := map[string]any{"env_type": "docker", "docker_image": "python:3.11", "cwd": dir, "timeout": 60, "lifetime_seconds": 3600}
	envConfigFn = func(*Env) (map[string]any, error) { return cfg, nil }
	checkAllGuardsFn = func(_ context.Context, _ *Env, _, envType string, _ bool) GuardResult {
		return GuardResult{Approved: envType == "local"}
	}
	taskID := "bot-delivery"
	cacheFakeEnv("host-local-"+taskID, newFakeTermEnv(dir, nil))
	spawner := &fakeSpawner{nextID: "proc_host"}
	terminalRegistry = func() processSpawner { return spawner }
	r := runTerminal(t, env, TerminalRequest{Command: "host-runner", TaskID: taskID, Workdir: dir, Background: true, HostLocal: true})
	if resultString(r, "session_id") != "proc_host" || len(spawner.local) != 1 || !strings.HasPrefix(spawner.local[0].TaskID, "host-local-") || len(spawner.viaEnv) != 0 {
		t.Fatalf("%v %+v", r, spawner)
	}
}

func TestConcurrentCommandsRecordTheirOwnObservedCwd(t *testing.T) {
	root := t.TempDir()
	env := newTerminalTestEnv(t, root)
	cwdA, cwdB := filepath.Join(root, "a"), filepath.Join(root, "b")
	os.MkdirAll(cwdA, 0o755)
	os.MkdirAll(cwdB, 0o755)
	aSet, bSet := make(chan struct{}), make(chan struct{})
	shared := newFakeTermEnv(root, nil)
	shared.execute = func(_ context.Context, command string, _ ExecOptions) (*ExecResult, error) {
		observed := cwdB
		if command == "session-a" {
			observed = cwdA
			shared.SetCwd(observed)
			close(aSet)
			<-bSet
		} else {
			<-aSet
			shared.SetCwd(observed)
			close(bSet)
		}
		return &ExecResult{Output: "ok", CwdObserved: true, Cwd: observed}, nil
	}
	cacheFakeEnv("default", shared)
	RecordSessionCwd(env, "task-a", cwdA)
	RecordSessionCwd(env, "task-b", cwdB)
	var wg sync.WaitGroup
	results := map[string]map[string]any{}
	var mu sync.Mutex
	for _, pair := range [][2]string{{"session-a", "task-a"}, {"session-b", "task-b"}} {
		wg.Add(1)
		go func(name, task string) {
			defer wg.Done()
			r := runTerminal(t, env, TerminalRequest{Command: name, TaskID: task})
			mu.Lock()
			results[task] = r
			mu.Unlock()
		}(pair[0], pair[1])
	}
	wg.Wait()
	if results["task-a"]["exit_code"] != float64(0) || results["task-b"]["exit_code"] != float64(0) {
		t.Fatalf("%v", results)
	}
	if a, _ := GetSessionCwd(env, "task-a"); a != cwdA {
		t.Fatalf("task-a cwd %q", a)
	}
	if b, _ := GetSessionCwd(env, "task-b"); b != cwdB {
		t.Fatalf("task-b cwd %q", b)
	}
}

// test_terminal_error_redaction.py
const redactionSecret = "OPENAI_API_KEY=sk-testterminalerrorredaction1234567890"

func assertSecretRedacted(t *testing.T, r map[string]any, field string) {
	t.Helper()
	v := resultString(r, field)
	if strings.Contains(v, redactionSecret) || strings.Contains(v, "sk-testterminalerrorredaction1234567890") || !strings.Contains(v, "***") {
		t.Fatalf("%s not redacted: %q", field, v)
	}
}

func redactingEnv(t *testing.T) *Env {
	t.Helper()
	env := newTerminalTestEnv(t, "/tmp")
	env.Secrets.Set("ICEBATH_REDACT_SECRETS", "1")
	cfg := localTestConfig("/tmp", map[string]any{"timeout": 1})
	envConfigFn = func(*Env) (map[string]any, error) { return cfg, nil }
	return env
}

func TestForegroundRetryErrorRedactsExceptionText(t *testing.T) {
	env := redactingEnv(t)
	cacheFakeEnv("default", newFakeTermEnv("/tmp", func(context.Context, string, ExecOptions) (*ExecResult, error) {
		return nil, errors.New("backend failed with " + redactionSecret)
	}))
	done := make(chan map[string]any, 1)
	go func() { done <- runTerminal(t, env, TerminalRequest{Command: "echo ok"}) }()
	select {
	case r := <-done:
		if r["exit_code"] != float64(-1) {
			t.Fatalf("%v", r)
		}
		assertSecretRedacted(t, r, "error")
	case <-time.After(40 * time.Second):
		t.Fatal("retry loop did not finish")
	}
}

func TestSuccessfulOutputPreservesRedactionOptOut(t *testing.T) {
	env := newTerminalTestEnv(t, "/tmp")
	env.Secrets.Set("ICEBATH_REDACT_SECRETS", "0")
	cacheFakeEnv("default", newFakeTermEnv("/tmp", func(context.Context, string, ExecOptions) (*ExecResult, error) {
		return &ExecResult{Output: redactionSecret + "\n"}, nil
	}))
	r := runTerminal(t, env, TerminalRequest{Command: "echo ok"})
	if r["exit_code"] != float64(0) || r["output"] != redactionSecret {
		t.Fatalf("%v", r)
	}
}

func TestEnvironmentCreationImportErrorRedactsExceptionText(t *testing.T) {
	env := redactingEnv(t)
	createEnvironmentFn = func(context.Context, *Env, string, string, string, int, map[string]any, map[string]any, map[string]any, string, string, bool) (Environment, error) {
		return nil, &BackendMissingError{"backend import failed with " + redactionSecret}
	}
	r := runTerminal(t, env, TerminalRequest{Command: "echo ok"})
	if r["exit_code"] != float64(-1) || r["status"] != "disabled" {
		t.Fatalf("%v", r)
	}
	assertSecretRedacted(t, r, "error")
}

func TestBackgroundStartErrorRedactsExceptionText(t *testing.T) {
	env := redactingEnv(t)
	cacheFakeEnv("default", newFakeTermEnv("/tmp", nil))
	terminalRegistry = func() processSpawner {
		return &fakeSpawner{spawnErr: errors.New("spawn failed with " + redactionSecret)}
	}
	r := runTerminal(t, env, TerminalRequest{Command: "echo ok", Background: true})
	if r["exit_code"] != float64(-1) {
		t.Fatalf("%v", r)
	}
	assertSecretRedacted(t, r, "error")
}

func TestOuterExceptionRedactsErrorAndTraceback(t *testing.T) {
	env := redactingEnv(t)
	envConfigFn = func(*Env) (map[string]any, error) { return nil, errors.New("config failed with " + redactionSecret) }
	r := runTerminal(t, env, TerminalRequest{Command: "echo ok"})
	if r["exit_code"] != float64(-1) || r["status"] != "error" {
		t.Fatalf("%v", r)
	}
	assertSecretRedacted(t, r, "error")
	assertSecretRedacted(t, r, "traceback")
}

// test_terminal_background_spawn_failure.py
func TestFailedSandboxLaunchReturnsErrorNotStartedSession(t *testing.T) {
	env := newTerminalTestEnv(t, "/root")
	terminalRegistry = func() processSpawner { return &fakeSpawner{} }
	for _, tc := range []struct {
		outcome *ExecResult
		err     error
		reason  string
	}{
		{&ExecResult{Output: `exec: "bash": executable file not found in $PATH`, ReturnCode: 127}, nil, `exec: "bash": executable file not found`},
		{nil, errors.New("container 808ea027557c is not running"), "container 808ea027557c is not running"},
	} {
		e := newFakeTermEnv("/root", func(context.Context, string, ExecOptions) (*ExecResult, error) { return tc.outcome, tc.err })
		e.envType = "docker"
		raw := spawnBackgroundProcess(context.Background(), env, e, backgroundSpawnRequest{command: "sleep 30", envType: "docker", effectiveTaskID: "bg-fail", taskID: "bg-fail", cwd: "/root", notifyOnComplete: true})
		r := map[string]any{}
		jsonDecode(t, raw, &r)
		if r["exit_code"] != float64(-1) || resultHas(r, "session_id") || r["output"] == "Background process started" {
			t.Fatalf("%v", r)
		}
		mustContain(t, resultString(r, "error"), tc.reason)
	}
}

func jsonDecode(t *testing.T, raw string, out *map[string]any) {
	t.Helper()
	if err := jsonUnmarshalString(raw, out); err != nil {
		t.Fatalf("bad JSON %s", raw)
	}
}

// test_terminal_watcher_arming.py
func procSessionForWatcher() *ProcessSession {
	s := NewProcessSession("proc_active", "x")
	s.WatcherPlatform, s.WatcherChatID, s.WatcherUserID, s.WatcherUserName = "telegram", "123", "u", "Ada"
	return s
}

func TestLiveGatewayArmsWatcherAtRegistration(t *testing.T) {
	resetTerminalState(t)
	var armed []map[string]any
	ArmProcessWatcher = func(w map[string]any) bool { armed = append(armed, w); return true }
	spawner := &fakeSpawner{}
	terminalRegistry = func() processSpawner { return spawner }
	registerCompletionWatcher(procSessionForWatcher(), "agent:main:telegram:dm:123")
	if len(armed) != 1 || armed[0]["session_id"] != "proc_active" || armed[0]["notify_on_complete"] != true || armed[0]["chat_id"] != "123" || len(spawner.pending) != 0 {
		t.Fatalf("%v %v", armed, spawner.pending)
	}
}

func TestWatcherStaysPendingWithoutAServingGateway(t *testing.T) {
	resetTerminalState(t)
	spawner := &fakeSpawner{}
	terminalRegistry = func() processSpawner { return spawner }
	ArmProcessWatcher = nil
	registerCompletionWatcher(procSessionForWatcher(), "agent:main:telegram:dm:123")
	ArmProcessWatcher = func(map[string]any) bool { return false }
	registerCompletionWatcher(procSessionForWatcher(), "agent:main:telegram:dm:123")
	if len(spawner.pending) != 2 {
		t.Fatalf("%v", spawner.pending)
	}
}

// test_terminal_tool_pty_fallback.py
func TestCommandRequiresPipeStdinDetectsGhWithToken(t *testing.T) {
	if !commandRequiresPipeStdin("gh auth login --hostname github.com --git-protocol https --with-token") || commandRequiresPipeStdin("gh auth login --web") {
		t.Fatal("pipe stdin detection")
	}
}

func TestTerminalBackgroundKeepsPtyForRegularInteractiveCommands(t *testing.T) {
	env := newTerminalTestEnv(t, t.TempDir())
	cacheFakeEnv("default", newFakeTermEnv("/tmp", nil))
	spawner := &fakeSpawner{}
	terminalRegistry = func() processSpawner { return spawner }
	r := runTerminal(t, env, TerminalRequest{Command: "python3 -c \"print(input())\"", Background: true, Pty: true})
	if !spawner.local[0].UsePty || resultHas(r, "pty_note") {
		t.Fatalf("%v %+v", r, spawner.local)
	}
	r = runTerminal(t, env, TerminalRequest{Command: "gh auth login --with-token", Background: true, Pty: true})
	if spawner.local[1].UsePty || !resultHas(r, "pty_note") {
		t.Fatalf("%v %+v", r, spawner.local)
	}
}

// test_terminal_plugin_image_override.py
func TestPluginBackendReadsItsOwnPerTaskImageOverride(t *testing.T) {
	overrides := map[string]any{"docker_image": "python:3.12", "acmebox_image": "python:3.12"}
	if selectImage("acmebox", overrides, nil) != "python:3.12" || selectImage("acmebox", map[string]any{"docker_image": "python:3.12"}, nil) != "" ||
		selectImage("local", overrides, nil) != "" || selectImage("docker", overrides, map[string]any{"docker_image": "cfg"}) != "python:3.12" {
		t.Fatal("image selection")
	}
}

func TestPluginImageOverrideMarksAnIsolatedRollout(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	RegisterTaskEnvOverrides(env, "rollout-1", map[string]any{"acmebox_image": "python:3.12"})
	RegisterTaskEnvOverrides(env, "cwd-only", map[string]any{"cwd": "/work"})
	if !hasIsolationOverrides(env, "rollout-1") || hasIsolationOverrides(env, "cwd-only") {
		t.Fatal("isolation override predicate")
	}
}

// test_terminal_inflight_reap_guard.py
type reapingEnv struct {
	fakeTermEnv
	cleanedMid *bool
}

func sweepAfterLongCall(env *Env) {
	termPool.Lock()
	for k := range termPool.lastActivity {
		termPool.lastActivity[k] = time.Now().Add(-600 * time.Second)
	}
	termPool.Unlock()
	cleanupInactiveEnvs(env, 300*time.Second)
}

func TestForegroundCallKeepsItsEnvironmentPastTheLifetime(t *testing.T) {
	env := newTerminalTestEnv(t, "/tmp")
	terminalRegistry = func() processSpawner { return &fakeSpawner{} }
	e := &reapingEnv{}
	e.envType, e.cwd = "local", "/tmp"
	e.execute = func(context.Context, string, ExecOptions) (*ExecResult, error) {
		sweepAfterLongCall(env)
		mid := e.wasCleaned()
		e.cleanedMid = &mid
		return &ExecResult{Output: "OK"}, nil
	}
	termPool.Lock()
	termPool.active["default"] = e
	termPool.lastActivity["default"] = time.Now()
	termPool.Unlock()
	r := runTerminal(t, env, TerminalRequest{Command: "echo hi", TaskID: "session-42"})
	if e.cleanedMid == nil || *e.cleanedMid || r["exit_code"] != float64(0) {
		t.Fatalf("reaped mid call: %v", r)
	}
	termPool.Lock()
	still := termPool.active["default"] == Environment(e)
	termPool.Unlock()
	if !still {
		t.Fatal("env dropped during the call")
	}
	sweepAfterLongCall(env)
	if !e.wasCleaned() {
		t.Fatal("idle env not reaped after the call")
	}
}

func TestOverlappingCallsHoldTheEnvUntilTheLastExits(t *testing.T) {
	env := newTerminalTestEnv(t, "/tmp")
	terminalRegistry = func() processSpawner { return &fakeSpawner{} }
	e := newFakeTermEnv("/tmp", nil)
	cacheFakeEnv("default", e)
	release1 := callInFlight("default")
	release2 := callInFlight("default")
	release2()
	sweepAfterLongCall(env)
	termPool.Lock()
	kept := termPool.active["default"] == Environment(e)
	termPool.Unlock()
	if !kept {
		t.Fatal("env reaped while a call was still in flight")
	}
	release1()
	cleanupInactiveEnvs(env, 300*time.Second)
	termPool.Lock()
	kept = termPool.active["default"] == Environment(e)
	termPool.Unlock()
	if !kept || e.wasCleaned() {
		t.Fatal("countdown did not restart at the last exit")
	}
	sweepAfterLongCall(env)
	if !e.wasCleaned() {
		t.Fatal("idle env not reaped a lifetime after the last exit")
	}
}

func TestACallThatFailsReleasesItsEnvironmentToTheReaper(t *testing.T) {
	env := newTerminalTestEnv(t, "/tmp")
	terminalRegistry = func() processSpawner { return &fakeSpawner{} }
	e := newFakeTermEnv("/tmp", nil)
	cacheFakeEnv("default", e)
	runApprovalGuardsFn = func(context.Context, *Env, string, string, map[string]any, bool) (approvalVerdict, error) {
		return approvalVerdict{}, errors.New("boom")
	}
	runTerminal(t, env, TerminalRequest{Command: "echo hi", TaskID: "session-42"})
	sweepAfterLongCall(env)
	if !e.wasCleaned() {
		t.Fatal("failed call kept the env held")
	}
}

// test_terminal_backend_unavailable_reason.py and test_terminal_requirements.py
type failingBackend struct {
	localBackend
	name string
	err  error
}

func (b failingBackend) Name() string                                 { return b.name }
func (b failingBackend) CheckRequirements(*Env, map[string]any) error { return b.err }

func registerTestBackend(t *testing.T, p BackendProvider) {
	t.Helper()
	if err := RegisterTerminalBackend(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RestoreTerminalBackend(p.Name(), SnapshotTerminalBackend(p.Name()), nil) })
}

func TestBackendRequirementFailureCapturesReason(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	registerTestBackend(t, failingBackend{name: "nodocker", err: errors.New("Docker is not installed")})
	if checkRequirements(env, "nodocker", map[string]any{}) {
		t.Fatal("failing backend passed")
	}
	mustContain(t, TerminalBackendUnavailableReason(), "Docker")
	recordUnavailableReason("stale")
	if !checkRequirements(env, "local", map[string]any{}) || TerminalBackendUnavailableReason() != "" {
		t.Fatal("successful check did not clear the reason")
	}
}

func TestCheckTerminalRequirementsExceptionCapturesReason(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	envConfigFn = func(*Env) (map[string]any, error) { return nil, errors.New("boom") }
	if CheckTerminalRequirements(env) {
		t.Fatal("passed")
	}
	mustContain(t, TerminalBackendUnavailableReason(), "boom")
}

func TestUnknownTerminalEnvLogsErrorAndReturnsFalse(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	cfg := localTestConfig("/tmp", map[string]any{"env_type": "unknown-backend"})
	envConfigFn = func(*Env) (map[string]any, error) { return cfg, nil }
	if CheckTerminalRequirements(env) {
		t.Fatal("unknown backend passed")
	}
	mustContain(t, TerminalBackendUnavailableReason(), "Unknown TERMINAL_ENV 'unknown-backend'")
}

func TestLocalTerminalRequirementsFromPolicy(t *testing.T) {
	resetTerminalState(t)
	env := newTestToolEnv(t)
	t.Setenv("TERMINAL_ENV", "local")
	ResetTerminalScopeCache()
	if !CheckTerminalRequirements(env) {
		t.Fatalf("local requirements failed: %s", TerminalBackendUnavailableReason())
	}
}

// test_terminal_sandbox_builder_dispatch.py
type recordingBackend struct {
	localBackend
	name     string
	received *CreateOptions
}

func (b recordingBackend) Name() string { return b.name }
func (b recordingBackend) CreateEnvironment(_ context.Context, _ *Env, opts CreateOptions) (Environment, error) {
	*b.received = opts
	return newFakeTermEnv(opts.Cwd, nil), nil
}

func TestCreateEnvironmentDispatchesPluginBackendByName(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	received := &CreateOptions{}
	registerTestBackend(t, recordingBackend{name: "plugin-sandbox", received: received})
	e, err := createEnvironment(context.Background(), env, "plugin-sandbox", "test-image", "/workspace", 42, nil, map[string]any{}, nil, "test-task", "", false)
	if err != nil || e == nil {
		t.Fatal(err)
	}
	if received.Cwd != "/workspace" || received.Timeout != 42 || received.TaskID != "test-task" || received.Image != "test-image" {
		t.Fatalf("%+v", received)
	}
	if _, err := createEnvironment(context.Background(), env, "nope", "", "/", 1, nil, nil, nil, "t", "", false); err == nil || !strings.Contains(err.Error(), "Unknown environment type: nope") {
		t.Fatalf("%v", err)
	}
}

// test_terminal_plugin_host_isolation.py
type hostedBackend struct {
	localBackend
	created *bool
}

func (hostedBackend) Name() string           { return "hostbox" }
func (hostedBackend) RunsInPluginHost() bool { return true }
func (b hostedBackend) CreateEnvironment(context.Context, *Env, CreateOptions) (Environment, error) {
	*b.created = true
	return newFakeTermEnv("/", nil), nil
}

func TestAHostedTerminalBackendRefusesWithTheIsolationSetting(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	created := false
	registerTestBackend(t, hostedBackend{created: &created})
	_, err := createEnvironment(context.Background(), env, "hostbox", "", "/", 10, nil, nil, nil, "default", "", false)
	if err == nil || !strings.Contains(err.Error(), "plugins.isolation: in_process") || created {
		t.Fatalf("%v %v", err, created)
	}
}

// test_terminal_self_repo_guard.py wiring
func TestSelfRepoGuardWiring(t *testing.T) {
	if !SelfRepoGuardActive() {
		dir := t.TempDir()
		env := newTerminalTestEnv(t, dir)
		e := newFakeTermEnv(dir, nil)
		cacheFakeEnv("default", e)
		r := runTerminal(t, env, TerminalRequest{Command: "git reset --hard origin/main"})
		if r["status"] == "blocked" || e.callCount() != 1 {
			t.Fatalf("POSIX guard fired: %v", r)
		}
		return
	}
	repo := filepath.Join(t.TempDir(), "icebath")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	env := newTerminalTestEnv(t, repo)
	e := newFakeTermEnv(repo, nil)
	cacheFakeEnv("default", e)
	r := runTerminal(t, env, TerminalRequest{Command: "git status"})
	if r["status"] == "blocked" {
		t.Fatalf("readonly git blocked: %v", r)
	}
}

// test_terminal_tool.py dispatch wrapper cases
func TestHandleTerminalRejectsForegroundNotificationIntent(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	call := &Call{Name: "terminal", Env: env, Args: map[string]any{"command": "echo hi", "notify": true}}
	mustContain(t, handleTerminal(context.Background(), call), "notify only applies to background commands")
	call.Args = map[string]any{"command": "echo hi", "pty": true}
	mustContain(t, handleTerminal(context.Background(), call), "pty requires background=true")
	call.Args = map[string]any{"command": "echo hi", "persist_on_release": true}
	mustContain(t, handleTerminal(context.Background(), call), "persist_on_release only applies")
	call.Args = map[string]any{"code": "print(1)"}
	mustContain(t, handleTerminal(context.Background(), call), "terminal received a 'code' parameter")
	call.Args = map[string]any{"command": "echo hi", "background": true, "heartbeat": true}
	mustContain(t, handleTerminal(context.Background(), call), "heartbeat must be a whole number")
	call.Args = map[string]any{"command": "echo hi", "background": true, "notify": "yes"}
	mustContain(t, handleTerminal(context.Background(), call), "notify must be true/false")
}

func TestHandleTerminalMapsNotifyOntoLegacyFlags(t *testing.T) {
	env := newTerminalTestEnv(t, t.TempDir())
	cacheFakeEnv("default", newFakeTermEnv("/tmp", nil))
	spawner := &fakeSpawner{}
	terminalRegistry = func() processSpawner { return spawner }
	call := &Call{Name: "terminal", Env: env, Args: map[string]any{"command": "sleep 1", "background": true, "notify": []any{"ready"}}}
	r := map[string]any{}
	jsonDecode(t, handleTerminal(context.Background(), call), &r)
	if pats, _ := r["watch_patterns"].([]any); len(pats) != 1 || pats[0] != "ready" || resultHas(r, "notify_on_complete") {
		t.Fatalf("%v", r)
	}
	call.Args = map[string]any{"command": "sleep 1", "background": true, "heartbeat": float64(60)}
	jsonDecode(t, handleTerminal(context.Background(), call), &r)
	if r["notify_on_complete"] != true || r["heartbeat_seconds"] != float64(60) {
		t.Fatalf("%v", r)
	}
	call.Args = map[string]any{"command": "sleep 1", "background": true}
	jsonDecode(t, handleTerminal(context.Background(), call), &r)
	mustContain(t, resultString(r, "hint"), "runs SILENTLY")
}

func TestTerminalRegisteredInDefaultRegistry(t *testing.T) {
	for _, name := range []string{"terminal", "read_terminal", "close_terminal"} {
		d := Default.Get(name)
		if d == nil {
			t.Fatalf("%s not registered", name)
		}
		if name == "terminal" && (d.Toolset != "terminal" || d.MaxResultChars != 100000) {
			t.Fatalf("terminal def %+v", d)
		}
	}
}

// Collects the metric events a tool env emits
func captureMetrics(env *Env) func() []Event {
	var mu sync.Mutex
	var events []Event
	env.Notify = func(ev Event) {
		if ev.Kind == "metric" {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		}
	}
	return func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(events)
	}
}

// Metric events of one name
func metricsNamed(events []Event, name string) []map[string]any {
	var out []map[string]any
	for _, ev := range events {
		if ev.Data["name"] == name {
			out = append(out, ev.Data)
		}
	}
	return out
}

// User terminal calls count against the configured backend while host local control plane calls never do
func TestTerminalCallsCountAgainstTheConfiguredBackend(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	events := captureMetrics(env)
	ok := runTerminal(t, env, TerminalRequest{Command: "echo hi"})
	failed := runTerminal(t, env, TerminalRequest{Command: "exit 3"})
	if ok["exit_code"] != float64(0) || failed["exit_code"] != float64(3) {
		t.Fatalf("exit codes %v %v", ok["exit_code"], failed["exit_code"])
	}
	TerminalTool(context.Background(), env, TerminalRequest{Command: "echo control-plane", HostLocal: true})
	backend := metricsNamed(events(), "icebath.execution_backend")
	if len(backend) != 2 {
		t.Fatalf("backend events %v", backend)
	}
	for _, data := range backend {
		if data["kind"] != "terminal" || data["backend"] != "local" || data["error_class"] != "" || data["result"] == "" {
			t.Fatalf("backend event %v", data)
		}
	}
	outcomes := metricsNamed(events(), "icebath.terminal.outcome")
	if len(outcomes) != 2 || outcomes[0]["command"] != "echo hi" || outcomes[0]["outcome"] != "ok" ||
		outcomes[1]["command"] != "exit 3" || outcomes[1]["outcome"] != "nonzero" || outcomes[1]["backend"] != "local" {
		t.Fatalf("outcome events %v", outcomes)
	}
}

// A foreground command past its deadline reports the timeout result and the timeout outcome
func TestForegroundTerminalTimeoutIsAFailedTimeout(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	events := captureMetrics(env)
	timeout := 1
	out := runTerminal(t, env, TerminalRequest{Command: "sleep 5", Timeout: &timeout})
	if out["exit_code"] != float64(124) {
		t.Fatalf("exit code %v", out["exit_code"])
	}
	backend := metricsNamed(events(), "icebath.execution_backend")
	if len(backend) != 1 || backend[0]["error_class"] != "" {
		t.Fatalf("backend events %v", backend)
	}
	outcomes := metricsNamed(events(), "icebath.terminal.outcome")
	if len(outcomes) != 1 || outcomes[0]["outcome"] != "timeout" || outcomes[0]["command"] != "sleep 5" {
		t.Fatalf("outcome events %v", outcomes)
	}
}

// A call whose planning fails never reached a backend and emits no backend metric
func TestTerminalPlanningFailureIsNotBackendUsage(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	events := captureMetrics(env)
	envConfigFn = func(*Env) (map[string]any, error) { return nil, errors.New("config unreadable") }
	TerminalTool(context.Background(), env, TerminalRequest{Command: "echo hi"})
	if got := metricsNamed(events(), "icebath.execution_backend"); len(got) != 0 {
		t.Fatalf("backend events %v", got)
	}
}

// Foreground commands report their command and outcome while a host local control plane call reports nothing
func TestForegroundTerminalCommandsCountByKindAndOutcome(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	events := captureMetrics(env)
	short, long := 1, 30
	runs := []struct {
		command string
		timeout *int
		exit    float64
		outcome string
	}{
		{"ls /definitely/not/here", &long, 2, "nonzero"},
		{"echo ok", &long, 0, "ok"},
		{"sh -c 'exit 124'", &long, 124, "nonzero"},
		{"sleep 5", &short, 124, "timeout"},
	}
	for _, r := range runs {
		if out := runTerminal(t, env, TerminalRequest{Command: r.command, TaskID: "harness", Timeout: r.timeout}); out["exit_code"] != r.exit {
			t.Fatalf("%s exit code %v", r.command, out["exit_code"])
		}
	}
	TerminalTool(context.Background(), env, TerminalRequest{Command: "true", TaskID: "harness", HostLocal: true})
	outcomes := metricsNamed(events(), "icebath.terminal.outcome")
	if len(outcomes) != len(runs) {
		t.Fatalf("outcome events %v", outcomes)
	}
	for i, r := range runs {
		if outcomes[i]["command"] != r.command || outcomes[i]["outcome"] != r.outcome || outcomes[i]["backend"] != "local" {
			t.Fatalf("outcome %d %v", i, outcomes[i])
		}
	}
}

// A backend exception that mentions a timeout never reached an exit status and emits no terminal outcome
func TestBackendExceptionMentioningTimeoutIsNotATerminalTimeout(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	events := captureMetrics(env)
	fake := newFakeTermEnv(t.TempDir(), func(context.Context, string, ExecOptions) (*ExecResult, error) {
		return nil, errors.New("ssh: connect to host example port 22: Connection timeout")
	})
	createEnvironmentFn = func(context.Context, *Env, string, string, string, int, map[string]any, map[string]any, map[string]any, string, string, bool) (Environment, error) {
		return fake, nil
	}
	TerminalTool(context.Background(), env, TerminalRequest{Command: "echo hi", TaskID: "harness-exc"})
	if got := metricsNamed(events(), "icebath.terminal.outcome"); len(got) != 0 {
		t.Fatalf("outcome events %v", got)
	}
}

// Exported guard floor in seconds matches the terminal tool guard duration
func TestPreExecGuardMinTimeoutSExportMatchesTheGuardFloor(t *testing.T) {
	if PreExecGuardMinTimeoutS != 30 || preExecGuardMinTimeout != 30*time.Second {
		t.Fatalf("guard floor %d %v", PreExecGuardMinTimeoutS, preExecGuardMinTimeout)
	}
}
