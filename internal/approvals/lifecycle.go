package approvals

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Commands that stop, restart or kill the server the agent runs in are refused there, since it would kill the command and every running
// agent with it; scripts the command runs are read and checked too

// Shapes of a command that targets the gateway's lifecycle, each anchored on a name of its own
var lifecyclePattern = regexp2.MustCompile(
	`(?i)(?:(?<![/\w.\-])icebath(?:\.(?:exe|cmd|bat|com|ps1))?\s+gateway\s+(?:restart|stop|uninstall)\b)`+
		`|(?:launchctl\s+(?:kickstart|unload|load|stop|restart|submit|bootstrap|bootout|remove|disable)\b[^\n]*\bicebath[.\-]?gateway)`+
		`|(?:systemctl\s+(?:-\S+\s+)*(?:restart|stop|start)\b[^\n]*\bicebath[.\-]?gateway)`+
		`|(?:\b(?:p?kill|taskkill|stop-process)\b[^\n]*\bicebath\b[^\n]*\bgateway)`+
		`|(?:\b(?:p?kill|taskkill|stop-process)\b[^\n]*\bgateway\b[^\n]*\bicebath)`, regexp2.None)

// The restart with a profile flag, refused only when it names the profile the guard runs for
var profileLifecycle = regexp2.MustCompile(dialect(
	`(?i)icebath\s+(?:-\S++(?:\s++(?!-\S)\S++)?\s++)*(?:--profile=([^\s]+)|(?:-p|--profile)\s+([^\s]+))(?:\s++-\S++(?:\s++(?!-\S)\S++)?)*\s+gateway\s+(?:restart|stop)`),
	regexp2.None)

// Lowered name of the program that hosts this process
var hostImage = func() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return ""
	}
	return strings.ToLower(filepath.Base(exe))
}()

var (
	interpreterImage = regexp.MustCompile(`^pythonw?(?:\d+(?:\.\d+)*)?(?:\.exe)?\n?$`)
	ereTail          = regexp.MustCompile(`[.*+?\\\[\](){}|].*$`)
	wildcardsOnly    = regexp.MustCompile(`^[.*+?$\s]*$`)
	killVerb         = regexp.MustCompile(`(?i)\b(?:kill|taskkill|stop-process)\b`)
	gatewayCmdline   = regexp.MustCompile(`(?i)icebath_cli|\bicebath\b[^\n]*\bgateway\b|\bgateway\b[^\n]*\bicebath\b`)
	taskkillFilter   = regexp.MustCompile(`(?i)^\s*['"]?imagename\s+eq\s+(\S+)`)
	continuation     = regexp.MustCompile(`\\\r?\n[ \t]*`)
	listPunctuation  = regexp.MustCompile(`[\[\],]+`)
	launchctlVerb    = regexp.MustCompile(`(?i)\blaunchctl\s+(?:kickstart|unload|load|stop|restart|bootout|kill|disable|remove)\b`)
	gatewayLabel     = regexp.MustCompile(`(?i)\bicebath[.\-]?gateway\b`)
	envAssign        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	dotArgument      = regexp.MustCompile(`^\.[A-Za-z]`)
	pipeToShell      = regexp.MustCompile(`\|\s*&?\s*(?:sudo\s+)?(?:sh|bash|dash|ksh|zsh|xargs|eval|source)\b`)
)

var pkillArgOptions = strs.Set("-g", "--pgroup", "-G", "--group", "-P", "--parent", "-s", "--session", "-t", "--terminal", "-u", "--euid", "-U",
	"--uid", "-F", "--pidfile", "--ns", "--nslist", "-d", "--delimiter", "--signal", "-O", "--older", "-r", "--runstates", "--cgroup", "-A",
	"--ignore-ancestors")

var killerArgOptions = map[string]map[string]bool{
	"pkill":   pkillArgOptions,
	"pgrep":   pkillArgOptions,
	"killall": strs.Set("-s", "--signal", "-u", "--user", "-o", "--older-than", "-y", "--younger-than", "-n", "--ns", "-Z", "--context"),
	"pidof":   strs.Set("-o", "--omit-pid", "-S", "--separator"),
}

var (
	nameKillers     = strs.Set("pkill", "killall", "taskkill", "stop-process")
	nameEnumerators = strs.Set("pgrep", "pidof", "get-process")
	// Executables whose arguments are data, read but not run
	dataSinks = strs.Set("grep", "egrep", "fgrep", "rg", "ag", "ack", "journalctl", "sqlite3", "psql")
	// Markers of an argument a data sink would run after all
	unsafeDataMarkers = []string{"`", "$(", "<(", ">(", `\!`}
	// Wrappers that run the command after them, with the options that take a value and the operands before the command
	transparent       = strs.Set("sudo", "doas", "env", "nohup", "setsid", "nice", "ionice", "stdbuf", "timeout", "exec", "command", "builtin", "eatmydata", "pkexec", "su", "runuser", "setpriv", "systemd-run", "nsenter", "unshare")
	transparentValues = map[string]map[string]bool{
		"sudo":        strs.Set("-u", "-g", "-U", "-C", "-p", "-r", "-t", "-T", "--user", "--group", "--prompt"),
		"doas":        strs.Set("-u", "-C"),
		"env":         strs.Set("-u", "--unset", "-S", "--split-string", "-C", "--chdir"),
		"nice":        strs.Set("-n", "--adjustment"),
		"ionice":      strs.Set("-c", "-n", "-p", "--class", "--classdata"),
		"stdbuf":      strs.Set("-i", "-o", "-e", "--input", "--output", "--error"),
		"timeout":     strs.Set("-s", "-k", "--signal", "--kill-after"),
		"pkexec":      strs.Set("--user"),
		"su":          strs.Set("-s", "--shell", "-g", "--group", "-G", "--supp-group"),
		"runuser":     strs.Set("-u", "--user", "-s", "--shell", "-g", "--group", "-G", "--supp-group"),
		"setpriv":     strs.Set("--reuid", "--regid", "--groups", "--inh-caps", "--ambient-caps", "--bounding-set", "--selinux-label", "--apparmor-profile"),
		"systemd-run": strs.Set("-u", "--unit", "-p", "--property", "-E", "--setenv", "--slice", "--description", "--uid", "--gid", "--on-calendar", "--service-type"),
		"nsenter":     strs.Set("-t", "--target", "-S", "--setuid", "-G", "--setgid", "-r", "--root", "-w", "--wd"),
		"unshare":     strs.Set("--map-user", "--map-group", "--setgroups", "-R", "--root", "-w", "--wd"),
	}
	transparentOperands = map[string]int{"timeout": 1}
)

const maxPeels = 8

// Text every surface shows for a kill aimed at the program hosting the server
const hostKillRefusal = "Blocked: this command kills every process whose image/name matches the interpreter hosting this gateway (and this command). " +
	"Stop only the process you own instead: process(action=\"kill\", session_id=\"proc_…\") for a background job Icebath started, or kill/taskkill " +
	"by its explicit PID."

// Whether a process or image name is the program hosting the server, a prefix of it counting when substring is set
func hostName(value string, substring bool) bool {
	name := strings.TrimSuffix(strings.ToLower(strings.Trim(strings.TrimSpace(value), `"'`)), ".exe")
	if name == "" {
		return false
	}
	if strings.HasSuffix(name, "*") {
		prefix := strings.TrimRight(name, "*")
		return strings.HasPrefix("python", prefix) || strings.HasPrefix(hostImage, prefix)
	}
	if interpreterImage.MatchString(name) || name == strings.TrimSuffix(hostImage, ".exe") {
		return true
	}
	return substring && len(name) >= 2 && strings.HasPrefix("python", name)
}

// Whether a pkill style pattern reaches the server by its name or its full command line
func reachesHost(pattern string, fullCmdline, exact bool) bool {
	core := strings.TrimLeft(strings.Trim(strings.TrimSpace(pattern), `"'`), "^")
	core = strings.TrimSuffix(core, "$")
	head, rest := core, ""
	if fullCmdline {
		head, rest, _ = strings.Cut(core, " ")
	}
	if !hostName(head, !exact) {
		if loc := ereTail.FindStringIndex(head); loc != nil {
			rest = head[loc[0]:] + " " + rest
			head = head[:loc[0]]
		}
	}
	if !hostName(head, !exact) {
		return fullCmdline && gatewayCmdline.MatchString(core)
	}
	return strings.TrimSpace(rest) == "" || wildcardsOnly.MatchString(rest) || strings.Contains(strings.ToLower(rest), "icebath")
}

// Whether the killer or lister with the arguments would pick the server's program
func killerReachesHost(name string, args []string) bool {
	switch name {
	case "pkill", "pgrep", "killall", "pidof":
		has := func(options ...string) bool {
			return slices.ContainsFunc(args, func(arg string) bool { return slices.Contains(options, arg) })
		}
		exact := name == "pidof" || (name == "killall") != has("-r", "--regexp", "-x", "--exact")
		full := (name == "pkill" || name == "pgrep") && has("-f", "--full")
		var operands []string
		for i := 0; i < len(args); {
			arg := args[i]
			if arg == "--" {
				operands = append(operands, args[i+1:]...)
				break
			}
			if killerArgOptions[name][arg] {
				i += 2
				continue
			}
			if !strings.HasPrefix(arg, "-") {
				operands = append(operands, arg)
			}
			i++
		}
		return slices.ContainsFunc(operands, func(op string) bool { return reachesHost(op, full, exact) })
	case "taskkill":
		for i := 0; i+1 < len(args); i++ {
			option, value := strings.TrimLeft(strings.ToLower(args[i]), "/-"), args[i+1]
			if option == "im" && hostName(value, false) {
				return true
			}
			if m := taskkillFilter.FindStringSubmatch(value); option == "fi" && m != nil && hostName(m[1], false) {
				return true
			}
		}
		return false
	}
	var values []string
	for i, arg := range args {
		option, inline, _ := strings.Cut(arg, ":")
		switch lower := strings.ToLower(option); {
		case lower == "-name" || lower == "-processname" || lower == "-n":
			switch {
			case inline != "":
				values = append(values, inline)
			case i+1 < len(args):
				values = append(values, args[i+1])
			default:
				values = append(values, "")
			}
		case name == "get-process" && !strings.HasPrefix(arg, "-") && (i == 0 || !strings.HasPrefix(args[i-1], "-")):
			values = append(values, arg)
		}
	}
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if hostName(part, false) {
				return true
			}
		}
	}
	return false
}

// Whether one command of the tokens runs one of the killers against the server's program
func segmentKillsHost(tokens []string, killers map[string]bool) bool {
	var candidates []int
	if index, ok := executedIndex(tokens); ok {
		candidates = append(candidates, index)
	}
	for i, token := range tokens {
		if killers[strings.TrimSuffix(strings.ToLower(executableName(token)), ".exe")] {
			candidates = append(candidates, i)
		}
	}
	for _, at := range candidates {
		name := strings.TrimSuffix(strings.ToLower(executableName(tokens[at])), ".exe")
		if killers[name] && killerReachesHost(name, tokens[at+1:]) {
			return true
		}
	}
	return false
}

// Whether the text holds a process killer aimed at the program hosting the server
func containsHostKill(text string) bool {
	normalized := continuation.ReplaceAllString(text, " ")
	verb := killVerb.MatchString(normalized)
	for _, segment := range lifecycleSegments(normalized) {
		joined := strings.Join(segment, " ")
		candidates := [][]string{segment}
		if stripped := listPunctuation.ReplaceAllString(joined, " "); stripped != joined {
			candidates = append(candidates, strings.Fields(stripped))
		}
		for _, tokens := range candidates {
			if segmentKillsHost(tokens, nameKillers) || verb && segmentKillsHost(tokens, nameEnumerators) {
				return true
			}
		}
	}
	return false
}

// Whether the text holds a command that stops, restarts or uninstalls the gateway, or kills the program hosting it; a profile flag counts
// only when it names the profile given
func containsLifecycle(text, profile string) bool {
	if text == "" {
		return false
	}
	normalized := continuation.ReplaceAllString(terminal.StripInertHeredocs(text), " ")
	if matches(lifecyclePattern, normalized) {
		return true
	}
	if m, err := profileLifecycle.FindStringMatch(normalized); err == nil && m != nil {
		named := m.GroupByNumber(1).String()
		if named == "" {
			named = m.GroupByNumber(2).String()
		}
		if named = strings.Trim(strings.TrimSpace(named), `"'`); profile != "" && strings.EqualFold(named, strings.TrimSpace(profile)) {
			return true
		}
	}
	for _, segment := range lifecycleSegments(normalized) {
		joined := strings.Join(segment, " ")
		if joined != "" && matches(lifecyclePattern, joined) {
			return true
		}
		if stripped := listPunctuation.ReplaceAllString(joined, " "); stripped != joined && matches(lifecyclePattern, stripped) {
			return true
		}
	}
	return launchctlVerb.MatchString(normalized) && gatewayLabel.MatchString(normalized) || containsHostKill(normalized)
}

// Lines of the text split at newlines outside quotes
func logicalLines(text string) []string {
	var lines []string
	var current strings.Builder
	single, double, escape := false, false, false
	for _, ch := range text {
		switch {
		case escape:
			escape = false
		case ch == '\\':
			escape = true
		case ch == '\'' && !double:
			single = !single
		case ch == '"' && !single:
			double = !double
		case ch == '\n' && !single && !double:
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

func lifecycleTokens(line string) ([]string, error) {
	return splitPunct(line, ";&|()", true)
}

func control(token string) bool {
	return token != "" && strings.Trim(token, ";&|()") == ""
}

// Runs of tokens between control operators, each operator a run of its own when kept
func splitSegments(tokens []string, keep bool) [][]string {
	var out [][]string
	var segment []string
	for _, token := range tokens {
		if !control(token) {
			segment = append(segment, token)
			continue
		}
		if len(segment) > 0 {
			out = append(out, segment)
			segment = nil
		}
		if keep {
			out = append(out, []string{token})
		}
	}
	if len(segment) > 0 {
		out = append(out, segment)
	}
	return out
}

// Commands of the text as words, line by line, physical lines read on their own where quoting doesn't close
func lifecycleSegments(command string) [][]string {
	var out [][]string
	for _, line := range logicalLines(strings.ReplaceAll(command, "\\\n", "")) {
		tokens, err := lifecycleTokens(line)
		if err == nil {
			out = append(out, splitSegments(tokens, false)...)
			continue
		}
		for _, physical := range strings.Split(line, "\n") {
			if physical == "" {
				continue
			}
			if tokens, err := lifecycleTokens(physical); err == nil {
				out = append(out, splitSegments(tokens, false)...)
			}
		}
	}
	return out
}

// Command name of an executable word, the word itself when its path names none
func executableName(token string) string {
	name := filepath.Base(token)
	if token == "" || token == "." || token == ".." || name == "." || name == string(filepath.Separator) || name == "/" {
		return token
	}
	return name
}

// Index of the command a chain of wrappers runs, which may be past the end
func peel(segment []string, index int) int {
	for range maxPeels {
		if index >= len(segment) {
			return index
		}
		name := executableName(segment[index])
		if !transparent[name] {
			return index
		}
		values := transparentValues[name]
		for index++; index < len(segment); {
			token := segment[index]
			if token == "--" {
				index++
				break
			}
			if values[token] {
				index += 2
				continue
			}
			if !strings.HasPrefix(token, "-") && !envAssign.MatchString(token) {
				break
			}
			index++
		}
		for range transparentOperands[name] {
			if index < len(segment) && !strings.HasPrefix(segment[index], "-") {
				index++
			}
		}
	}
	return index
}

// Index of the first word that is no assignment
func commandIndex(segment []string) (int, bool) {
	for i, token := range segment {
		if !envAssign.MatchString(token) {
			return i, true
		}
	}
	return 0, false
}

// Index of the command a segment runs, assignments and wrappers peeled
func executedIndex(segment []string) (int, bool) {
	index, ok := commandIndex(segment)
	if !ok {
		return 0, false
	}
	if index = peel(segment, index); index < len(segment) {
		return index, true
	}
	return 0, false
}

// Whether the command runs launchctl submit or bootstrap
func containsLaunchctlSubmit(command string) bool {
	for _, segment := range lifecycleSegments(command) {
		if index, ok := executedIndex(segment); ok && executableName(segment[index]) == "launchctl" && index+1 < len(segment) {
			if verb := strings.ToLower(segment[index+1]); verb == "submit" || verb == "bootstrap" {
				return true
			}
		}
	}
	return false
}

// Text with the arguments of data sinks replaced by a placeholder, unless one of them would run something
func maskDataArguments(text string) string {
	lines := strings.Split(text, "\n")
	if text != "" && strings.HasSuffix(text, "\n") {
		lines = lines[:len(lines)-1]
	}
	var out []string
	changed := false
	for _, line := range lines {
		if pipeToShell.MatchString(line) {
			out = append(out, line)
			continue
		}
		tokens, err := lifecycleTokens(line)
		if err != nil {
			out = append(out, line)
			continue
		}
		var rebuilt []string
		for _, segment := range splitSegments(tokens, true) {
			index, ok := commandIndex(segment)
			if ok && dataSinks[filepath.Base(segment[index])] {
				args := segment[index+1:]
				unsafe := slices.ContainsFunc(args, func(arg string) bool {
					return dotArgument.MatchString(arg) || slices.ContainsFunc(unsafeDataMarkers, func(m string) bool { return strings.Contains(arg, m) })
				})
				if !unsafe {
					changed = true
					rebuilt = append(rebuilt, segment[:index+1]...)
					for range args {
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

// Lifecycle check that leaves matches inside the arguments of data sinks alone
func lifecycleOutsideData(text, profile string) bool {
	return containsLifecycle(text, profile) && containsLifecycle(maskDataArguments(continuation.ReplaceAllString(text, " ")), profile)
}

// Lifecycle checks of the text alone: the commands it holds and launchctl submit
func directLifecycle(text, profile string) bool {
	return lifecycleOutsideData(text, profile) || containsLaunchctlSubmit(text)
}

// Largest script the walk reads, and how deep it follows scripts into scripts
const (
	maxScriptBytes = 1024 * 1024
	maxScriptDepth = 8
	sniffBytes     = 4096
)

// Work one walk may do
const (
	maxWalkBytes     = maxScriptBytes
	maxWalkLines     = 16384
	maxWalkLineBytes = 64 * 1024
	maxWalkPaths     = 1024
	maxRemoteReads   = 64
)

var (
	shellExecutables = strs.Set("sh", "bash", "dash", "ksh", "zsh")
	shellValueFlags  = strs.Set("-O", "+O", "-o", "+o")
	shellCommandFlag = strs.Set("-c", "--command")
	cloudMarkers     = strs.Set("Mobile Documents", "CloudStorage")
	stringCommands   = map[string][]string{"env": {"-S", "--split-string"}, "su": {"-c", "--command"}, "runuser": {"-c", "--command"}}
	binaryMagics     = [][]byte{[]byte("\x7fELF"), {0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf}, {0xce, 0xfa, 0xed, 0xfe},
		{0xcf, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}, []byte("MZ"), []byte("!<arch>"), {0x1f, 0x8b}, []byte("PK\x03\x04")}
)

// What one walk over a command and the scripts it runs may still spend, and why it gave up
type budget struct {
	bytes, lines, paths, remote int
	refusal                     string
}

func newBudget() *budget {
	return &budget{bytes: maxWalkBytes, lines: maxWalkLines, paths: maxWalkPaths, remote: maxRemoteReads}
}

// Charges the text, false when it doesn't fit
func (b *budget) text(text string) bool {
	lines := strings.Count(text, "\n") + 1
	if len(text) > b.bytes || lines > b.lines {
		return false
	}
	for _, line := range strings.Split(text, "\n") {
		if utf8Len(line) > maxWalkLineBytes {
			return false
		}
	}
	b.bytes -= len(text)
	b.lines -= lines
	return true
}

func utf8Len(s string) int { return len([]rune(s)) }

// Whether the text may go through the token checks on a fresh budget
func withinBudget(text string) bool {
	return newBudget().text(text)
}

func (b *budget) exhausted(what string, depth int) bool {
	slog.Warn("lifecycle guard scan budget exhausted, failing closed", "what", what, "depth", depth)
	b.refusal = fmt.Sprintf("the scan budget was exhausted (%s at depth %d)", what, depth)
	return true
}

func (b *budget) unreadable(path, reason string) bool {
	slog.Warn("lifecycle guard cannot scan referenced script, failing closed", "path", path, "reason", reason)
	b.refusal = reason
	return true
}

// Why a script that runs was refused unread
func unreadableReason(path string) string {
	st, err := os.Stat(path)
	switch {
	case err != nil:
		return fmt.Sprintf("`%s` could not be read", path)
	case !st.Mode().IsRegular():
		return fmt.Sprintf("`%s` is not a regular file", path)
	}
	return fmt.Sprintf("`%s` is larger than the scan cap (%d bytes) or the remaining walk budget", path, maxScriptBytes)
}

// Whether the path is in a macOS FileProvider folder, where opening a placeholder can hang
func cloudPath(path string) bool {
	inside := func(p string) bool {
		parts := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
		for i := 1; i < len(parts); i++ {
			if parts[i-1] == "Library" && cloudMarkers[parts[i]] {
				return true
			}
		}
		return false
	}
	return inside(path) || strings.IndexByte(path, 0) < 0 && inside(resolve(path))
}

// Candidate path anchored on the folder, false for none
func candidate(word, cwd string) (string, bool) {
	if word == "" || strings.IndexByte(word, 0) >= 0 {
		return "", false
	}
	path := fsx.ExpandHome(word)
	if filepath.IsAbs(path) {
		return path, true
	}
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", false
		}
		cwd = wd
	}
	return filepath.Join(cwd, path), true
}

// Scripts the word at index runs: sourced, given to a shell, or run by its path
func scriptsAt(segment []string, index int, cwd string) []string {
	if index >= len(segment) {
		return nil
	}
	executable := segment[index]
	name := executableName(executable)
	add := func(word string) []string {
		if p, ok := candidate(word, cwd); ok {
			return []string{p}
		}
		return nil
	}
	switch {
	case name == "." || name == "source":
		if len(segment) > index+1 {
			return add(segment[index+1])
		}
		return nil
	case shellExecutables[name]:
		args := segment[index+1:]
		i := 0
		for i < len(args) {
			arg := args[i]
			if arg == "--" {
				i++
				break
			}
			if shellCommandFlag[arg] {
				break
			}
			if shellValueFlags[arg] {
				i += 2
				continue
			}
			if !strings.HasPrefix(arg, "-") {
				break
			}
			i++
		}
		if i < len(args) && !shellCommandFlag[args[i]] {
			return add(args[i])
		}
		return nil
	case strings.Trim(executable, "/") != "" && (strings.Contains(executable, "/") || strings.HasSuffix(executable, ".sh") ||
		strings.HasSuffix(executable, ".bash") || strings.HasSuffix(executable, ".zsh")):
		return add(executable)
	}
	return nil
}

// Scripts the command runs, read at the word itself and at the command its wrappers run
func referencedScripts(command, cwd string) []string {
	var out []string
	for _, segment := range lifecycleSegments(command) {
		index, ok := commandIndex(segment)
		if !ok {
			continue
		}
		out = append(out, scriptsAt(segment, index, cwd)...)
		if peeled := peel(segment, index); peeled != index {
			out = append(out, scriptsAt(segment, peeled, cwd)...)
		}
	}
	return out
}

// Shell text the command hands a shell -c, su -c or env -S
func shellPayloads(command string) []string {
	var out []string
	for _, segment := range lifecycleSegments(command) {
		index, ok := commandIndex(segment)
		if !ok {
			continue
		}
		for _, option := range stringCommands[executableName(segment[index])] {
			for i := index + 1; i < len(segment); i++ {
				if segment[i] == option && i+1 < len(segment) {
					out = append(out, segment[i+1])
				} else if value, ok := strings.CutPrefix(segment[i], option+"="); ok {
					out = append(out, value)
				}
			}
		}
		if index, ok = executedIndex(segment); !ok || !shellExecutables[executableName(segment[index])] {
			continue
		}
		args := segment[index+1:]
		for i := 0; i+1 < len(args); i++ {
			if shellCommandFlag[args[i]] {
				out = append(out, args[i+1])
				break
			}
		}
	}
	return out
}

// Whether the bytes start like a compiled program, a shebang always counting as a script
func binary(data []byte) bool {
	if bytes.HasPrefix(data, []byte("#!")) {
		return false
	}
	return slices.ContainsFunc(binaryMagics, func(magic []byte) bool { return bytes.HasPrefix(data, magic) })
}

// Text of a regular file up to the limit, whether there was text, and whether it must be refused unread: folders hold nothing to scan,
// devices, oversized files and cloud placeholders fail closed
func readScript(path string, limit int) (string, bool, bool) {
	limit = min(max(limit, 0), maxScriptBytes)
	if cloudPath(path) {
		return "", false, true
	}
	if strings.IndexByte(path, 0) >= 0 {
		return "", false, false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", false, false
	}
	if !st.Mode().IsRegular() {
		return "", false, !st.IsDir()
	}
	buf := make([]byte, sniffBytes)
	n, err := f.Read(buf)
	if err != nil && n == 0 && !errors.Is(err, io.EOF) {
		return "", false, false
	}
	data := buf[:n]
	if binary(data) {
		return "", false, false
	}
	if st.Size() > int64(limit) {
		return "", false, true
	}
	rest, _ := io.ReadAll(io.LimitReader(f, int64(limit+1-len(data))))
	if data = append(data, rest...); len(data) > limit {
		return "", false, true
	}
	return terminal.Decode(bytes.ReplaceAll(data, []byte{0}, nil)), true, false
}

// Walk over the command, the shell text it carries and the scripts it runs; true when any of them stops or restarts the gateway, or when
// one that runs can't be read
func (b *budget) walk(command, cwd, profile string, depth int, visited map[string]bool, read func(string) (string, bool), runs bool) bool {
	if !b.text(command) {
		return runs && b.exhausted("text", depth)
	}
	if directLifecycle(command, profile) {
		return true
	}
	if depth >= maxScriptDepth {
		return runs
	}
	plain := terminal.StripInertHeredocs(command)
	for _, payload := range shellPayloads(plain) {
		if b.walk(payload, cwd, profile, depth+1, visited, read, runs) {
			return true
		}
	}
	type script struct {
		path string
		runs bool
	}
	var scripts []script
	for _, p := range referencedScripts(plain, cwd) {
		scripts = append(scripts, script{p, runs})
	}
	if plain != command {
		for _, p := range referencedScripts(command, cwd) {
			scripts = append(scripts, script{p, false})
		}
	}
	for _, s := range scripts {
		if cloudPath(s.path) {
			if s.runs {
				return b.unreadable(s.path, fmt.Sprintf("`%s` lives on a cloud-synced path (iCloud Drive / ~/Library/CloudStorage) that the guard refuses to open", s.path))
			}
			continue
		}
		resolved := s.path
		if strings.IndexByte(resolved, 0) < 0 {
			resolved = resolve(resolved)
		}
		if visited[resolved] {
			continue
		}
		if b.paths <= 0 {
			if s.runs {
				return b.exhausted("paths", depth)
			}
			break
		}
		b.paths--
		visited[resolved] = true
		text, present, refuse := readScript(s.path, b.bytes)
		if refuse {
			if s.runs {
				return b.unreadable(s.path, unreadableReason(s.path))
			}
			continue
		}
		if !present && read != nil {
			if b.remote <= 0 {
				if s.runs {
					return b.exhausted("remote reads", depth)
				}
				break
			}
			b.remote--
			remote, ok := read(s.path)
			switch {
			case !ok || remote == "" || strings.IndexByte(remote, 0) >= 0:
				text, present = "", false
			case len(remote) > min(max(b.bytes, 0), maxScriptBytes):
				if s.runs {
					return b.unreadable(s.path, fmt.Sprintf("`%s` read from the backend exceeds the scan cap (%d bytes) or the remaining walk budget", s.path, maxScriptBytes))
				}
				continue
			default:
				text, present = remote, true
			}
		}
		if !present || text == "" {
			continue
		}
		dir := cwd
		if filepath.IsAbs(resolved) {
			dir = filepath.Dir(resolved)
		}
		if b.walk(text, dir, profile, depth+1, visited, read, s.runs) {
			return true
		}
	}
	return false
}

// Whether the command, or a script it runs, stops or restarts the gateway, and why the walk gave up when it failed closed; read fetches a
// script through the session's backend when it is not on this machine
func scanLifecycle(command, cwd, profile string, read func(string) (string, bool)) (unsafe bool, refusal string) {
	b := newBudget()
	defer func() {
		if p := recover(); p != nil {
			slog.Warn("lifecycle guard referenced-script walk failed, falling back to direct-scan verdict", "panic", fmt.Sprint(p))
			unsafe, refusal = directLifecycle(command, profile), ""
		}
	}()
	if b.walk(command, cwd, profile, 0, map[string]bool{}, read, true) {
		return true, b.refusal
	}
	return false, ""
}
