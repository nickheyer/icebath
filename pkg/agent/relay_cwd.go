package agent

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

var relayCwdSentinels = map[string]bool{"": true, ".": true, "./": true, "auto": true, "cwd": true}

// Lookups the relay cwd resolution reads, each failing the way its hermes counterpart raises
var (
	relayRecordedSessionCwd = func(env *tools.Env, key string) (any, error) {
		if cwd, ok := tools.GetSessionCwd(env, key); ok {
			return cwd, nil
		}
		return nil, nil
	}
	relayScopedSessionCwd = func(env *tools.Env) (any, error) { return tools.ScopedSessionCwd(env), nil }
	relaySessionEnv       = func(env *tools.Env, name, def string) (string, error) {
		return tools.GetSessionEnv(env, name, def), nil
	}
	relayTerminalEnv     = tools.TerminalEnvRead
	relayResolveAgentCwd = tools.ResolveAgentCwd
)

// Trimmed cwd text, empty for a non string or a sentinel naming no directory
func cleanRelayCwd(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	s = pystr.Strip(s)
	if relayCwdSentinels[pystr.Lower(s)] {
		return ""
	}
	return s
}

// Cleaned cwd recorded for a session or task key, empty for an empty key
func relayRecordedCwd(env *tools.Env, key string) (string, error) {
	if key == "" {
		return "", nil
	}
	v, err := relayRecordedSessionCwd(env, key)
	if err != nil {
		return "", err
	}
	return cleanRelayCwd(v), nil
}

// Absolute form of a host path as Path.resolve gives it, the path itself when it cannot be resolved
func resolveHostPath(p string) string {
	resolved, err := pypath.Resolve(p)
	if err != nil {
		return p
	}
	return resolved
}

// Logical session and turn cwd for relay scopes, keeping remote paths as declared and omitting unknown ones
func resolveRelayScopeCwds(a *AIAgent, taskID, sessionID, platform string) (*string, *string) {
	var env *tools.Env
	agentCwd := ""
	if a != nil {
		env, agentCwd = a.env, a.sessionCwd
	}
	taskCwd, err := relayRecordedCwd(env, taskID)
	if err != nil {
		runAgentLog().Debug(fmt.Sprintf("Unable to read the Relay turn cwd: %v", err))
		taskCwd = ""
	}
	sessionCwd := ""
	if scoped, err := relayScopedSessionCwd(env); err != nil {
		runAgentLog().Debug(fmt.Sprintf("Unable to read the scoped Relay session cwd: %v", err))
	} else {
		sessionCwd = cleanRelayCwd(scoped)
	}
	if sessionCwd == "" {
		if err := func() error {
			sessionKey, err := relaySessionEnv(env, "ICEBATH_SESSION_KEY", "")
			if err != nil {
				return err
			}
			seen := map[string]bool{}
			for _, key := range []string{sessionKey, sessionID} {
				if key == "" || seen[key] {
					continue
				}
				seen[key] = true
				recorded, err := relayRecordedCwd(env, key)
				if err != nil {
					return err
				}
				if recorded != "" {
					sessionCwd = recorded
					break
				}
			}
			return nil
		}(); err != nil {
			runAgentLog().Debug(fmt.Sprintf("Unable to read the recorded Relay session cwd: %v", err))
		}
	}
	if sessionCwd == "" {
		sessionCwd = cleanRelayCwd(agentCwd)
	}
	backend := ""
	if sessionCwd == "" {
		if err := func() error {
			value, err := relayTerminalEnv(env, "TERMINAL_ENV", "local")
			if err != nil {
				return err
			}
			backend = pystr.Lower(pystr.Strip(value))
			cwd, err := relayTerminalEnv(env, "TERMINAL_CWD", "")
			if err != nil {
				return err
			}
			sessionCwd = cleanRelayCwd(cwd)
			return nil
		}(); err != nil {
			runAgentLog().Debug(fmt.Sprintf("Unable to read the configured Relay session cwd: %v", err))
		}
	}
	if sessionCwd == "" && (platform == "" || platform == "cli") && (backend == "" || backend == "local") {
		if resolved, err := relayResolveAgentCwd(env); err != nil {
			runAgentLog().Debug(fmt.Sprintf("Unable to resolve the local Relay session cwd: %v", err))
		} else if pypath.IsAbs(resolved) {
			sessionCwd = cleanRelayCwd(resolved)
		} else if absolute, err := pypath.Resolve(resolved); err != nil {
			runAgentLog().Debug(fmt.Sprintf("Unable to resolve the local Relay session cwd: %v", err))
		} else {
			sessionCwd = cleanRelayCwd(absolute)
		}
	}
	turnCwd := taskCwd
	if turnCwd == "" {
		turnCwd = sessionCwd
	}
	if (platform == "subagent" || platform == "cron") && taskCwd != "" {
		sessionCwd = taskCwd
	}
	if sessionCwd == "" {
		sessionCwd = turnCwd
	}
	return &sessionCwd, &turnCwd
}
