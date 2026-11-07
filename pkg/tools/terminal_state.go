package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Environment cache, activity stamps, creation locks and in flight counters keyed by task id
var termPool = struct {
	sync.Mutex
	active        map[string]Environment
	lastActivity  map[string]time.Time
	callsInFlight map[string]int
	creationMu    sync.Mutex
	creationLocks map[string]*sync.Mutex
	cleanupStop   chan struct{}
}{
	active:        map[string]Environment{},
	lastActivity:  map[string]time.Time{},
	callsInFlight: map[string]int{},
	creationLocks: map[string]*sync.Mutex{},
}

// Per task sandbox overrides never exposed to the model
var taskEnvOverrides = struct {
	sync.Mutex
	m map[string]map[string]any
}{m: map[string]map[string]any{}}

// Per session cwd records keyed by the raw session key
var sessionCwdRecords = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

// Subagent to parent container aliasing
var containerAliases = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

// Once per process guard for the docker orphan reaper
var dockerOrphanReaper struct {
	sync.Mutex
	ran bool
}

// Bound session context variable, the engaged session map first, the session values and process env second
func sessionContextValue(env *Env, name string) string {
	if vars, _ := sessionContextState(env); vars != nil {
		if v, ok := vars[name]; ok {
			return v
		}
	}
	key := strings.ToLower(strings.TrimPrefix(name, "ICEBATH_SESSION_"))
	return sessionValue(env, key, name)
}

// Bound session context variable for plugin sessions, the engaged session map before session values and process env
func SessionEnv(env *Env, name string) string { return sessionContextValue(env, name) }

// Active gateway or WebUI session key, empty outside sessions
func currentSessionKey(env *Env) string {
	return sessionContextValue(env, "ICEBATH_SESSION_KEY")
}

// Active session profile name, empty when none is stamped
func currentSessionProfile(env *Env) string {
	return sessionContextValue(env, "ICEBATH_SESSION_PROFILE")
}

// Home routed for this session when it is not the launch home, empty otherwise
func homeOverride(env *Env) string {
	if env == nil || strings.TrimSpace(env.Home) == "" {
		return ""
	}
	if isRoutedHome(env.Home) {
		return env.Home
	}
	return ""
}

// Profile whose persistent container a session bound command uses
func containerProfile(env *Env) string {
	override := homeOverride(env)
	session := currentSessionProfile(env)
	bound := ""
	if override != "" {
		bound = home.ProfileNameFor(override)
	}
	if bound != "" {
		if session != "" {
			if bound != session {
				return bound
			}
		} else if isRoutedHome(override) {
			return bound
		}
	}
	if session != "" {
		return session
	}
	return homeProfileName(env)
}

// Runs the docker orphan reaper once per process when enabled
func maybeReapDockerOrphans(env *Env, containerConfig map[string]any) error {
	if v, ok := containerConfig["docker_orphan_reaper"]; ok && v != nil && !truthy(v) {
		return nil
	}
	dockerOrphanReaper.Lock()
	if dockerOrphanReaper.ran {
		dockerOrphanReaper.Unlock()
		return nil
	}
	dockerOrphanReaper.ran = true
	dockerOrphanReaper.Unlock()
	policy, err := terminalPolicy(env)
	if err != nil {
		return err
	}
	lifetime, err := parseTerminalInt(policy, "TERMINAL_LIFETIME_SECONDS", "300")
	if err != nil {
		lifetime = 300
	}
	if lifetime < 60 {
		lifetime = 60
	}
	maxAge := lifetime * 2
	sharedKey, _ := containerConfig["docker_shared_container_key"].(string)
	profile := dockerContainerIdentity(env, sharedKey)
	removed, err := dockerReapOrphanContainers(env, maxAge, profile)
	if err != nil {
		envLog(env).Debug("Docker orphan reaper raised", "error", err)
		return nil
	}
	if removed > 0 {
		envLog(env).Info("Docker orphan reaper removed stale containers", "count", removed, "profile", profile)
	}
	return nil
}

// RecordSessionCwd records the working directory a session finished in
func RecordSessionCwd(env *Env, sessionKey, cwd string) {
	if strings.TrimSpace(cwd) == "" {
		return
	}
	if sessionKey == "" {
		sessionKey = "default"
	}
	key := qualifyTaskKey(env, sessionKey)
	sessionCwdRecords.Lock()
	if sessionCwdRecords.m[key] != cwd {
		sessionCwdRecords.m[key] = cwd
	}
	sessionCwdRecords.Unlock()
}

// Recorded cwd of a session, absent when none was recorded
func getSessionCwdFor(env *Env, sessionKey string) (string, bool) {
	if sessionKey == "" {
		sessionKey = "default"
	}
	sessionCwdRecords.Lock()
	defer sessionCwdRecords.Unlock()
	v, ok := sessionCwdRecords.m[qualifyTaskKey(env, sessionKey)]
	return v, ok
}

// Recorded cwd of a session under the launch home qualification
func getSessionCwd(sessionKey string) (string, bool) {
	return getSessionCwdFor(nil, sessionKey)
}

// GetSessionCwd is the recorded cwd of a session, false when none was recorded
func GetSessionCwd(env *Env, sessionKey string) (string, bool) {
	return getSessionCwdFor(env, sessionKey)
}

// ClearSessionCwd drops a session's cwd record at teardown
func ClearSessionCwd(env *Env, sessionKey string) {
	sessionCwdRecords.Lock()
	delete(sessionCwdRecords.m, qualifyTaskKey(env, sessionKey))
	sessionCwdRecords.Unlock()
}

// Cwd to write into a live cached env, false to leave it untouched
func sanitizeCwdForLiveEnv(env *Env, e Environment, newCwd string) (string, bool) {
	envType := e.EnvType()
	newCwd = coerceSSHRemoteCwd(env, newCwd, envType)
	if envType == "" || !isContainerBackend(envType) {
		return newCwd, true
	}
	mounted := e.HostCwd()
	if !isUnusableContainerCwd(newCwd, mounted) {
		return newCwd, true
	}
	if mounted != "" {
		containerMount := e.HostCwdMount()
		if containerMount == "" {
			containerMount = "/workspace"
		}
		if isMountedHostCwd(newCwd, mounted) {
			return containerMount, true
		}
		if translated := translateMountedHostPath(newCwd, mounted, containerMount); translated != "" {
			return translated, true
		}
	}
	return "", false
}

// RegisterTaskEnvOverrides registers per task sandbox overrides before the agent loop runs
func RegisterTaskEnvOverrides(env *Env, taskID string, overrides map[string]any) error {
	taskEnvOverrides.Lock()
	taskEnvOverrides.m[qualifyTaskKey(env, taskID)] = overrides
	taskEnvOverrides.Unlock()
	newCwd, _ := overrides["cwd"].(string)
	if strings.TrimSpace(newCwd) == "" {
		return nil
	}
	RecordSessionCwd(env, taskID, newCwd)
	containerID, err := resolveContainerTaskID(env, taskID)
	if err != nil {
		return err
	}
	termPool.Lock()
	live := termPool.active[taskID]
	if live == nil {
		live = termPool.active[containerID]
	}
	termPool.Unlock()
	if live != nil {
		if sanitized, ok := sanitizeCwdForLiveEnv(env, live, newCwd); ok {
			live.SetCwd(sanitized)
		}
	}
	return nil
}

// ClearTaskEnvOverrides drops a task's overrides, cwd record and container alias
func ClearTaskEnvOverrides(env *Env, taskID string) {
	taskEnvOverrides.Lock()
	delete(taskEnvOverrides.m, qualifyTaskKey(env, taskID))
	taskEnvOverrides.Unlock()
	ClearSessionCwd(env, taskID)
	containerAliases.Lock()
	delete(containerAliases.m, taskID)
	containerAliases.Unlock()
}

// RegisterContainerAlias makes a child task resolve to its parent's container
func RegisterContainerAlias(childTaskID, parentTaskID string) {
	if childTaskID == "" {
		return
	}
	if parentTaskID == "" {
		parentTaskID = "default"
	}
	containerAliases.Lock()
	containerAliases.m[childTaskID] = parentTaskID
	containerAliases.Unlock()
}

// Follows the child to parent alias chain cycle safe
func resolveContainerAlias(taskID string) string {
	seen := map[string]bool{}
	key := taskID
	containerAliases.Lock()
	defer containerAliases.Unlock()
	for {
		next, ok := containerAliases.m[key]
		if !ok || seen[key] {
			return key
		}
		seen[key] = true
		key = next
	}
}

// Reports whether an override key is an image or env_type isolation signal
func isIsolationOverrideKey(key string) bool {
	return key == "env_type" || strings.HasSuffix(key, "_image")
}

// Reports whether a task registered image or env_type overrides
func hasIsolationOverrides(env *Env, taskID string) bool {
	if taskID == "" {
		return false
	}
	taskEnvOverrides.Lock()
	overrides := taskEnvOverrides.m[qualifyTaskKey(env, taskID)]
	taskEnvOverrides.Unlock()
	for key := range overrides {
		if isIsolationOverrideKey(key) {
			return true
		}
	}
	return false
}

// Backend scope snapshot of this call
func sessionScopeOf(env *Env) (sessionScope, error) {
	policy, err := terminalPolicy(env)
	if err != nil {
		return sessionScope{}, err
	}
	return currentSessionScope(policy), nil
}

// Reports whether the docker workspace mount and session scoped teardown apply
func dockerSessionIsolationEnabled(env *Env) (bool, error) {
	scope, err := sessionScopeOf(env)
	if err != nil {
		return false, err
	}
	return scope.dockerSessionIsolated(), nil
}

// Key for a session less task serving a routed profile home, empty otherwise
func routedHomeTaskKey(env *Env, profileScoped bool) string {
	override := homeOverride(env)
	if override == "" {
		return ""
	}
	if profileScoped {
		if profile := home.ProfileNameFor(override); profile != "" {
			return profileContainerKey(profile)
		}
	}
	return "home:" + realPathOf(override)
}

// Container key when no session is bound
func sessionlessContainerKey(env *Env, profileScoped bool) string {
	if routed := routedHomeTaskKey(env, profileScoped); routed != "" {
		return routed
	}
	if profileScoped {
		return profileContainerKey(homeProfileName(env))
	}
	return "default"
}

// Prefixes a session derived key with the routed profile home, unchanged without one
func qualifyTaskKey(env *Env, key string) string {
	if scope := routedHomeTaskKey(env, true); scope != "" {
		return scope + ":" + key
	}
	return key
}

// Maps a tool call task id to the environment cache key
func resolveContainerTaskID(env *Env, taskID string) (string, error) {
	if taskID != "" && hasIsolationOverrides(env, taskID) {
		return qualifyTaskKey(env, taskID), nil
	}
	policy, err := terminalPolicy(env)
	if err != nil {
		return "", err
	}
	scope := currentSessionScope(policy)
	if taskID != "" && scope.sessionIsolated() {
		return qualifyTaskKey(env, resolveContainerAlias(taskID)), nil
	}
	sessionKey := currentSessionKey(env)
	shared := ""
	if scope.dockerProfileScoped() {
		shared = strings.TrimSpace(policy.Get("TERMINAL_DOCKER_SHARED_CONTAINER_KEY", ""))
	}
	if shared != "" {
		return "shared:" + shared, nil
	}
	if sessionKey == "" {
		return sessionlessContainerKey(env, scope.dockerProfileScoped()), nil
	}
	if !scope.dockerProfileScoped() {
		return qualifyTaskKey(env, "session:"+sessionKey), nil
	}
	return workspaceContainerKey(env, policy, profileContainerKey(containerProfile(env)), taskID), nil
}

// Exported face of the cache key mapping for pkg/codeexec
func ResolveContainerTaskID(env *Env, taskID string) (string, error) {
	return resolveContainerTaskID(env, taskID)
}

// WorkspaceKeyFor is the digest of a host workspace computed once when the session binds it
func WorkspaceKeyFor(cwd string) string {
	resolved := realPathOf(expandUser(cwd))
	sum := sha256.Sum256([]byte(resolved))
	return hex.EncodeToString(sum[:])[:16]
}

// Persistent docker key for a session that registered a workspace
func workspaceContainerKey(env *Env, policy TerminalPolicy, base, taskID string) string {
	if taskID == "" {
		return base
	}
	if strings.HasPrefix(taskID, base+":ws:") {
		return taskID
	}
	if !policy.GetBool("TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE", "false") {
		return base
	}
	keys := []string{taskID}
	if alias := resolveContainerAlias(taskID); alias != taskID {
		keys = append(keys, alias)
	}
	for _, key := range keys {
		taskEnvOverrides.Lock()
		overrides := taskEnvOverrides.m[qualifyTaskKey(env, key)]
		taskEnvOverrides.Unlock()
		if overrides == nil || overrides["cwd_source"] != "session" {
			continue
		}
		digest, _ := overrides["workspace_key"].(string)
		cwd, _ := overrides["cwd"].(string)
		if digest == "" && strings.TrimSpace(cwd) != "" && !strings.Contains(cwd, "\x00") {
			digest = WorkspaceKeyFor(cwd)
		}
		if digest != "" {
			return base + ":ws:" + digest
		}
	}
	return base
}

// ResolveTaskOverrides returns the env overrides for a task, raw key first then collapsed
func ResolveTaskOverrides(env *Env, taskID string) (map[string]any, error) {
	raw := taskID
	if raw == "" {
		raw = "default"
	}
	taskEnvOverrides.Lock()
	overrides := taskEnvOverrides.m[qualifyTaskKey(env, raw)]
	taskEnvOverrides.Unlock()
	if overrides != nil {
		return overrides, nil
	}
	collapsed, err := resolveContainerTaskID(env, raw)
	if err != nil {
		return nil, err
	}
	taskEnvOverrides.Lock()
	overrides = taskEnvOverrides.m[collapsed]
	taskEnvOverrides.Unlock()
	if overrides == nil {
		overrides = map[string]any{}
	}
	return overrides, nil
}

// Backends that take an image keyed to the override or config key carrying it
var imageKeyByBackend = map[string]string{"docker": "docker_image", "modal": "modal_image", "daytona": "daytona_image"}

// Image for a backend, per task override first then config, empty for imageless backends
func selectImage(envType string, overrides, cfg map[string]any) string {
	key, ok := imageKeyByBackend[envType]
	if !ok {
		v, _ := overrides[envType+"_image"].(string)
		return v
	}
	if v, _ := overrides[key].(string); v != "" {
		return v
	}
	v, _ := cfg[key].(string)
	return v
}

// Cached env for the collapsed id, else the raw task id, refreshing the activity stamp, caller holds the lock
func lookupActiveEnv(effectiveTaskID, taskID string) Environment {
	for _, key := range []string{effectiveTaskID, taskID} {
		if key == "" {
			continue
		}
		if e, ok := termPool.active[key]; ok {
			termPool.lastActivity[key] = time.Now()
			return e
		}
	}
	return nil
}

// Host directory to bind into a task's container, empty when none may mount
func resolveTaskHostCwd(env *Env, cfg map[string]any, taskID string) (string, error) {
	if cfg["env_type"] != "docker" || !truthy(cfg["docker_mount_cwd_to_workspace"]) {
		return "", nil
	}
	isolated, err := dockerSessionIsolationEnabled(env)
	if err != nil {
		return "", err
	}
	containerID, err := resolveContainerTaskID(env, taskID)
	if err != nil {
		return "", err
	}
	if !isolated || containerID == "default" {
		v, _ := cfg["host_cwd"].(string)
		return v, nil
	}
	overrides, err := ResolveTaskOverrides(env, taskID)
	if err != nil {
		return "", err
	}
	candidate, _ := overrides["cwd"].(string)
	if overrides["cwd_source"] == "process" || strings.TrimSpace(candidate) == "" {
		return "", nil
	}
	candidate, err = filepath.Abs(expandUser(candidate))
	if err != nil {
		return "", nil
	}
	st, err := os.Stat(candidate)
	if err != nil || !st.IsDir() || strings.HasPrefix(candidate, "/workspace") || strings.HasPrefix(candidate, "/root") {
		return "", nil
	}
	return candidate, nil
}

// Default cwd per backend, plugins may declare one, container backends default to root
var defaultCwdByBackend = map[string]string{"ssh": "~", "vercel_sandbox": VercelSandboxDefaultCwd}

func backendDefaultCwd(env *Env, policy TerminalPolicy, envType string) string {
	if envType == "local" {
		return safeGetcwd(env, policy)
	}
	if v, ok := defaultCwdByBackend[envType]; ok {
		return v
	}
	if !builtinBackends[envType] {
		if p := GetTerminalBackend(envType); p != nil {
			declared := p.DefaultCwd()
			if declared == "~" || strings.HasPrefix(declared, "/") {
				return declared
			}
		}
	}
	return "/root"
}

// Cwd and host cwd from TERMINAL_CWD for a backend with the container sanity checks applied
func resolveConfigCwd(env *Env, policy TerminalPolicy, envType string, mountDockerCwd bool) (string, string) {
	defaultCwd := backendDefaultCwd(env, policy, envType)
	cwd := policy.Get("TERMINAL_CWD", defaultCwd)
	if cwd != "" && cwd != defaultCwd && !isSSHRemoteTildeCwd(envType, cwd) {
		cwd = expandUser(cwd)
	}
	hostCwd := ""
	switch {
	case envType == "docker" && mountDockerCwd:
		raw := policy.Get("TERMINAL_CWD", "")
		if raw == "" {
			raw = safeGetcwd(env, policy)
		}
		candidate, err := filepath.Abs(expandUser(raw))
		if err != nil {
			candidate = expandUser(raw)
		}
		st, statErr := os.Stat(candidate)
		if isHostCwd(candidate) || (filepath.IsAbs(candidate) && statErr == nil && st.IsDir() &&
			!strings.HasPrefix(candidate, "/workspace") && !strings.HasPrefix(candidate, "/root")) {
			hostCwd = candidate
			cwd = "/workspace"
		}
	case envType == "docker" && isWindowsDrivePath(cwd):
		candidate := expandUser(cwd)
		if isWindowsHost {
			if abs, err := filepath.Abs(candidate); err == nil {
				candidate = abs
			}
		}
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			hostCwd = candidate
			cwd = candidate
		} else {
			envLog(env).Info("Ignoring TERMINAL_CWD for backend (host/relative path won't work in sandbox)", "cwd", cwd, "backend", envType, "using", defaultCwd)
			cwd = defaultCwd
		}
	case isContainerBackend(envType) && cwd != "" && isUnusableContainerCwd(cwd, "") && cwd != defaultCwd:
		envLog(env).Info("Ignoring TERMINAL_CWD for backend (host/relative path won't work in sandbox)", "cwd", cwd, "backend", envType, "using", defaultCwd)
		cwd = defaultCwd
	}
	return cwd, hostCwd
}

// Terminal configuration seam
var envConfigFn = resolveEnvConfig

// Resolves the terminal configuration through the seam
func getEnvConfig(env *Env) (map[string]any, error) { return envConfigFn(env) }

// Resolves the terminal backend config the terminal and file tools share
func GetEnvConfig(env *Env) (map[string]any, error) { return getEnvConfig(env) }

// Resolves the terminal configuration from the session policy
func resolveEnvConfig(env *Env) (map[string]any, error) {
	policy, err := terminalPolicy(env)
	if err != nil {
		return nil, err
	}
	envType := policy.Get("TERMINAL_ENV", "local")
	mountDockerCwd := policy.GetBool("TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE", "false")
	var containerCPU float64 = 1
	containerMemory, containerDisk := 5120, 51200
	if isContainerBackend(envType) {
		if containerCPU, err = parseTerminalFloat(policy, "TERMINAL_CONTAINER_CPU", "1"); err != nil {
			return nil, err
		}
		if containerMemory, err = parseTerminalInt(policy, "TERMINAL_CONTAINER_MEMORY", "5120"); err != nil {
			return nil, err
		}
		if containerDisk, err = parseTerminalInt(policy, "TERMINAL_CONTAINER_DISK", "51200"); err != nil {
			return nil, err
		}
	}
	var dockerForwardEnv, dockerVolumes, dockerExtraArgs []any
	dockerEnv := map[string]any{}
	dockerShmSize := "1g"
	if envType == "docker" {
		if err := parseTerminalJSON(policy, "TERMINAL_DOCKER_FORWARD_ENV", "[]", &dockerForwardEnv); err != nil {
			return nil, err
		}
		if err := parseTerminalJSON(policy, "TERMINAL_DOCKER_VOLUMES", "[]", &dockerVolumes); err != nil {
			return nil, err
		}
		if err := parseTerminalJSON(policy, "TERMINAL_DOCKER_ENV", "{}", &dockerEnv); err != nil {
			return nil, err
		}
		if err := parseTerminalJSON(policy, "TERMINAL_DOCKER_EXTRA_ARGS", "[]", &dockerExtraArgs); err != nil {
			return nil, err
		}
		dockerShmSize = policy.Get("TERMINAL_DOCKER_SHM_SIZE", "1g")
	}
	if dockerForwardEnv == nil {
		dockerForwardEnv = []any{}
	}
	if dockerVolumes == nil {
		dockerVolumes = []any{}
	}
	if dockerExtraArgs == nil {
		dockerExtraArgs = []any{}
	}
	if dockerEnv == nil {
		dockerEnv = map[string]any{}
	}
	cwd, hostCwd := resolveConfigCwd(env, policy, envType, mountDockerCwd)
	timeout, err := parseTerminalInt(policy, "TERMINAL_TIMEOUT", "180")
	if err != nil {
		return nil, err
	}
	lifetime, err := parseTerminalInt(policy, "TERMINAL_LIFETIME_SECONDS", "300")
	if err != nil {
		return nil, err
	}
	sshPort, err := parseTerminalInt(policy, "TERMINAL_SSH_PORT", "22")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"env_type":                        envType,
		"modal_mode":                      CoerceModalMode(policy.Get("TERMINAL_MODAL_MODE", "auto")),
		"docker_image":                    policy.Get("TERMINAL_DOCKER_IMAGE", defaultSandboxImage),
		"docker_image_pinned":             policy.Get("TERMINAL_DOCKER_IMAGE_PINNED", "0") == "1",
		"docker_forward_env":              dockerForwardEnv,
		"modal_image":                     policy.Get("TERMINAL_MODAL_IMAGE", defaultSandboxImage),
		"daytona_image":                   policy.Get("TERMINAL_DAYTONA_IMAGE", defaultSandboxImage),
		"vercel_runtime":                  strings.TrimSpace(policy.Get("TERMINAL_VERCEL_RUNTIME", "")),
		"vercel_image":                    strings.TrimSpace(policy.Get("TERMINAL_VERCEL_IMAGE", "")),
		"cwd":                             cwd,
		"host_cwd":                        hostCwd,
		"docker_mount_cwd_to_workspace":   mountDockerCwd,
		"timeout":                         timeout,
		"lifetime_seconds":                lifetime,
		"ssh_host":                        policy.Get("TERMINAL_SSH_HOST", ""),
		"ssh_user":                        policy.Get("TERMINAL_SSH_USER", ""),
		"ssh_port":                        sshPort,
		"ssh_key":                         policy.Get("TERMINAL_SSH_KEY", ""),
		"ssh_persistent":                  policy.GetBool("TERMINAL_SSH_PERSISTENT", policy.Get("TERMINAL_PERSISTENT_SHELL", "true")),
		"local_persistent":                policy.GetBool("TERMINAL_LOCAL_PERSISTENT", "true"),
		"container_cpu":                   containerCPU,
		"container_memory":                containerMemory,
		"container_disk":                  containerDisk,
		"container_persistent":            policy.GetBool("TERMINAL_CONTAINER_PERSISTENT", "true"),
		"docker_volumes":                  dockerVolumes,
		"docker_env":                      dockerEnv,
		"docker_run_as_host_user":         policy.GetBool("TERMINAL_DOCKER_RUN_AS_HOST_USER", "false"),
		"docker_snap_compat":              policy.GetBool("TERMINAL_DOCKER_SNAP_COMPAT", "false"),
		"docker_network":                  policy.GetBool("TERMINAL_DOCKER_NETWORK", "true"),
		"docker_extra_args":               dockerExtraArgs,
		"docker_shm_size":                 dockerShmSize,
		"docker_persist_across_processes": policy.GetBool("TERMINAL_DOCKER_PERSIST_ACROSS_PROCESSES", "true"),
		"docker_shared_container_key":     strings.TrimSpace(policy.Get("TERMINAL_DOCKER_SHARED_CONTAINER_KEY", "")),
		"docker_orphan_reaper":            policy.GetBool("TERMINAL_DOCKER_ORPHAN_REAPER", "true"),
	}, nil
}

// Reports whether a docker sandbox exposes host paths through bind mounts
func dockerHasHostAccessConfig(cfg map[string]any) bool {
	if cfg["env_type"] != "docker" {
		return false
	}
	if host, _ := cfg["host_cwd"].(string); host != "" && truthy(cfg["docker_mount_cwd_to_workspace"]) {
		return true
	}
	switch vols := cfg["docker_volumes"].(type) {
	case []any:
		for _, v := range vols {
			if s, ok := v.(string); ok && dockerVolumeUsesHostPath(s) {
				return true
			}
		}
	case []string:
		for _, s := range vols {
			if dockerVolumeUsesHostPath(s) {
				return true
			}
		}
	}
	return false
}

// Reports whether a docker sandbox config exposes host paths through bind mounts
func DockerHasHostAccess(cfg map[string]any) bool { return dockerHasHostAccessConfig(cfg) }

// Short task id for log lines
func shortTaskID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Formats a task id with its backend for log lines
func taskLabel(envType, taskID string) string {
	return fmt.Sprintf("%s/%s", envType, shortTaskID(taskID))
}
