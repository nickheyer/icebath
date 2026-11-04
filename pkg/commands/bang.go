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
	"syscall"
	"time"
)

// BangUsageHint explains bare exclamation mark input
const BangUsageHint = "Usage: !<command> — run a shell command without spending a model turn (e.g. !git status)"

// BangDefaultTimeout caps one bang command well under the terminal tool foreground limit
const BangDefaultTimeout = 120 * time.Second

// IsBangCommand reports whether text is a shell mode submission with a leading exclamation mark
func IsBangCommand(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "!")
}

// ParseBangCommand returns the shell command inside a bang submission, empty when bare
func ParseBangCommand(text string) string {
	if !IsBangCommand(text) {
		return ""
	}
	return strings.TrimSpace(strings.TrimSpace(text)[1:])
}

func envEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// BangShellEnabled is true only for interactive local sessions, never gateway, cron or platform runs
func BangShellEnabled() bool {
	return !(envEnabled("ICEBATH_GATEWAY_SESSION") || envEnabled("ICEBATH_CRON_SESSION") ||
		strings.TrimSpace(os.Getenv("ICEBATH_SESSION_PLATFORM")) != "")
}

// Approval is the terminal guard verdict for a user typed command
type Approval struct {
	Approved    bool
	Message     string
	Description string
}

// BangRunner executes bang commands with the sanitized environment and approval gate of the terminal tool
type BangRunner struct {
	Env     func() []string
	Approve func(command string) Approval
	CWD     func(sessionKey string) string
	Timeout time.Duration
	Shell   []string
}

// ErrBangNoEnv means no sanitized environment builder was wired so the launch is refused
var ErrBangNoEnv = errors.New("bang shell: no sanitized environment builder bound")

// ErrBangNoGuard means no approval guard was wired so the command is refused
var ErrBangNoGuard = errors.New("bang shell: no approval guard bound")

// CommandDeniedText formats the refusal shown for a guarded command
func CommandDeniedText(a Approval) string {
	if a.Message != "" {
		return a.Message
	}
	reason := a.Description
	if reason == "" {
		reason = "flagged as dangerous"
	}
	return "Command denied: " + reason
}

// BangExitedText formats the trailing exit status line
func BangExitedText(code int) string { return fmt.Sprintf("! exited %d", code) }

// Handle runs a bang submission end to end and reports whether it was handled
func (b *BangRunner) Handle(ctx context.Context, text, sessionKey string, emit func(string)) (bool, error) {
	if !IsBangCommand(text) || !BangShellEnabled() {
		return false, nil
	}
	command := ParseBangCommand(text)
	if command == "" {
		emit(BangUsageHint)
		return true, nil
	}
	if b.Approve == nil {
		return true, ErrBangNoGuard
	}
	if verdict := b.Approve(command); !verdict.Approved {
		emit(CommandDeniedText(verdict))
		return true, nil
	}
	cwd := ""
	if b.CWD != nil {
		cwd = b.CWD(sessionKey)
	}
	code, err := b.Run(ctx, command, cwd, emit)
	if err != nil {
		return true, err
	}
	if code != 0 {
		emit(BangExitedText(code))
	}
	return true, nil
}

// Run executes a command through the shell streaming merged output and returns the exit code
func (b *BangRunner) Run(ctx context.Context, command, cwd string, emit func(string)) (int, error) {
	if b.Env == nil {
		return 127, ErrBangNoEnv
	}
	timeout := b.Timeout
	if timeout <= 0 {
		timeout = BangDefaultTimeout
	}
	shell := b.Shell
	if len(shell) == 0 {
		shell = []string{"/bin/sh", "-c"}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, shell[0], append(shell[1:], command)...)
	if cwd != "" {
		if expanded := expandHome(cwd); isDir(expanded) {
			cmd.Dir = expanded
		}
	}
	cmd.Env = b.Env()
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		emit(fmt.Sprintf("!: failed to run command: %v", err))
		return 127, nil
	}
	done := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			emit(scanner.Text())
		}
		close(done)
	}()
	waitErr := cmd.Wait()
	pw.Close()
	<-done
	if runCtx.Err() == context.DeadlineExceeded {
		emit(fmt.Sprintf("!: command timed out after %ds", int(timeout/time.Second)))
		return 124, nil
	}
	if ctx.Err() != nil {
		emit("!: interrupted")
		return 130, nil
	}
	if waitErr != nil {
		var exit *exec.ExitError
		if errors.As(waitErr, &exit) {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return 128 + int(status.Signal()), nil
			}
			return exit.ExitCode(), nil
		}
		return 1, waitErr
	}
	return 0, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
