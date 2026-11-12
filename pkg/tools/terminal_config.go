package tools

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
)

// Reports whether this host is Windows
var isWindowsHost = runtime.GOOS == "windows"

// Reports whether this host is Linux
var isLinuxHost = runtime.GOOS == "linux"

// Session flag read from the shared values first and the process environment second
func sessionFlag(env *Env, key, envVar string) bool {
	if env != nil && env.Values != nil {
		if v, ok := env.Values.Load("session." + key); ok {
			if b, ok := v.(bool); ok {
				return b
			}
		}
	}
	return truthyEnvValue(os.Getenv(envVar))
}

// Reports whether an environment value spells true
func truthyEnvValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Session string read from the shared values, then the bound session variable, then the process environment
func sessionValue(env *Env, key, envVar string) string {
	if env != nil && env.Values != nil {
		if v, ok := env.Values.Load("session." + key); ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return GetSessionEnv(env, envVar, "")
}

// Stable identity of an environment for cache scopes
func envIdentity(env *Env) string {
	if env == nil {
		return ""
	}
	return fmt.Sprintf("%p", env)
}

// User facing spelling of the home directory
func displayHome(env *Env) string {
	if env == nil || env.Home == "" {
		return "~/.icebath"
	}
	return home.Display(env.Home)
}

// Parses a terminal setting with a clear error naming the variable
func parseTerminalInt(policy TerminalPolicy, name, def string) (int, error) {
	raw := policy.Get(name, def)
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("Invalid value for %s: %q (expected integer). Check ~/.icebath/.env or environment variables.", name, raw)
	}
	return v, nil
}

// Parses a terminal setting as a number with a clear error naming the variable
func parseTerminalFloat(policy TerminalPolicy, name, def string) (float64, error) {
	raw := policy.Get(name, def)
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("Invalid value for %s: %q (expected number). Check ~/.icebath/.env or environment variables.", name, raw)
	}
	return v, nil
}

// Parses a terminal setting as JSON with a clear error naming the variable
func parseTerminalJSON(policy TerminalPolicy, name, def string, out any) error {
	raw := policy.Get(name, def)
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return fmt.Errorf("Invalid value for %s: %q (expected valid JSON). Check ~/.icebath/.env or environment variables.", name, raw)
	}
	return nil
}

// Profile name of the current home, default when it is not a named profile
func homeProfileName(env *Env) string {
	if env == nil || env.Home == "" {
		return "default"
	}
	if name := home.ProfileNameFor(env.Home); name != "" {
		return name
	}
	return "default"
}

// Persistent Docker container key of a profile, the default profile stays literally default
func profileContainerKey(profile string) string {
	if profile == "default" {
		return "default"
	}
	return "profile:" + profile
}

// Working directory tolerant of a deleted or protected process cwd
func safeGetcwd(env *Env, policy TerminalPolicy) string {
	if env != nil && env.Cwd != "" {
		return env.Cwd
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	if v := policy.Get("TERMINAL_CWD", ""); v != "" {
		return v
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.TempDir()
}

// Host cwd shapes that cannot exist inside a container sandbox
var hostCwdPrefixes = []string{"/Users/", "/home/"}

// Any drive letter path
var windowsDriveRe = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// Reports whether a path is a host user directory or a Windows drive path
func isHostCwd(p string) bool {
	return hasAnyPrefix(p, hostCwdPrefixes) || windowsDriveRe.MatchString(p)
}

// Reports whether a path is a Windows drive path
func isWindowsDrivePath(p string) bool {
	return p != "" && windowsDriveRe.MatchString(p)
}

// Comparison key of a host path ignoring slash style and drive letter case
func hostPathKey(p string) string {
	text := strings.ReplaceAll(p, "\\", "/")
	if len(text) >= 2 && text[1] == ':' {
		text = strings.ToLower(text[:1]) + text[1:]
	}
	if len(text) > 3 {
		text = strings.TrimRight(text, "/")
	}
	return text
}

// Maps a host path onto the container mount when it is the host root or a child of it
func translateMountedHostPath(p, hostRoot, containerRoot string) string {
	if p == "" || hostRoot == "" || containerRoot == "" {
		return ""
	}
	key, root := hostPathKey(p), hostPathKey(hostRoot)
	if key == "" || root == "" {
		return ""
	}
	mount := strings.TrimRight(containerRoot, "/")
	if mount == "" {
		mount = "/"
	}
	if key == root {
		return mount
	}
	prefix := root
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if !strings.HasPrefix(key, prefix) {
		return ""
	}
	return mount + "/" + key[len(prefix):]
}

// Reports whether a cwd was the host workspace or the assumed workspace view of it
func cwdFollowsHostMount(cwd, mount string) bool {
	if mount == "" || cwd == "" || cwd == mount {
		return false
	}
	if isUnusableContainerCwd(cwd, "") {
		return true
	}
	return cwd == "/workspace" && mount != "/workspace"
}

// Built in backends whose filesystem is rooted away from the host
var containerBackends = map[string]bool{"docker": true, "modal": true, "daytona": true, "vercel_sandbox": true}

// Every backend shipped in this package
var builtinBackends = map[string]bool{"docker": true, "modal": true, "daytona": true, "vercel_sandbox": true, "local": true, "ssh": true, "managed_modal": true}

// Every shipped terminal backend name in sorted order
func BuiltinTerminalBackends() []string { return slices.Sorted(maps.Keys(builtinBackends)) }

// Reports whether a backend has its own filesystem rooted away from the host
func isContainerBackend(envType string) bool {
	if containerBackends[envType] {
		return true
	}
	return providerFlagBool(envType, func(p BackendProvider) bool { return p.IsContainer() })
}

// Sandbox home a plugin environment started in, empty when it declared none
func pluginSandboxHome(e Environment) string {
	if e == nil {
		return ""
	}
	h := e.SandboxHome()
	if h != "" && path.IsAbs(h) && h != "/" {
		return h
	}
	return ""
}

// Reports whether cwd is at or under the sandbox home of the environment
func inSandboxHome(cwd string, e Environment) bool {
	h := pluginSandboxHome(e)
	if h == "" || cwd == "" || !path.IsAbs(cwd) {
		return false
	}
	h, cwd = path.Clean(h), path.Clean(cwd)
	return cwd == h || strings.HasPrefix(cwd, h+"/")
}

// Rewrites a cwd for the SSH backend so the host subprocess home lands on the remote home
func coerceSSHRemoteCwd(env *Env, cwd, envType string) string {
	if strings.ToLower(strings.TrimSpace(envType)) != "ssh" {
		return cwd
	}
	text := strings.TrimSpace(cwd)
	if text == "" || strings.HasPrefix(text, "~") {
		if text == "" {
			return "~"
		}
		return text
	}
	subHome := subprocessHome(env, nil)
	if subHome == "" || !path.IsAbs(text) || path.Clean(subHome) == path.Clean(realHome(env, nil)) {
		return text
	}
	rel, err := filepath.Rel(path.Clean(subHome), path.Clean(text))
	if err != nil {
		return text
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return "~"
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return text
	}
	return "~/" + rel
}

// Reports whether cwd is the host directory mounted at the workspace
func isMountedHostCwd(cwd, mountedHost string) bool {
	if cwd == "" || mountedHost == "" {
		return false
	}
	left, err1 := filepath.Abs(expandUser(cwd))
	right, err2 := filepath.Abs(expandUser(mountedHost))
	if err1 != nil || err2 != nil {
		return false
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

// Reports whether cwd is a host or relative path that cannot be a container workdir
func isUnusableContainerCwd(cwd, mountedHost string) bool {
	if cwd == "" {
		return false
	}
	if isMountedHostCwd(cwd, mountedHost) {
		return true
	}
	return isHostCwd(cwd) || !path.IsAbs(cwd)
}

// Expands a leading tilde to the user home directory
func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		h, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return h + p[1:]
	}
	return p
}

// Expands tilde and environment references in a path
func expandPathVars(p string) string {
	return expandUser(os.ExpandEnv(p))
}

// Normalised form of a home path for comparison
func normHomePath(p string) string { return home.NormHomePath(p) }

// Real OS user home, never the profile home of the session or the child environment
func realHome(env *Env, child map[string]string) string {
	return home.RealHome(visionCtx(nil, env), child)
}

// HOME a child process gets under the configured home mode, empty when it keeps its own
func subprocessHome(env *Env, child map[string]string) string {
	return home.SubprocessHome(visionCtx(nil, env), child)
}

// HOME a tool subprocess gets under the home mode, empty when it keeps its own
func SubprocessHome(env *Env) string { return subprocessHome(env, map[string]string{}) }

// Reports whether this process runs inside a container
func runningInContainer() bool { return hostinfo.IsContainer() }

// Applies the subprocess HOME contract and the scratch temp directories to a child environment
func applySubprocessHomeEnv(env *Env, child map[string]string) {
	home.ApplySubprocessHomeEnv(visionCtx(nil, env), child)
}

// Default sandbox image every container backend uses unless configured
var defaultSandboxImage = config.DefaultSandboxImage

// Default Vercel sandbox image
const defaultVercelImage = "vercel/sandbox/universal:latest"
