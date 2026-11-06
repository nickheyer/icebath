package tools

import (
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Cwd spellings that name no concrete directory
var relayCwdSentinels = []string{"", ".", "./", "auto", "cwd"}

// Stripped cwd text, empty for a non string or a sentinel spelling
func cleanRelayCwd(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	text = pystr.Strip(text)
	if slices.Contains(relayCwdSentinels, pystr.Lower(text)) {
		return ""
	}
	return text
}

// Lookups the relay resolution reads, seams for the session record, scoped cwd, session env, terminal policy and host cwd
var (
	relaySessionCwdRecord = GetSessionCwd
	relayScopedSessionCwd = ScopedSessionCwd
	relaySessionEnv       = SessionEnv
	relayTerminalEnv      = TerminalEnvRead
	relayResolveAgentCwd  = ResolveAgentCwd
)

// Runs one lookup step, a panic or returned error logged at debug and leaving the turn intact
func relayGuard(env *Env, message string, step func() error) {
	defer func() {
		if r := recover(); r != nil {
			toolsLogger(env).Debug(message, "panic", r)
		}
	}()
	if err := step(); err != nil {
		toolsLogger(env).Debug(message, "error", err)
	}
}

// Recorded session cwd of a key cleaned of sentinels, empty for an empty key or no record
func recordedRelayCwd(env *Env, key string) string {
	if key == "" {
		return ""
	}
	if cwd, ok := relaySessionCwdRecord(env, key); ok {
		return cleanRelayCwd(cwd)
	}
	return ""
}

// ResolveRelayScopeCwds returns the logical session and turn cwds of a Relay scope, unknown paths omitted
func ResolveRelayScopeCwds(env *Env, agentSessionCwd any, taskID, sessionID, platform string) (string, string) {
	taskCwd, sessionCwd, backend := "", "", ""
	relayGuard(env, "Unable to read the Relay turn cwd", func() error {
		taskCwd = recordedRelayCwd(env, taskID)
		return nil
	})
	relayGuard(env, "Unable to read the scoped Relay session cwd", func() error {
		sessionCwd = cleanRelayCwd(relayScopedSessionCwd(env))
		return nil
	})
	if sessionCwd == "" {
		relayGuard(env, "Unable to read the recorded Relay session cwd", func() error {
			var keys []string
			for _, key := range []string{relaySessionEnv(env, "ICEBATH_SESSION_KEY"), sessionID} {
				if key != "" && !slices.Contains(keys, key) {
					keys = append(keys, key)
				}
			}
			for _, key := range keys {
				if recorded := recordedRelayCwd(env, key); recorded != "" {
					sessionCwd = recorded
					break
				}
			}
			return nil
		})
	}
	if sessionCwd == "" {
		sessionCwd = cleanRelayCwd(agentSessionCwd)
	}
	if sessionCwd == "" {
		relayGuard(env, "Unable to read the configured Relay session cwd", func() error {
			value, err := relayTerminalEnv(env, "TERMINAL_ENV", "local")
			if err != nil {
				return err
			}
			backend = pystr.Lower(pystr.Strip(value))
			if value, err = relayTerminalEnv(env, "TERMINAL_CWD", ""); err != nil {
				return err
			}
			sessionCwd = cleanRelayCwd(value)
			return nil
		})
	}
	if sessionCwd == "" && (platform == "" || platform == "cli") && (backend == "" || backend == "local") {
		relayGuard(env, "Unable to resolve the local Relay session cwd", func() error {
			resolved, err := relayResolveAgentCwd(env)
			if err == nil && !pypath.IsAbs(resolved) {
				resolved, err = pypath.Resolve(resolved)
			}
			if err != nil {
				return err
			}
			sessionCwd = cleanRelayCwd(resolved)
			return nil
		})
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
	return sessionCwd, turnCwd
}
