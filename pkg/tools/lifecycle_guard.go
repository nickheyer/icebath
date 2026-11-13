package tools

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/home"
)

// Shell level command shapes that target the gateway lifecycle, each anchored on a concrete identifier
var gatewayLifecyclePattern = regexp2.MustCompile(
	`(?i)(?:(?<![/\w.\-])icebath(?:\.(?:exe|cmd|bat|com|ps1))?\s+gateway\s+(?:restart|stop|uninstall)\b)` +
		`|(?:launchctl\s+(?:kickstart|unload|load|stop|restart|submit|bootstrap|bootout|remove|disable)\b[^\n]*\bicebath[.\-]?gateway)` +
		`|(?:systemctl\s+(?:-\S+\s+)*(?:restart|stop|start)\b[^\n]*\bicebath[.\-]?gateway)` +
		`|(?:\b(?:p?kill|taskkill|stop-process)\b[^\n]*\bicebath\b[^\n]*\bgateway)` +
		`|(?:\b(?:p?kill|taskkill|stop-process)\b[^\n]*\bgateway\b[^\n]*\bicebath)`)

// Lower cased process names the gateway answers to, without any exe suffix
var gatewayImageNames = func() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		name := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.TrimSpace(p))), ".exe")
		if name != "" && name != "." && name != string(filepath.Separator) && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	add("icebath")
	if len(os.Args) > 0 {
		add(os.Args[0])
	}
	if exe, err := os.Executable(); err == nil {
		add(exe)
	}
	if comm, err := os.ReadFile("/proc/self/comm"); err == nil {
		add(string(comm))
	}
	return out
}()

// Command lines a full command line match sees for the gateway, this process and the service form
var gatewayCmdlines = func() []string {
	out := []string{strings.Join(os.Args, " "), "icebath gateway run"}
	if exe, err := os.Executable(); err == nil {
		out = append(out, exe+" gateway run")
	}
	return out
}()

var pkillValueOptions = setOf(
	"-g", "--pgroup", "-G", "--group", "-P", "--parent", "-s", "--session", "-t", "--terminal",
	"-u", "--euid", "-U", "--uid", "-F", "--pidfile", "--ns", "--nslist", "-d", "--delimiter",
	"--signal", "-O", "--older", "-r", "--runstates", "--cgroup", "-A", "--ignore-ancestors",
)

var killerValueOptions = map[string]map[string]bool{
	"pkill": pkillValueOptions,
	"pgrep": pkillValueOptions,
	"killall": setOf("-s", "--signal", "-u", "--user", "-o", "--older-than", "-y", "--younger-than",
		"-n", "--ns", "-Z", "--context"),
	"pidof": setOf("-o", "--omit-pid", "-S", "--separator"),
}

var (
	nameKillers            = setOf("pkill", "killall", "taskkill", "stop-process")
	nameEnumerators        = setOf("pgrep", "pidof", "get-process")
	killVerbRe             = regexp.MustCompile(`(?i)\b(?:kill|taskkill|stop-process)\b`)
	signalTokenRe          = regexp.MustCompile(`^[0-9A-Z+]+$`)
	taskkillFilterRe       = regexp.MustCompile(`(?i)^\s*['"]?imagename\s+eq\s+(\S+)`)
	shellLineContinuation  = regexp.MustCompile(`\\\r?\n[ \t]*`)
	argvListPunctuationRe  = regexp.MustCompile(`[\[\],]+`)
	launchctlLifecycleRe   = regexp.MustCompile(`(?i)\blaunchctl\s+(?:kickstart|unload|load|stop|restart|bootout|kill|disable|remove)\b`)
	gatewayLabelRe         = regexp.MustCompile(`(?i)\bicebath[.\-]?gateway\b`)
	lifecycleEnvAssignRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	dotCommandArgumentRe   = regexp.MustCompile(`^\.[A-Za-z]`)
	pipeToInterpreterRe    = regexp.MustCompile(`\|\s*&?\s*(?:sudo\s+)?(?:sh|bash|dash|ksh|zsh|xargs|eval|source)\b`)
	unsafeDataArgMarkers   = []string{"`", "$(", "<(", ">(", "\\!"}
	dataSinkExecutables    = setOf("grep", "egrep", "fgrep", "rg", "ag", "ack", "journalctl", "sqlite3", "psql")
	lifecycleControlChars  = ";&|()"
	transparentPrefixWords = setOf(
		"sudo", "doas", "env", "nohup", "setsid", "nice", "ionice", "stdbuf",
		"timeout", "exec", "command", "builtin", "eatmydata",
		"pkexec", "su", "runuser", "setpriv", "systemd-run", "nsenter", "unshare",
	)
	transparentPrefixValueOptions = map[string]map[string]bool{
		"sudo":        setOf("-u", "-g", "-U", "-C", "-p", "-r", "-t", "-T", "--user", "--group", "--prompt"),
		"doas":        setOf("-u", "-C"),
		"env":         setOf("-u", "--unset", "-S", "--split-string", "-C", "--chdir"),
		"nice":        setOf("-n", "--adjustment"),
		"ionice":      setOf("-c", "-n", "-p", "--class", "--classdata"),
		"stdbuf":      setOf("-i", "-o", "-e", "--input", "--output", "--error"),
		"timeout":     setOf("-s", "-k", "--signal", "--kill-after"),
		"pkexec":      setOf("--user"),
		"su":          setOf("-s", "--shell", "-g", "--group", "-G", "--supp-group"),
		"runuser":     setOf("-u", "--user", "-s", "--shell", "-g", "--group", "-G", "--supp-group"),
		"setpriv":     setOf("--reuid", "--regid", "--groups", "--inh-caps", "--ambient-caps", "--bounding-set", "--selinux-label", "--apparmor-profile"),
		"systemd-run": setOf("-u", "--unit", "-p", "--property", "-E", "--setenv", "--slice", "--description", "--uid", "--gid", "--on-calendar", "--service-type"),
		"nsenter":     setOf("-t", "--target", "-S", "--setuid", "-G", "--setgid", "-r", "--root", "-w", "--wd"),
		"unshare":     setOf("--map-user", "--map-group", "--setgroups", "-R", "--root", "-w", "--wd"),
	}
	transparentPrefixOperands = map[string]int{"timeout": 1}
	maxPrefixPeels            = 8
)

// Profile flag form of the gateway restart, blocked only when the named profile runs the guard
var profileFlagLifecyclePattern = regexp2.MustCompile(pyPatternToRegexp2(
	`(?i)icebath\s+(?:-\S++(?:\s++(?!-\S)\S++)?\s++)*(?:--profile=([^\s]+)|(?:-p|--profile)\s+([^\s]+))(?:\s++-\S++(?:\s++(?!-\S)\S++)?)*\s+gateway\s+(?:restart|stop)`))

// GatewayImageKillRejection is the text every surface shows for a kill aimed at the gateway image
const GatewayImageKillRejection = "Blocked: this command kills every process whose image, name or command line " +
	"matches the icebath executable running this gateway. " +
	"Stop only the process you own instead: process(action=\"kill\", session_id=\"proc_…\") " +
	"for a background job Icebath started, or kill/taskkill by its explicit PID."

// Whether a pkill or killall regular expression selects the gateway by name or by full command line
func regexSelectsGateway(pattern string, fullCmdline, anchored bool) bool {
	expr := strings.NewReplacer(`\<`, `\b`, `\>`, `\b`).Replace(pattern)
	if anchored {
		expr = "^(?:" + expr + ")$"
	}
	re, err := regexp.Compile("(?i)" + expr)
	if err != nil {
		return true
	}
	subjects := gatewayImageNames
	if fullCmdline {
		subjects = gatewayCmdlines
	}
	for _, subject := range subjects {
		if re.MatchString(subject) {
			return true
		}
	}
	return false
}

// Whether a literal process name is one the gateway answers to
func nameSelectsGateway(value string) bool {
	name := strings.TrimSuffix(strings.ToLower(strings.Trim(strings.TrimSpace(value), "\"'")), ".exe")
	for _, image := range gatewayImageNames {
		if name == image {
			return true
		}
	}
	return false
}

// Whether a taskkill or PowerShell wildcard image name selects the gateway, a malformed pattern counting as a match
func globSelectsGateway(value string) bool {
	pattern := strings.ToLower(strings.Trim(strings.TrimSpace(value), "\"'"))
	if pattern == "" {
		return false
	}
	for _, image := range gatewayImageNames {
		for _, subject := range []string{image, image + ".exe"} {
			if ok, err := path.Match(pattern, subject); err != nil || ok {
				return true
			}
		}
	}
	return false
}

// Full command line and exact match switches of a pkill or pgrep argv, short option clusters included
func pkillMatchFlags(args []string) (fullCmdline, exact bool) {
	for i := 0; i < len(args); i++ {
		token := args[i]
		switch {
		case token == "--":
			return fullCmdline, exact
		case token == "--full":
			fullCmdline = true
		case token == "--exact":
			exact = true
		case pkillValueOptions[token]:
			i++
		case strings.HasPrefix(token, "--") || !strings.HasPrefix(token, "-") || signalTokenRe.MatchString(token[1:]):
		default:
			fullCmdline = fullCmdline || strings.ContainsRune(token[1:], 'f')
			exact = exact || strings.ContainsRune(token[1:], 'x')
		}
	}
	return fullCmdline, exact
}

// Whether a killer or enumerator with the argv would select the gateway process
func killerTargetsGateway(name string, args []string) bool {
	switch name {
	case "pkill", "pgrep", "killall", "pidof":
		hasFlag := func(opts ...string) bool {
			for _, t := range args {
				for _, o := range opts {
					if t == o {
						return true
					}
				}
			}
			return false
		}
		var operandSelects func(string) bool
		switch name {
		case "pkill", "pgrep":
			fullCmdline, exact := pkillMatchFlags(args)
			operandSelects = func(op string) bool { return regexSelectsGateway(op, fullCmdline, exact) }
		case "killall":
			if hasFlag("-r", "--regexp") {
				operandSelects = func(op string) bool { return regexSelectsGateway(op, false, false) }
			} else {
				operandSelects = nameSelectsGateway
			}
		default:
			operandSelects = nameSelectsGateway
		}
		var operands []string
		position := 0
		for position < len(args) {
			token := args[position]
			if token == "--" {
				operands = append(operands, args[position+1:]...)
				break
			}
			if killerValueOptions[name][token] {
				position += 2
				continue
			}
			if !strings.HasPrefix(token, "-") {
				operands = append(operands, token)
			}
			position++
		}
		for _, op := range operands {
			if operandSelects(op) {
				return true
			}
		}
		return false
	case "taskkill":
		for position := 0; position+1 < len(args); position++ {
			option := strings.TrimLeft(strings.ToLower(args[position]), "/-")
			value := args[position+1]
			if option == "im" && globSelectsGateway(value) {
				return true
			}
			if option == "fi" {
				if m := taskkillFilterRe.FindStringSubmatch(value); m != nil && globSelectsGateway(m[1]) {
					return true
				}
			}
		}
		return false
	}
	var values []string
	for position, token := range args {
		option, _, inline := partition(token, ":")
		lower := strings.ToLower(option)
		if lower == "-name" || lower == "-processname" || lower == "-n" {
			if inline != "" {
				values = append(values, inline)
			} else if position+1 < len(args) {
				values = append(values, args[position+1])
			} else {
				values = append(values, "")
			}
		} else if name == "get-process" && !strings.HasPrefix(token, "-") && (position == 0 || !strings.HasPrefix(args[position-1], "-")) {
			values = append(values, token)
		}
	}
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if globSelectsGateway(part) {
				return true
			}
		}
	}
	return false
}

// Whether a tokenized segment runs one of the killers against the gateway process
func segmentNamesGatewayImage(tokens []string, killers map[string]bool) bool {
	var candidates []int
	if index, ok := executedCommandIndex(tokens); ok {
		candidates = append(candidates, index)
	}
	for i, token := range tokens {
		if killers[strings.TrimSuffix(strings.ToLower(lifecycleExecutableName(token)), ".exe")] {
			candidates = append(candidates, i)
		}
	}
	for _, position := range candidates {
		name := strings.TrimSuffix(strings.ToLower(lifecycleExecutableName(tokens[position])), ".exe")
		if killers[name] && killerTargetsGateway(name, tokens[position+1:]) {
			return true
		}
	}
	return false
}

// ContainsGatewayImageKill reports a process killer aimed at the icebath process running the gateway
func ContainsGatewayImageKill(text string) bool {
	normalized := shellLineContinuation.ReplaceAllString(text, " ")
	killVerbPresent := killVerbRe.MatchString(normalized)
	for _, segment := range iterLifecycleCommandSegments(normalized) {
		joined := strings.Join(segment, " ")
		stripped := argvListPunctuationRe.ReplaceAllString(joined, " ")
		candidates := [][]string{segment}
		if stripped != joined {
			candidates = append(candidates, strings.Fields(stripped))
		}
		for _, tokens := range candidates {
			if segmentNamesGatewayImage(tokens, nameKillers) {
				return true
			}
			if killVerbPresent && segmentNamesGatewayImage(tokens, nameEnumerators) {
				return true
			}
		}
	}
	return false
}

// Profile executing the guard, empty when the process home is no profile
func currentProfileName() string {
	return home.ProfileNameFor(home.Process())
}

// Reports whether the named profile is the one executing the guard
func namedProfileIsCurrent(named string) bool {
	current := currentProfileName()
	return current != "" && strings.EqualFold(strings.TrimSpace(named), strings.TrimSpace(current))
}

// Order independent launchctl pass, verb anywhere and label anywhere
func containsLaunchctlGatewayLifecycle(normalized string) bool {
	return launchctlLifecycleRe.MatchString(normalized) && gatewayLabelRe.MatchString(normalized)
}

// ContainsGatewayLifecycleCommand reports whether text holds a gateway lifecycle command shape
func ContainsGatewayLifecycleCommand(text string) bool {
	if text == "" {
		return false
	}
	text = StripInertHeredocBodies(text)
	normalized := shellLineContinuation.ReplaceAllString(text, " ")
	if regexp2Match(gatewayLifecyclePattern, normalized) {
		return true
	}
	if m, err := profileFlagLifecyclePattern.FindStringMatch(normalized); err == nil && m != nil {
		named := m.GroupByNumber(1).String()
		if named == "" {
			named = m.GroupByNumber(2).String()
		}
		if named != "" && namedProfileIsCurrent(strings.Trim(strings.TrimSpace(named), "\"'")) {
			return true
		}
	}
	for _, segment := range iterLifecycleCommandSegments(normalized) {
		joined := strings.Join(segment, " ")
		if joined != "" && regexp2Match(gatewayLifecyclePattern, joined) {
			return true
		}
		stripped := argvListPunctuationRe.ReplaceAllString(joined, " ")
		if stripped != joined && regexp2Match(gatewayLifecyclePattern, stripped) {
			return true
		}
	}
	return containsLaunchctlGatewayLifecycle(normalized) || ContainsGatewayImageKill(normalized)
}

// Splits on newlines outside quotes honoring escapes
func splitLogicalLines(text string) []string {
	var lines []string
	var current strings.Builder
	inSingle, inDouble, escape := false, false, false
	for _, ch := range text {
		switch {
		case escape:
			escape = false
		case ch == '\\':
			escape = true
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		case ch == '\n' && !inSingle && !inDouble:
			lines = append(lines, current.String())
			current.Reset()
			continue
		}
		current.WriteRune(ch)
	}
	if current.Len() > 0 {
		lines = append(lines, current.String())
	}
	return lines
}

// POSIX tokenizes one shell line honoring quotes, comments, and control operator punctuation
func lifecycleShlexTokens(line string) ([]string, error) {
	return shlexSplitPunct(line, ";&|()", true)
}

func isControlToken(token string) bool {
	if token == "" {
		return false
	}
	for _, r := range token {
		if !strings.ContainsRune(lifecycleControlChars, r) {
			return false
		}
	}
	return true
}

// Non empty token runs between control operators, each control token its own run when kept
func splitLifecycleSegments(tokens []string, keepControls bool) [][]string {
	var out [][]string
	var segment []string
	for _, token := range tokens {
		if isControlToken(token) {
			if len(segment) > 0 {
				out = append(out, segment)
				segment = nil
			}
			if keepControls {
				out = append(out, []string{token})
			}
			continue
		}
		segment = append(segment, token)
	}
	if len(segment) > 0 {
		out = append(out, segment)
	}
	return out
}

// Shell tokenized command segments per logical line, falling back to physical lines on bad quoting
func iterLifecycleCommandSegments(command string) [][]string {
	var out [][]string
	for _, line := range splitLogicalLines(strings.ReplaceAll(command, "\\\n", "")) {
		tokens, err := lifecycleShlexTokens(line)
		if err != nil {
			for _, physical := range strings.Split(line, "\n") {
				if physical == "" {
					continue
				}
				if t, err := lifecycleShlexTokens(physical); err == nil {
					out = append(out, splitLifecycleSegments(t, false)...)
				}
			}
			continue
		}
		out = append(out, splitLifecycleSegments(tokens, false)...)
	}
	return out
}

// Command name of an executable token, the raw token when the path has no name
func lifecycleExecutableName(token string) string {
	name := filepath.Base(token)
	if token == "" || token == "." || token == ".." || name == "." || name == string(filepath.Separator) || name == "/" {
		return token
	}
	return name
}

// Index of the command a wrapper chain actually executes, may equal the segment length
func peelTransparentPrefixes(segment []string, index int) int {
	for range maxPrefixPeels {
		if index >= len(segment) {
			return index
		}
		name := lifecycleExecutableName(segment[index])
		if !transparentPrefixWords[name] {
			return index
		}
		valueOptions := transparentPrefixValueOptions[name]
		index++
		for index < len(segment) {
			token := segment[index]
			if token == "--" {
				index++
				break
			}
			if valueOptions[token] {
				index += 2
				continue
			}
			if strings.HasPrefix(token, "-") || lifecycleEnvAssignRe.MatchString(token) {
				index++
				continue
			}
			break
		}
		for range transparentPrefixOperands[name] {
			if index < len(segment) && !strings.HasPrefix(segment[index], "-") {
				index++
			}
		}
	}
	return index
}

// Executable token index after simple env assignments
func commandTokenIndex(segment []string) (int, bool) {
	for i, token := range segment {
		if !lifecycleEnvAssignRe.MatchString(token) {
			return i, true
		}
	}
	return 0, false
}

// Index of the command a segment actually executes with assignments and wrappers peeled
func executedCommandIndex(segment []string) (int, bool) {
	index, ok := commandTokenIndex(segment)
	if !ok {
		return 0, false
	}
	index = peelTransparentPrefixes(segment, index)
	if index < len(segment) {
		return index, true
	}
	return 0, false
}

// ContainsLaunchctlSubmitCommand detects an executed launchctl submit or bootstrap
func ContainsLaunchctlSubmitCommand(command string) bool {
	for _, segment := range iterLifecycleCommandSegments(command) {
		index, ok := executedCommandIndex(segment)
		if ok && lifecycleExecutableName(segment[index]) == "launchctl" {
			arguments := segment[index+1:]
			if len(arguments) > 0 {
				verb := strings.ToLower(arguments[0])
				if verb == "submit" || verb == "bootstrap" {
					return true
				}
			}
		}
	}
	return false
}

// Replaces data sink executable arguments with a neutral placeholder, fail closed on any execution marker
func maskDataSinkArguments(text string) string {
	lines := strings.Split(text, "\n")
	if text == "" {
		lines = []string{text}
	} else if strings.HasSuffix(text, "\n") {
		lines = lines[:len(lines)-1]
	}
	var out []string
	changed := false
	for _, line := range lines {
		if pipeToInterpreterRe.MatchString(line) {
			out = append(out, line)
			continue
		}
		tokens, err := lifecycleShlexTokens(line)
		if err != nil {
			out = append(out, line)
			continue
		}
		var rebuilt []string
		for _, segment := range splitLifecycleSegments(tokens, true) {
			index, ok := commandTokenIndex(segment)
			if ok && dataSinkExecutables[filepath.Base(segment[index])] {
				arguments := segment[index+1:]
				unsafe := false
				for _, argument := range arguments {
					if dotCommandArgumentRe.MatchString(argument) {
						unsafe = true
						break
					}
					for _, marker := range unsafeDataArgMarkers {
						if strings.Contains(argument, marker) {
							unsafe = true
							break
						}
					}
					if unsafe {
						break
					}
				}
				if !unsafe {
					changed = true
					rebuilt = append(rebuilt, segment[:index+1]...)
					for range arguments {
						rebuilt = append(rebuilt, "arg")
					}
					continue
				}
			}
			rebuilt = append(rebuilt, segment...)
		}
		out = append(out, strings.Join(rebuilt, " "))
	}
	if !changed {
		return text
	}
	return strings.Join(out, "\n")
}

// Lifecycle scan exempting matches inside data arguments
func lifecycleCommandScanWithDataExemption(text string) bool {
	if !ContainsGatewayLifecycleCommand(text) {
		return false
	}
	normalized := shellLineContinuation.ReplaceAllString(text, " ")
	return ContainsGatewayLifecycleCommand(maskDataSinkArguments(normalized))
}

// DirectLifecycleScan runs the pure string lifecycle scans, data exempted regex plus submit
func DirectLifecycleScan(command string) bool {
	return lifecycleCommandScanWithDataExemption(command) || ContainsLaunchctlSubmitCommand(command)
}
