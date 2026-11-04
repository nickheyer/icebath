package tools

import (
	"regexp"
	"strings"
)

// Output characters the failure hints scan so hints key on error headers
const hintScanChars = 4000

// One output pattern mapped to a recovery hint
type outputHint struct {
	re      *regexp.Regexp
	message func(groups []string) string
}

// Hint for a bare interpreter name missing from a python3 only system
var missingCommandHints = map[string]string{
	"python": "This system has no bare `python` — use `python3`, or the project venv's interpreter (e.g. .venv/bin/python).",
	"pip":    "This system has no bare `pip` — use `pip3`, `python3 -m pip`, or the project venv's pip (e.g. .venv/bin/pip).",
}

// Hint for a command the shell could not find
func missingCommandHint(missing string) string {
	if hint, ok := missingCommandHints[missing]; ok {
		return hint
	}
	return "`" + missing + "` is not installed or not on PATH. Verify with `which " + missing + "`; install it or use an absolute path instead of retrying the same command."
}

// Hint whose message interpolates the first capture group
func groupHint(pattern string, message string) outputHint {
	return outputHint{re: regexp.MustCompile(pattern), message: func(groups []string) string {
		if len(groups) > 1 {
			return strings.ReplaceAll(message, "{0}", groups[1])
		}
		return message
	}}
}

// Output hints ordered by production frequency, first match wins
var outputHints = []outputHint{
	groupHint(`Unknown JSON field: "?(\w+)`, "The installed gh does not support the JSON field '{0}'. The valid field list is printed in the output above — retry using only fields from that list."),
	groupHint(`(?m)^CONFLICT |Automatic merge failed|needs merge`, "Git merge conflict. Do not retry this command. Resolve the conflicted files listed above (edit, then `git add`), then continue (`git rebase --continue` / commit the merge) — or abort with `--abort`."),
	{re: regexp.MustCompile(`(?:bash: line \d+: |bash: |sh: \d*:? ?)?([\w.+-]+): command not found`), message: func(groups []string) string {
		return missingCommandHint(groups[1])
	}},
	groupHint(`(?:ModuleNotFoundError|ImportError): No module named '?([\w.]+)`, "Python cannot import '{0}'. Most often the wrong interpreter is running: activate the project venv (e.g. `source .venv/bin/activate`) or invoke its python directly. Only pip install if the package is genuinely absent from that venv."),
	groupHint(`(?:fatal|error):.*?'([^']+)' already exists`, "'{0}' already exists — retrying unchanged will keep failing. Reuse it, choose another name, or delete it first if it is genuinely stale."),
	groupHint(`API rate limit|was submitted too quickly`, "GitHub API rate limit hit — immediate retries will keep failing. Continue with other work and retry this operation later."),
	groupHint(`Permission denied|EACCES`, "Permission denied. Check ownership/mode of the target path (`ls -la`); prefer a user-writable location. Only escalate to sudo if the task genuinely requires it."),
}

// Exit code only hints for codes the exit semantics table does not cover
var exitCodeHints = map[int]string{
	126: "Exit 126: the file was found but is not executable — `chmod +x` it or invoke it via its interpreter (e.g. `bash script.sh`).",
	137: "Exit 137: the process was SIGKILLed — usually out-of-memory or an external kill. Reduce memory use or check `dmesg | tail` before retrying.",
	124: "Exit 124: the command hit its timeout. Raise timeout= (foreground max 600s) or run it with background=true and notify_on_complete=true.",
}

// Consumers whose exit status says nothing about the upstream command
const passthroughConsumers = `(?:tail|head|cat|tee|less|more|wc|sort|uniq)`

// Notes for command shapes that swallow an upstream status
var maskingNotes = [2]string{
	"exit_code 0 here is the status of the last pipeline command (tail/head/cat/...), NOT of the command before the pipe — and the output contains failure indicators. Treat this run as FAILED until proven otherwise: re-run the command WITHOUT the pipe (output is auto-truncated and the full text is saved to a file, so piping through tail/head is never needed) to get the real exit code.",
	"exit_code 0 here is the status of the `||` fallback (echo/true), NOT of the command before it — and the output contains failure indicators. Treat this run as FAILED until proven otherwise: re-run the command bare to get its real exit code.",
}

// Fallback operator followed by a status swallowing command
var orFallbackRe = regexp.MustCompile(`\|\|\s*(?:echo\b|printf\b|true\b|:\s|:$)`)

// Top level pipe into a passthrough consumer as the last pipeline segment
var pipeConsumerRe = regexp.MustCompile(`\|\s*` + passthroughConsumers + `\b[^|]*$`)

// Command heads whose output legitimately carries error text
var readonlyHeads = map[string]bool{
	"grep": true, "rg": true, "ag": true, "find": true, "ls": true, "cat": true, "head": true, "tail": true,
	"jq": true, "awk": true, "sed": true, "strings": true, "zcat": true, "journalctl": true, "dmesg": true,
	"echo": true, "printf": true,
}

// Strong tool specific failure shapes
var failureShapes = regexp.MustCompile(`(?:error\[E\d+\]|error: could not compile|error: aborting due to|Traceback \(most recent call last\)|(?m:^(?:=+ )?\d+ failed)|(?m:^FAILED (?:\S+::|\S+\.py))|compilation terminated\.|npm ERR!|BUILD FAILED|Build FAILED|FAILED: |(?m:^make(?:\[\d+\])?: \*\*\*))`)

// Basename of the command head after leading assignments
func firstCommandToken(command string) string {
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

// Pipe to passthrough consumer check that excludes the or operator before the pipe
func pipeToPassthrough(command string) bool {
	loc := pipeConsumerRe.FindStringIndex(command)
	if loc == nil {
		return false
	}
	if loc[0] > 0 && command[loc[0]-1] == '|' {
		return false
	}
	if loc[0]+1 < len(command) && command[loc[0]+1] == '|' {
		return false
	}
	return true
}

// AnnotateMaskedSuccess warns when an exit zero result likely hides an upstream failure
func AnnotateMaskedSuccess(command, output string) string {
	window := output
	if len(window) > hintScanChars {
		window = window[:hintScanChars]
	}
	if command == "" || window == "" || readonlyHeads[firstCommandToken(command)] || !failureShapes.MatchString(window) {
		return ""
	}
	if pipeToPassthrough(command) {
		return maskingNotes[0]
	}
	if orFallbackRe.MatchString(command) {
		return maskingNotes[1]
	}
	return ""
}

// AnnotateFailure returns one recovery hint for a failed command or an empty string
func AnnotateFailure(command string, exitCode int, output string) string {
	if exitCode == 0 {
		return ""
	}
	window := output
	if len(window) > hintScanChars {
		window = window[:hintScanChars]
	}
	if window != "" {
		for _, hint := range outputHints {
			if groups := hint.re.FindStringSubmatch(window); groups != nil {
				if text := hint.message(groups); text != "" {
					return text
				}
			}
		}
	}
	return exitCodeHints[exitCode]
}
