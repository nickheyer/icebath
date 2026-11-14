package terminal

import (
	"fmt"
	"regexp"
	"strings"
)

const scanChars = 4000

var signalExitNotes = map[int]string{
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

func interpretSignalExit(code int) string {
	if code < 0 {
		signum := -code
		if signum == 2 {
			return ""
		}
		if note, ok := signalExitNotes[signum]; ok {
			return fmt.Sprintf("Command terminated by signal %d: %s", signum, note)
		}
		name, ok := signalName(signum)
		if !ok {
			name = fmt.Sprintf("signal %d", signum)
		}
		return fmt.Sprintf("Command terminated by %s (signal %d)", name, signum)
	}
	if code > 128 {
		if note, ok := signalExitNotes[code-128]; ok {
			return fmt.Sprintf("Exit code %d usually means the command was terminated by signal %d: %s", code, code-128, note)
		}
	}
	return ""
}

var exitCodeSemantics = func() map[string]map[int]string {
	m := map[string]map[int]string{
		"find": {1: "Some directories were inaccessible (partial results may still be valid)"},
		"curl": {6: "Could not resolve host", 7: "Failed to connect to host",
			22: "HTTP response code indicated error (e.g. 404, 500)", 28: "Operation timed out"},
		"git": {1: "Non-zero exit (often normal — e.g. 'git diff' returns 1 when files differ)"},
	}
	for _, k := range []string{"grep", "egrep", "fgrep", "rg", "ag", "ack"} {
		m[k] = map[int]string{1: "No matches found (not an error)"}
	}
	for _, k := range []string{"diff", "colordiff"} {
		m[k] = map[int]string{1: "Files differ (expected, not an error)"}
	}
	for _, k := range []string{"test", "["} {
		m[k] = map[int]string{1: "Condition evaluated to false (expected, not an error)"}
	}
	return m
}()

var segmentSplitRe = regexp.MustCompile(`\s*(?:\|\||&&|[|;])\s*`)

func interpretExitCode(command string, code int) string {
	if code == 0 {
		return ""
	}
	if note := interpretSignalExit(code); note != "" {
		return note
	}
	segments := segmentSplitRe.Split(command, -1)
	last := command
	if len(segments) > 0 {
		last = segments[len(segments)-1]
	}
	last = strings.TrimSpace(last)
	base := ""
	for _, w := range strings.Fields(last) {
		if !strings.Contains(w, "=") || strings.HasPrefix(w, "-") {
			parts := strings.Split(w, "/")
			base = parts[len(parts)-1]
			break
		}
	}
	return exitCodeSemantics[base][code]
}

type outputHint struct {
	re      *regexp.Regexp
	message func(groups []string) string
}

func fixedHint(pattern, message string) outputHint {
	return outputHint{re: regexp.MustCompile(pattern), message: func(groups []string) string {
		out := message
		for i, g := range groups {
			out = strings.ReplaceAll(out, fmt.Sprintf("{%d}", i), g)
		}
		return out
	}}
}

var missingCommandHints = map[string]string{
	"python": "This system has no bare `python` — use `python3`, or the project venv's " +
		"interpreter (e.g. .venv/bin/python).",
	"pip": "This system has no bare `pip` — use `pip3`, `python3 -m pip`, or the project venv's " +
		"pip (e.g. .venv/bin/pip).",
}

func missingCommandHint(missing string) string {
	if h, ok := missingCommandHints[missing]; ok && h != "" {
		return h
	}
	return fmt.Sprintf("`%s` is not installed or not on PATH. Verify with `which %s`; install it "+
		"or use an absolute path instead of retrying the same command.", missing, missing)
}

var outputHints = []outputHint{
	fixedHint(`Unknown JSON field: "?(\w+)`,
		"The installed gh does not support the JSON field '{0}'. The valid field list is "+
			"printed in the output above — retry using only fields from that list."),
	fixedHint(`(?m)^CONFLICT |Automatic merge failed|needs merge`,
		"Git merge conflict. Do not retry this command. Resolve the conflicted files "+
			"listed above (edit, then `git add`), then continue (`git rebase --continue` / "+
			"commit the merge) — or abort with `--abort`."),
	{re: regexp.MustCompile(`(?:bash: line \d+: |bash: |sh: \d*:? ?)?([\w.+-]+): command not found`),
		message: func(groups []string) string { return missingCommandHint(groups[0]) }},
	fixedHint(`[A-Za-z]+Error: No module named '?([\w.]+)`,
		"Python cannot import '{0}'. Most often the wrong interpreter is running: "+
			"activate the project venv (e.g. `source .venv/bin/activate`) or invoke its python "+
			"directly. Only pip install if the package is genuinely absent from that venv."),
	fixedHint(`(?:fatal|error):.*?'([^']+)' already exists`,
		"'{0}' already exists — retrying unchanged will keep failing. Reuse it, choose "+
			"another name, or delete it first if it is genuinely stale."),
	fixedHint(`API rate limit|was submitted too quickly`,
		"GitHub API rate limit hit — immediate retries will keep failing. Continue with "+
			"other work and retry this operation later."),
	fixedHint(`Permission denied|EACCES`,
		"Permission denied. Check ownership/mode of the target path (`ls -la`); prefer a "+
			"user-writable location. Only escalate to sudo if the task genuinely requires it."),
}

var exitCodeHints = map[int]string{
	126: "Exit 126: the file was found but is not executable — `chmod +x` it or invoke it via its interpreter (e.g. `bash script.sh`).",
	137: "Exit 137: the process was SIGKILLed — usually out-of-memory or an external kill. Reduce memory use or check `dmesg | tail` before retrying.",
	124: "Exit 124: the command hit its timeout. Raise timeout= (foreground max 600s) or run it with background=true and notify_on_complete=true.",
}

var passthroughTailRe = regexp.MustCompile(`^\s*(?:tail|head|cat|tee|less|more|wc|sort|uniq)\b[^|]*$`)

var orFallbackRe = regexp.MustCompile(`\|\|\s*(?:echo\b|printf\b|true\b|:\s|:\z)`)

const pipeMaskNote = "exit_code 0 here is the status of the last pipeline command (tail/head/cat/...), NOT of " +
	"the command before the pipe — and the output contains failure indicators. Treat this run " +
	"as FAILED until proven otherwise: re-run the command WITHOUT the pipe (output is " +
	"auto-truncated and the full text is saved to a file, so piping through tail/head is never " +
	"needed) to get the real exit code."

const fallbackMaskNote = "exit_code 0 here is the status of the `||` fallback (echo/true), NOT of the command before " +
	"it — and the output contains failure indicators. Treat this run as FAILED until proven " +
	"otherwise: re-run the command bare to get its real exit code."

func pipesIntoPassthrough(cmd string) bool {
	for i := 0; i < len(cmd); i++ {
		if cmd[i] != '|' {
			continue
		}
		if i > 0 && cmd[i-1] == '|' {
			continue
		}
		if i+1 < len(cmd) && cmd[i+1] == '|' {
			continue
		}
		if passthroughTailRe.MatchString(cmd[i+1:]) {
			return true
		}
	}
	return false
}

var readonlyHeads = map[string]bool{
	"grep": true, "rg": true, "ag": true, "find": true, "ls": true, "cat": true, "head": true, "tail": true, "jq": true, "awk": true,
	"sed": true, "strings": true, "zcat": true, "journalctl": true, "dmesg": true, "echo": true, "printf": true,
}

var failureShapes = regexp.MustCompile(`(?:` +
	`error\[E\d+\]` +
	`|error: could not compile` +
	`|error: aborting due to` +
	`|Traceback \(most recent call last\)` +
	`|(?m:^(?:=+ )?\d+ failed)` +
	`|(?m:^FAILED (?:\S+::|\S+\.py))` +
	`|compilation terminated\.` +
	`|npm ERR!` +
	`|BUILD FAILED|Build FAILED` +
	`|FAILED: ` +
	`|(?m:^make(?:\[\d+\])?: \*\*\*)` +
	`)`)

func firstToken(command string) string {
	for _, tok := range strings.Fields(strings.TrimSpace(command)) {
		if !strings.Contains(tok, "=") || strings.HasPrefix(tok, "=") || strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "/") {
			if i := strings.LastIndex(tok, "/"); i >= 0 {
				return tok[i+1:]
			}
			return tok
		}
	}
	return ""
}

func annotateMaskedSuccess(command, output string) string {
	window := firstRunes(output, scanChars)
	if command == "" || window == "" || readonlyHeads[firstToken(command)] || !failureShapes.MatchString(window) {
		return ""
	}
	if pipesIntoPassthrough(command) {
		return pipeMaskNote
	}
	if orFallbackRe.MatchString(command) {
		return fallbackMaskNote
	}
	return ""
}

func annotateFailure(command string, code int, output string) string {
	if code == 0 {
		return ""
	}
	window := firstRunes(output, scanChars)
	if window != "" {
		for _, h := range outputHints {
			if m := h.re.FindStringSubmatch(window); m != nil {
				if hint := h.message(m[1:]); hint != "" {
					return hint
				}
			}
		}
	}
	return exitCodeHints[code]
}

func failureHint(command string, code int, output, exitNote string) string {
	if code == 0 {
		return annotateMaskedSuccess(command, output)
	}
	if exitNote == "" {
		return annotateFailure(command, code, output)
	}
	return ""
}
