package codeexec

import (
	"context"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Backend whose every command outlives the env lifetime, one reaper sweep firing from inside it
type reapingEnv struct {
	*shellEnv
	owner          *tools.Env
	mu             sync.Mutex
	cleaned        bool
	cleanedMidCall *bool
}

func (r *reapingEnv) Cleanup(bool) error {
	r.mu.Lock()
	r.cleaned = true
	r.mu.Unlock()
	return nil
}

func (r *reapingEnv) Execute(context.Context, string, tools.ExecOptions) (*tools.ExecResult, error) {
	tools.CleanupInactiveEnvs(r.owner, -1)
	r.mu.Lock()
	mid := r.cleaned
	r.cleanedMidCall = &mid
	r.mu.Unlock()
	return &tools.ExecResult{Output: "OK"}, nil
}

func TestRemoteExecuteCodeCallKeepsItsEnvironmentPastTheLifetime(t *testing.T) {
	env := testEnv(t)
	key, err := tools.ResolveContainerTaskID(env, "session-42")
	if err != nil || key != "default" {
		t.Fatalf("collapsed key %q err %v", key, err)
	}
	reaping := &reapingEnv{shellEnv: newShellEnv(t, t.TempDir()), owner: env}
	tools.RegisterTaskEnvironment(key, reaping)
	t.Cleanup(func() { tools.CleanupInactiveEnvs(env, -1) })
	prev := executeInRemoteKernelFn
	executeInRemoteKernelFn = func(ctx context.Context, _ *tools.Env, envr tools.Environment, _ string, _ remoteCell) (*pyjson.Object, error) {
		if _, err := envr.Execute(ctx, "python3 cell.py", tools.ExecOptions{}); err != nil {
			return nil, err
		}
		result := pyjson.NewObject()
		result.Set("status", "success")
		result.Set("stdout", "1\n")
		return result, nil
	}
	t.Cleanup(func() { executeInRemoteKernelFn = prev })
	result := decodeResult(t, executeRemote(t.Context(), env, "print(1)", "session-42", nil, false))
	if result["status"] != "success" || outputOf(result) != "1\n" {
		t.Fatalf("result %v", result)
	}
	if reaping.cleanedMidCall == nil || *reaping.cleanedMidCall {
		t.Fatal("the reaper tore the environment down mid call")
	}
	if live, err := tools.TouchTaskActivity(env, "session-42"); err != nil || live != tools.Environment(reaping) {
		t.Fatalf("cached env %v err %v", live, err)
	}
}
