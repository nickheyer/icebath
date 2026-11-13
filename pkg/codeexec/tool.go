package codeexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	maxStderrBytes        = 10_000
	maxSpilledStdoutBytes = 5_000_000
)

// Stdout byte cap of one result, swapped by tests
var maxStdoutBytes = 50_000

// Byte capped stdout with 40 percent head and 60 percent tail, truncation metadata and the spill path
func truncateStdoutText(env *tools.Env, stdoutText string) (string, *pyjson.Object) {
	raw := pystr.EncodeUTF8Replace(stdoutText)
	total := len(raw)
	captured := min(total, maxStdoutBytes)
	meta := pyjson.NewObject()
	meta.Set("stdout_truncated", total > captured)
	meta.Set("stdout_bytes_captured", captured)
	meta.Set("stdout_bytes_total", total)
	meta.Set("stdout_bytes_omitted", total-captured)
	if total <= maxStdoutBytes {
		return pystr.DecodeUTF8Replace(raw), meta
	}
	head, tail := tools.HeadTailSplit(maxStdoutBytes)
	text := pystr.DecodeUTF8Replace(raw[:head]) + tools.TruncationNotice(total-captured, total, "", "bytes") +
		pystr.DecodeUTF8Replace(raw[len(raw)-tail:])
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
	if pystr.Len(stdoutText) > maxSpilledStdoutBytes {
		stdoutText = pystr.FromRunes(pystr.Runes(stdoutText)[:maxSpilledStdoutBytes]) + "\n\n[... spill capped at 5,000,000 bytes ...]"
	}
	dir := home.HermesDir(envHome(env), "cache/exec", "exec_spill")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		logger(env).Debug("Failed to spill execute_code stdout: " + err.Error())
		return ""
	}
	sum := sha256.Sum256(pystr.EncodeUTF8Replace(stdoutText))
	path := filepath.Join(dir, "stdout-"+hex.EncodeToString(sum[:])[:12]+".txt")
	if err := tools.WriteTextExclusive(path, stdoutText, false, true); err != nil {
		logger(env).Debug("Failed to spill execute_code stdout: " + err.Error())
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
func cleanOutput(env *tools.Env, stdoutText string) (string, *pyjson.Object) {
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
	body := pyjson.NewObject()
	body.Set("status", "error")
	body.Set("error", msg)
	body.Set("tool_calls_made", toolCallsMade)
	body.Set("duration_seconds", duration)
	if userSummary != "" {
		body.Set("user_summary", userSummary)
	}
	out, err := pyjson.Dumps(body, pyjson.EnsureASCII(false))
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
			logger(nil).Debug(fmt.Sprintf("Could not terminate process tree: %v", r))
			if err := kill(); err != nil {
				logger(nil).Debug("Could not kill process: " + err.Error())
			}
		}
	}()
	killProcessTree(pid, sig)
}

// Effective code_execution section of the active config, empty when it cannot load
func loadConfig(env *tools.Env) map[string]any {
	h := envHome(env)
	lookup := config.NewSecrets(h).Lookup()
	if env != nil && env.Secrets != nil {
		lookup = env.Secrets.Lookup()
	}
	tree, err := config.LoadReadonly(context.Background(), h, lookup)
	if err != nil {
		return map[string]any{}
	}
	if section, ok := tree["code_execution"].(map[string]any); ok {
		return section
	}
	return map[string]any{}
}

var executionModes = []string{"project", "strict"}

const defaultExecutionMode = "project"

// Configured code_execution.mode, an unknown value warned about and replaced by project
func getExecutionMode(env *tools.Env) string {
	raw, ok := loadConfig(env)["mode"]
	if !ok {
		raw = defaultExecutionMode
	}
	value := pystr.Lower(pystr.Strip(pyrepr.Str(raw)))
	for _, mode := range executionModes {
		if value == mode {
			return value
		}
	}
	logger(env).Warn("Ignoring code_execution.mode=" + pyrepr.StrRepr(value) + " (expected one of ('project', 'strict')), falling back to 'project'")
	return defaultExecutionMode
}
