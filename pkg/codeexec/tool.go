package codeexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	maxStderrBytes        = 10_000
	maxSpilledStdoutBytes = 5_000_000
)

// Stdout byte cap of one result, swapped by tests
var maxStdoutBytes = 50_000

// Byte capped stdout with 40 percent head and 60 percent tail, truncation metadata and the spill path
func truncateStdoutText(env *tools.Env, stdoutText string) (string, *jsonutil.Object) {
	raw := []byte(stdoutText)
	total := len(raw)
	captured := min(total, maxStdoutBytes)
	meta := jsonutil.NewObject()
	meta.Set("stdout_truncated", total > captured)
	meta.Set("stdout_bytes_captured", captured)
	meta.Set("stdout_bytes_total", total)
	meta.Set("stdout_bytes_omitted", total-captured)
	if total <= maxStdoutBytes {
		return strings.ToValidUTF8(string(raw), "\uFFFD"), meta
	}
	head, tail := tools.HeadTailSplit(maxStdoutBytes)
	text := strings.ToValidUTF8(string(raw[:head]), "\uFFFD") + tools.TruncationNotice(total-captured, total, "", "bytes") +
		strings.ToValidUTF8(string(raw[len(raw)-tail:]), "\uFFFD")
	meta.Set("warning", "execute_code stdout was truncated; the script did run, but only "+
		"the captured head/tail output is included. Re-run only with "+
		"narrower output if the omitted data is required.")
	if spill := spillFullStdout(env, stdoutText); spill != "" {
		meta.Set("stdout_spill_path", spill)
		meta.Set("warning", "execute_code stdout was truncated (head/tail shown); the "+
			"script did run. FULL output saved to "+spill+" — page it "+
			`with read_file(path="`+spill+`", offset=...) instead of re-running.`)
	}
	return text, meta
}

// Full stdout written under cache/exec keyed by digest, empty when the write fails
var spillFullStdout = func(env *tools.Env, stdoutText string) string {
	if utf8.RuneCountInString(stdoutText) > maxSpilledStdoutBytes {
		stdoutText = string([]rune(stdoutText)[:maxSpilledStdoutBytes]) + "\n\n[... spill capped at 5,000,000 bytes ...]"
	}
	dir := home.IcebathDir(envHome(env), "cache/exec", "exec_spill")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		pkgLog(env).Debug("Failed to spill execute_code stdout: " + err.Error())
		return ""
	}
	sum := sha256.Sum256([]byte(stdoutText))
	path := filepath.Join(dir, "stdout-"+hex.EncodeToString(sum[:])[:12]+".txt")
	if err := tools.WriteTextExclusive(path, stdoutText, false, true); err != nil {
		pkgLog(env).Debug("Failed to spill execute_code stdout: " + err.Error())
		return ""
	}
	return path
}

// Session home of the env, else the active home of the process
func envHome(env *tools.Env) string {
	if env != nil && env.Home != "" {
		return env.Home
	}
	return home.Dir(context.Background())
}

// Interruption marker appended to stdout with the recorded cause when one exists
func formatInterruptedOutput(ctx context.Context, stdoutText string) string {
	marker := "[execution interrupted]"
	if reason := tools.InterruptReason(ctx); reason != "" {
		marker = "[execution interrupted — " + reason + "]"
	}
	if stdoutText != "" {
		return stdoutText + "\n" + marker
	}
	return marker
}

// Byte cap with spill, ANSI strip and code file secret redaction of stdout
func cleanOutput(env *tools.Env, stdoutText string) (string, *jsonutil.Object) {
	text, meta := truncateStdoutText(env, stdoutText)
	return tools.RedactSensitiveTextWith(env, tools.StripANSI(text), tools.RedactOptions{CodeFile: true}), meta
}

// Timeout message appended to the output so an empty result never reads as success
func withTimeoutNotice(stdoutText, timeoutMsg string) string {
	if stdoutText != "" {
		return stdoutText + "\n\n⏰ " + timeoutMsg
	}
	return "⏰ " + timeoutMsg
}

// Error result JSON with the call count, duration and the optional one sentence user summary
func errorResult(msg string, toolCallsMade int, duration any, userSummary string) string {
	body := jsonutil.NewObject()
	body.Set("status", "error")
	body.Set("error", msg)
	body.Set("tool_calls_made", toolCallsMade)
	body.Set("duration_seconds", duration)
	if userSummary != "" {
		body.Set("user_summary", userSummary)
	}
	out, err := jsonutil.Encode(body, jsonutil.Spaced())
	if err != nil {
		return toolError(err.Error())
	}
	return out
}

// Tree kill and the bounded wait for the child's exit, swapped by tests
var (
	killProcessTree = provider.KillProcessTree
	waitExited      = func(exited <-chan struct{}, d time.Duration) bool {
		select {
		case <-exited:
			return true
		case <-time.After(d):
			return false
		}
	}
)

// Terminates the process tree, escalating to SIGKILL after a five second grace, falling back to a plain kill and never panicking
var killProcessGroup = func(pid int, kill func() error, exited <-chan struct{}, escalate bool) {
	signalTree(pid, kill, syscall.SIGTERM)
	if escalate && !waitExited(exited, 5*time.Second) {
		signalTree(pid, kill, syscall.SIGKILL)
	}
}

// Signals the process tree, falling back to a plain kill of the leader when the tree walk panics
func signalTree(pid int, kill func() error, sig syscall.Signal) {
	defer func() {
		if r := recover(); r != nil {
			pkgLog(nil).Debug(fmt.Sprintf("Could not terminate process tree: %v", r))
			if err := kill(); err != nil {
				pkgLog(nil).Debug("Could not kill process: " + err.Error())
			}
		}
	}()
	killProcessTree(pid, sig)
}

// Configured code_execution.mode, project or strict, an unknown value taking the setting's default
func getExecutionMode(env *tools.Env) string { return settings.CodeExecutionMode.Get(settingsCtx(env)) }
