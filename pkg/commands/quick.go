package commands

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// QuickExecTimeout caps one type exec quick command
const QuickExecTimeout = 30 * time.Second

// QuickRunner executes type exec quick commands with a sanitized environment and redacted output
type QuickRunner struct {
	Env     func() []string
	Redact  func(string) string
	Surface string
	Shell   []string
}

// ErrQuickNoEnv means no sanitized environment builder was wired
var ErrQuickNoEnv = errors.New("quick command: no sanitized environment builder bound")

// ErrQuickNoRedactor means no output redactor was wired
var ErrQuickNoRedactor = errors.New("quick command: no output redactor bound")

// Run executes the shell snippet in cwd and returns the reply text for the user
func (q *QuickRunner) Run(ctx context.Context, command, cwd string) (string, error) {
	if q.Env == nil {
		return "", ErrQuickNoEnv
	}
	if q.Redact == nil {
		return "", ErrQuickNoRedactor
	}
	msgs := quickMessages(q.Surface)
	shell := q.Shell
	if len(shell) == 0 {
		shell = []string{"/bin/sh", "-c"}
	}
	runCtx, cancel := context.WithTimeout(ctx, QuickExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, shell[0], append(shell[1:], command)...)
	cmd.Env = q.Env()
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return msgs.TimedOut, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return msgs.Error(err.Error()), nil
	}
	output := strings.TrimSpace(stdout.String())
	if output == "" {
		output = strings.TrimSpace(stderr.String())
	}
	if output == "" {
		return msgs.NoOutput, nil
	}
	return q.Redact(output), nil
}

// QuickNoOutputText returns the empty output reply for a surface
func QuickNoOutputText(surface string) string { return quickMessages(surface).NoOutput }

// QuickTimedOutText returns the timeout reply for a surface
func QuickTimedOutText(surface string) string { return quickMessages(surface).TimedOut }

// QuickErrorText returns the failure reply for a surface
func QuickErrorText(surface, err string) string { return quickMessages(surface).Error(err) }
