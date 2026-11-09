package tools

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Sudo passwords entered interactively, keyed by session scope
var sudoPasswordCache = struct {
	sync.Mutex
	byScope map[string]string
}{byScope: map[string]string{}}

// Cache scope of an interactive sudo password, the session key else the task else the process
func sudoCacheScope(env *Env, sessionKey string) string {
	if sessionKey != "" {
		return "session:" + sessionKey
	}
	if env != nil && env.Ask != nil {
		return "asker:" + envIdentity(env)
	}
	return "process"
}

// Cached sudo password for the scope
func cachedSudoPassword(scope string) string {
	sudoPasswordCache.Lock()
	defer sudoPasswordCache.Unlock()
	return sudoPasswordCache.byScope[scope]
}

// Stores a sudo password for the scope, an empty value drops the entry
func setCachedSudoPassword(scope, password string) {
	sudoPasswordCache.Lock()
	defer sudoPasswordCache.Unlock()
	if password == "" {
		delete(sudoPasswordCache.byScope, scope)
		return
	}
	sudoPasswordCache.byScope[scope] = password
}

// ResetCachedSudoPasswords clears every cached sudo password
func ResetCachedSudoPasswords() {
	sudoPasswordCache.Lock()
	defer sudoPasswordCache.Unlock()
	sudoPasswordCache.byScope = map[string]string{}
}

// Messages sudo prints when it needs a password it cannot ask for
var sudoHeadlessFailures = []string{"sudo: a password is required", "sudo: no tty present", "sudo: a terminal is required"}

// Appends the SUDO_PASSWORD tip when sudo failed in a headless context
func handleSudoFailure(env *Env, output string, envType string) string {
	isGateway := sessionFlag(env, "gateway_session", "ICEBATH_GATEWAY_SESSION")
	noUser := noSudoUser(env)
	if !(isGateway || noUser) {
		return output
	}
	failed := false
	for _, f := range sudoHeadlessFailures {
		if strings.Contains(output, f) {
			failed = true
			break
		}
	}
	if !failed {
		return output
	}
	display := displayHome(env)
	if noUser {
		return output + "\n\n💡 Tip: This session cannot prompt for a sudo password. Add SUDO_PASSWORD to " + display + "/.env on the agent machine, or run the command without sudo."
	}
	return output + "\n\n💡 Tip: To enable sudo over messaging, add SUDO_PASSWORD to " + display + "/.env on the agent machine."
}

// Messages sudo prints when a piped password is wrong
var sudoWrongPasswordMarkers = []string{
	"sudo: authentication failed",
	"sudo: incorrect password attempt",
	"sudo: maximum 3 incorrect authentication attempts",
	"sudo: 3 incorrect password attempts",
}

// Reports whether sudo rejected a piped password
func sudoWrongPasswordFailure(output string) bool {
	lowered := strings.ToLower(output)
	for _, m := range sudoWrongPasswordMarkers {
		if strings.Contains(lowered, m) {
			return true
		}
	}
	return false
}

// Drops a session cached sudo password after sudo rejected it, leaving a configured one alone
func invalidateCachedSudoOnAuthFailure(env *Env, scope, command, output string) bool {
	if _, configured := env.Secrets.Get("SUDO_PASSWORD"); configured {
		return false
	}
	if !sudoWrongPasswordFailure(output) || countRealSudoInvocations(command) == 0 || cachedSudoPassword(scope) == "" {
		return false
	}
	setCachedSudoPassword(scope, "")
	return true
}

// Asks the user for a sudo password through the host form or the terminal, empty on skip, timeout or error
func promptForSudoPassword(ctx context.Context, env *Env, command string, timeout time.Duration) string {
	human := humanInputRequest(env, "sudo", command, "")
	if env == nil || env.Ask == nil {
		password, done := readHiddenPasswordFromTTY(timeout)
		switch {
		case !done:
			human.finish(env, "timeout")
		case password != "":
			human.finish(env, "provided")
		default:
			human.finish(env, "skipped")
		}
		return password
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	release := humanWaitWindow(env, "")
	result, err := env.Ask.Ask(ctx, []Question{{
		Header: "Sudo password required",
		Text:   "The command `" + command + "` needs sudo. Enter the password (hidden), or submit an empty answer to skip (the command fails gracefully).",
	}})
	release()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			human.finish(env, "timeout")
		} else {
			human.finish(env, "error")
		}
		return ""
	}
	if len(result.Answers) == 0 || result.Answers[0].Text == "" {
		human.finish(env, "skipped")
		return ""
	}
	human.finish(env, "provided")
	return result.Answers[0].Text
}

// Reports whether a shell token is a leading environment assignment
func looksLikeEnvAssignment(token string) bool {
	if !strings.Contains(token, "=") || strings.HasPrefix(token, "=") {
		return false
	}
	name := token[:strings.IndexByte(token, '=')]
	return envNameRe.MatchString(name)
}

// Shell variable name
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// One shell word with quotes and escapes preserved
var shellWordRe = regexp.MustCompile(`(?s)^(?:'[^']*'?|"(?:\\.|[^"])*"?|\\.|[^\s;|&()])*`)

// Reads one shell token starting at start, preserving quotes and escapes
func readShellToken(command string, start int) (string, int) {
	loc := shellWordRe.FindStringIndex(command[start:])
	end := start + loc[1]
	return command[start:end], end
}

// One lexical event of the shell scanner
type shellEvent struct {
	kind    string
	start   int
	end     int
	atStart bool
}

// Quote aware shell scanner shared by the sudo rewrite and the compound background rewrite
func scanShell(command string, background bool) []shellEvent {
	var events []shellEvent
	i, n := 0, len(command)
	atStart := true
	parens, braces := 0, 0
	twoCharOps := []string{"&&", "||", ";;"}
	if background {
		twoCharOps = []string{"&&", "||", "&>"}
	}
	for i < n {
		ch := command[i]
		grouped := parens > 0 || braces > 0
		wasStart := atStart
		var kind string
		var end int
		switch {
		case isSpaceByte(ch):
			kind, end = "ws", i+1
			if grouped {
				kind = "skip"
			}
			atStart = atStart || ch == '\n'
		case ch == '#':
			idx := strings.IndexByte(command[i:], '\n')
			kind = "comment"
			if idx < 0 {
				end = n
			} else {
				end = i + idx
			}
		case background && ch == '\\' && i+1 < n:
			kind, end = "escape", i+2
		case background && (ch == '\'' || ch == '"'):
			_, end = readShellToken(command, i)
			kind = "word"
		case background && (ch == '(' || ch == ')' || (ch == '{' && i+1 < n && isSpaceByte(command[i+1])) || (ch == '}' && braces > 0)):
			if ch == '(' {
				parens++
			} else if ch == ')' && parens > 0 {
				parens--
			}
			if ch == '{' {
				braces++
			} else if ch == '}' {
				braces--
			}
			kind, end = "op", i+1
		case background && grouped:
			kind, end = "skip", i+1
		case hasAnyPrefix(command[i:], twoCharOps):
			kind, end, atStart = "op", i+2, true
		case strings.IndexByte(";|&()", ch) >= 0:
			kind, end = "op", i+1
			atStart = ch != ')'
		default:
			token, tokenEnd := readShellToken(command, i)
			end = tokenEnd
			kind = "word"
			atStart = atStart && looksLikeEnvAssignment(token)
		}
		if end <= i {
			end = i + 1
		}
		events = append(events, shellEvent{kind: kind, start: i, end: end, atStart: wasStart})
		i = end
	}
	return events
}

// Reports whether s starts with any of the prefixes
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Options of env that consume an operand
var envValueOptions = map[string]bool{"-u": true, "--unset": true, "-C": true, "--chdir": true, "-a": true, "--argv0": true}

// Options of env that take no operand
var envFlagOptions = map[string]bool{"-i": true, "--ignore-environment": true, "-0": true, "--null": true, "-v": true, "--debug": true}

// Rewrites literal sudo executables to read the password from stdin, returning the rewrite and the count
func rewriteRealSudoInvocations(command string) (string, int) {
	var out []string
	sudoCount := 0
	inEnv, envOperand, envOptions := false, false, false
	for _, ev := range scanShell(command, false) {
		text := command[ev.start:ev.end]
		out = append(out, text)
		if ev.kind == "op" || (ev.kind == "ws" && text == "\n") {
			inEnv, envOperand, envOptions = false, false, false
		}
		if ev.kind != "word" || !(ev.atStart || inEnv) {
			continue
		}
		words, err := shlexSplit(text)
		if err != nil {
			inEnv = false
			continue
		}
		if len(words) != 1 || strings.ContainsAny(text, "$`*?[]~<>{}") {
			inEnv = false
			continue
		}
		word := words[0]
		if inEnv {
			if envOperand {
				envOperand = false
				continue
			}
			if envOptions && (word == "-" || word == "--") {
				envOptions = false
				continue
			}
			if envOptions && envValueOptions[word] {
				envOperand = true
				continue
			}
			if envOptions && (envFlagOptions[word] || envLongValueOption(word) || envShortAttached(word)) {
				continue
			}
			if envOptions && strings.HasPrefix(word, "-") {
				inEnv = false
				continue
			}
			if looksLikeEnvAssignment(word) {
				envOptions = false
				continue
			}
		} else if looksLikeEnvAssignment(text) {
			continue
		}
		executable := word
		if idx := strings.LastIndex(word, "/"); idx >= 0 {
			executable = word[idx+1:]
		}
		inEnv = executable == "env"
		envOptions = inEnv
		if executable == "sudo" {
			out[len(out)-1] += " -S -p ''"
			sudoCount++
		}
	}
	return strings.Join(out, ""), sudoCount
}

// Reports whether a word is a long env option with an attached value
func envLongValueOption(word string) bool {
	for opt := range envValueOptions {
		if strings.HasPrefix(opt, "--") && strings.HasPrefix(word, opt+"=") {
			return true
		}
	}
	return false
}

// Reports whether a word is a short env option with its value attached
func envShortAttached(word string) bool {
	for _, opt := range []string{"-u", "-C", "-a"} {
		if strings.HasPrefix(word, opt) && len(word) > 2 {
			return true
		}
	}
	return false
}

// Number of real sudo command words in the command
func countRealSudoInvocations(command string) int {
	_, n := rewriteRealSudoInvocations(command)
	return n
}

// RewriteCompoundBackground wraps A && B & as A && { B & } so bash never parks a subshell waiting on B
func RewriteCompoundBackground(command string) string {
	chainEnd := -1
	var rewrites [][2]int
	for _, ev := range scanShell(command, true) {
		text := command[ev.start:ev.end]
		switch {
		case ev.kind == "op" && (text == "&&" || text == "||"):
			chainEnd = ev.end
		case (ev.kind == "ws" && text == "\n") || (ev.kind == "op" && (text == ";" || text == "|" || text == "}")):
			chainEnd = -1
		case ev.kind == "op" && text == "&":
			j := ev.start - 1
			for j >= 0 && isSpaceByte(command[j]) {
				j--
			}
			if j >= 0 && (command[j] == '<' || command[j] == '>') {
				continue
			}
			if chainEnd >= 0 {
				rewrites = append(rewrites, [2]int{chainEnd, ev.start})
			}
			chainEnd = -1
		}
	}
	result := command
	for k := len(rewrites) - 1; k >= 0; k-- {
		chain, amp := rewrites[k][0], rewrites[k][1]
		insert := chain
		for insert < amp && isSpaceByte(result[insert]) {
			insert++
		}
		suffix := result[amp+1:]
		tail := strings.TrimLeft(suffix, " \t")
		needsSeparator := tail != "" && (strings.IndexByte(";\n&|)}", tail[0]) < 0 || strings.HasPrefix(tail, "&>"))
		separator := ""
		if needsSeparator {
			separator = " ;"
		}
		result = result[:insert] + "{ " + result[insert:amp] + "& }" + separator + suffix
	}
	return result
}

// Reports whether nobody can answer a sudo prompt in this session
func noSudoUser(env *Env) bool {
	return sessionFlag(env, "delegated_child", "ICEBATH_DELEGATED_CHILD_CONTEXT") || sessionFlag(env, "single_query", "ICEBATH_SINGLE_QUERY_SESSION")
}

// Rewrites command position sudo to read a piped password and returns the stdin lines to prepend
func transformSudoCommand(ctx context.Context, env *Env, sessionKey, command string, nopasswd func() bool) (string, *string) {
	transformed, count := rewriteRealSudoInvocations(command)
	if count == 0 {
		return command, nil
	}
	configured, hasConfigured := env.Secrets.Get("SUDO_PASSWORD")
	scope := sudoCacheScope(env, sessionKey)
	password := configured
	if !hasConfigured {
		password = cachedSudoPassword(scope)
	}
	shouldPrompt := (sessionFlag(env, "interactive", "ICEBATH_INTERACTIVE") || env.Ask != nil) && !noSudoUser(env)
	if !hasConfigured && password == "" && shouldPrompt {
		if nopasswd != nil && nopasswd() {
			return command, nil
		}
		password = promptForSudoPassword(ctx, env, command, 45*time.Second)
		if password != "" {
			setCachedSudoPassword(scope, password)
		}
	}
	if hasConfigured || password != "" {
		stdin := strings.Repeat(password+"\n", count)
		return transformed, &stdin
	}
	return command, nil
}
