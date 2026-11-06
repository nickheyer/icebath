package tools

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

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

// Every shipped backend name is listed once in a stable order
func TestEveryShippedTerminalBackendIsListed(t *testing.T) {
	want := []string{"daytona", "docker", "local", "managed_modal", "modal", "ssh", "vercel_sandbox"}
	if got := BuiltinTerminalBackends(); !slices.Equal(got, want) {
		t.Fatalf("backends %q", got)
	}
}

// The exported guard floor and docker host access follow the terminal tool forms
func TestCodeexecExportsMirrorTheTerminalTool(t *testing.T) {
	if PreExecGuardMinTimeoutS != 30 || preExecGuardMinTimeout != 30*time.Second {
		t.Fatalf("guard floor %d %v", PreExecGuardMinTimeoutS, preExecGuardMinTimeout)
	}
	cases := []struct {
		cfg  map[string]any
		want bool
	}{
		{map[string]any{"env_type": "local", "docker_volumes": []any{"/host:/c"}}, false},
		{map[string]any{"env_type": "docker", "host_cwd": "/repo", "docker_mount_cwd_to_workspace": true}, true},
		{map[string]any{"env_type": "docker", "host_cwd": "/repo", "docker_mount_cwd_to_workspace": false}, false},
		{map[string]any{"env_type": "docker", "docker_volumes": []any{"/host/data:/data"}}, true},
		{map[string]any{"env_type": "docker", "docker_volumes": []any{"named:/data"}}, false},
	}
	for _, c := range cases {
		if got := DockerHasHostAccess(c.cfg); got != c.want {
			t.Fatalf("DockerHasHostAccess(%v) = %v", c.cfg, got)
		}
	}
}

// The execute_code task environment is created once per cache key, registered with activity and errors surface
func TestCreateTaskEnvironmentSharesTheTerminalCache(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	cfg, err := getEnvConfig(env)
	if err != nil {
		t.Fatal(err)
	}
	cfg["env_type"], cfg["docker_image"] = "docker", "python:3.12"
	var gotCwd, gotImage string
	var gotContainer map[string]any
	fake := newFakeTermEnv("/workspace", nil)
	createEnvironmentFn = func(ctx context.Context, env *Env, envType, image, cwd string, timeout int, sshConfig, containerConfig, localConfig map[string]any, taskID, hostCwd string, probeOnly bool) (Environment, error) {
		gotCwd, gotImage, gotContainer = cwd, image, containerConfig
		return fake, nil
	}
	cleanups := 0
	startCleanupThreadFn = func(*Env) { cleanups++ }
	effective, err := resolveContainerTaskID(env, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	taskEnvOverrides.Lock()
	taskEnvOverrides.m[effective] = map[string]any{"cwd": "/workspace/task"}
	taskEnvOverrides.Unlock()
	if cached, err := TouchTaskActivity(env, "task-1"); err != nil || cached != nil {
		t.Fatalf("cached before creation %v %v", cached, err)
	}
	created, err := CreateTaskEnvironment(context.Background(), env, cfg, "task-1")
	if err != nil || created != Environment(fake) {
		t.Fatalf("created %v %v", created, err)
	}
	if gotCwd != "/workspace/task" || gotImage != "python:3.12" || gotContainer == nil || cleanups != 1 {
		t.Fatalf("cwd %q image %q container %v cleanups %d", gotCwd, gotImage, gotContainer, cleanups)
	}
	termPool.Lock()
	termPool.lastActivity[effective] = time.Time{}
	termPool.Unlock()
	cached, err := TouchTaskActivity(env, "task-1")
	termPool.Lock()
	stamped := termPool.lastActivity[effective]
	termPool.Unlock()
	if err != nil || cached != Environment(fake) || stamped.IsZero() {
		t.Fatalf("cached %v err %v stamp %v", cached, err, stamped)
	}
	first, err := TaskCreationLock(env, "task-1")
	second, _ := TaskCreationLock(env, "task-1")
	if err != nil || first != second || first != creationLockFor(effective) {
		t.Fatal("creation lock differs per call")
	}
	createEnvironmentFn = func(context.Context, *Env, string, string, string, int, map[string]any, map[string]any, map[string]any, string, string, bool) (Environment, error) {
		return nil, errors.New("docker daemon unreachable")
	}
	if _, err := CreateTaskEnvironment(context.Background(), env, cfg, "task-2"); err == nil || err.Error() != "docker daemon unreachable" {
		t.Fatalf("creation error %v", err)
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
