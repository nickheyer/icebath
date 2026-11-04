package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Environment double whose Execute is a closure, with the hermes test attribute shape
type fakeTermEnv struct {
	mu           sync.Mutex
	envType      string
	cwd          string
	hostCwd      string
	hostCwdMount string
	persistent   bool
	sessionScope bool
	cleaned      bool
	cleanedMid   *bool
	calls        []fakeExecCall
	execute      func(ctx context.Context, command string, opts ExecOptions) (*ExecResult, error)
}

type fakeExecCall struct {
	command string
	opts    ExecOptions
}

func newFakeTermEnv(cwd string, execute func(ctx context.Context, command string, opts ExecOptions) (*ExecResult, error)) *fakeTermEnv {
	return &fakeTermEnv{envType: "local", cwd: cwd, execute: execute}
}

func (f *fakeTermEnv) Execute(ctx context.Context, command string, opts ExecOptions) (*ExecResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeExecCall{command, opts})
	f.mu.Unlock()
	if f.execute == nil {
		return &ExecResult{Output: "ok"}, nil
	}
	return f.execute(ctx, command, opts)
}

func (f *fakeTermEnv) Cleanup(force bool) error {
	f.mu.Lock()
	f.cleaned = true
	f.mu.Unlock()
	return nil
}

func (f *fakeTermEnv) EnvType() string { return f.envType }
func (f *fakeTermEnv) Cwd() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cwd
}
func (f *fakeTermEnv) SetCwd(cwd string) {
	f.mu.Lock()
	f.cwd = cwd
	f.mu.Unlock()
}
func (f *fakeTermEnv) TempDir() string                                      { return "/tmp" }
func (f *fakeTermEnv) HostCwd() string                                      { return f.hostCwd }
func (f *fakeTermEnv) HostCwdMount() string                                 { return f.hostCwdMount }
func (f *fakeTermEnv) SandboxHome() string                                  { return "" }
func (f *fakeTermEnv) IsPersistent() bool                                   { return f.persistent }
func (f *fakeTermEnv) IsSessionScoped() bool                                { return f.sessionScope }
func (f *fakeTermEnv) UsesHostFilesystem() bool                             { return f.envType == "local" }
func (f *fakeTermEnv) FetchFile(context.Context, string, string, int) error { return nil }
func (f *fakeTermEnv) FetchRealpath(_ context.Context, p string) string     { return p }

func (f *fakeTermEnv) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeTermEnv) wasCleaned() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cleaned
}

// Skill lock source with no locked skills
type noSkillLocks struct{}

func (noSkillLocks) LockedSkillNames(*Env) []string { return nil }
func (noSkillLocks) SkillsDir(env *Env) string      { return filepath.Join(env.Home, "skills") }
func (noSkillLocks) AllSkillsDirs(env *Env) []string {
	return []string{filepath.Join(env.Home, "skills")}
}
func (noSkillLocks) IsExcludedSkillPath(string, string) bool { return false }
func (noSkillLocks) FindSkillDirName(*Env, string) string    { return "" }

// Process registry double recording spawns
type fakeSpawner struct {
	mu       sync.Mutex
	local    []SpawnLocalOptions
	commands []string
	viaEnv   []string
	spawnErr error
	pending  []map[string]any
	nextID   string
	nextPid  int
}

func (f *fakeSpawner) SpawnLocal(env *Env, command string, o SpawnLocalOptions) (*ProcessSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.local = append(f.local, o)
	f.commands = append(f.commands, command)
	if f.spawnErr != nil {
		return nil, f.spawnErr
	}
	id := f.nextID
	if id == "" {
		id = "proc_test"
	}
	s := NewProcessSession(id, command)
	s.Pid = f.nextPid
	if s.Pid == 0 {
		s.Pid = 1234
	}
	s.TaskID, s.OwnerTaskID, s.SessionKey, s.Cwd = o.TaskID, o.OwnerTaskID, o.SessionKey, o.Cwd
	return s, nil
}

func (f *fakeSpawner) SpawnViaEnv(ctx context.Context, env *Env, e Environment, command, cwd, taskID, sessionKey string, timeout int, ownerTaskID string, persistOnRelease bool) (*ProcessSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.viaEnv = append(f.viaEnv, command)
	if f.spawnErr != nil {
		return nil, f.spawnErr
	}
	res, err := e.Execute(ctx, command, ExecOptions{Cwd: cwd})
	if err != nil {
		return nil, err
	}
	if res.ReturnCode != 0 {
		return nil, &backgroundLaunchError{res.Output}
	}
	s := NewProcessSession("proc_env", command)
	s.TaskID, s.OwnerTaskID, s.SessionKey, s.Cwd = taskID, ownerTaskID, sessionKey, cwd
	return s, nil
}

type backgroundLaunchError struct{ text string }

func (e *backgroundLaunchError) Error() string { return e.text }

func (f *fakeSpawner) AdoptLocal(env *Env, proc *ExecProcess, command, cwd, taskID, sessionKey, ownerTaskID, outputSoFar string, notify bool) *ProcessSession {
	s := NewProcessSession("proc_adopted", command)
	s.Pid, s.NotifyOnComplete = proc.Pid(), notify
	return s
}
func (f *fakeSpawner) HasActiveProcesses(string) bool                  { return false }
func (f *fakeSpawner) AnyRunning(func(*ProcessSession) bool) bool      { return false }
func (f *fakeSpawner) ArmHeartbeat(s *ProcessSession, seconds int) int { return seconds }
func (f *fakeSpawner) AddPendingWatcher(w map[string]any) {
	f.mu.Lock()
	f.pending = append(f.pending, w)
	f.mu.Unlock()
}
func (f *fakeSpawner) RequestCloseTerminal(*Env, string) map[string]any {
	return map[string]any{"status": "requested"}
}

// Hermes shaped minimal config dict for the local backend
func localTestConfig(cwd string, overrides map[string]any) map[string]any {
	cfg := map[string]any{
		"env_type": "local", "timeout": 180, "cwd": cwd, "host_cwd": "", "modal_mode": "auto",
		"docker_image": "", "modal_image": "", "daytona_image": "", "lifetime_seconds": 300,
	}
	for k, v := range overrides {
		cfg[k] = v
	}
	return cfg
}

// Resets the terminal tool globals and seams, restoring them when the test ends
func resetTerminalState(t *testing.T) {
	t.Helper()
	StopCleanupThread()
	termPool.Lock()
	termPool.active = map[string]Environment{}
	termPool.lastActivity = map[string]time.Time{}
	termPool.callsInFlight = map[string]int{}
	termPool.Unlock()
	termPool.creationMu.Lock()
	termPool.creationLocks = map[string]*sync.Mutex{}
	termPool.creationMu.Unlock()
	taskEnvOverrides.Lock()
	taskEnvOverrides.m = map[string]map[string]any{}
	taskEnvOverrides.Unlock()
	sessionCwdRecords.Lock()
	sessionCwdRecords.m = map[string]string{}
	sessionCwdRecords.Unlock()
	containerAliases.Lock()
	containerAliases.m = map[string]string{}
	containerAliases.Unlock()
	recordUnavailableReason("")
	ResetTerminalScopeCache()
	prevCfg, prevGuards, prevCleanup, prevPlan, prevAcquire := envConfigFn, checkAllGuardsFn, startCleanupThreadFn, planExecutionFn, acquireEnvFn
	prevApproval, prevPre, prevFg, prevCreate, prevReg, prevLocks, prevHint := runApprovalGuardsFn, preExecBlockFn, runForegroundFn, createEnvironmentFn, terminalRegistry, SkillLocks, TerminalBackendInstallHint
	prevMin, prevHook, prevEvidence, prevArm := preExecGuardMinTimeout, TerminalOutputTransformHook, VerificationEvidenceRecorder, ArmProcessWatcher
	startCleanupThreadFn = func(*Env) {}
	SkillLocks = noSkillLocks{}
	TerminalBackendInstallHint = func(string) string { return "" }
	t.Cleanup(func() {
		StopCleanupThread()
		envConfigFn, checkAllGuardsFn, startCleanupThreadFn, planExecutionFn, acquireEnvFn = prevCfg, prevGuards, prevCleanup, prevPlan, prevAcquire
		runApprovalGuardsFn, preExecBlockFn, runForegroundFn, createEnvironmentFn, terminalRegistry, SkillLocks, TerminalBackendInstallHint = prevApproval, prevPre, prevFg, prevCreate, prevReg, prevLocks, prevHint
		preExecGuardMinTimeout, TerminalOutputTransformHook, VerificationEvidenceRecorder, ArmProcessWatcher = prevMin, prevHook, prevEvidence, prevArm
		ResetTerminalScopeCache()
	})
}

// Tool env with the terminal globals reset and a local config dict installed
func newTerminalTestEnv(t *testing.T, cwd string) *Env {
	t.Helper()
	resetTerminalState(t)
	env := newTestToolEnv(t)
	t.Setenv("ICEBATH_HOME", env.Home)
	env.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	if cwd == "" {
		cwd = t.TempDir()
	}
	cfg := localTestConfig(cwd, nil)
	envConfigFn = func(*Env) (map[string]any, error) { return cfg, nil }
	checkAllGuardsFn = func(context.Context, *Env, string, string, bool) GuardResult { return GuardResult{Approved: true} }
	return env
}

// Caches a fake env under the collapsed default key
func cacheFakeEnv(key string, e Environment) {
	termPool.Lock()
	termPool.active[key] = e
	termPool.lastActivity[key] = time.Time{}
	termPool.Unlock()
}

func runTerminal(t *testing.T, env *Env, req TerminalRequest) map[string]any {
	t.Helper()
	raw := TerminalTool(context.Background(), env, req)
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("result is not JSON: %s", raw)
	}
	return out
}

func resultString(r map[string]any, key string) string {
	s, _ := r[key].(string)
	return s
}

func resultHas(r map[string]any, key string) bool {
	_, ok := r[key]
	return ok
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("%q does not contain %q", s, sub)
	}
}

func jsonUnmarshalString(raw string, out *map[string]any) error {
	return json.Unmarshal([]byte(raw), out)
}
