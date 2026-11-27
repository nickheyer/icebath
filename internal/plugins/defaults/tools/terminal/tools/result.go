package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	terminalconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/term"
)

// What a terminal call tells the model, in the order it reads it
type result struct {
	Output         string  `json:"output"`
	ExitCode       *int    `json:"exit_code"`
	Error          *string `json:"error"`
	Status         string  `json:"status,omitempty"`
	UserSummary    string  `json:"user_summary,omitempty"`
	SessionID      string  `json:"session_id,omitempty"`
	Pid            int     `json:"pid,omitempty"`
	Reason         string  `json:"reason,omitempty"`
	RetryHint      string  `json:"retry_hint,omitempty"`
	Cwd            string  `json:"cwd,omitempty"`
	Recreated      string  `json:"environment_recreated,omitempty"`
	TotalChars     int     `json:"output_total_chars,omitempty"`
	FullOutput     string  `json:"full_output_path,omitempty"`
	TruncationNote string  `json:"truncation_note,omitempty"`
	// What the command proved when it checked the session's workspace
	Evidence          *tools.Evidence `json:"verification_evidence,omitempty"`
	Approval          string          `json:"approval,omitempty"`
	ExitMeaning       string          `json:"exit_code_meaning,omitempty"`
	Hint              string          `json:"hint,omitempty"`
	SudoAuthFailed    bool            `json:"sudo_auth_failed,omitempty"`
	SudoCacheCleared  bool            `json:"sudo_cache_cleared,omitempty"`
	Persist           bool            `json:"persist_on_release,omitempty"`
	PtyNote           string          `json:"pty_note,omitempty"`
	AutoBackgrounded  string          `json:"auto_backgrounded,omitempty"`
	Notify            *bool           `json:"notify_on_complete,omitempty"`
	NotifyUnsupported string          `json:"notify_unsupported,omitempty"`
	WatchIgnored      string          `json:"watch_patterns_ignored,omitempty"`
	HeartbeatSeconds  int             `json:"heartbeat_seconds,omitempty"`
	HeartbeatIgnored  string          `json:"heartbeat_ignored,omitempty"`
	WatchPatterns     []string        `json:"watch_patterns,omitempty"`
	Promoted          string          `json:"promoted_from_foreground,omitempty"`
	Note              string          `json:"note,omitempty"`
}

// Warning when the backend made its sandbox again while the command ran
const recreatedNote = "The execution environment was recreated while running this command (the previous container/sandbox was gone). Persistent " +
	"files were restored where the backend supports it, but background processes and any files outside persisted paths from earlier commands " +
	"may be lost — verify state before relying on prior work."

// Turns a finished command into what the model reads: the output cleaned and cut, the folder it ended in, what its exit code means
func (r *registrant) finish(ctx context.Context, call *tools.Call, lease *terminal.Lease, req request, command, cwd, note string,
	res *terminal.Result) string {
	cfg := call.Env.Config
	if req.workdir == "" && res.Cwd != "" {
		lease.Record(res.Cwd)
	}
	output, authFailed, cleared := r.sudoNotes(call, command, res.Output)
	// The transform_terminal_output hooks may replace the output before it is cut to fit and the model reads it
	code := res.Code
	output = r.hooks.TransformTerminal(ctx, cfg, hooks.TerminalOutput{Command: command, Output: output, ReturnCode: &code,
		Task: cmp.Or(hooks.TurnOf(ctx).Task, call.SessionID), Backend: lease.Backend.Name, ID: call.ToolCallID})
	limit := cfg.GetInt(toolsconfig.OutputMaxBytes)
	output = term.StripANSI(terminal.Truncate(output, limit, ""))
	output = terminal.Redact(cfg, strings.TrimSpace(output), command, false)
	meaning := exitMeaning(command, res.Code)
	hint := ""
	if res.Code == 0 {
		hint = maskedSuccess(command, output)
	} else if meaning == "" {
		hint = failureHint(res.Code, output, cfg.GetInt(terminalconfig.MaxForeground))
	}
	changed := ""
	if res.Cwd != "" && cwd != "" && realPath(res.Cwd) != realPath(cwd) {
		changed = res.Cwd
	}
	if note != "" && res.Code == 130 && strings.Contains(output, "[Command interrupted]") {
		note = strings.TrimRight(note, ".") + ", then interrupted."
	}
	out := result{Output: output, ExitCode: &res.Code, Cwd: changed, Approval: note, ExitMeaning: meaning, Hint: hint,
		SudoAuthFailed: authFailed, SudoCacheCleared: cleared}
	out.Evidence = r.tools.Verified(call.Env, tools.Run{Command: command, Cwd: cwd, Session: call.SessionID, Code: res.Code, Output: output,
		Pipeline: res.Pipeline})
	if res.Recreated {
		out.Recreated = recreatedNote
	}
	if res.Spilled != "" {
		if err := cleanSpill(cfg, res.Spilled, command); err != nil {
			slog.Debug("spill cleanup failed, dropping spill handle", "error", err)
			os.Remove(res.Spilled)
		} else {
			saved := r.host.Visible(cfg, res.Spilled)
			out.TotalChars, out.FullOutput = res.Total, saved
			out.TruncationNote = fmt.Sprintf("Output exceeded the capture window (head+tail shown). Full output (%s chars) saved to %s — search it "+
				"with search_files or page it with read_file instead of re-running the command.", strs.Thousands(res.Total), saved)
		}
	}
	return encode(out)
}

// Rewrites the spill file without its escape sequences and with its secrets masked as the result's output is
func cleanSpill(cfg *config.Config, path, command string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := strings.TrimPrefix(terminal.Decode(raw), "\ufeff")
	return terminal.WriteExclusive(path, terminal.Redact(cfg, term.StripANSI(text), command, false), true, true)
}

func realPath(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return filepath.Clean(path)
}

// Notes a background spawn result that a foreground timeout past the ceiling ran as, a failed spawn left alone
func promote(out string, requested, ceiling int) string {
	var res result
	if json.Unmarshal([]byte(out), &res) != nil || res.Error != nil {
		return out
	}
	template := promotedPollOnly
	if res.Notify != nil && *res.Notify {
		template = promotedNote
	}
	res.Promoted = strings.NewReplacer("{requested}", strconv.Itoa(requested), "{cap}", strconv.Itoa(ceiling)).Replace(template)
	return encode(res)
}

const promotedNote = "Requested foreground timeout {requested}s exceeds the {cap}s cap, so this command was started as a tracked background process " +
	"with notify_on_complete=true instead of being refused. Do NOT re-run it. Its completion (exit code + output tail) arrives as a notification; " +
	"poll with process_manage(action=\"poll\", session_id=...) if you need it sooner."

const promotedPollOnly = "Requested foreground timeout {requested}s exceeds the {cap}s cap, so this command was started as a tracked background " +
	"process instead of being refused. Do NOT re-run it. This session cannot receive completion notifications, so poll it with " +
	"process_manage(action=\"poll\", session_id=...) until it exits."

// Notes of the deaths by signal, by signal number, for both a negative code and 128 plus the number
var signalNotes = map[int]string{
	3:  "SIGQUIT (quit from keyboard)",
	4:  "SIGILL (illegal instruction — corrupt binary or wrong architecture)",
	6:  "SIGABRT (abort — assertion failure, fatal runtime error, or glibc abort)",
	7:  "SIGBUS (bus error — misaligned or unmapped memory access)",
	8:  "SIGFPE (fatal arithmetic error, e.g. integer division by zero)",
	9:  "SIGKILL — often the kernel OOM killer on memory exhaustion, or an explicit kill -9",
	11: "SIGSEGV (segmentation fault — the program crashed)",
	13: "SIGPIPE (wrote to a closed pipe — e.g. output piped to a reader that exited)",
	15: "SIGTERM (terminated — kill/timeout or shutdown requested it to stop)",
	24: "SIGXCPU (CPU time limit exceeded)",
	25: "SIGXFSZ (file size limit exceeded)",
}

// POSIX signal names in Linux numbering
var signalNames = []string{"", "SIGHUP", "SIGINT", "SIGQUIT", "SIGILL", "SIGTRAP", "SIGABRT", "SIGBUS", "SIGFPE", "SIGKILL", "SIGUSR1", "SIGSEGV",
	"SIGUSR2", "SIGPIPE", "SIGALRM", "SIGTERM", "SIGSTKFLT", "SIGCHLD", "SIGCONT", "SIGSTOP", "SIGTSTP", "SIGTTIN", "SIGTTOU", "SIGURG", "SIGXCPU",
	"SIGXFSZ", "SIGVTALRM", "SIGPROF", "SIGWINCH", "SIGIO", "SIGPWR", "SIGSYS"}

// Note of an exit code that means a signal ended the command, empty otherwise
func signalExit(code int) string {
	if code < 0 {
		signum := -code
		if signum == 2 {
			return ""
		}
		if note, ok := signalNotes[signum]; ok {
			return fmt.Sprintf("Command terminated by signal %d: %s", signum, note)
		}
		name := fmt.Sprintf("signal %d", signum)
		if signum < len(signalNames) {
			name = signalNames[signum]
		}
		return fmt.Sprintf("Command terminated by %s (signal %d)", name, signum)
	}
	if note, ok := signalNotes[code-128]; code > 128 && ok {
		return fmt.Sprintf("Exit code %d usually means the command was terminated by signal %d: %s", code, code-128, note)
	}
	return ""
}

// Non zero exit codes that inform rather than fail, by command
var exitSemantics = func() map[string]map[int]string {
	m := map[string]map[int]string{}
	for _, name := range []string{"grep", "egrep", "fgrep", "rg", "ag", "ack"} {
		m[name] = map[int]string{1: "No matches found (not an error)"}
	}
	for _, name := range []string{"diff", "colordiff"} {
		m[name] = map[int]string{1: "Files differ (expected, not an error)"}
	}
	for _, name := range []string{"test", "["} {
		m[name] = map[int]string{1: "Condition evaluated to false (expected, not an error)"}
	}
	m["find"] = map[int]string{1: "Some directories were inaccessible (partial results may still be valid)"}
	m["curl"] = map[int]string{6: "Could not resolve host", 7: "Failed to connect to host", 22: "HTTP response code indicated error (e.g. 404, 500)", 28: "Operation timed out"}
	m["git"] = map[int]string{1: "Non-zero exit (often normal — e.g. 'git diff' returns 1 when files differ)"}
	return m
}()

var segments = regexp.MustCompile(`\s*(?:\|\||&&|[|;])\s*`)

// What a non zero exit code means when it is a signal or informs rather than fails, empty otherwise
func exitMeaning(command string, code int) string {
	if code == 0 {
		return ""
	}
	if note := signalExit(code); note != "" {
		return note
	}
	parts := segments.Split(command, -1)
	last := parts[len(parts)-1]
	base := ""
	for _, word := range strings.Fields(strings.TrimSpace(last)) {
		if !strings.Contains(word, "=") || strings.HasPrefix(word, "-") {
			base = word[strings.LastIndex(word, "/")+1:]
			break
		}
	}
	return exitSemantics[base][code]
}

// Output scanned for failure hints, so hints key on error headers
const hintScan = 4000

// Hint for a bare interpreter missing from a python3 only system
var missingHints = map[string]string{
	"python": "This system has no bare `python` — use `python3`, or the project venv's interpreter (e.g. .venv/bin/python).",
	"pip":    "This system has no bare `pip` — use `pip3`, `python3 -m pip`, or the project venv's pip (e.g. .venv/bin/pip).",
}

func missingHint(name string) string {
	if hint, ok := missingHints[name]; ok {
		return hint
	}
	return "`" + name + "` is not installed or not on PATH. Verify with `which " + name + "`; install it or use an absolute path instead of retrying the same command."
}

// One output pattern and its recovery hint
type outputHint struct {
	pattern *regexp.Regexp
	message func(groups []string) string
}

// Hint quoting the first group of its pattern
func quoted(pattern, message string) outputHint {
	return outputHint{regexp.MustCompile(pattern), func(groups []string) string {
		if len(groups) > 1 {
			return strings.ReplaceAll(message, "{0}", groups[1])
		}
		return message
	}}
}

// Output hints by how often they help, the first match winning
var outputHints = []outputHint{
	quoted(`Unknown JSON field: "?(\w+)`, "The installed gh does not support the JSON field '{0}'. The valid field list is printed in the output above — retry using only fields from that list."),
	quoted(`(?m)^CONFLICT |Automatic merge failed|needs merge`, "Git merge conflict. Do not retry this command. Resolve the conflicted files listed above (edit, then `git add`), then continue (`git rebase --continue` / commit the merge) — or abort with `--abort`."),
	{regexp.MustCompile(`(?:bash: line \d+: |bash: |sh: \d*:? ?)?([\w.+-]+): command not found`), func(groups []string) string { return missingHint(groups[1]) }},
	quoted(`(?:ModuleNotFoundError|ImportError): No module named '?([\w.]+)`, "Python cannot import '{0}'. Most often the wrong interpreter is running: activate the project venv (e.g. `source .venv/bin/activate`) or invoke its python directly. Only pip install if the package is genuinely absent from that venv."),
	quoted(`(?:fatal|error):.*?'([^']+)' already exists`, "'{0}' already exists — retrying unchanged will keep failing. Reuse it, choose another name, or delete it first if it is genuinely stale."),
	quoted(`API rate limit|was submitted too quickly`, "GitHub API rate limit hit — immediate retries will keep failing. Continue with other work and retry this operation later."),
	quoted(`Permission denied|EACCES`, "Permission denied. Check ownership/mode of the target path (`ls -la`); prefer a user-writable location. Only escalate to sudo if the task genuinely requires it."),
}

// Hints of exit codes the semantics above leave out, the foreground ceiling quoted
func exitHint(code, ceiling int) string {
	switch code {
	case 126:
		return "Exit 126: the file was found but is not executable — `chmod +x` it or invoke it via its interpreter (e.g. `bash script.sh`)."
	case 137:
		return "Exit 137: the process was SIGKILLed — usually out-of-memory or an external kill. Reduce memory use or check `dmesg | tail` before retrying."
	case 124:
		return fmt.Sprintf("Exit 124: the command hit its timeout. Raise timeout= (foreground max %ds) or run it with background=true and notify=true.", ceiling)
	}
	return ""
}

// One recovery hint for a failed command, empty when none fits
func failureHint(code int, output string, ceiling int) string {
	window := output[:min(len(output), hintScan)]
	if window != "" {
		for _, hint := range outputHints {
			if groups := hint.pattern.FindStringSubmatch(window); groups != nil {
				if text := hint.message(groups); text != "" {
					return text
				}
			}
		}
	}
	return exitHint(code, ceiling)
}

// Notes for command shapes whose exit code hides the status of what ran before
var maskingNotes = [2]string{
	"exit_code 0 here is the status of the last pipeline command (tail/head/cat/...), NOT of the command before the pipe — and the output contains failure indicators. Treat this run as FAILED until proven otherwise: re-run the command WITHOUT the pipe (output is auto-truncated and the full text is saved to a file, so piping through tail/head is never needed) to get the real exit code.",
	"exit_code 0 here is the status of the `||` fallback (echo/true), NOT of the command before it — and the output contains failure indicators. Treat this run as FAILED until proven otherwise: re-run the command bare to get its real exit code.",
}

var (
	orFallback   = regexp.MustCompile(`\|\|\s*(?:echo\b|printf\b|true\b|:\s|:$)`)
	pipeConsumer = regexp.MustCompile(`\|\s*(?:tail|head|cat|tee|less|more|wc|sort|uniq)\b[^|]*$`)
	// Strong failure shapes of common tools
	failureShapes = regexp.MustCompile(`(?:error\[E\d+\]|error: could not compile|error: aborting due to|Traceback \(most recent call last\)|(?m:^(?:=+ )?\d+ failed)|(?m:^FAILED (?:\S+::|\S+\.py))|compilation terminated\.|npm ERR!|BUILD FAILED|Build FAILED|FAILED: |(?m:^make(?:\[\d+\])?: \*\*\*))`)
)

// Commands whose output carries error text without failing
var readonly = map[string]bool{"grep": true, "rg": true, "ag": true, "find": true, "ls": true, "cat": true, "head": true, "tail": true, "jq": true,
	"awk": true, "sed": true, "strings": true, "zcat": true, "journalctl": true, "dmesg": true, "echo": true, "printf": true}

// Command name after any leading assignments
func head(command string) string {
	for _, word := range strings.Fields(strings.TrimSpace(command)) {
		if !strings.Contains(word, "=") || strings.HasPrefix(word, "=") || strings.HasPrefix(word, "./") || strings.HasPrefix(word, "/") {
			return word[strings.LastIndex(word, "/")+1:]
		}
	}
	return ""
}

// Whether the last pipeline segment is a consumer whose status says nothing of the command before it
func pipedToConsumer(command string) bool {
	loc := pipeConsumer.FindStringIndex(command)
	return loc != nil && !(loc[0] > 0 && command[loc[0]-1] == '|') && !(loc[0]+1 < len(command) && command[loc[0]+1] == '|')
}

// Warning for an exit code 0 that likely hides a failure before it, empty otherwise
func maskedSuccess(command, output string) string {
	window := output[:min(len(output), hintScan)]
	if command == "" || window == "" || readonly[head(command)] || !failureShapes.MatchString(window) {
		return ""
	}
	switch {
	case pipedToConsumer(command):
		return maskingNotes[0]
	case orFallback.MatchString(command):
		return maskingNotes[1]
	}
	return ""
}
