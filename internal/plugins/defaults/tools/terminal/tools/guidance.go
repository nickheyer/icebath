package tools

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/approvals"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
)

// Characters besides letters and digits a workdir may hold
const workdirChars = "/\\:_-.~ +@=,"

// Why a workdir is refused, empty when it is a plain path
func validateWorkdir(workdir string) string {
	for _, ch := range workdir {
		if ch < 32 || ch == 127 || !unicode.IsLetter(ch) && !unicode.IsDigit(ch) && !strings.ContainsRune(workdirChars, ch) {
			return fmt.Sprintf("Blocked: workdir contains disallowed character %s. Use a simple filesystem path without shell metacharacters.", strconv.Quote(string(ch)))
		}
	}
	return ""
}

var (
	shellBackground  = regexp.MustCompile(`(?im)(?:^|[;&|]\s*|&&\s*|\|\|\s*|\$\(\s*)(?:nohup|disown|setsid)\b`)
	inlineAmp        = regexp.MustCompile(`\s&\s`)
	trailingAmpNoted = regexp.MustCompile(`\s&\s*(?:#.*)?$`)
	trailingAmpEnd   = regexp.MustCompile(`\s&\s*$`)
	singleQuoted     = regexp.MustCompile(`'[^']*'`)
	doubleQuoted     = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	backticked       = regexp.MustCompile("`[^`]*`")
	otherAmps        = regexp.MustCompile(`&&|\d*[<>]&(?:\d+|-)?|&>>?|\|&`)
	substitutions    = []string{"$(", "`", "<(", ">("}
	longLived        = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:npm|pnpm|yarn|bun)\s+(?:run\s+)?(?:dev|start|serve|watch)\b`),
		regexp.MustCompile(`(?i)\bdocker\s+compose\s+up\b`),
		regexp.MustCompile(`(?i)\bnext\s+dev\b`),
		regexp.MustCompile(`(?i)\bvite(?:\s|$)`),
		regexp.MustCompile(`(?i)\bnodemon\b`),
		regexp.MustCompile(`(?i)\buvicorn\b`),
		regexp.MustCompile(`(?i)\bgunicorn\b`),
		regexp.MustCompile(`(?i)\bpython(?:3)?\s+-m\s+http\.server\b`),
	}
)

// Command with quoted, backticked and inert heredoc text blanked, so keyword checks never match inside strings
func unquoted(command string) string {
	command = terminal.StripInertHeredocs(command)
	command = singleQuoted.ReplaceAllString(command, "''")
	command = doubleQuoted.ReplaceAllString(command, `""`)
	return backticked.ReplaceAllString(command, "``")
}

// Guidance for a foreground command that backgrounds itself or never ends, the first that fits
var guidance = []struct {
	hit     func(string) bool
	message string
}{
	{
		shellBackground.MatchString,
		"Foreground command uses shell-level background wrappers (nohup/disown/setsid). Re-send WITHOUT the wrapper as " +
			"terminal(command=\"<cmd>\", background=true, notify=true) so Icebath tracks the process, then run readiness checks and tests in " +
			"separate commands.",
	},
	{
		func(s string) bool { return inlineAmp.MatchString(s) || trailingAmpNoted.MatchString(s) },
		"Foreground command uses '&' backgrounding. Re-send WITHOUT the '&' as terminal(command=\"<cmd>\", background=true) — add notify=true " +
			"for bounded jobs — then run health checks and tests in follow-up terminal calls.",
	},
	{
		func(s string) bool {
			for _, pattern := range longLived {
				if pattern.MatchString(s) {
					return true
				}
			}
			return false
		},
		"This foreground command appears to start a long-lived server/watch process. Run it with background=true, verify readiness (health " +
			"endpoint/log signal), then execute tests in a separate command.",
	},
}

// Whether the command only asks for help or a version
func helpOrVersion(command string) bool {
	normalized := strings.Join(strings.Fields(strings.ToLower(command)), " ")
	return strings.Contains(normalized, " --help") || strings.HasSuffix(normalized, " -h") || strings.Contains(normalized, " --version") || strings.HasSuffix(normalized, " -v")
}

// Guidance when a foreground command backgrounds itself or looks long lived, empty otherwise
func foregroundGuidance(command string) string {
	if helpOrVersion(command) {
		return ""
	}
	plain := unquoted(command)
	for _, g := range guidance {
		if g.hit(plain) {
			return g.message
		}
	}
	return ""
}

// The command without its trailing & when that & is all that backgrounds it, which then runs as a tracked background process; empty
// otherwise, and when dropping it would change what the approval rules make of the command
func trailingAmp(cfg *config.Config, command string) string {
	if helpOrVersion(command) {
		return ""
	}
	for _, marker := range substitutions {
		if strings.Contains(command, marker) {
			return ""
		}
	}
	plain := unquoted(command)
	if !trailingAmpEnd.MatchString(plain) || !strings.HasSuffix(strings.TrimRightFunc(command, unicode.IsSpace), "&") {
		return ""
	}
	body := strings.TrimRightFunc(plain, unicode.IsSpace)
	body = body[:len(body)-1]
	if strings.ContainsAny(body, "\n\r;#") || shellBackground.MatchString(body) || strings.Contains(otherAmps.ReplaceAllString(body, ""), "&") {
		return ""
	}
	trimmed := strings.TrimRightFunc(command, unicode.IsSpace)
	stripped := trimmed[:len(trimmed)-1]
	end := len(stripped)
	for end > 0 && strings.ContainsRune(" \t\n\r\v\f", rune(stripped[end-1])) {
		if escapes := len(stripped[:end-1]) - len(strings.TrimRight(stripped[:end-1], "\\")); escapes%2 == 1 {
			break
		}
		end--
	}
	stripped = stripped[:end]
	if strings.TrimSpace(stripped) == "" || approvals.Identity(cfg, command) != approvals.Identity(cfg, stripped) {
		return ""
	}
	return stripped
}

// Whether the command wants a plain stdin pipe and its end of file, which a pseudo terminal would break
func pipeStdin(command string) bool {
	normalized := strings.Join(strings.Fields(strings.ToLower(command)), " ")
	return strings.HasPrefix(normalized, "gh auth login") && strings.Contains(normalized, "--with-token")
}

// Whether the command is a homebrewed CI poller shape known to fail silently
func ciPoller(command string) bool {
	gh := strings.Contains(command, "gh pr view") || strings.Contains(command, "gh pr checks")
	jq := strings.Contains(command, " jq ") || strings.Contains(command, "| jq") || strings.Contains(command, "$(jq")
	return strings.Contains(command, "statusCheckRollup") || gh && jq
}

// Open flags that don't open a file in a new front window
var openFlags = map[string]bool{"-R": true, "-e": true, "-t": true, "-f": true, "-W": true, "-n": true, "-g": true, "-h": true, "-b": true, "-D": true}

// A bare `open FILE` or `open -a APP FILE` on macOS followed by a ladder of ways to bring the opened document to the front, saying
// whether it got there; any other command unchanged
func macosOpen(command string) string {
	words, err := terminal.Split(command)
	if err != nil || len(words) < 2 || words[0] != "open" {
		return command
	}
	app, file, named := "", words[1], false
	if strings.HasPrefix(file, "-") {
		if openFlags[file] || file != "-a" || len(words) != 4 || strings.HasPrefix(words[3], "-") {
			return command
		}
		app, file, named = words[2], words[3], true
	} else if len(words) != 2 {
		return command
	}
	return command + "; " + raiseLadder(app, named, file)
}

// Shell steps that put the app holding the file in front: activate, then setfront, then reopen and unminimise, reporting the result
func raiseLadder(app string, named bool, file string) string {
	const splice = `"'"$_H_APP"'"`
	osascript := func(script string) string { return "osascript -e '" + script + "' 2>/dev/null" }
	front := `osascript -e 'tell application "System Events" to get name of first application process whose frontmost is true' 2>/dev/null`
	assign := `_H_APP="$(` + front + `)"`
	if named {
		assign = "_H_APP=" + terminal.Quote(app)
	}
	quoted := terminal.Quote(file)
	activate := osascript("tell application " + splice + " to activate")
	frontmost := osascript(`tell application "System Events" to get frontmost of application process ` + splice)
	unminimise := osascript(`tell application "System Events" to set visible of every process to true`)
	setfront := `[ -n "$_H_ASN" ] && lsappinfo setfront "$_H_ASN" 2>/dev/null`
	report := "if " + frontmost + "; then echo open: " + quoted + " is now frontmost; else _H_FOCUS=$(" + front + "); echo open: " + quoted +
		` opened but another application holds focus; echo "frontmost: $_H_FOCUS"; fi`
	return assign + "; if ! " + frontmost + "; then " + activate + "; sleep 0.2; if ! " + frontmost + "; then " +
		`_H_ASN=$(lsappinfo find "$_H_APP" 2>/dev/null | awk '{print $1}'); ` + setfront + "; sleep 0.2; if ! " + frontmost + "; then " +
		`open -a "$_H_APP" ` + quoted + " 2>/dev/null; sleep 0.2; " + unminimise + "; " + activate + "; fi; fi; fi; sleep 0.8; if ! " + frontmost +
		"; then " + activate + "; sleep 0.4; fi; " + report
}
