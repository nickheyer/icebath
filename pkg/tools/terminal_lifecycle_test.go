package tools

import (
	"context"
	"errors"
	"testing"
	"time"
)

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

func TestCallInFlightExportHoldsUntilRelease(t *testing.T) {
	key := t.Name()
	release := CallInFlight(key, key, "")
	termPool.Lock()
	held := termPool.callsInFlight[key]
	termPool.Unlock()
	if held != 1 {
		t.Fatalf("held %d, want 1", held)
	}
	release()
	termPool.Lock()
	_, still := termPool.callsInFlight[key]
	termPool.Unlock()
	if still {
		t.Fatal("hold survived the release")
	}
}

func TestCleanupInactiveEnvsExportReapsIdleEnvs(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	key := t.Name()
	termPool.Lock()
	termPool.active[key] = newFakeTermEnv("/w", nil)
	termPool.lastActivity[key] = time.Now()
	termPool.Unlock()
	CleanupInactiveEnvs(env, time.Hour)
	termPool.Lock()
	_, kept := termPool.active[key]
	termPool.Unlock()
	if !kept {
		t.Fatal("fresh env reaped")
	}
	termPool.Lock()
	termPool.lastActivity[key] = time.Now().Add(-2 * time.Hour)
	termPool.Unlock()
	CleanupInactiveEnvs(env, time.Hour)
	termPool.Lock()
	_, kept = termPool.active[key]
	termPool.Unlock()
	if kept {
		t.Fatal("idle env survived the sweep")
	}
}

func TestRegisterTaskEnvironmentCachesWithFreshActivity(t *testing.T) {
	key := t.Name()
	fake := newFakeTermEnv("/w", nil)
	before := time.Now()
	RegisterTaskEnvironment(key, fake)
	termPool.Lock()
	got, stamp := termPool.active[key], termPool.lastActivity[key]
	delete(termPool.active, key)
	delete(termPool.lastActivity, key)
	termPool.Unlock()
	if got != Environment(fake) {
		t.Fatalf("cached env %v, want the registered one", got)
	}
	if stamp.Before(before) {
		t.Fatalf("activity stamp %v predates the registration %v", stamp, before)
	}
}
