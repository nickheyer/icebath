package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	rgVersionRe = regexp.MustCompile(`(?m)^ripgrep` + pyWSClass + `+((?:0|[1-9]\p{Nd}*))\.` +
		`(?:0|[1-9]\p{Nd}*)\.(?:0|[1-9]\p{Nd}*)` +
		`(?:-(?:(?:0|[1-9]\p{Nd}*)|(?:[0-9A-Za-z-]*[A-Za-z-]` +
		`[0-9A-Za-z-]*))(?:\.(?:(?:0|[1-9]\p{Nd}*)|` +
		`(?:[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)))*)?` +
		`(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?` +
		`(?:` + pyWSClass + `+\(rev [^)]+\))?` + pyWSClass + `*$`)
	bareExecutableRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	regexMetaRe      = regexp.MustCompile(`[.\[\](){}?*+^$\\|]`)
)

// Ripgrep binary PM selected for this install, swapped by tests that pin a bundled rg
var pmRipgrepBinary = func() (string, bool) { return pmInstalledBinary("ripgrep") }

// Pinned ripgrep forward slashed for the shell transports, empty when PM has no usable binary
func bundledRg() string {
	binary, ok := pmRipgrepBinary()
	if !ok || binary == "" {
		return ""
	}
	if st, err := os.Stat(binary); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	return strings.ReplaceAll(binary, `\`, "/")
}

// Executable in the command host's namespace, rg caching only hits so a later install shows up
func (s *shellFileOps) resolveCommand(cmd string) string {
	if cmd != "rg" {
		if s.hasCommand(cmd) {
			return cmd
		}
		return ""
	}
	s.cacheMu.Lock()
	cached := s.rgResolutionCache[cmd]
	s.cacheMu.Unlock()
	if cached != "" {
		return cached
	}
	remember := func(resolved string) string {
		s.cacheMu.Lock()
		s.rgResolutionCache[cmd] = resolved
		s.cacheMu.Unlock()
		return resolved
	}
	onHost := EnvUsesHostFilesystem(s.terminalEnv)
	if onHost {
		if bundled := bundledRg(); bundled != "" {
			return remember(bundled)
		}
	}
	result := s.exec("command -v rg 2>/dev/null", "", 0)
	if out := pyStrip(result.Stdout); result.ExitCode == 0 && out != "" {
		resolved := pySplitLines(out)[0]
		if resolved == "yes" {
			resolved = "rg"
		}
		return remember(resolved)
	}
	if isWindowsHost && onHost {
		userProfile := os.Getenv("USERPROFILE")
		if userProfile == "" {
			userProfile = searchDefaultHome()
		}
		scoop := os.Getenv("SCOOP")
		if scoop == "" {
			scoop = pathJoinOS(userProfile, "scoop")
		}
		candidates := []string{
			pathJoinOS(pathJoinOS(pathJoinOS(userProfile, ".cargo"), "bin"), "rg.exe"),
			pathJoinOS(pathJoinOS(scoop, "shims"), "rg.exe"),
		}
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			candidates = append(candidates, pathJoinOS(pathJoinOS(pathJoinOS(pathJoinOS(localAppData, "Microsoft"), "WinGet"), "Links"), "rg.exe"))
		}
		for _, candidate := range candidates {
			if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() {
				return remember(strings.ReplaceAll(candidate, `\`, "/"))
			}
		}
	}
	return ""
}

// Cached actionable error unless the rg build reports a full SemVer at major 14 or newer
func (s *shellFileOps) modifiedRgCapabilityError(executable string) string {
	s.cacheMu.Lock()
	cached, ok := s.rgModifiedCapability[executable]
	s.cacheMu.Unlock()
	if ok {
		if cached == nil {
			return ""
		}
		return *cached
	}
	result := s.exec(s.quoteExecutable(executable)+" --version", "", 10*time.Second)
	var errText *string
	m := rgVersionRe.FindStringSubmatch(result.Stdout)
	if !(result.ExitCode == 0 && m != nil && rgMajorAtLeast(m[1], 14)) {
		msg := "Exact modification-time order requires ripgrep 14 or newer; " +
			"upgrade ripgrep or use order='discovery'."
		errText = &msg
	}
	s.cacheMu.Lock()
	s.rgModifiedCapability[executable] = errText
	s.cacheMu.Unlock()
	if errText == nil {
		return ""
	}
	return *errText
}

// Whether a decimal major version string is at least the floor at any magnitude
func rgMajorAtLeast(digits string, floor int64) bool {
	n, ok := new(big.Int).SetString(pyDigitsASCII(digits), 10)
	return ok && n.Cmp(big.NewInt(floor)) >= 0
}

// Exec candidates Python Popen tries in order, the name alone when it holds a slash
func popenCandidates(name string, runEnv map[string]string) []string {
	if strings.Contains(name, "/") {
		return []string{name}
	}
	pathValue, ok := runEnv["PATH"]
	if !ok {
		pathValue = "/bin:/usr/bin"
	}
	var out []string
	for _, dir := range strings.Split(pathValue, string(os.PathListSeparator)) {
		out = append(out, pyOSPathJoin(dir, name))
	}
	return out
}

// Reads stderr to EOF keeping the first 64 KiB plus the first non permission line past the cap
func drainStderrCapped(r io.Reader, chunks *[][]byte, omitted *int) {
	br := bufio.NewReader(r)
	kept, keptOther := 0, false
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			switch {
			case len(*chunks) > 0 && kept+len(raw) > stderrRetainBytes:
				line := pyStrip(decodeUTF8Replace(raw))
				if !keptOther && line != "" && !permissionDeniedRe.MatchString(line) {
					*chunks = append(*chunks, raw)
					keptOther = true
				} else {
					*omitted++
				}
			default:
				*chunks = append(*chunks, raw)
				kept += len(raw)
			}
		}
		if err != nil {
			return
		}
	}
}

// Runs shell quoted rg words as a direct process keeping fetchLimit stdout lines under a deadline
func (s *shellFileOps) runRgNative(argv []string, fetchLimit, timeout int, mergeStderr bool) *ExecuteResult {
	cwd := s.liveCwd()
	args, err := shlexSplit(strings.Join(argv, " "))
	if err != nil {
		panic(err)
	}
	var childEnv map[string]string
	if ce, ok := s.terminalEnv.(interface{ ChildEnv() map[string]string }); ok {
		childEnv = ce.ChildEnv()
	}
	runEnv, err := makeRunEnv(s.env, childEnv)
	if err != nil {
		panic(err)
	}
	if chdirErr := popenChdirError(cwd); chdirErr != nil {
		return &ExecuteResult{Stdout: "rg: " + pathErrorText(chdirErr, cwd), ExitCode: 2}
	}
	spawnFailure := func(err error, files ...*os.File) *ExecuteResult {
		for _, f := range files {
			if f != nil {
				f.Close()
			}
		}
		return &ExecuteResult{Stdout: "rg: " + pathErrorText(err, args[0]), ExitCode: 2}
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return spawnFailure(err)
	}
	var stderrR, stderrW *os.File
	if mergeStderr {
		if stderrR, stderrW, err = os.Pipe(); err != nil {
			return spawnFailure(err, stdoutR, stdoutW)
		}
	}
	var cmd *exec.Cmd
	var saved, last error
	for _, candidate := range popenCandidates(args[0], runEnv) {
		c := &exec.Cmd{Path: candidate, Args: args, Dir: cwd, Env: envSlice(runEnv), Stdout: stdoutW}
		if stderrW != nil {
			c.Stderr = stderrW
		}
		startNewSession(c)
		startErr := c.Start()
		if startErr == nil {
			cmd = c
			break
		}
		var errno syscall.Errno
		if !errors.As(startErr, &errno) {
			return spawnFailure(startErr, stdoutR, stdoutW, stderrR, stderrW)
		}
		if errno != syscall.ENOENT && errno != syscall.ENOTDIR && saved == nil {
			saved = errno
		}
		last = errno
	}
	if cmd == nil {
		if saved == nil {
			saved = last
		}
		return spawnFailure(saved, stdoutR, stdoutW, stderrR, stderrW)
	}
	stdoutW.Close()
	if stderrW != nil {
		stderrW.Close()
	}
	proc := &ExecProcess{Cmd: cmd, waitDone: make(chan struct{})}
	go func() {
		defer close(proc.waitDone)
		werr := cmd.Wait()
		proc.code = exitCodeOf(cmd, werr)
	}()
	if pgid, gerr := processGroupOf(proc.Pid()); gerr == nil {
		proc.pgid = pgid
		if start, ok := processStartTime(proc.Pid()); ok {
			proc.pgidStart = strconv.FormatInt(start, 10)
		}
	}
	var lines [][]byte
	var bounded atomic.Bool
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		br := bufio.NewReader(stdoutR)
		for {
			raw, rerr := br.ReadBytes('\n')
			if len(raw) > 0 {
				lines = append(lines, raw)
				if len(lines) >= fetchLimit {
					bounded.Store(true)
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	var errChunks [][]byte
	errOmitted := 0
	var errDone chan struct{}
	if mergeStderr {
		errDone = make(chan struct{})
		go func() {
			defer close(errDone)
			drainStderrCapped(stderrR, &errChunks, &errOmitted)
		}()
	}
	parent := s.ctx
	if parent == nil {
		parent = context.Background()
	}
	runCtx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Second)
	defer cancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	exitCode := 0
	for {
		select {
		case <-drainDone:
		case <-runCtx.Done():
		case <-tick.C:
		}
		if chanClosed(drainDone) || bounded.Load() {
			break
		}
		if parent.Err() != nil || searchIsInterrupted(parent) {
			exitCode = 130
			break
		}
		if runCtx.Err() != nil {
			exitCode = 124
			break
		}
	}
	if _, done := proc.Poll(); !done {
		if kerr := killLocalProcessTree(proc); kerr != nil {
			panic(kerr)
		}
	}
	proc.Wait(0)
	<-drainDone
	stdoutR.Close()
	stdout := decodeUTF8Replace(joinBytes(lines))
	if mergeStderr {
		<-errDone
		stderrR.Close()
		errText := decodeUTF8Replace(joinBytes(errChunks))
		if errOmitted > 0 {
			errText = strings.TrimRight(errText, "\n") + "\n" + omittedDiagnosticsLine(errOmitted) + "\n"
		}
		stdout = errText + stdout
	}
	switch exitCode {
	case 124:
		return &ExecuteResult{Stdout: stdout + fmt.Sprintf("\n[Command timed out after %ds]", timeout), ExitCode: 124}
	case 130:
		return &ExecuteResult{Stdout: stdout + "\n[Command interrupted]", ExitCode: 130}
	}
	if bounded.Load() {
		return &ExecuteResult{Stdout: stdout}
	}
	return &ExecuteResult{Stdout: stdout, ExitCode: proc.code}
}

// Whether the channel has been closed, without blocking
func chanClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Concatenation of byte chunks
func joinBytes(chunks [][]byte) []byte {
	n := 0
	for _, c := range chunks {
		n += len(c)
	}
	out := make([]byte, 0, n)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

// First fetchLimit lines of an rg run, natively on a local POSIX host else through the shell and head
func (s *shellFileOps) runRgBounded(words []string, fetchLimit, timeout int, mergeStderr, nativeOK bool, shellPrefix string) *ExecuteResult {
	if nativeOK && s.nativeReadEnabled() {
		return s.runRgNative(words, fetchLimit, timeout, mergeStderr)
	}
	stderr := " 2>/dev/null"
	if mergeStderr {
		stderr = ""
	}
	return s.exec(shellPrefix+strings.Join(words, " ")+stderr+" | head -n "+strconv.Itoa(fetchLimit), "", time.Duration(timeout)*time.Second)
}

// Executable quoted for the shell without leaking controller path semantics to a remote host
func (s *shellFileOps) quoteExecutable(executable string) string {
	if bareExecutableRe.MatchString(executable) {
		return executable
	}
	if EnvUsesHostFilesystem(s.terminalEnv) {
		return s.escapeNativeToolArg(executable)
	}
	return "'" + strings.ReplaceAll(executable, "'", `'"'"'`) + "'"
}

// Glob pairs excluding the macOS protected folders below the root from an rg run
func (s *shellFileOps) rgExclusionGlobs(path string) []string {
	var out []string
	for _, item := range s.macosSearchExclusions(path) {
		out = append(out, "--glob", s.escapeShellArg("!"+item+"/**", true))
	}
	return out
}

// Globs pruning the shared heavyweight directory names both at the root and at any depth
func (s *shellFileOps) searchPruneGlobArgs() string {
	names := make([]string, 0, len(SearchPruneDirNames))
	for name := range SearchPruneDirNames {
		names = append(names, name)
	}
	sort.Strings(names)
	var globs []string
	for _, dirname := range names {
		for _, prefix := range []string{"", "**/"} {
			globs = append(globs, "--glob", s.escapeShellArg("!"+prefix+dirname+"/**", true))
		}
	}
	return strings.Join(globs, " ")
}

// One count only rg probe and the hint sentence it yields when it finds matches
type zeroMatchProbeSpec struct {
	flags    string
	template string
}

// Case insensitive, hidden or ignored, and fixed string probes in the order they run
var zeroMatchProbes = []zeroMatchProbeSpec{
	{"-i", "0 exact matches, but %s case-insensitive match(es) in %d file(s): " +
		"%s — the pattern's casing may be wrong."},
	{"--hidden --no-ignore", "0 matches in visible files, but %s match(es) in %d " +
		"hidden or gitignored file(s): %s — these are excluded by default."},
	{"-F", "0 regex matches, but %s literal match(es) in %d file(s): %s — the " +
		"pattern contains regex metacharacters that likely need escaping " +
		"(or pass a simpler substring)."},
}

// Steering hint for a zero match content search from the first cheap rg probe that hits
func (s *shellFileOps) zeroMatchProbe(pattern, path, fileGlob string) (string, error) {
	rgExecutable := s.resolveCommand("rg")
	if rgExecutable == "" {
		return "", nil
	}
	rg := s.quoteExecutable(rgExecutable)
	hasMeta := regexMetaRe.MatchString(pattern)
	globExpr := ""
	if fileGlob != "" {
		globExpr = " --glob " + s.escapeShellArg(fileGlob, true)
	}
	for _, probe := range zeroMatchProbes {
		if probe.flags == "-F" && !hasMeta {
			continue
		}
		globExprProbe := globExpr
		if strings.HasPrefix(probe.flags, "--hidden") {
			globExprProbe = globExpr + " " + s.searchPruneGlobArgs()
		}
		words := []string{rg, probe.flags, "--count-matches", globExprProbe,
			s.escapeShellArg(pattern, false), s.escapeNativeToolArg(path)}
		res := s.runRgBounded(words, 50, 30, false, true, "")
		total := new(big.Int)
		var perFile []string
		for _, line := range pySplitLines(pyStrip(res.Stdout)) {
			p, n := "", line
			if i := strings.LastIndex(line, ":"); i >= 0 {
				p, n = line[:i], line[i+1:]
			}
			if !pyIsDigitText(n) {
				continue
			}
			digits, ok := pyIntDigits(n)
			if !ok {
				return "", fmt.Errorf("invalid literal for int() with base 10: %s", pyReprStr(n))
			}
			addPyInt(total, digits)
			perFile = append(perFile, p)
		}
		if total.Sign() > 0 {
			paths := strings.Join(perFile[:min(5, len(perFile))], ", ")
			if extra := len(perFile) - 5; extra > 0 {
				paths += fmt.Sprintf(" (+%d more)", extra)
			}
			return fmt.Sprintf(probe.template, total.String(), len(perFile), paths), nil
		}
	}
	return "", nil
}

// Probe hint with any failure inside the probe swallowed into no hint
func (s *shellFileOps) zeroMatchHint(pattern, path, fileGlob string) (hint string) {
	defer func() {
		if recover() != nil {
			hint = ""
		}
	}()
	h, err := s.zeroMatchProbe(pattern, path, fileGlob)
	if err != nil {
		return ""
	}
	return h
}

// Filename search through rg files mode with root scoped exclusions and a directory supplement
func (s *shellFileOps) searchFilesRg(pattern string, roots []string, rootList bool, limit, offset int, order, rgExecutable string) *SearchResult {
	globPattern := pattern
	if !strings.Contains(pattern, "/") && !strings.HasPrefix(pattern, "*") {
		globPattern = "*" + pattern
	}
	fetchLimit := limit + offset + 1
	effective := s.effectiveMacosSearchExclusions(roots)
	scoped, scopedCommon := false, ""
	commandRoots := roots
	var exclusionTerms []string
	if len(roots) > 1 && len(effective) > 0 && posixRoots(roots) {
		cwd := s.liveCwd()
		absoluteRoots := make([]string, len(roots))
		for i, root := range roots {
			if !strings.HasPrefix(root, "/") {
				root = posixJoin(cwd, root)
			}
			absoluteRoots[i] = posixNormpath(root)
		}
		scoped, scopedCommon = true, posixCommonpath(absoluteRoots)
		commandRoots = make([]string, len(absoluteRoots))
		for i, root := range absoluteRoots {
			commandRoots[i] = posixRelpath(root, scopedCommon)
		}
		for _, ex := range effective {
			exclusionTerms = append(exclusionTerms, "--glob "+s.escapeShellArg("!"+posixRelpath(ex.absolute, scopedCommon)+"/**", true))
		}
	} else {
		for _, ex := range effective {
			exclusionTerms = append(exclusionTerms, "--glob "+s.escapeShellArg("!"+ex.relative+"/**", true))
		}
	}
	var unique []string
	for _, term := range exclusionTerms {
		if !containsString(unique, term) {
			unique = append(unique, term)
		}
	}
	exclusionArgs := ""
	if len(unique) > 0 {
		exclusionArgs = " " + strings.Join(unique, " ")
	}
	if rgExecutable == "" {
		rgExecutable = s.resolveCommand("rg")
	}
	if rgExecutable == "" {
		return &SearchResult{Error: "File search requires ripgrep (rg)."}
	}
	if order == "modified" {
		if capabilityError := s.modifiedRgCapabilityError(rgExecutable); capabilityError != "" {
			return &SearchResult{Error: capabilityError}
		}
	}
	rg := s.quoteExecutable(rgExecutable)
	sortArg := ""
	if order == "modified" {
		sortArg = " --sortr=modified"
	}
	rootArgs := make([]string, len(commandRoots))
	for i, root := range commandRoots {
		rootArgs[i] = s.escapeNativeToolArg(root)
	}
	cdPrefix := ""
	if scoped {
		cdPrefix = "cd " + s.escapeShellArg(scopedCommon, true) + " && "
	}
	rgCmd := rg + " --files" + sortArg + " -g " + s.escapeShellArg(globPattern, true) +
		exclusionArgs + " -- " + strings.Join(rootArgs, " ")
	result := s.runRgBounded([]string{rgCmd}, fetchLimit, 60, true, !scoped, "set -o pipefail; "+cdPrefix)
	stdout, limitReason := searchStdoutAndLimit(result)
	stdoutLines := pySplitLines(stdout)
	var diagLines, allFiles []string
	for _, line := range stdoutLines {
		if hasAnyPrefix(pyLStrip(line), []string{"rg: ", "grep: "}) {
			diagLines = append(diagLines, line)
		} else if line != "" {
			allFiles = append(allFiles, line)
		}
	}
	diagnostics := strings.Join(diagLines, "\n")
	var skip searchSkip
	if isPermissionOnlyError(diagnostics) {
		skip = skippedFields(diagnostics)
	}
	permissionOnly := result.ExitCode == 2 && len(skip.paths) > 0
	warning := prependSkippedPathsWarning(skip, "")
	if scoped {
		for i, f := range allFiles {
			if !strings.HasPrefix(f, "/") {
				allFiles[i] = posixNormpath(posixJoin(scopedCommon, f))
			}
		}
	}
	boundedSigpipe := result.ExitCode == 141 && len(allFiles) >= fetchLimit
	if code := result.ExitCode; code != 0 && code != 1 && code != 124 && !boundedSigpipe && !permissionOnly {
		detail := pyStrip(diagnostics)
		if detail == "" {
			detail = "no diagnostics"
		}
		if order == "modified" {
			return &SearchResult{Error: "Exact modification-time order search failed (ripgrep 14+ " +
				"required for --sortr=modified; the installed rg passed the " +
				"version check, so this is an invocation failure: " + detail + "). " +
				"Use order='discovery'."}
		}
		return &SearchResult{Error: "File search failed while running ripgrep: " + detail}
	}
	if limitReason != "search_timeout" && !rootList && s.hasCommand("find") {
		dirs, directoryLimitReason := s.searchDirectoriesWithFind(globPattern, roots[0], fetchLimit)
		if len(dirs) > 0 {
			existing := setOf(allFiles...)
			combined := append([]string{}, allFiles...)
			for _, d := range dirs {
				if !existing[d] {
					combined = append(combined, d)
				}
			}
			allFiles = s.sortPathsByMtime(combined)
			if limitReason == "" {
				limitReason = directoryLimitReason
			}
		}
	}
	if permissionOnly && len(allFiles) == 0 {
		return skip.apply(&SearchResult{Warning: warning + " " + permissionSearchHint})
	}
	return skip.apply(&SearchResult{Files: sliceWindow(allFiles, offset, limit), TotalCount: len(allFiles),
		Truncated: len(allFiles) > offset+limit || limitReason != "", LimitReason: limitReason, Warning: warning})
}

// Content search through rg, else grep, with zero match hints and the grep newline note
func (s *shellFileOps) searchContent(pattern, path, fileGlob string, limit, offset int, outputMode string, context int) *SearchResult {
	usedRg := s.hasCommand("rg")
	var result *SearchResult
	switch {
	case usedRg:
		exe := s.resolveCommand("rg")
		if exe == "" {
			exe = "rg"
		}
		result = s.searchWithRg(pattern, path, fileGlob, limit, offset, outputMode, context, exe)
	case s.hasCommand("grep"):
		result = s.searchWithGrep(pattern, path, fileGlob, limit, offset, outputMode, context)
	default:
		return &SearchResult{Error: "Content search requires ripgrep (rg) or grep. " +
			"Install ripgrep: https://github.com/BurntSushi/ripgrep#installation"}
	}
	if result.Error == "" && result.TotalCount == 0 && len(result.Matches) == 0 && len(result.Files) == 0 && len(result.Counts) == 0 {
		if hint := s.zeroMatchHint(pattern, path, fileGlob); hint != "" {
			if result.Warning == "" {
				result.Warning = hint
			} else {
				result.Warning += " " + hint
			}
		}
	}
	if usedRg {
		return result
	}
	return maybeWarnLineOrientedNewlinePattern(result, pattern)
}

// Bounded pipeline run and parse, grep lanes through the shell with a column cap
func (s *shellFileOps) runSearchPipeline(cmdParts []string, outputMode string, limit, offset, context int, warning string, lineCap bool) *SearchResult {
	fetchLimit := limit + offset
	if context > 0 {
		fetchLimit += 200
	}
	var result *ExecuteResult
	if lineCap {
		parts := append(append([]string{}, cmdParts...), "|", "head", "-n", strconv.Itoa(fetchLimit))
		if outputMode != "files_only" && outputMode != "count" {
			parts = append(parts, "|", "cut", "-c1-2000")
		}
		result = s.exec("set -o pipefail; "+strings.Join(parts, " "), "", 60*time.Second)
	} else {
		result = s.runRgBounded(cmdParts, fetchLimit, 60, true, true, "set -o pipefail; ")
	}
	return parseSearchOutput(result, outputMode, limit, offset, context, warning)
}

// Content search with ripgrep, column capped, multiline for newline regexes
func (s *shellFileOps) searchWithRg(pattern, path, fileGlob string, limit, offset int, outputMode string, context int, rgExecutable string) *SearchResult {
	if rgExecutable == "" {
		rgExecutable = s.resolveCommand("rg")
	}
	if rgExecutable == "" {
		return &SearchResult{Error: "Content search requires ripgrep (rg)."}
	}
	cmdParts := []string{s.quoteExecutable(rgExecutable), "--line-number", "--no-heading", "--with-filename"}
	if outputMode != "files_only" && outputMode != "count" {
		cmdParts = append(cmdParts, "--max-columns", "2000", "--max-columns-preview")
	}
	multiline := patternHasRegexNewline(pattern)
	if multiline {
		cmdParts = append(cmdParts, "--multiline")
	}
	if context > 0 {
		cmdParts = append(cmdParts, "-C", strconv.Itoa(context))
	}
	cmdParts = append(cmdParts, s.rgExclusionGlobs(path)...)
	if fileGlob != "" {
		cmdParts = append(cmdParts, "--glob", s.escapeShellArg(fileGlob, true))
	}
	if flag, ok := outputModeFlags[outputMode]; ok {
		cmdParts = append(cmdParts, flag)
	}
	cmdParts = append(cmdParts, s.escapeShellArg(pattern, false), s.escapeNativeToolArg(path))
	mlNote := ""
	if multiline {
		mlNote = "Pattern contains \\n — multiline mode (-U) was enabled automatically " +
			"so the regex can match across line boundaries."
	}
	return s.runSearchPipeline(cmdParts, outputMode, limit, offset, context, mlNote, false)
}
