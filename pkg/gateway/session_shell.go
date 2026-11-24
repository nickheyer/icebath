package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
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
	if paramsOutside(params, "session_id", "profile", "command") || ref.SessionID == "" || !isStr || strings.TrimSpace(command) == "" {
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
		backend = strings.ToLower(strings.TrimSpace(pyrepr.Str(backendValue)))
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
	result, err := forwardOffThread(ctx, func() (tuigateway.ShellExecResult, error) { return runShellCommand(ctx, command, policy.Cwd) })
	if err != nil {
		return nil, err
	}
	if err := keepShellLine(a.ProfileID, ref.SessionID, shellLine{At: float64(time.Now().UnixMicro()) / 1e6, Command: command, Result: result}); err != nil {
		pkgLog().Warn(fmt.Sprintf("shell line of session %s not kept for its transcript: %v", ref.SessionID, err))
	}
	return tuigateway.Plain(result), nil
}

// Shell line a person ran in a session, kept beside its stored rows for the transcript and never part of what the model reads
type shellLine struct {
	At      float64                    `json:"at"`
	Command string                     `json:"command"`
	Result  tuigateway.ShellExecResult `json:"result"`
}

// File of the shell lines of a session in the profile's sessions folder
func shellLinesPath(profileHome, sessionID string) string {
	return state.ShellLinesPath(home.SessionsDir(profileHome), sessionID)
}

// Appends a shell line to the session's file
func keepShellLine(profileHome, sessionID string, line shellLine) error {
	data, err := json.Marshal(line)
	if err != nil {
		return err
	}
	path := shellLinesPath(profileHome, sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Display rows with the session's shell lines placed among them by time, each a shell row carrying the command and what it printed
func withShellLines(rows []any, profileHome, sessionID string) ([]any, error) {
	data, err := os.ReadFile(shellLinesPath(profileHome, sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return rows, nil
	}
	if err != nil {
		return nil, err
	}
	var lines []shellLine
	for _, raw := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var line shellLine
		if err := json.Unmarshal(raw, &line); err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	out := make([]any, 0, len(rows)+len(lines))
	next := 0
	place := func(before float64) {
		for ; next < len(lines) && lines[next].At < before; next++ {
			line := lines[next]
			out = append(out, map[string]any{"role": "user", "text": line.Command, "display_kind": "shell", "timestamp": line.At,
				"shell": tuigateway.Plain(line.Result)})
		}
	}
	for _, row := range rows {
		if m, isRow := row.(map[string]any); isRow {
			if ts, isNumber := m["timestamp"].(float64); isNumber {
				place(ts)
			}
		}
		out = append(out, row)
	}
	place(math.Inf(1))
	return out, nil
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

// Runs the command through the platform shell with the scoped profile's scrubbed env, its redacted output tails returned
func runShellCommand(ctx context.Context, command, cwd string) (tuigateway.ShellExecResult, error) {
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return tuigateway.ShellExecResult{}, runtimeRefusal("cwd_unavailable")
	}
	env, err := tools.BuildSubprocessEnvOrdered(ctx, tools.EnvForProfileHome(ctx, nil, home.Dir(ctx)), tools.SubprocessEnvOptions{
		InheritProfileHome: true, ScrubSecrets: true, StripLaunchProfile: true})
	if err != nil {
		return tuigateway.ShellExecResult{}, err
	}
	cmd, err := pysubprocess.ShellCommand(command)
	if err != nil {
		return tuigateway.ShellExecResult{}, runtimeRefusal("command_failed")
	}
	var stdout, stderr bytes.Buffer
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = cwd, env.Environ(), nil, &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return tuigateway.ShellExecResult{}, runtimeRefusal("command_failed")
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
		return tuigateway.ShellExecResult{}, runtimeRefusal("command_timeout")
	}
	code := 0
	if waitErr != nil {
		exitErr, ok := waitErr.(*exec.ExitError)
		if !ok {
			return tuigateway.ShellExecResult{}, runtimeRefusal("command_failed")
		}
		code = pysubprocess.Returncode(exitErr)
	}
	redact := func(raw []byte, n int) string {
		text := universalNewlines(pystr.DecodeUTF8Replace(raw))
		return lastRunes(tools.RedactSensitiveTextWith(nil, text, tools.RedactOptions{Force: true, URLCredentials: true}), n)
	}
	return tuigateway.ShellExecResult{Stdout: redact(stdout.Bytes(), 4000), Stderr: redact(stderr.Bytes(), 2000), Code: int64(code)}, nil
}
