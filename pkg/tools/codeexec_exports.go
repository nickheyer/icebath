package tools

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
)

// Minimum seconds the pre exec guard probe waits, the terminal tool floor execute_code shares
const PreExecGuardMinTimeoutS = preExecGuardMinTimeoutS

// GetEnvConfig resolves the terminal backend config the terminal and file tools share
func GetEnvConfig(env *Env) (map[string]any, error) { return getEnvConfig(env) }

// DockerHasHostAccess reports whether a docker sandbox config exposes host paths through bind mounts
func DockerHasHostAccess(cfg map[string]any) bool { return dockerHasHostAccessConfig(cfg) }

// IsSupervisedGatewayProcess reports whether this process runs as the supervised gateway
func IsSupervisedGatewayProcess() bool { return isSupervisedGatewayProcess() }

// CheckVercelSandboxRequirements reports whether the vercel sandbox backend can run, recording the reason when not
func CheckVercelSandboxRequirements(env *Env, cfg map[string]any) bool {
	return checkRequirements(env, "vercel_sandbox", cfg)
}

// PeekNousAccessToken returns the stored Nous access token without refreshing it, empty when none
func PeekNousAccessToken(env *Env) string { return peekNousAccessToken(env) }

// BuiltinTerminalBackends lists every shipped terminal backend name in sorted order
func BuiltinTerminalBackends() []string { return slices.Sorted(maps.Keys(builtinBackends)) }

// TouchTaskActivity returns the active environment of the task's cache key and stamps its activity, nil when none
func TouchTaskActivity(env *Env, taskID string) (Environment, error) {
	effective, err := resolveContainerTaskID(env, taskID)
	if err != nil {
		return nil, err
	}
	termPool.Lock()
	defer termPool.Unlock()
	cached := termPool.active[effective]
	if cached != nil {
		termPool.lastActivity[effective] = time.Now()
	}
	return cached, nil
}

// TaskCreationLock returns the creation lock of the task's environment cache key
func TaskCreationLock(env *Env, taskID string) (*sync.Mutex, error) {
	effective, err := resolveContainerTaskID(env, taskID)
	if err != nil {
		return nil, err
	}
	return creationLockFor(effective), nil
}

// CreateTaskEnvironment creates and caches the task environment execute_code shares with the terminal tool
func CreateTaskEnvironment(ctx context.Context, env *Env, cfg map[string]any, taskID string) (Environment, error) {
	effective, err := resolveContainerTaskID(env, taskID)
	if err != nil {
		return nil, err
	}
	envType, _ := cfg["env_type"].(string)
	taskEnvOverrides.Lock()
	overrides := taskEnvOverrides.m[effective]
	taskEnvOverrides.Unlock()
	if overrides == nil {
		overrides = map[string]any{}
	}
	envLog(env).Info(fmt.Sprintf("Creating new %s environment for execute_code task %s...", envType, shortTaskID(effective)))
	cwd, _ := overrides["cwd"].(string)
	if cwd == "" {
		cwd, _ = cfg["cwd"].(string)
	}
	timeout, _ := cfg["timeout"].(int)
	hostCwd, err := resolveTaskHostCwd(env, cfg, taskID)
	if err != nil {
		return nil, err
	}
	created, err := createConfiguredEnv(ctx, env, cfg, envType, selectImage(envType, overrides, cfg), cwd, timeout, effective, hostCwd)
	if err != nil {
		return nil, err
	}
	termPool.Lock()
	termPool.active[effective] = created
	termPool.lastActivity[effective] = time.Now()
	termPool.Unlock()
	startCleanupThreadFn(env)
	envLog(env).Info(fmt.Sprintf("%s environment ready for execute_code task %s", envType, shortTaskID(effective)))
	return created, nil
}
