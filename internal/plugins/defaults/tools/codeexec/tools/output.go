package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/tools"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/internal/tools/redact"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/term"
)

const (
	// Bytes of stdout a result shows inline, its head and tail, and of stderr kept
	maxStdout = 50_000
	maxStderr = 10_000
	// Characters of the full stdout saved aside
	maxSpilled = 5_000_000
)

// What one run comes to, as the model reads it
type result struct {
	Status        string      `json:"status"`
	Output        string      `json:"output"`
	ExitCode      int         `json:"exit_code"`
	Error         string      `json:"error,omitempty"`
	ToolCallsMade int         `json:"tool_calls_made"`
	Duration      float64     `json:"duration_seconds"`
	Kernel        *kernelInfo `json:"kernel,omitempty"`
	stdout
	ToolErrors []toolError `json:"tool_errors,omitempty"`
	Hint       string      `json:"hint,omitempty"`
}

// What became of the stdout of a run: how much of it the result shows, and where the whole of it went
type stdout struct {
	Truncated bool   `json:"stdout_truncated"`
	Captured  int    `json:"stdout_bytes_captured"`
	Total     int    `json:"stdout_bytes_total"`
	Omitted   int    `json:"stdout_bytes_omitted"`
	Warning   string `json:"warning,omitempty"`
	Spill     string `json:"stdout_spill_path,omitempty"`
}

// The session kernel a run went to
type kernelInfo struct {
	Mode           string `json:"mode,omitempty"`
	Remote         bool   `json:"remote,omitempty"`
	Reused         bool   `json:"reused"`
	ExecutionCount int    `json:"execution_count"`
	StateReset     bool   `json:"state_reset"`
	StateLost      bool   `json:"state_lost,omitempty"`
	Ended          bool   `json:"ended,omitempty"`
	Note           string `json:"note,omitempty"`
}

// Result of a run that failed before or around its script, with the calls it made and how long it took
func failed(message string, calls int, seconds float64, summary string) string {
	return tools.JSON(struct {
		Status        string  `json:"status"`
		Error         string  `json:"error"`
		ToolCallsMade int     `json:"tool_calls_made"`
		Duration      float64 `json:"duration_seconds"`
		UserSummary   string  `json:"user_summary,omitempty"`
	}{"error", message, calls, seconds, summary})
}

// Stdout cut to its head and tail when longer than a result shows, the whole of it saved to a file the result names
func (r *registrant) truncate(cfg *config.Config, env *tools.Env, text string) (string, stdout) {
	raw := []byte(text)
	total := len(raw)
	captured := min(total, maxStdout)
	meta := stdout{Truncated: total > captured, Captured: captured, Total: total, Omitted: total - captured}
	if total <= maxStdout {
		return strings.ToValidUTF8(text, "�"), meta
	}
	head := maxStdout * 2 / 5
	tail := maxStdout - head
	shown := strings.ToValidUTF8(string(raw[:head]), "�") +
		fmt.Sprintf("\n\n... [OUTPUT TRUNCATED - %s bytes omitted out of %s total] ...\n\n", strs.Thousands(total-captured), strs.Thousands(total)) +
		strings.ToValidUTF8(string(raw[len(raw)-tail:]), "�")
	meta.Warning = "execute_code stdout was truncated; the script did run, but only the captured head/tail output is included. Re-run only with " +
		"narrower output if the omitted data is required."
	if path := spill(cfg, text); path != "" {
		if env.Visible != nil {
			path = env.Visible(path)
		}
		meta.Spill = path
		meta.Warning = "execute_code stdout was truncated (head/tail shown); the script did run. FULL output saved to " + path + " — page it with " +
			`read_file(path="` + path + `", offset=...) instead of re-running.`
	}
	return shown, meta
}

// Saves the whole stdout under the profile's exec cache by its digest, empty when it could not be written
func spill(cfg *config.Config, text string) string {
	if utf8.RuneCountInString(text) > maxSpilled {
		text = string([]rune(text)[:maxSpilled]) + "\n\n[... spill capped at 5,000,000 bytes ...]"
	}
	dir := filepath.Join(cfg.ProfileDir(cfg.Profile()), "cache", "exec")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Debug("Failed to spill execute_code stdout: " + err.Error())
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	path := filepath.Join(dir, "stdout-"+hex.EncodeToString(sum[:])[:12]+".txt")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return path
	}
	if err != nil {
		slog.Debug("Failed to spill execute_code stdout: " + err.Error())
		return ""
	}
	_, err = f.WriteString(text)
	if err = errors.Join(err, f.Close()); err != nil {
		os.Remove(path)
		slog.Debug("Failed to spill execute_code stdout: " + err.Error())
		return ""
	}
	return path
}

// Output as the model may read it: terminal escapes gone, secrets masked when the profile hides them
func clean(cfg *config.Config, text string) string {
	text = term.StripANSI(text)
	if !cfg.GetBool(toolsconfig.RedactSecrets) {
		return text
	}
	return redact.Text(text, false, redact.Ends)
}

// Marker ending the output of a run the user stopped, with why when it is known
func interrupted(ctx context.Context, text string) string {
	marker := "[execution interrupted]"
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		marker = "[execution interrupted — " + cause.Error() + "]"
	}
	if text != "" {
		return text + "\n" + marker
	}
	return marker
}

// Output with the timeout said after it, so an empty one never reads as success
func timedOut(text, message string) string {
	if text != "" {
		return text + "\n\n⏰ " + message
	}
	return "⏰ " + message
}

func withStderr(out, errText string) string {
	return out + "\n--- stderr ---\n" + errText
}
