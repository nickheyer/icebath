package gateway

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds one captured shell command may run
const shellTimeoutSeconds = 30

var localShellBackends = map[string]bool{"": true, "local": true}

// Shell verb of a connection keyed by method name
func shellHandlers(c *AuthorityConnection) map[string]controlHandler {
	return map[string]controlHandler{
		"shell.exec": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return shellExec(ctx, c, ref, params)
		},
	}
}

// Runs one captured command for a local terminal session in its frozen launch cwd behind the dangerous command refusal
func shellExec(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	command, isStr := params.GetOr("command", nil).(string)
	if paramsOutside(params, "session_id", "profile", "command") || ref.SessionID == "" || !isStr || pystr.Strip(command) == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	a := c.authority
	if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:control"); err != nil {
		return nil, err
	}
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, err
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	local, err := AuthorizeLocalSource(a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	if local == nil || !*local {
		return nil, runtimeRefusal("permission_denied")
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return nil, runtimeRefusal("permission_denied")
	}
	terminal, err := pyjson.Loads(policy.TerminalJSON)
	if err != nil {
		return nil, err
	}
	backendValue, err := pyGet(pyjson.Plain(terminal), "TERMINAL_ENV", nil)
	if err != nil {
		return nil, err
	}
	backend := ""
	if pyrepr.Truthy(backendValue) {
		backend = pystr.Lower(pystr.Strip(pyrepr.Str(backendValue)))
	}
	if !localShellBackends[backend] {
		field, detail := "terminal.backend", backend
		return nil, state.NewRuntimeStoreError("unsupported_terminal_backend", &field, &detail)
	}
	if hardline, _ := tools.DetectHardlineCommand(command); hardline {
		return nil, runtimeRefusal("dangerous_command")
	}
	if _, _, dangerous := tools.DetectDangerousCommand(command); dangerous {
		return nil, runtimeRefusal("dangerous_command")
	}
	return forwardOffThread(ctx, func() (map[string]any, error) { return runShellCommand(ctx, command, policy.Cwd) })
}

// Python text mode newline translation of captured output
func universalNewlines(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// Last n characters of a string
func lastRunes(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[len(runes)-n:])
}

// Runs the command through the platform shell with a scrubbed env, its redacted output tails returned
func runShellCommand(ctx context.Context, command, cwd string) (map[string]any, error) {
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, runtimeRefusal("cwd_unavailable")
	}
	env, err := tools.BuildSubprocessEnvOrdered(ctx, nil, tools.SubprocessEnvOptions{
		InheritProfileHome: true, ScrubSecrets: true, StripLaunchProfile: true})
	if err != nil {
		return nil, err
	}
	cmd, err := pysubprocess.ShellCommand(command)
	if err != nil {
		return nil, runtimeRefusal("command_failed")
	}
	var stdout, stderr bytes.Buffer
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = cwd, env.Environ(), nil, &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, runtimeRefusal("command_failed")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(shellTimeoutSeconds * time.Second)
	defer timer.Stop()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-done
		return nil, runtimeRefusal("command_timeout")
	}
	code := 0
	if waitErr != nil {
		exitErr, ok := waitErr.(*exec.ExitError)
		if !ok {
			return nil, runtimeRefusal("command_failed")
		}
		code = pysubprocess.Returncode(exitErr)
	}
	redact := func(raw []byte, n int) string {
		text := universalNewlines(pystr.DecodeUTF8Replace(raw))
		return lastRunes(tools.RedactSensitiveTextWith(nil, text, tools.RedactOptions{Force: true, URLCredentials: true}), n)
	}
	return map[string]any{"stdout": redact(stdout.Bytes(), 4000), "stderr": redact(stderr.Bytes(), 2000), "code": code}, nil
}
