package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Human reason for the most recent failed requirements check, empty after a passing one
var terminalUnavailable = struct {
	sync.Mutex
	reason string
}{}

// Logs the reason, remembers it for the user facing notice, and returns false for the checker
func rejectBackend(env *Env, reason string) bool {
	recordUnavailableReason(reason)
	envLog(env).Error(reason)
	return false
}

// Stores or clears the last failure reason without logging
func recordUnavailableReason(reason string) {
	terminalUnavailable.Lock()
	terminalUnavailable.reason = reason
	terminalUnavailable.Unlock()
}

// TerminalBackendUnavailableReason is why the last terminal requirements check failed, empty when it passed
func TerminalBackendUnavailableReason() string {
	terminalUnavailable.Lock()
	defer terminalUnavailable.Unlock()
	return terminalUnavailable.reason
}

// VercelSandboxDefaultCwd is the working directory a Vercel sandbox starts in
const VercelSandboxDefaultCwd = "/vercel/sandbox"

// SupportedVercelRuntimes lists the runtimes the Vercel sandbox backend accepts
var SupportedVercelRuntimes = []string{"node24", "node22", "python3.13"}

const builtinBackendList = "local, docker, modal, daytona, vercel_sandbox, ssh"

// Config key tables shaping the backend kwargs, out key, config key, default
type configKeyRow struct {
	out string
	key string
	def any
}

var (
	sshConfigKeys = []configKeyRow{
		{"host", "ssh_host", ""}, {"user", "ssh_user", ""}, {"port", "ssh_port", 22},
		{"key", "ssh_key", ""}, {"persistent", "ssh_persistent", true},
	}
	resourceConfigKeys = []configKeyRow{
		{"cpu", "container_cpu", 1}, {"memory", "container_memory", 5120},
		{"disk", "container_disk", 51200}, {"persistent_filesystem", "container_persistent", true},
	}
	containerConfigKeys = []configKeyRow{
		{"container_cpu", "container_cpu", 1}, {"container_memory", "container_memory", 5120}, {"container_disk", "container_disk", 51200},
		{"container_persistent", "container_persistent", true}, {"modal_mode", "modal_mode", "auto"}, {"vercel_runtime", "vercel_runtime", ""}, {"vercel_image", "vercel_image", ""},
		{"docker_volumes", "docker_volumes", []any{}}, {"docker_mount_cwd_to_workspace", "docker_mount_cwd_to_workspace", false}, {"docker_forward_env", "docker_forward_env", []any{}},
		{"docker_env", "docker_env", map[string]any{}}, {"docker_run_as_host_user", "docker_run_as_host_user", false}, {"docker_extra_args", "docker_extra_args", []any{}},
		{"docker_shm_size", "docker_shm_size", "1g"}, {"docker_network", "docker_network", true}, {"docker_persist_across_processes", "docker_persist_across_processes", true},
		{"docker_shared_container_key", "docker_shared_container_key", ""}, {"docker_orphan_reaper", "docker_orphan_reaper", true}, {"docker_snap_compat", "docker_snap_compat", false},
		{"docker_image_pinned", "docker_image_pinned", false},
	}
	dockerKwargKeys = []configKeyRow{
		{"volumes", "docker_volumes", []any{}}, {"auto_mount_cwd", "docker_mount_cwd_to_workspace", false},
		{"forward_env", "docker_forward_env", []any{}}, {"env", "docker_env", map[string]any{}},
		{"run_as_host_user", "docker_run_as_host_user", false}, {"network", "docker_network", true},
		{"extra_args", "docker_extra_args", []any{}}, {"persist_across_processes", "docker_persist_across_processes", true},
		{"shared_container_key", "docker_shared_container_key", ""}, {"shm_size", "docker_shm_size", "1g"},
		{"snap_compat", "docker_snap_compat", false}, {"image_pinned", "docker_image_pinned", false},
	}
)

// Picks rows out of a config map applying the defaults
func shapeConfig(cfg map[string]any, rows []configKeyRow) map[string]any {
	out := map[string]any{}
	for _, r := range rows {
		if v, ok := cfg[r.key]; ok && v != nil {
			out[r.out] = v
		} else {
			out[r.out] = r.def
		}
	}
	return out
}

// SSH kwargs for createEnvironment
func sshConfigFromConfig(cfg map[string]any) map[string]any {
	return shapeConfig(cfg, sshConfigKeys)
}

// Local kwargs for createEnvironment, TERMINAL_LOCAL_PERSISTENT default on
func localConfigFromConfig(cfg map[string]any) map[string]any {
	persistent := true
	if v, ok := cfg["local_persistent"]; ok && v != nil {
		persistent = truthy(v)
	}
	return map[string]any{"persistent": persistent}
}

// Container kwargs for createEnvironment
func containerConfigFromConfig(cfg map[string]any) map[string]any {
	return shapeConfig(cfg, containerConfigKeys)
}

// Common sandbox resource kwargs, cpu, memory in MB, disk in MB and persistence
func sandboxResources(cc map[string]any) map[string]any {
	return shapeConfig(cc, resourceConfigKeys)
}

// IsSupportedVercelRuntime reports whether a runtime name is empty or one the sandbox accepts
func IsSupportedVercelRuntime(runtime string) bool {
	if runtime == "" {
		return true
	}
	for _, r := range SupportedVercelRuntimes {
		if r == runtime {
			return true
		}
	}
	return false
}

// HostedBackend marks a provider whose environments live in a plugin host outside this process
type HostedBackend interface {
	RunsInPluginHost() bool
}

// StampSandboxHome records the provider declared home on this environment
func (b *baseEnv) StampSandboxHome(home string) {
	b.mu.Lock()
	b.sandboxHome = home
	b.mu.Unlock()
}

// Records the provider declared home, a tilde meaning the cwd this sandbox resolved when started there
func stampSandboxHome(e Environment, provider BackendProvider, cwd string) {
	declared := provider.DefaultCwd()
	home := declared
	if declared == "~" {
		home = ""
		if cwd == "~" {
			home = e.Cwd()
		}
	}
	if !strings.HasPrefix(home, "/") || home == "/" {
		return
	}
	if s, ok := e.(interface{ StampSandboxHome(string) }); ok {
		s.StampSandboxHome(home)
	}
}

// TerminalBackendInstallHint names the install command of a backend that left core, installed by the plugins package
var TerminalBackendInstallHint func(backend string) string

func terminalBackendInstallHint(backend string) string {
	if TerminalBackendInstallHint == nil {
		panic("tools: TerminalBackendInstallHint is nil, pkg/plugins must install the left core hint")
	}
	return TerminalBackendInstallHint(backend)
}

// Registered backend names that are not built in
func pluginBackendNames() []string {
	var out []string
	for _, n := range TerminalBackendNames() {
		if !builtinBackends[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Unknown environment type error naming every usable backend
func unknownBackendError(envType string) error {
	names := append(strings.Split(builtinBackendList, ", "), pluginBackendNames()...)
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, "'"+n+"'")
	}
	return fmt.Errorf("Unknown environment type: %s. Use %s%s", envType, strings.Join(quoted, ", "), terminalBackendInstallHint(envType))
}

// Builds the docker container config with the session scoped lifecycle applied
func dockerContainerConfig(env *Env, cc map[string]any, taskID string) (map[string]any, error) {
	if err := maybeReapDockerOrphans(env, cc); err != nil {
		return nil, err
	}
	isolated, err := dockerSessionIsolationEnabled(env)
	if err != nil {
		return nil, err
	}
	sessionScoped := isolated && taskID != "default" && !hasIsolationOverrides(env, taskID)
	out := map[string]any{}
	for k, v := range cc {
		out[k] = v
	}
	for k, v := range sandboxResources(cc) {
		out[k] = v
	}
	for k, v := range shapeConfig(cc, dockerKwargKeys) {
		out[k] = v
	}
	if sessionScoped {
		out["persist_across_processes"] = false
		out["docker_persist_across_processes"] = false
	}
	out["session_scoped"] = sessionScoped
	return out, nil
}

// Environment creation seam
var createEnvironmentFn = createEnvironment

// Creates an execution environment for a backend name, unknown names fall through to plugin backends
func createEnvironment(ctx context.Context, env *Env, envType, image, cwd string, timeout int, sshConfig, containerConfig, localConfig map[string]any, taskID, hostCwd string, probeOnly bool) (Environment, error) {
	provider := GetTerminalBackend(envType)
	if provider == nil {
		return nil, unknownBackendError(envType)
	}
	if hosted, ok := provider.(HostedBackend); ok && hosted.RunsInPluginHost() {
		return nil, fmt.Errorf("terminal.backend is %s, a plugin that runs in the plugin host (plugins.isolation: host), "+
			"and the host cannot hand its environment to Icebath, so commands cannot run in it. Set "+
			"plugins.isolation: in_process to use this backend.", envType)
	}
	cc := map[string]any{}
	for k, v := range containerConfig {
		cc[k] = v
	}
	switch envType {
	case "local":
		persistent := true
		if v, ok := localConfig["persistent"]; ok && v != nil {
			persistent = truthy(v)
		}
		cc["persistent"] = persistent
	case "ssh":
		host, _ := sshConfig["host"].(string)
		user, _ := sshConfig["user"].(string)
		if host == "" || user == "" {
			return nil, fmt.Errorf("SSH environment requires ssh_host and ssh_user to be configured")
		}
		for k, v := range sshConfig {
			cc["ssh_"+k] = v
		}
	case "docker":
		merged, err := dockerContainerConfig(env, cc, taskID)
		if err != nil {
			return nil, err
		}
		cc = merged
	default:
		for k, v := range sandboxResources(cc) {
			cc[k] = v
		}
	}
	e, err := provider.CreateEnvironment(ctx, env, CreateOptions{
		Cwd: cwd, Timeout: timeout, TaskID: taskID, Image: image, ContainerConfig: cc, HostCwd: hostCwd, ProbeOnly: probeOnly,
	})
	if err != nil {
		return nil, err
	}
	if !builtinBackends[envType] {
		stampSandboxHome(e, provider, cwd)
	}
	return e, nil
}

// Runs the backend requirements check recording the failure reason
func checkRequirements(env *Env, envType string, cfg map[string]any) bool {
	recordUnavailableReason("")
	provider := GetTerminalBackend(envType)
	if provider == nil {
		return rejectBackend(env, fmt.Sprintf("Unknown TERMINAL_ENV '%s'. Use one of: %s, or a plugin-registered backend%s",
			envType, builtinBackendList, terminalBackendInstallHint(envType)))
	}
	if err := provider.CheckRequirements(env, cfg); err != nil {
		if builtinBackends[envType] {
			return rejectBackend(env, err.Error())
		}
		return rejectBackend(env, fmt.Sprintf("the '%s' plugin backend reported its requirements are not met: %s", envType, err.Error()))
	}
	return true
}

// Reports whether the vercel sandbox backend can run, recording the reason when not
func CheckVercelSandboxRequirements(env *Env, cfg map[string]any) bool {
	return checkRequirements(env, "vercel_sandbox", cfg)
}

// CheckTerminalRequirements reports whether the configured backend can run, keeping the reason on failure
func CheckTerminalRequirements(env *Env) bool {
	cfg, err := envConfigFn(env)
	if err != nil {
		envLog(env).Error("Terminal requirements check failed", "error", err)
		recordUnavailableReason(fmt.Sprintf("the requirements check failed: %s", err))
		return false
	}
	envType, _ := cfg["env_type"].(string)
	return checkRequirements(env, envType, cfg)
}
