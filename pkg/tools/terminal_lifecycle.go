package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Creates an environment with the ssh, local and container kwargs shaped from the config
func createConfiguredEnv(ctx context.Context, env *Env, cfg map[string]any, envType, image, cwd string, timeout int, taskID, hostCwd string) (Environment, error) {
	var sshConfig, containerConfig, localConfig map[string]any
	if envType == "ssh" {
		sshConfig = sshConfigFromConfig(cfg)
	}
	if isContainerBackend(envType) {
		containerConfig = containerConfigFromConfig(cfg)
	}
	if envType == "local" {
		localConfig = localConfigFromConfig(cfg)
	}
	return createEnvironmentFn(ctx, env, envType, image, cwd, timeout, sshConfig, containerConfig, localConfig, taskID, hostCwd, false)
}

// IdleStopper is a persistent workspace container that stops instead of being removed when idle
type IdleStopper interface {
	StopIdle() error
}

// CleanupWaiter blocks until a backend's detached teardown worker finishes
type CleanupWaiter interface {
	WaitForCleanup(timeout time.Duration)
}

// Tears down one environment, the force flag reaching backends that honour it
func cleanupEnv(e Environment, forceRemove bool) error {
	return e.Cleanup(forceRemove)
}

// Tears down an environment or stops it idle, logging the outcome
func teardownEnv(env *Env, e Environment, taskID string, forceRemove bool, doneMsg string, stopIdle bool) {
	var err error
	if stopIdle {
		err = e.(IdleStopper).StopIdle()
	} else {
		err = cleanupEnv(e, forceRemove)
	}
	if err == nil {
		envLog(env).Info(doneMsg, "task", taskID)
		return
	}
	text := err.Error()
	if strings.Contains(text, "404") || strings.Contains(strings.ToLower(text), "not found") {
		envLog(env).Info("Environment for task already cleaned up", "task", taskID)
		return
	}
	envLog(env).Warn("Error cleaning up environment for task", "task", taskID, "error", err)
}

// Pops a task from the env cache, activity map and creation locks, returning the env
func unregisterEnv(taskID string) Environment {
	termPool.Lock()
	e := termPool.active[taskID]
	delete(termPool.active, taskID)
	delete(termPool.lastActivity, taskID)
	termPool.Unlock()
	termPool.creationMu.Lock()
	delete(termPool.creationLocks, taskID)
	termPool.creationMu.Unlock()
	return e
}

// Holds the envs cached under the keys against the idle reaper, the release restamps activity
func callInFlight(keys ...string) func() {
	var held []string
	seen := map[string]bool{}
	for _, k := range keys {
		if k != "" && !seen[k] {
			seen[k] = true
			held = append(held, k)
		}
	}
	termPool.Lock()
	for _, k := range held {
		termPool.callsInFlight[k]++
	}
	termPool.Unlock()
	return func() {
		termPool.Lock()
		now := time.Now()
		for _, k := range held {
			left := termPool.callsInFlight[k] - 1
			if left > 0 {
				termPool.callsInFlight[k] = left
			} else {
				delete(termPool.callsInFlight, k)
			}
			if _, ok := termPool.lastActivity[k]; ok {
				termPool.lastActivity[k] = now
			}
		}
		termPool.Unlock()
	}
}

// Exported face of the in flight hold for pkg/codeexec, the returned func releases
func CallInFlight(keys ...string) func() { return callInFlight(keys...) }

// Reports whether a key is a persistent docker key bound to one session workspace
func isWorkspaceKey(key string) bool {
	return strings.Contains(key, ":ws:")
}

// Cleans up environments inactive for longer than the lifetime
func cleanupInactiveEnvs(env *Env, lifetime time.Duration) {
	now := time.Now()
	registry := terminalRegistry()
	termPool.Lock()
	for taskID := range termPool.lastActivity {
		key := taskID
		if registry.HasActiveProcesses(key) || (isWorkspaceKey(key) && registry.AnyRunning(func(s *ProcessSession) bool { return s.TaskID == key })) {
			termPool.lastActivity[key] = now
		}
	}
	type stale struct {
		task string
		env  Environment
	}
	var toStop []stale
	for t, last := range termPool.lastActivity {
		if _, busy := termPool.callsInFlight[t]; now.Sub(last) > lifetime && !busy {
			toStop = append(toStop, stale{t, termPool.active[t]})
		}
	}
	for _, s := range toStop {
		delete(termPool.active, s.task)
		delete(termPool.lastActivity, s.task)
	}
	termPool.creationMu.Lock()
	for _, s := range toStop {
		delete(termPool.creationLocks, s.task)
	}
	termPool.creationMu.Unlock()
	termPool.Unlock()
	for _, s := range toStop {
		if s.env == nil {
			continue
		}
		ClearFileOpsCache(s.task)
		if _, ok := s.env.(IdleStopper); ok && isWorkspaceKey(s.task) {
			teardownEnv(env, s.env, s.task, false, "Cleaned up inactive environment for task", true)
		} else {
			teardownEnv(env, s.env, s.task, false, "Cleaned up inactive environment for task", false)
		}
	}
}

// Exported face of the inactive environment sweep for pkg/codeexec
func CleanupInactiveEnvs(env *Env, lifetime time.Duration) { cleanupInactiveEnvs(env, lifetime) }

// GetActiveEnv returns the active environment of a task or nil
func GetActiveEnv(env *Env, taskID string) (Environment, error) {
	lookup, err := resolveContainerTaskID(env, taskID)
	if err != nil {
		return nil, err
	}
	termPool.Lock()
	defer termPool.Unlock()
	if e := termPool.active[lookup]; e != nil {
		return e, nil
	}
	return termPool.active[taskID], nil
}

// Active environment of the task's cache key with its activity stamped, nil when none
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

// Per task creation lock
func creationLockFor(taskID string) *sync.Mutex {
	termPool.creationMu.Lock()
	defer termPool.creationMu.Unlock()
	l := termPool.creationLocks[taskID]
	if l == nil {
		l = &sync.Mutex{}
		termPool.creationLocks[taskID] = l
	}
	return l
}

// Creation lock of the task's environment cache key
func TaskCreationLock(env *Env, taskID string) (*sync.Mutex, error) {
	effective, err := resolveContainerTaskID(env, taskID)
	if err != nil {
		return nil, err
	}
	return creationLockFor(effective), nil
}

// EnsureTaskEnv lazily creates and caches the sandbox env of a task, nil on the local backend
func EnsureTaskEnv(ctx context.Context, env *Env, taskID string) (Environment, error) {
	cfg, err := getEnvConfig(env)
	if err != nil {
		return nil, err
	}
	envType, _ := cfg["env_type"].(string)
	if envType == "local" {
		return nil, nil
	}
	effective, err := resolveContainerTaskID(env, taskID)
	if err != nil {
		return nil, err
	}
	if existing, err := GetActiveEnv(env, effective); err != nil {
		return nil, err
	} else if existing != nil {
		termPool.Lock()
		termPool.lastActivity[effective] = time.Now()
		termPool.Unlock()
		return existing, nil
	}
	overrides, err := ResolveTaskOverrides(env, taskID)
	if err != nil {
		return nil, err
	}
	image := selectImage(envType, overrides, cfg)
	startCleanupThreadFn(env)
	lock := creationLockFor(effective)
	lock.Lock()
	defer lock.Unlock()
	if existing, err := GetActiveEnv(env, effective); err != nil || existing != nil {
		return existing, err
	}
	hostCwd, err := resolveTaskHostCwd(env, cfg, taskID)
	if err != nil {
		return nil, err
	}
	cwd, _ := cfg["cwd"].(string)
	timeout, _ := cfg["timeout"].(int)
	created, err := createConfiguredEnv(ctx, env, cfg, envType, image, cwd, timeout, effective, hostCwd)
	if err != nil {
		envLog(env).Warn("Lazy environment init failed", "backend", envType, "task", shortTaskID(effective), "error", err)
		return nil, nil
	}
	RegisterTaskEnvironment(effective, created)
	envLog(env).Info("Environment lazily initialized", "backend", envType, "task", shortTaskID(effective))
	return created, nil
}

// Caches an environment under its effective task id with a fresh activity stamp
func RegisterTaskEnvironment(effectiveTaskID string, e Environment) {
	termPool.Lock()
	termPool.active[effectiveTaskID] = e
	termPool.lastActivity[effectiveTaskID] = time.Now()
	termPool.Unlock()
}

// Creates and caches the task environment execute_code shares with the terminal tool
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
	RegisterTaskEnvironment(effective, created)
	startCleanupThreadFn(env)
	envLog(env).Info(fmt.Sprintf("%s environment ready for execute_code task %s", envType, shortTaskID(effective)))
	return created, nil
}

// IsPersistentEnv reports whether a task's active env persists across turns
func IsPersistentEnv(env *Env, taskID string) (bool, error) {
	e, err := GetActiveEnv(env, taskID)
	if err != nil || e == nil {
		return false, err
	}
	return e.IsSessionScoped() || e.IsPersistent(), nil
}

// CleanupAllEnvironments tears down every active environment at process exit, returning the count
func CleanupAllEnvironments(env *Env) int {
	KillLiveForegroundProcesses(true)
	termPool.Lock()
	keys := make([]string, 0, len(termPool.active))
	for k := range termPool.active {
		keys = append(keys, k)
	}
	termPool.Unlock()
	cleaned := 0
	for _, k := range keys {
		CleanupVM(env, k, false)
		cleaned++
	}
	if cleaned > 0 {
		envLog(env).Info("Cleaned environments", "count", cleaned)
	}
	return cleaned
}

// CleanupVM manually cleans up one environment by task id, force reaching backends that honour it
func CleanupVM(env *Env, taskID string, forceRemove bool) {
	e := unregisterEnv(taskID)
	ClearFileOpsCache(taskID)
	if e == nil {
		return
	}
	teardownEnv(env, e, taskID, forceRemove, "Manually cleaned up environment for task", false)
}

// Drops any cached env of a task and its collapsed key after an infrastructure failure
func evictEnvironmentForTask(env *Env, taskID string) error {
	collapsed, err := resolveContainerTaskID(env, taskID)
	if err != nil {
		return err
	}
	keys := map[string]bool{collapsed: true}
	if taskID != "" {
		keys[taskID] = true
	}
	var evicted []Environment
	termPool.Lock()
	for key := range keys {
		if e := termPool.active[key]; e != nil {
			evicted = append(evicted, e)
		}
		delete(termPool.active, key)
		delete(termPool.lastActivity, key)
	}
	termPool.Unlock()
	for _, e := range evicted {
		if err := e.Cleanup(false); err != nil {
			envLog(env).Debug("cleanup of degraded environment failed", "error", err)
		}
	}
	return nil
}

// Starts the idle reaper goroutine once
func startCleanupThread(env *Env) {
	termPool.Lock()
	defer termPool.Unlock()
	if termPool.cleanupStop != nil {
		return
	}
	stop := make(chan struct{})
	termPool.cleanupStop = stop
	go func() {
		for {
			lifetime := 300
			if cfg, err := getEnvConfig(env); err == nil {
				if v, ok := cfg["lifetime_seconds"].(int); ok {
					lifetime = v
				}
			} else {
				envLog(env).Warn("Error in cleanup thread", "error", err)
			}
			cleanupInactiveEnvs(env, time.Duration(lifetime)*time.Second)
			select {
			case <-stop:
				return
			case <-time.After(60 * time.Second):
			}
		}
	}()
}

// StopCleanupThread stops the idle reaper
func StopCleanupThread() {
	termPool.Lock()
	stop := termPool.cleanupStop
	termPool.cleanupStop = nil
	termPool.Unlock()
	if stop != nil {
		close(stop)
	}
}

// ShutdownTerminalEnvironments stops the reaper and tears down every remaining sandbox at exit
func ShutdownTerminalEnvironments(env *Env) {
	StopCleanupThread()
	termPool.Lock()
	remaining := make([]Environment, 0, len(termPool.active))
	for _, e := range termPool.active {
		remaining = append(remaining, e)
	}
	termPool.Unlock()
	if len(remaining) > 0 {
		envLog(env).Info("Shutting down remaining sandboxes", "count", len(remaining))
		CleanupAllEnvironments(env)
		for _, e := range remaining {
			if w, ok := e.(CleanupWaiter); ok {
				w.WaitForCleanup(15 * time.Second)
			}
		}
	}
	WaitForAllBackendTeardowns(15 * time.Second)
}
