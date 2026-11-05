package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const backendProbeCmd = "printf 'os=%s\\nkernel=%s\\n' \"$(uname -s 2>/dev/null || echo unknown)\" \"$(uname -r 2>/dev/null || echo unknown)\""

var backendImageKeys = map[string]string{"docker": "docker_image", "modal": "modal_image", "daytona": "daytona_image"}

type backendProbeKey struct{ home, envType, cwd string }

var backendProbeCache = struct {
	sync.Mutex
	entries map[backendProbeKey]string
}{entries: map[backendProbeKey]string{}}

// Whether backend names a built in remote or sandbox terminal backend
func IsRemoteTerminalBackend(backend string) bool { return envProbeRemoteBackends[backend] }

// Whether a plugin registered backend declares itself remote
func PluginBackendIsRemote(backend string) bool { return pluginBackendIsRemote(backend) }

// Terminal setting of the env profile, a refused terminal scope returned as the error
func TerminalEnvRead(env *Env, name, def string) (string, error) {
	policy, err := terminalPolicy(env)
	if err != nil {
		return "", err
	}
	return policy.Get(name, def), nil
}

// Integer config value of the probe environment, def when absent or not a number
func probeConfigInt(v any, def int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return def
}

// Runs the OS probe inside a freshly built probe only backend and tears it down, empty on a failed probe
func runBackendProbe(ctx context.Context, env *Env, envType string) (string, error) {
	cfg, err := getEnvConfig(env)
	if err != nil {
		return "", err
	}
	image := ""
	if key, ok := backendImageKeys[envType]; ok {
		image, _ = cfg[key].(string)
	}
	cwd, _ := cfg["cwd"].(string)
	hostCwd, _ := cfg["host_cwd"].(string)
	var sshConfig, containerConfig map[string]any
	if envType == "ssh" {
		sshConfig = sshConfigFromConfig(cfg)
	}
	if isContainerBackend(envType) {
		containerConfig = containerConfigFromConfig(cfg)
	}
	e, err := createEnvironment(ctx, env, envType, image, cwd, probeConfigInt(cfg["timeout"], 180), sshConfig, containerConfig, nil,
		"prompt-backend-probe", hostCwd, true)
	if err != nil {
		return "", err
	}
	result, execErr := e.Execute(ctx, backendProbeCmd, ExecOptions{Timeout: 4})
	if cerr := cleanupEnv(e, true); cerr != nil {
		slog.Debug("Backend probe cleanup failed", "error", cerr)
	}
	if execErr != nil {
		return "", execErr
	}
	if result.ReturnCode != 0 {
		slog.Debug(fmt.Sprintf("Backend probe returned non-zero: %+v", *result))
		return "", nil
	}
	return pystr.Strip(result.Output), nil
}

// Indented OS summary from the probe key value lines, empty when nothing usable came back
func formatBackendProbe(output string) string {
	parsed := map[string]string{}
	for _, line := range pystr.Splitlines(output, false) {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			parsed[pystr.Strip(k)] = pystr.Strip(v)
		}
	}
	parts := []string{}
	for _, key := range []string{"os", "kernel"} {
		if v := parsed[key]; v != "" && v != "unknown" {
			parts = append(parts, v)
		}
	}
	if osLine := strings.Join(parts, " "); osLine != "" {
		return "  OS: " + osLine
	}
	return ""
}

// Cached live description of a non local backend, false when the probe failed or returned nothing
func ProbeRemoteBackend(ctx context.Context, env *Env, envType string) (string, bool, error) {
	cwd, err := TerminalEnvRead(env, "TERMINAL_CWD", "")
	if err != nil {
		return "", false, err
	}
	key := backendProbeKey{home.Key(envHome(env)), envType, cwd}
	backendProbeCache.Lock()
	formatted, ok := backendProbeCache.entries[key]
	backendProbeCache.Unlock()
	if !ok {
		output, err := runBackendProbe(ctx, env, envType)
		if err != nil {
			slog.Debug(fmt.Sprintf("Backend probe failed: %v", err))
		}
		formatted = formatBackendProbe(output)
		backendProbeCache.Lock()
		backendProbeCache.entries[key] = formatted
		backendProbeCache.Unlock()
	}
	return formatted, formatted != "", nil
}

// Drops every cached backend probe so a changed backend is probed again
func ClearBackendProbeCache() {
	backendProbeCache.Lock()
	backendProbeCache.entries = map[backendProbeKey]string{}
	backendProbeCache.Unlock()
}
