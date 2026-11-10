package commands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/tools"
)

// BangUsageHint explains bare exclamation mark input
const BangUsageHint = "Usage: !<command> — run a shell command without spending a model turn (e.g. !git status)"

// BangDefaultTimeout caps one bang command in seconds well under the terminal tool foreground limit
const BangDefaultTimeout = 120

// Child environment a bang command runs with, swapped by tests
var bangEnv = func(ctx context.Context, env *tools.Env) (map[string]string, error) {
	return tools.BuildSubprocessEnv(ctx, env, tools.SubprocessEnvOptions{InheritProfileHome: true, ScrubSecrets: true})
}

// Shell process a bang command launches, swapped by tests
var bangShellCommand = pysubprocess.ShellCommand

// Reports a shell mode submission, a string whose stripped text starts with an exclamation mark
func IsBangCommand(text any) bool {
	s, ok := text.(string)
	return ok && strings.HasPrefix(pystr.Strip(s), "!")
}

// Shell command inside a bang submission with exactly one leading mark removed, empty when bare
func ParseBangCommand(text string) string {
	if !IsBangCommand(text) {
		return ""
	}
	return pystr.Strip(pystr.Strip(text)[1:])
}

// Reports whether an environment variable holds a truthy string
func envVarEnabled(name string) bool {
	switch pystr.Lower(pystr.Strip(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Reports an interactive local session, false for gateway, cron or platform runs
func BangShellEnabled() bool {
	return !(envVarEnabled("ICEBATH_GATEWAY_SESSION") || envVarEnabled("ICEBATH_CRON_SESSION") ||
		pystr.Strip(os.Getenv("ICEBATH_SESSION_PLATFORM")) != "")
}

// Directory a bang command runs in, the session cd record then the configured terminal cwd, empty for none
func ResolveBangCwd(env *tools.Env, sessionKey string) string {
	if cwd, ok := tools.GetSessionCwd(env, sessionKey); ok && cwd != "" {
		return cwd
	}
	cfg, err := tools.GetEnvConfig(env)
	if err != nil {
		return ""
	}
	cwd, _ := cfg["cwd"].(string)
	return cwd
}

// Terminal tool approval verdict for a user typed command run locally without host access
func CheckBangApproval(ctx context.Context, env *tools.Env, command string) tools.GuardResult {
	return tools.CheckAllCommandGuards(ctx, env, command, "local", false)
}

// Reads universal newline lines decoded as utf-8 with replacement, the terminator dropped
func readBangLine(r *bufio.Reader) (string, bool) {
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return pystr.DecodeUTF8Replace(buf), len(buf) > 0
		}
		switch b {
		case '\n':
			return pystr.DecodeUTF8Replace(buf), true
		case '\r':
			if next, err := r.Peek(1); err == nil && next[0] == '\n' {
				_, _ = r.ReadByte()
			}
			return pystr.DecodeUTF8Replace(buf), true
		}
		buf = append(buf, b)
	}
}

// Runs a command through the shell streaming merged output lines to writer and returns the exit code
func RunBangCommand(ctx context.Context, env *tools.Env, command, cwd string, timeout int, writer func(string)) int {
	emit := writer
	if emit == nil {
		emit = func(line string) {
			end := "\n"
			if strings.HasSuffix(line, "\n") {
				end = ""
			}
			fmt.Fprint(os.Stdout, line+end)
		}
	}
	runCwd := ""
	if cwd != "" {
		runCwd = pypath.Expanduser(cwd)
	}
	if runCwd != "" {
		if st, err := os.Stat(runCwd); err != nil || !st.IsDir() {
			runCwd = ""
		}
	}
	cmd, stdout, err := startBangProcess(ctx, env, command, runCwd)
	if err != nil {
		emit(fmt.Sprintf("!: failed to run command: %v", err))
		return 127
	}
	defer stdout.Close()
	var interrupted atomic.Bool
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			interrupted.Store(true)
			_ = cmd.Process.Kill()
		case <-stopWatch:
		}
	}()
	reader := bufio.NewReader(stdout)
	for {
		line, ok := readBangLine(reader)
		if !ok {
			break
		}
		emit(line)
	}
	close(stopWatch)
	<-watchDone
	if interrupted.Load() {
		_ = cmd.Wait()
		emit("!: interrupted")
		return 130
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	timer := time.NewTimer(time.Duration(timeout) * time.Second)
	defer timer.Stop()
	var waitErr error
	select {
	case waitErr = <-waited:
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-waited
		emit(fmt.Sprintf("!: command timed out after %ds", timeout))
		return 124
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-waited
		emit("!: interrupted")
		return 130
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return pysubprocess.Returncode(exitErr)
	}
	return 0
}

// Starts the shell with merged output in the run directory and the sanitized environment
func startBangProcess(ctx context.Context, env *tools.Env, command, cwd string) (*exec.Cmd, io.ReadCloser, error) {
	childEnv, err := bangEnv(ctx, env)
	if err != nil {
		return nil, nil, err
	}
	cmd, err := bangShellCommand(command)
	if err != nil {
		return nil, nil, err
	}
	cmd.Dir = cwd
	cmd.Env = make([]string, 0, len(childEnv))
	for k, v := range childEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, nil, err
	}
	pw.Close()
	return cmd, pr, nil
}
