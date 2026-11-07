package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Non alphanumeric characters a workdir may contain
const workdirSafeASCIIChars = "/\\:_-.~ +@=,"

// Reports whether a workdir character is allowed by the allowlist
func isSafeWorkdirChar(ch rune) bool {
	if ch < 32 || ch == 127 {
		return false
	}
	return unicode.IsLetter(ch) || unicode.IsDigit(ch) || strings.ContainsRune(workdirSafeASCIIChars, ch)
}

// Error message when a workdir holds a disallowed character, empty otherwise
func validateWorkdir(workdir string) string {
	for _, ch := range workdir {
		if !isSafeWorkdirChar(ch) {
			return fmt.Sprintf("Blocked: workdir contains disallowed character %s. Use a simple filesystem path without shell metacharacters.", pyRepr(string(ch)))
		}
	}
	return ""
}

// Workdir guard verdict, empty when the workdir is allowed
func ValidateWorkdir(workdir string) string { return validateWorkdir(workdir) }

// Log safe preview of a possibly invalid command value
func safeCommandPreview(command any, limit int) string {
	if command == nil {
		return "<None>"
	}
	if s, ok := command.(string); ok {
		if len(s) > limit {
			return s[:limit]
		}
		return s
	}
	r := fmt.Sprintf("%#v", command)
	if len(r) > limit {
		r = r[:limit]
	}
	return r
}

// Python repr of a short string for user facing messages
func pyRepr(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch {
		case r == '\'':
			b.WriteString(`\'`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 32 || r == 127:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// Guard result envelope with exit code 1, the error and the status
func blockedJSON(errText, status string) string {
	return JSON(map[string]any{"output": "", "exit_code": 1, "error": errText, "status": status})
}

var (
	shellLevelBackgroundRe  = regexp.MustCompile(`(?im)(?:^|[;&|]\s*|&&\s*|\|\|\s*|\$\(\s*)(?:nohup|disown|setsid)\b`)
	inlineBackgroundAmpRe   = regexp.MustCompile(`\s&\s`)
	trailingBackgroundAmpRe = regexp.MustCompile(`\s&\s*(?:#.*)?$`)
	guardSingleQuotedRe     = regexp.MustCompile(`'[^']*'`)
	guardDoubleQuotedRe     = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	guardBacktickRe         = regexp.MustCompile("`[^`]*`")
	nonBackgroundAmpRe      = regexp.MustCompile(`&&|\d*[<>]&(?:\d+|-)?|&>>?|\|&`)
	trailingAmpOnlyRe       = regexp.MustCompile(`\s&\s*$`)
	executableSubstitutions = []string{"$(", "`", "<(", ">("}
	longLivedForegroundRes  = []*regexp.Regexp{
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

// Blanks quoted, backtick and provably inert heredoc content so keyword checks cannot match inside strings
func stripQuotesForGuard(command string) string {
	result := StripInertHeredocBodies(command)
	result = guardSingleQuotedRe.ReplaceAllString(result, "''")
	result = guardDoubleQuotedRe.ReplaceAllString(result, `""`)
	return guardBacktickRe.ReplaceAllString(result, "``")
}

// Ordered foreground guidance, first predicate hit wins
var foregroundGuidance = []struct {
	hit     func(string) bool
	message string
}{
	{
		func(s string) bool { return shellLevelBackgroundRe.MatchString(s) },
		"Foreground command uses shell-level background wrappers (nohup/disown/setsid). " +
			"Re-send WITHOUT the wrapper as terminal(command=\"<cmd>\", background=true, " +
			"notify_on_complete=true) so Icebath tracks the process, then run readiness " +
			"checks and tests in separate commands.",
	},
	{
		func(s string) bool {
			return inlineBackgroundAmpRe.MatchString(s) || trailingBackgroundAmpRe.MatchString(s)
		},
		"Foreground command uses '&' backgrounding. Re-send WITHOUT the '&' as " +
			"terminal(command=\"<cmd>\", background=true) \u2014 add notify_on_complete=true " +
			"for bounded jobs \u2014 then run health checks and tests in follow-up terminal calls.",
	},
	{
		func(s string) bool {
			for _, p := range longLivedForegroundRes {
				if p.MatchString(s) {
					return true
				}
			}
			return false
		},
		"This foreground command appears to start a long-lived server/watch process. " +
			"Run it with background=true, verify readiness (health endpoint/log signal), " +
			"then execute tests in a separate command.",
	},
}

// Reports whether a command is an informational help or version invocation
func looksLikeHelpOrVersionCommand(command string) bool {
	normalized := strings.Join(strings.Fields(strings.ToLower(command)), " ")
	return strings.Contains(normalized, " --help") || strings.HasSuffix(normalized, " -h") ||
		strings.Contains(normalized, " --version") || strings.HasSuffix(normalized, " -v")
}

// Guidance when a foreground command looks long lived or uses shell backgrounding, empty otherwise
func foregroundBackgroundGuidance(command string) string {
	if looksLikeHelpOrVersionCommand(command) {
		return ""
	}
	unquoted := stripQuotesForGuard(command)
	for _, g := range foregroundGuidance {
		if g.hit(unquoted) {
			return g.message
		}
	}
	return ""
}

// Command minus its final ampersand and the unescaped whitespace right before it
func stripTrailingAmp(command string) string {
	body := strings.TrimRightFunc(command, unicode.IsSpace)
	body = body[:len(body)-1]
	end := len(body)
	for end > 0 && isSpaceByte(body[end-1]) {
		before := body[:end-1]
		backslashes := len(before) - len(strings.TrimRight(before, "\\"))
		if backslashes%2 == 1 {
			break
		}
		end--
	}
	return body[:end]
}

// Everything the approval path keys a decision on for the command
func approvalIdentity(env *Env, command string) string {
	return CommandApprovalIdentity(env, &Call{Args: map[string]any{"command": command}})
}

// Command without its trailing ampersand when that is its only backgrounding, empty otherwise
func trailingAmpBackgroundCommandFor(env *Env, command string) string {
	if looksLikeHelpOrVersionCommand(command) {
		return ""
	}
	for _, marker := range executableSubstitutions {
		if strings.Contains(command, marker) {
			return ""
		}
	}
	unquoted := stripQuotesForGuard(command)
	if !trailingAmpOnlyRe.MatchString(unquoted) || !strings.HasSuffix(strings.TrimRightFunc(command, unicode.IsSpace), "&") {
		return ""
	}
	body := strings.TrimRightFunc(unquoted, unicode.IsSpace)
	body = body[:len(body)-1]
	if strings.ContainsAny(body, "\n\r;#") || shellLevelBackgroundRe.MatchString(body) {
		return ""
	}
	if strings.Contains(nonBackgroundAmpRe.ReplaceAllString(body, ""), "&") {
		return ""
	}
	stripped := stripTrailingAmp(command)
	if strings.TrimSpace(stripped) == "" {
		return ""
	}
	if approvalIdentity(env, command) != approvalIdentity(env, stripped) {
		return ""
	}
	return stripped
}

// Trailing ampersand conversion keyed on the process wide approval policy
func trailingAmpBackgroundCommand(command string) string {
	return trailingAmpBackgroundCommandFor(nil, command)
}

// Bounded script read, host filesystem first then head -c through the environment, empty when unreadable
func readScriptForGuard(ctx context.Context, e Environment, guardCwd, scriptPath string, maxBytes int) (string, bool) {
	if e == nil {
		return "", false
	}
	localPath := expandUser(scriptPath)
	if !filepath.IsAbs(localPath) {
		localPath = filepath.Join(guardCwd, localPath)
	}
	if st, err := os.Stat(localPath); err == nil && st.Mode().IsRegular() && st.Size() <= int64(maxBytes) {
		if data, err := os.ReadFile(localPath); err == nil && len(data) <= maxBytes {
			if bytes.IndexByte(data, 0) >= 0 {
				return "", false
			}
			return termDecodeUTF8(data), true
		}
	}
	res, err := e.Execute(ctx, fmt.Sprintf("head -c %d < %s", maxBytes+1, shQuote(scriptPath)), ExecOptions{})
	if err != nil || res == nil || res.ReturnCode != 0 {
		return "", false
	}
	if res.Output != "" && strings.Contains(res.Output, "\x00") {
		return "", false
	}
	return res.Output, true
}

// Refuses gateway lifecycle commands issued from inside the supervised gateway, JSON error when blocked
func gatewayLifecycleBlock(ctx context.Context, env *Env, command string, e Environment, envType, cwd, workdir, sessionKey string) string {
	if !isSupervisedGatewayProcess() {
		return ""
	}
	if LifecycleScanRootWithinBudget(command) && ContainsLaunchctlSubmitCommand(command) {
		return blockedJSON(
			"Blocked: launchctl submit/bootstrap is restricted inside a supervised "+
				"gateway regardless of the job label, to prevent indirect gateway "+
				"restart loops. This guard does not inspect the job's KeepAlive settings "+
				"or determine whether it is independent of Icebath. Perform authorized "+
				"LaunchAgent maintenance from a separate shell outside the gateway, "+
				"not by switching launchctl verbs to bypass this rejection.",
			"error")
	}
	guardCwdBase, ok := getSessionCwd(sessionKey)
	if !ok {
		guardCwdBase = cwd
		if e != nil && e.Cwd() != "" {
			guardCwdBase = e.Cwd()
		}
	}
	mountedHost := ""
	if e != nil {
		mountedHost = e.HostCwd()
	}
	guardCwd := resolveCommandCwd(env, workdir, guardCwdBase, sessionKey, envType, mountedHost, e)
	unsafe, refusal := ScanGatewayLifecycle(command, guardCwd, func(p string) (string, bool) {
		return readScriptForGuard(ctx, e, guardCwd, p, MaxReferencedScriptBytes)
	})
	if unsafe && refusal != "" {
		return blockedJSON(
			fmt.Sprintf("Blocked: the lifecycle guard could not scan this command or referenced script: %s. ", refusal)+
				"Nothing in the command is known to contain a gateway lifecycle command, but a "+
				"script the command executes must be scannable (a regular text file under 1 MiB) "+
				"before it can run inside the gateway process.",
			"error")
	}
	if unsafe {
		if LifecycleScanRootWithinBudget(command) && ContainsHostInterpreterKill(command) {
			return blockedJSON(HostInterpreterKillRejection, "error")
		}
		return blockedJSON(
			"Blocked: command or referenced script cannot restart, stop, or "+
				"uninstall the gateway from inside the gateway process. The gateway would "+
				"kill this command before it could complete (SIGTERM propagates "+
				"to child processes). Run `icebath gateway restart` from a "+
				"separate shell outside the running gateway.",
			"error")
	}
	return ""
}

// Windows only guard against git mutating the checkout backing this process, JSON error when blocked
func selfRepoBlock(env *Env, command, cwd, workdir, sessionKey string) string {
	if !SelfRepoGuardActive() {
		return ""
	}
	guardCwd := resolveCommandCwd(env, workdir, cwd, sessionKey, "", "", nil)
	hit, msg := DetectSelfRepoGitMutation(env, command, guardCwd, RunningSourceRoot())
	if !hit {
		return ""
	}
	envLog(env).Warn("Blocked self-repo git mutation", "command", safeCommandPreview(command, 200))
	return blockedJSON(msg, "blocked")
}

// Refuses commands that write into a locked skill, change the lock record, or unlock a skill
func lockedSkillBlock(env *Env, command, cwd, workdir, sessionKey string) string {
	guardCwd := resolveCommandCwd(env, workdir, cwd, sessionKey, "", "", nil)
	reason := SkillLockCommandRefusal(env, command, guardCwd)
	if reason == "" {
		return ""
	}
	envLog(env).Warn("Blocked write to a locked skill", "command", safeCommandPreview(command, 200))
	return blockedJSON("Blocked: "+reason, "blocked")
}

// Decodes bytes as UTF-8 with one replacement character per invalid byte
func termDecodeUTF8(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	var b strings.Builder
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 {
			b.WriteRune(utf8.RuneError)
		} else {
			b.WriteRune(r)
		}
		data = data[size:]
	}
	return b.String()
}
