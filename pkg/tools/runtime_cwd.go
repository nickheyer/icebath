package tools

import (
	"os"
	"path/filepath"
	"strings"
)

const sessionCwdKey = "session.cwd"

// Reports whether this process is a registered per turn worker
func isWorkerProcess() bool {
	return IsWorkerProcess != nil && IsWorkerProcess()
}

// PackageRoot is the running install tree the context discovery fallback guards, resolved once at startup
var PackageRoot = RunningSourceRoot()

// IsInstallTree reports whether a path is the running install tree or sits inside it
func IsInstallTree(p string) bool {
	root := PackageRoot
	if root == "" {
		return false
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		rootResolved = root
	}
	return resolved == rootResolved || isUnder(resolved, rootResolved)
}

// SetLogicalSessionCwd pins the logical cwd of the session, returning the previous value for reset
func SetLogicalSessionCwd(env *Env, cwd string) string {
	prev := ScopedSessionCwd(env)
	if env != nil && env.Values != nil {
		env.Values.Store(sessionCwdKey, strings.TrimSpace(cwd))
	}
	return prev
}

// ClearLogicalSessionCwd drops the pinned logical cwd of the session
func ClearLogicalSessionCwd(env *Env) {
	if env != nil && env.Values != nil {
		env.Values.Store(sessionCwdKey, "")
	}
}

// ScopedSessionCwd returns the declared session cwd without validating it locally
func ScopedSessionCwd(env *Env) string {
	if env == nil || env.Values == nil {
		return ""
	}
	v, ok := env.Values.Load(sessionCwdKey)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// Scope aware TERMINAL_CWD value read through the per turn terminal scope
func scopeTerminalCwd(env *Env) (string, error) {
	policy, err := terminalPolicy(env)
	if err != nil {
		return "", err
	}
	return policy.Get("TERMINAL_CWD", ""), nil
}

// Scope aware TERMINAL_CWD value for plugin sessions, read through the per turn terminal scope
func ScopeTerminalCwd(env *Env) (string, error) { return scopeTerminalCwd(env) }

func existingDir(env *Env, raw, label string) string {
	p := expandUser(raw)
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p
	}
	envLog(env).Warn(label+" does not exist", "path", raw)
	return ""
}

// Session override then TERMINAL_CWD, each validated as a real directory
func resolveConfiguredCwd(env *Env, overrideIsFinal, includeSessionOverride bool) (string, error) {
	if includeSessionOverride {
		if override := ScopedSessionCwd(env); override != "" {
			p := existingDir(env, override, "configured working directory")
			if p != "" || overrideIsFinal {
				return p, nil
			}
		}
	}
	raw, err := scopeTerminalCwd(env)
	if err != nil {
		return "", err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	return existingDir(env, raw, "TERMINAL_CWD"), nil
}

// ResolveAgentCwd returns the configured cwd else the launch directory
func ResolveAgentCwd(env *Env) (string, error) {
	p, err := resolveConfiguredCwd(env, false, true)
	if err != nil {
		return "", err
	}
	if p != "" {
		return p, nil
	}
	if env != nil && env.Cwd != "" {
		return env.Cwd, nil
	}
	return os.Getwd()
}

// ResolveContextCwd returns the configured cwd for context file discovery or empty for the launch dir
func ResolveContextCwd(env *Env, includeSessionOverride bool) (string, error) {
	return resolveConfiguredCwd(env, true, includeSessionOverride)
}
