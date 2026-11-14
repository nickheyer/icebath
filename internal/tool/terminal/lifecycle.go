package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool/approval"
)

const (
	maxReferencedScriptBytes = 1 << 20
	maxReferencedScriptDepth = 8
	maxScanLines             = 16384
	maxScanLineBytes         = 64 * 1024
	maxScanPaths             = 1024
	maxScanRemoteReads       = 64
	maxPrefixPeels           = 8
	binarySniffBytes         = 4096
	controlChars             = ";&|()"
)

var (
	errNoClosingQuotation  = errors.New("No closing quotation")
	errNoEscapedCharacter  = errors.New("No escaped character")
	lineContinuationRe     = regexp.MustCompile(`\\\r?\n[ \t]*`)
	pipeToInterpreterRe    = regexp.MustCompile(`\|\s*&?\s*(?:sudo\s+)?(?:sh|bash|dash|ksh|zsh|xargs|eval|source)\b`)
	dotCommandArgumentRe   = regexp.MustCompile(`^\.[A-Za-z]`)
	envAssignmentPrefixRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	scriptShellExecutables = map[string]bool{"sh": true, "bash": true, "dash": true, "ksh": true, "zsh": true}
	shellOptionsWithValues = map[string]bool{"-O": true, "+O": true, "-o": true, "+o": true}
	shellCommandFlags      = map[string]bool{"-c": true, "--command": true}
	cloudPlaceholderDirs   = map[string]bool{"Mobile Documents": true, "CloudStorage": true}
	dataSinkExecutables    = map[string]bool{"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true,
		"journalctl": true, "sqlite3": true, "psql": true}
	unsafeDataArgMarkers = []string{"`", "$(", "<(", ">(", "\\!"}
	transparentPrefixes  = map[string]bool{"sudo": true, "doas": true, "env": true, "nohup": true, "setsid": true, "nice": true, "ionice": true, "stdbuf": true, "timeout": true, "exec": true, "command": true, "builtin": true, "eatmydata": true, "pkexec": true, "su": true, "runuser": true, "setpriv": true, "systemd-run": true, "nsenter": true, "unshare": true}
	transparentValueOpts = map[string]map[string]bool{"sudo": set("-u", "-g", "-U", "-C", "-p", "-r", "-t", "-T", "--user", "--group", "--prompt"), "doas": set("-u", "-C"), "env": set("-u", "--unset", "-S", "--split-string", "-C", "--chdir"), "nice": set("-n", "--adjustment"), "ionice": set("-c", "-n", "-p", "--class", "--classdata"), "stdbuf": set("-i", "-o", "-e", "--input", "--output", "--error"), "timeout": set("-s", "-k", "--signal", "--kill-after"), "pkexec": set("--user"), "su": set("-s", "--shell", "-g", "--group", "-G", "--supp-group"), "runuser": set("-u", "--user", "-s", "--shell", "-g", "--group", "-G", "--supp-group"), "setpriv": set("--reuid", "--regid", "--groups", "--inh-caps", "--ambient-caps", "--bounding-set", "--selinux-label", "--apparmor-profile"), "systemd-run": set("-u", "--unit", "-p", "--property", "-E", "--setenv", "--slice", "--description", "--uid", "--gid", "--on-calendar", "--service-type"), "nsenter": set("-t", "--target", "-S", "--setuid", "-G", "--setgid", "-r", "--root", "-w", "--wd"), "unshare": set("--map-user", "--map-group", "--setgroups", "-R", "--root", "-w", "--wd")}
	stringCommandOptions = map[string][]string{"env": {"-S", "--split-string"}, "su": {"-c", "--command"}, "runuser": {"-c", "--command"}}
	transparentOperands  = map[string]int{"timeout": 1}
	binaryMagics         = [][]byte{[]byte("\x7fELF"), {0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf}, {0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}, []byte("MZ"), []byte("!<arch>"), {0x1f, 0x8b}, []byte("PK\x03\x04")}
)

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

type scanBudget struct {
	bytes, lines, paths, remote int
	refusal                     string
}

func newScanBudget() *scanBudget {
	return &scanBudget{bytes: maxReferencedScriptBytes, lines: maxScanLines, paths: maxScanPaths, remote: maxScanRemoteReads}
}

func (b *scanBudget) chargeText(text string) bool {
	if utf8.RuneCountInString(text) > b.bytes || len(text) > b.bytes {
		return false
	}
	lines := strings.Count(text, "\n") + 1
	if lines > b.lines {
		return false
	}
	for _, line := range strings.Split(text, "\n") {
		if utf8.RuneCountInString(line) > maxScanLineBytes {
			return false
		}
	}
	b.bytes -= len(text)
	b.lines -= lines
	return true
}

func (b *scanBudget) chargePath() bool {
	if b.paths <= 0 {
		return false
	}
	b.paths--
	return true
}

func (b *scanBudget) chargeRemoteRead() bool {
	if b.remote <= 0 {
		return false
	}
	b.remote--
	return true
}

func cappedReadLimit(maxBytes int) int {
	return min(maxReferencedScriptBytes, max(0, maxBytes))
}

func lifecycleScanRootWithinBudget(text string) bool {
	return newScanBudget().chargeText(text)
}

func shlexPunct(line string) ([]string, error) {
	const ws = " \t\r\n"
	r := []rune(line)
	pos := 0
	var pushback []rune
	next := func() (rune, bool) {
		if n := len(pushback); n > 0 {
			c := pushback[n-1]
			pushback = pushback[:n-1]
			return c, true
		}
		if pos >= len(r) {
			return 0, false
		}
		c := r[pos]
		pos++
		return c, true
	}
	skipLine := func() {
		for pos < len(r) {
			c := r[pos]
			pos++
			if c == '\n' {
				return
			}
		}
	}
	in := func(c rune, s string) bool { return strings.ContainsRune(s, c) }
	const stateNone, stateSpace, stateWord, statePunct, stateEscape rune = 0, ' ', 'a', 'c', '\\'
	state := stateSpace
	var tokens []string
	for state != stateNone {
		var tok []rune
		quoted := false
		escapedState := stateSpace
	read:
		for {
			c, ok := next()
			switch state {
			case stateSpace:
				switch {
				case !ok:
					state = stateNone
					break read
				case in(c, ws):
					if len(tok) > 0 || quoted {
						break read
					}
				case c == '#':
					skipLine()
				case c == '\\':
					escapedState, state = stateWord, stateEscape
				case in(c, controlChars):
					tok, state = []rune{c}, statePunct
				case c == '\'' || c == '"':
					state = c
				default:
					tok, state = []rune{c}, stateWord
				}
			case stateWord, statePunct:
				switch {
				case !ok:
					state = stateNone
					break read
				case in(c, ws):
					state = stateSpace
					if len(tok) > 0 || quoted {
						break read
					}
				case c == '#':
					skipLine()
					state = stateSpace
					if len(tok) > 0 || quoted {
						break read
					}
				case state == statePunct:
					if in(c, controlChars) {
						tok = append(tok, c)
					} else {
						if !in(c, ws) {
							pushback = append(pushback, c)
						}
						state = stateSpace
						break read
					}
				case c == '\'' || c == '"':
					state = c
				case c == '\\':
					escapedState, state = stateWord, stateEscape
				case !in(c, controlChars):
					tok = append(tok, c)
				default:
					pushback = append(pushback, c)
					state = stateSpace
					if len(tok) > 0 || quoted {
						break read
					}
				}
			case '\'', '"':
				quoted = true
				switch {
				case !ok:
					return nil, errNoClosingQuotation
				case c == state:
					state = stateWord
				case c == '\\' && state == '"':
					escapedState, state = state, stateEscape
				default:
					tok = append(tok, c)
				}
			case stateEscape:
				if !ok {
					return nil, errNoEscapedCharacter
				}
				if (escapedState == '\'' || escapedState == '"') && c != stateEscape && c != escapedState {
					tok = append(tok, '\\')
				}
				tok = append(tok, c)
				state = escapedState
			}
		}
		if len(tok) == 0 && !quoted {
			break
		}
		tokens = append(tokens, string(tok))
	}
	return tokens, nil
}

func splitPhysicalLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func splitLogicalLines(text string) []string {
	var lines []string
	var current []rune
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
			lines = append(lines, string(current))
			current = nil
			continue
		}
		current = append(current, ch)
	}
	if len(current) > 0 {
		lines = append(lines, string(current))
	}
	return lines
}

func isControlToken(token string) bool {
	if token == "" {
		return false
	}
	for _, r := range token {
		if !strings.ContainsRune(controlChars, r) {
			return false
		}
	}
	return true
}

func splitSegments(tokens []string, keepControls bool) [][]string {
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

func commandSegments(command string) [][]string {
	var out [][]string
	for _, line := range splitLogicalLines(strings.ReplaceAll(command, "\\\n", "")) {
		tokens, err := shlexPunct(line)
		if err != nil {
			for _, physical := range splitPhysicalLines(line) {
				if toks, err := shlexPunct(physical); err == nil {
					out = append(out, splitSegments(toks, false)...)
				}
			}
			continue
		}
		out = append(out, splitSegments(tokens, false)...)
	}
	return out
}

func pathSeparators() string {
	if runtime.GOOS == "windows" {
		return `/\`
	}
	return "/"
}

func pathComponents(p string) []string {
	seps := pathSeparators()
	if runtime.GOOS == "windows" && len(p) >= 2 && p[1] == ':' {
		p = p[2:]
	}
	var parts []string
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return strings.ContainsRune(seps, r) }) {
		if part != "." {
			parts = append(parts, part)
		}
	}
	return parts
}

func pathBase(token string) string {
	parts := pathComponents(token)
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func executableName(token string) string {
	if name := pathBase(token); name != "" {
		return name
	}
	return token
}

func peelTransparentPrefixes(segment []string, index int) int {
	for range maxPrefixPeels {
		if index >= len(segment) {
			return index
		}
		name := executableName(segment[index])
		if !transparentPrefixes[name] {
			return index
		}
		valueOpts := transparentValueOpts[name]
		index++
		for index < len(segment) {
			token := segment[index]
			if token == "--" {
				index++
				break
			}
			if valueOpts[token] {
				index += 2
				continue
			}
			if strings.HasPrefix(token, "-") || envAssignmentPrefixRe.MatchString(token) {
				index++
				continue
			}
			break
		}
		for range transparentOperands[name] {
			if index < len(segment) && !strings.HasPrefix(segment[index], "-") {
				index++
			}
		}
	}
	return index
}

func commandTokenIndex(segment []string) (int, bool) {
	for i, token := range segment {
		if !envAssignmentPrefixRe.MatchString(token) {
			return i, true
		}
	}
	return 0, false
}

func executedCommandIndex(segment []string) (int, bool) {
	index, ok := commandTokenIndex(segment)
	if !ok {
		return 0, false
	}
	index = peelTransparentPrefixes(segment, index)
	return index, index < len(segment)
}

func containsLaunchctlSubmit(command string) bool {
	for _, segment := range commandSegments(command) {
		index, ok := executedCommandIndex(segment)
		if ok && executableName(segment[index]) == "launchctl" {
			args := segment[index+1:]
			if len(args) > 0 {
				if verb := strings.ToLower(args[0]); verb == "submit" || verb == "bootstrap" {
					return true
				}
			}
		}
	}
	return false
}

func maskDataSinkArguments(text string) string {
	lines := splitPhysicalLines(text)
	if len(lines) == 0 {
		lines = []string{text}
	}
	var out []string
	changed := false
	for _, line := range lines {
		if pipeToInterpreterRe.MatchString(line) {
			out = append(out, line)
			continue
		}
		tokens, err := shlexPunct(line)
		if err != nil {
			out = append(out, line)
			continue
		}
		var rebuilt []string
		for _, segment := range splitSegments(tokens, true) {
			index, ok := commandTokenIndex(segment)
			if ok && dataSinkExecutables[pathBase(segment[index])] {
				args := segment[index+1:]
				unsafe := false
				for _, arg := range args {
					if dotCommandArgumentRe.MatchString(arg) {
						unsafe = true
					}
					for _, marker := range unsafeDataArgMarkers {
						if strings.Contains(arg, marker) {
							unsafe = true
						}
					}
				}
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
	if changed {
		return strings.Join(out, "\n")
	}
	return text
}

func lifecycleWithDataExemption(det *approval.Detector, text string) bool {
	if !det.GatewayLifecycle(text) {
		return false
	}
	return det.GatewayLifecycle(maskDataSinkArguments(lineContinuationRe.ReplaceAllString(text, " ")))
}

func directLifecycleScan(det *approval.Detector, command string) bool {
	return lifecycleWithDataExemption(det, command) || containsLaunchctlSubmit(command)
}

func isCloudPlaceholderPath(p string) bool {
	parts := pathComponents(p)
	for i := 1; i < len(parts); i++ {
		if parts[i-1] == "Library" && cloudPlaceholderDirs[parts[i]] {
			return true
		}
	}
	return false
}

func onCloudPath(p string) bool {
	return isCloudPlaceholderPath(p) || isCloudPlaceholderPath(resolveLenient(p))
}

func expandCandidatePath(candidate string) (string, bool) {
	if candidate == "" || strings.ContainsRune(candidate, 0) {
		return "", false
	}
	return home.ExpandUser(candidate), true
}

func resolvedOrNothing(candidate, cwd string) []string {
	p, ok := expandCandidatePath(candidate)
	if !ok {
		return nil
	}
	if !filepath.IsAbs(p) {
		base := cwd
		if base == "" {
			wd, err := os.Getwd()
			if err != nil {
				return nil
			}
			base = wd
		}
		p = filepath.Join(base, p)
	}
	return []string{p}
}

func optionValues(segment []string, start int, option string) []string {
	var out []string
	prefix := option + "="
	for position := start + 1; position < len(segment); position++ {
		token := segment[position]
		if token == option && position+1 < len(segment) {
			out = append(out, segment[position+1])
		} else if strings.HasPrefix(token, prefix) {
			out = append(out, token[len(prefix):])
		}
	}
	return out
}

func referencesAt(segment []string, index int, cwd string) []string {
	if index >= len(segment) {
		return nil
	}
	executable := segment[index]
	name := executableName(executable)
	if name == "." || name == "source" {
		if len(segment) > index+1 {
			return resolvedOrNothing(segment[index+1], cwd)
		}
		return nil
	}
	if scriptShellExecutables[name] {
		args := segment[index+1:]
		i := 0
	options:
		for i < len(args) {
			arg := args[i]
			switch {
			case arg == "--":
				i++
				break options
			case shellCommandFlags[arg]:
				break options
			case shellOptionsWithValues[arg]:
				i += 2
			case strings.HasPrefix(arg, "-"):
				i++
			default:
				break options
			}
		}
		if i < len(args) && !shellCommandFlags[args[i]] {
			return resolvedOrNothing(args[i], cwd)
		}
		return nil
	}
	if strings.Trim(executable, "/") != "" && (strings.Contains(executable, "/") || strings.HasSuffix(executable, ".sh") ||
		strings.HasSuffix(executable, ".bash") || strings.HasSuffix(executable, ".zsh")) {
		return resolvedOrNothing(executable, cwd)
	}
	return nil
}

func referencedShellScripts(command, cwd string) []string {
	var out []string
	for _, segment := range commandSegments(command) {
		index, ok := commandTokenIndex(segment)
		if !ok {
			continue
		}
		out = append(out, referencesAt(segment, index, cwd)...)
		if peeled := peelTransparentPrefixes(segment, index); peeled != index {
			out = append(out, referencesAt(segment, peeled, cwd)...)
		}
	}
	return out
}

func shellCommandPayloads(command string) []string {
	var out []string
	for _, segment := range commandSegments(command) {
		index, ok := commandTokenIndex(segment)
		if !ok {
			continue
		}
		for _, option := range stringCommandOptions[executableName(segment[index])] {
			out = append(out, optionValues(segment, index, option)...)
		}
		index, ok = executedCommandIndex(segment)
		if !ok || !scriptShellExecutables[executableName(segment[index])] {
			continue
		}
		args := segment[index+1:]
		for i := 0; i+1 < len(args); i++ {
			if shellCommandFlags[args[i]] {
				out = append(out, args[i+1])
				break
			}
		}
	}
	return out
}

func hasBinaryMagic(data []byte) bool {
	if bytes.HasPrefix(data, []byte("#!")) {
		return false
	}
	for _, magic := range binaryMagics {
		if bytes.HasPrefix(data, magic) {
			return true
		}
	}
	return false
}

func readReferencedScriptUnlocked(path string, maxBytes int) (string, bool, bool) {
	limit := cappedReadLimit(maxBytes)
	if onCloudPath(path) {
		return "", false, true
	}
	f, err := openNonblocking(path)
	if err != nil {
		return "", false, false
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			slog.Debug("closing referenced script failed", "path", path, "err", cerr)
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return "", false, false
	}
	if !info.Mode().IsRegular() {
		return "", false, !info.IsDir()
	}
	buf := make([]byte, binarySniffBytes)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, false
	}
	data := buf[:n]
	if hasBinaryMagic(data) {
		return "", false, false
	}
	if info.Size() > int64(limit) {
		return "", false, true
	}
	for len(data) <= limit {
		chunk := make([]byte, limit+1-len(data))
		n, err := f.Read(chunk)
		if n == 0 {
			if err != nil && !errors.Is(err, io.EOF) {
				return "", false, false
			}
			break
		}
		data = append(data, chunk[:n]...)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", false, false
		}
	}
	if hasBinaryMagic(data) {
		return "", false, false
	}
	if len(data) > limit {
		return "", false, true
	}
	data = bytes.ReplaceAll(data, []byte{0}, nil)
	return strings.ToValidUTF8(string(data), replacementChar), true, false
}

func (t *Terminal) readReferencedScript(path string, maxBytes int) (string, bool, bool) {
	var text string
	var present, unsafe bool
	err := t.o.Databases.OfflineAccess(path, "read referenced script", func() error {
		text, present, unsafe = readReferencedScriptUnlocked(path, maxBytes)
		return nil
	})
	if err != nil {
		return "", false, t.o.Databases.HasLiveConnection(path)
	}
	return text, present, unsafe
}

func sanitizeRemoteScriptText(text string, maxBytes int) (string, bool, bool) {
	if text == "" || strings.ContainsRune(text, 0) {
		return "", false, false
	}
	limit := cappedReadLimit(maxBytes)
	if utf8.RuneCountInString(text) > limit || len(text) > limit {
		return "", false, true
	}
	return text, true, false
}

func (t *Terminal) unreadableReason(path string) string {
	if t.o.Databases.HasLiveConnection(path) {
		return fmt.Sprintf("`%s` is a SQLite database open in this gateway process", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("`%s` could not be read", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("`%s` is not a regular file", path)
	}
	return fmt.Sprintf("`%s` is larger than the scan cap (%d bytes) or the remaining walk budget", path, maxReferencedScriptBytes)
}

type lifecycleScan struct {
	t          *Terminal
	ctx        context.Context
	det        *approval.Detector
	readRemote func(path string) string
	budget     *scanBudget
	visited    map[string]bool
}

func (s *lifecycleScan) run(command, cwd string) (bool, string) {
	s.budget = newScanBudget()
	s.visited = map[string]bool{}
	if s.walk(command, cwd, 0, true) {
		return true, s.budget.refusal
	}
	return false, ""
}

func (s *lifecycleScan) exhausted(what string, depth int) bool {
	slog.Warn(fmt.Sprintf("lifecycle guard scan budget exhausted (%s at depth %d); failing closed", what, depth))
	s.budget.refusal = fmt.Sprintf("the scan budget was exhausted (%s at depth %d)", what, depth)
	return true
}

func (s *lifecycleScan) refuse(path, reason string) bool {
	slog.Warn(fmt.Sprintf("lifecycle guard cannot scan referenced script %s: %s; failing closed", path, reason))
	s.budget.refusal = reason
	return true
}

func (s *lifecycleScan) scriptDirectory(path string) string {
	raw, ok := expandCandidatePath(path)
	if !ok {
		return ""
	}
	if filepath.IsAbs(raw) {
		return filepath.Dir(raw)
	}
	h, err := s.t.o.Homes.Home(s.ctx)
	if err != nil {
		return ""
	}
	return filepath.Dir(filepath.Join(h, "scripts", raw))
}

type scriptCandidate struct {
	path     string
	executed bool
}

func (s *lifecycleScan) walk(command, cwd string, depth int, executed bool) bool {
	if !s.budget.chargeText(command) {
		if executed {
			return s.exhausted("text", depth)
		}
		return false
	}
	if directLifecycleScan(s.det, command) {
		return true
	}
	if depth >= maxReferencedScriptDepth {
		return executed
	}
	walkCommand := approval.StripInertHeredocs(command)
	for _, payload := range shellCommandPayloads(walkCommand) {
		if s.walk(payload, cwd, depth+1, executed) {
			return true
		}
	}
	var candidates []scriptCandidate
	for _, p := range referencedShellScripts(walkCommand, cwd) {
		candidates = append(candidates, scriptCandidate{p, executed})
	}
	if walkCommand != command {
		for _, p := range referencedShellScripts(command, cwd) {
			candidates = append(candidates, scriptCandidate{p, false})
		}
	}
	for _, cand := range candidates {
		if onCloudPath(cand.path) {
			if cand.executed {
				return s.refuse(cand.path, fmt.Sprintf("`%s` lives on a cloud-synced path (iCloud Drive / "+
					"~/Library/CloudStorage) that the guard refuses to open", cand.path))
			}
			continue
		}
		resolved := resolveLenient(cand.path)
		if s.visited[resolved] {
			continue
		}
		if !s.budget.chargePath() {
			if cand.executed {
				return s.exhausted("paths", depth)
			}
			break
		}
		s.visited[resolved] = true
		text, present, unsafe := s.t.readReferencedScript(cand.path, s.budget.bytes)
		if unsafe {
			if cand.executed {
				return s.refuse(cand.path, s.t.unreadableReason(cand.path))
			}
			continue
		}
		if !present && s.readRemote != nil {
			if !s.budget.chargeRemoteRead() {
				if cand.executed {
					return s.exhausted("remote reads", depth)
				}
				break
			}
			text, _, unsafe = sanitizeRemoteScriptText(s.readRemote(cand.path), s.budget.bytes)
			if unsafe {
				if cand.executed {
					return s.refuse(cand.path, fmt.Sprintf("`%s` read from the backend exceeds the scan cap "+
						"(%d bytes) or the remaining walk budget", cand.path, maxReferencedScriptBytes))
				}
				continue
			}
		}
		if text == "" {
			continue
		}
		dir := s.scriptDirectory(resolved)
		if dir == "" {
			dir = cwd
		}
		if s.walk(text, dir, depth+1, cand.executed) {
			return true
		}
	}
	return false
}
