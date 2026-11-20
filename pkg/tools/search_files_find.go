package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Home folders macOS guards with privacy prompts that broad searches must not walk into
var macosTCCProtectedHomeDirs = []string{"Desktop", "Documents", "Downloads", "Library", "Movies", "Music", "Pictures"}

// Protected home folders relative to a macOS search root that is the home or one of its ancestors
func macosProtectedSearchExclusions(path, cwd, home, platform string) []string {
	if platform != "darwin" {
		return nil
	}
	root := pyExpandUser(path)
	if !strings.HasPrefix(root, "/") {
		base := cwd
		if base == "" {
			base, _ = os.Getwd()
		}
		root = posixJoin(base, root)
	}
	root = posixNormpath(root)
	if home == "" {
		home = searchDefaultHome()
	}
	homePath := posixNormpath(pyExpandUser(home))
	var exclusions []string
	for _, dirname := range macosTCCProtectedHomeDirs {
		if rel, ok := purePosixRelativeTo(posixJoin(homePath, dirname), root); ok && rel != "" {
			exclusions = append(exclusions, rel)
		}
	}
	return exclusions
}

// Components of a POSIX path the way PurePosixPath splits them, anchor first
func purePosixParts(p string) []string {
	var parts []string
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		parts = append(parts, "//")
	case strings.HasPrefix(p, "/"):
		parts = append(parts, "/")
	}
	for _, c := range strings.Split(p, "/") {
		if c != "" && c != "." {
			parts = append(parts, c)
		}
	}
	return parts
}

// PurePosixPath relative_to as a slash joined remainder, false when target is not under root
func purePosixRelativeTo(target, root string) (string, bool) {
	tp, rp := purePosixParts(target), purePosixParts(root)
	if len(rp) > len(tp) {
		return "", false
	}
	for i := range rp {
		if tp[i] != rp[i] {
			return "", false
		}
	}
	return strings.Join(tp[len(rp):], "/"), true
}

// Host flavour components of a path, drive and root as the anchor, case folded on Windows when asked
func hostPathParts(p string, fold bool) []string {
	if !isWindowsHost {
		return purePosixParts(p)
	}
	drive, root, rest := ntSplitRoot(p)
	var parts []string
	if anchor := drive + strings.ReplaceAll(root, "/", `\`); anchor != "" {
		parts = append(parts, anchor)
	}
	for _, c := range strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == '\\' }) {
		if c != "." {
			parts = append(parts, c)
		}
	}
	if fold {
		for i := range parts {
			parts[i] = strings.ToLower(parts[i])
		}
	}
	return parts
}

// os.path.abspath for the host flavour against the process working directory
func hostAbspath(p string) string {
	if isWindowsHost {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return filepath.Clean(p)
	}
	return posixAbspath(p)
}

// os.path.realpath for the host flavour, non strict
func hostRealpath(p string) string {
	if isWindowsHost {
		return resolveNonStrict(p)
	}
	return pyRealpath(p)
}

// posixpath.commonpath of absolute paths
func posixCommonpath(paths []string) string {
	split := make([][]string, len(paths))
	for i, p := range paths {
		for _, c := range strings.Split(p, "/") {
			if c != "" && c != "." {
				split[i] = append(split[i], c)
			}
		}
	}
	common := split[0]
	for _, s := range split[1:] {
		n := 0
		for n < len(common) && n < len(s) && common[n] == s[n] {
			n++
		}
		common = common[:n]
	}
	prefix := ""
	if strings.HasPrefix(paths[0], "/") {
		prefix = "/"
	}
	return prefix + strings.Join(common, "/")
}

// posixpath.relpath of an absolute path from an absolute start
func posixRelpath(path, start string) string {
	split := func(p string) []string {
		var out []string
		for _, c := range strings.Split(posixAbspath(p), "/") {
			if c != "" {
				out = append(out, c)
			}
		}
		return out
	}
	pl, sl := split(path), split(start)
	n := 0
	for n < len(pl) && n < len(sl) && pl[n] == sl[n] {
		n++
	}
	rel := make([]string, 0, len(sl)-n+len(pl)-n)
	for range sl[n:] {
		rel = append(rel, "..")
	}
	rel = append(rel, pl[n:]...)
	if len(rel) == 0 {
		return "."
	}
	return strings.Join(rel, "/")
}

// os.path.commonpath of two absolute host paths, false where Python raises for mixed drives
func hostCommonpath(a, b string) (string, bool) {
	if !isWindowsHost {
		return posixCommonpath([]string{a, b}), true
	}
	da, ra, _ := ntSplitRoot(a)
	db, rb, _ := ntSplitRoot(b)
	if !strings.EqualFold(da, db) || (ra == "") != (rb == "") {
		return "", false
	}
	pa, pb := hostPathParts(a, false), hostPathParts(b, false)
	n := 0
	for n < len(pa) && n < len(pb) && strings.EqualFold(pa[n], pb[n]) {
		n++
	}
	if n == 0 {
		return da + ra, true
	}
	anchor := da + ra
	rest := pa[1:n]
	if anchor == "" {
		rest = pa[:n]
	}
	return anchor + strings.Join(rest, `\`), true
}

// Admission key of one filename walk, the backend type and its normalized root
type filenameSearchKey struct {
	envType string
	root    string
}

var (
	filenameSearchMu          sync.Mutex
	filenameSearchChanged     = make(chan struct{})
	activeFilenameSearchRoots = map[filenameSearchKey]bool{}
)

// Poll interval while a filename walk waits for another walk on the same root
const filenameSearchWait = 50 * time.Millisecond

// Sleeps until a walk releases its roots or the poll interval passes, swapped by tests
var filenameSearchAwait = func(changed <-chan struct{}) {
	select {
	case <-changed:
	case <-time.After(filenameSearchWait):
	}
}

// Filename walk root normalized for its backend without resolving a remote path locally
func normalizedFilenameSearchRoot(env Environment, root, fallbackCwd string) string {
	cwd := ""
	if env != nil {
		cwd = env.Cwd()
	}
	if cwd == "" {
		cwd = fallbackCwd
	}
	if EnvUsesHostFilesystem(env) {
		if isWindowsHost {
			root, cwd = msysToWindowsPath(root), msysToWindowsPath(cwd)
		}
		if !filepath.IsAbs(root) {
			root = pathJoinOS(cwd, root)
		}
		return guardNormcasePath(hostAbspath(hostNormpath(root)))
	}
	if !strings.HasPrefix(root, "/") {
		root = posixJoin(cwd, root)
	}
	return posixNormpath(root)
}

// Unique backend and root admission keys in sorted order
func filenameSearchRootKeys(env Environment, roots []string, fallbackCwd string) []filenameSearchKey {
	envType := fmt.Sprintf("%T", env)
	seen := map[filenameSearchKey]bool{}
	var keys []filenameSearchKey
	for _, root := range roots {
		k := filenameSearchKey{envType, normalizedFilenameSearchRoot(env, root, fallbackCwd)}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].envType != keys[j].envType {
			return keys[i].envType < keys[j].envType
		}
		return keys[i].root < keys[j].root
	})
	return keys
}

// Whether any key is held by a running walk, caller holds the admission lock
func anyFilenameSearchRootActive(keys []filenameSearchKey) bool {
	for _, k := range keys {
		if activeFilenameSearchRoots[k] {
			return true
		}
	}
	return false
}

// Claims every key at once, polling for the worker interrupt while another walk holds one
func (s *shellFileOps) acquireFilenameSearchRoots(keys []filenameSearchKey) bool {
	filenameSearchMu.Lock()
	defer filenameSearchMu.Unlock()
	for anyFilenameSearchRootActive(keys) {
		if searchIsInterrupted(s.ctx) {
			return false
		}
		changed := filenameSearchChanged
		filenameSearchMu.Unlock()
		filenameSearchAwait(changed)
		filenameSearchMu.Lock()
		if searchIsInterrupted(s.ctx) {
			return false
		}
	}
	if searchIsInterrupted(s.ctx) {
		return false
	}
	return RunIfNotInterrupted(s.ctx, func() {
		for _, k := range keys {
			activeFilenameSearchRoots[k] = true
		}
	})
}

// Releases a finished walk's keys and wakes every waiter
func releaseFilenameSearchRoots(keys []filenameSearchKey) {
	filenameSearchMu.Lock()
	for _, k := range keys {
		delete(activeFilenameSearchRoots, k)
	}
	close(filenameSearchChanged)
	filenameSearchChanged = make(chan struct{})
	filenameSearchMu.Unlock()
}

// Paths and find diagnostics from a bounded find run, timestamps stripped in modified order
func splitFindOutput(stdout, order string) ([]string, []string) {
	var rawFiles, diagnostics []string
	for _, line := range pySplitLines(stdout) {
		switch {
		case strings.HasPrefix(line, "find: "):
			diagnostics = append(diagnostics, line)
		case order == "modified":
			parts := strings.SplitN(line, " ", 2)
			if len(parts) == 2 && pyIsDigitText(strings.Replace(parts[0], ".", "", 1)) {
				rawFiles = append(rawFiles, parts[1])
			}
		case line != "":
			rawFiles = append(rawFiles, line)
		}
	}
	return rawFiles, diagnostics
}

// Terminal failure or partial warning for a bounded find run from its exit code and diagnostics
func findExitVerdict(exitCode int, rawFiles, diagnostics []string, fetchLimit int, order string) (*SearchResult, string) {
	boundedSigpipe := exitCode == 141 && len(rawFiles) >= fetchLimit
	if exitCode != 0 && exitCode != 1 && exitCode != 124 && !boundedSigpipe {
		if order == "modified" {
			return &SearchResult{Error: findModifiedOrderError}, ""
		}
		return &SearchResult{Error: "File search failed while running bounded find traversal."}, ""
	}
	diagnosticText := strings.Join(diagnostics, "\n")
	skipped := extractUnreadablePaths(diagnosticText)
	warning := ""
	if len(skipped) > 0 {
		warning = skippedUnreadableWarning(skipped, 0)
	}
	if exitCode != 1 {
		return nil, warning
	}
	if len(rawFiles) == 0 && isPermissionOnlyError(diagnosticText) {
		return &SearchResult{Warning: warning + " " + permissionSearchHint, SkippedPaths: skipped}, ""
	}
	if len(rawFiles) == 0 {
		if order == "modified" {
			return &SearchResult{Error: findModifiedOrderError}, ""
		}
		detail := "."
		if len(diagnostics) > 0 {
			detail = ": " + diagnostics[0]
		}
		return &SearchResult{Error: "File search failed while running bounded find traversal" + detail}, ""
	}
	for _, d := range diagnostics {
		if other := pyStrip(d); other != "" && !permissionDeniedRe.MatchString(other) {
			msg := "Some paths could not be read: " + other + ". Results may be incomplete."
			if warning != "" {
				msg += " " + warning
			}
			return nil, msg
		}
	}
	if warning != "" {
		return nil, warning
	}
	return nil, "find traversal skipped one or more unreadable paths (restricted " +
		"permissions); results are partial."
}

// One protected folder to prune, the root it hangs off, its root relative and absolute forms
type searchExclusion struct {
	root, relative, absolute string
}

// Protected descendants to prune below a root, only on a backend running on the controller host
func (s *shellFileOps) macosSearchExclusions(path string) []string {
	if il, ok := s.terminalEnv.(interface{ IsLocal() bool }); ok && !il.IsLocal() {
		return nil
	}
	return macosProtectedSearchExclusions(path, s.liveCwd(), searchHome, searchHostPlatform)
}

// Protected folder paths joined onto the root for find path pruning
func (s *shellFileOps) protectedPrunePaths(path string) []string {
	var out []string
	for _, item := range s.macosSearchExclusions(path) {
		out = append(out, hostNormpath(pathJoinOS(path, item)))
	}
	return out
}

// Unique protected exclusions across the roots, never pruning a root the caller chose itself
func (s *shellFileOps) effectiveMacosSearchExclusions(roots []string) []searchExclusion {
	cwd := s.liveCwd()
	usePosix := posixRoots(roots)
	normalized := func(root string) string {
		if usePosix {
			if !strings.HasPrefix(root, "/") {
				root = posixJoin(cwd, root)
			}
			return posixNormpath(root)
		}
		return guardNormcasePath(hostAbspath(hostNormpath(root)))
	}
	normalizedRoots := make([]string, len(roots))
	explicit := map[string]bool{}
	for i, root := range roots {
		normalizedRoots[i] = normalized(root)
		explicit[normalizedRoots[i]] = true
	}
	seen := map[string]bool{}
	var effective []searchExclusion
	for i, root := range roots {
		for _, relative := range s.macosSearchExclusions(root) {
			var absolute, key string
			if usePosix {
				absolute = posixNormpath(posixJoin(normalizedRoots[i], relative))
				key = absolute
			} else {
				absolute = hostNormpath(pathJoinOS(root, relative))
				key = guardNormcasePath(hostAbspath(absolute))
			}
			if explicit[key] || seen[key] {
				continue
			}
			seen[key] = true
			effective = append(effective, searchExclusion{root, relative, absolute})
		}
	}
	return effective
}

// Warning naming the protected folders a broad search skipped
func macosProtectedSearchWarning(paths []string) string {
	names := make([]string, len(paths))
	for i, item := range paths {
		names[i] = pathBasenameOS(item)
	}
	return "Skipped macOS protected folders during broad search to avoid " +
		"an unattended privacy prompt: " + strings.Join(names, ", ") + ". Search a protected " +
		"folder directly when access is intentional."
}

// find clause pruning hidden directories while keeping each explicitly selected start point
func hiddenPruneExpr(qRoots []string) string {
	var exemptions strings.Builder
	for _, root := range qRoots {
		exemptions.WriteString(" ! -path " + root)
	}
	return `\( -type d -name '.*'` + exemptions.String() + ` \) -prune`
}

// find clause pruning the protected folder paths
func (s *shellFileOps) pruneExpr(protectedPaths []string) string {
	terms := make([]string, len(protectedPaths))
	for i, item := range protectedPaths {
		terms[i] = "-path " + s.escapeShellArg(item, true)
	}
	return `\( ` + strings.Join(terms, " -o ") + ` \) -prune`
}

// True when the search root or any of its ancestors is dot named
func (s *shellFileOps) rootUnderHiddenDir(path string) bool {
	if path == "" {
		path = "."
	}
	root := normalizedFilenameSearchRoot(s.terminalEnv, path, s.cwd)
	for _, part := range strings.Split(strings.ReplaceAll(root, `\`, "/"), "/") {
		if strings.HasPrefix(part, ".") && part != "." && part != ".." {
			return true
		}
	}
	return false
}

// Existence probe whose stdout holds exists or not_found, natively on the host lane
func (s *shellFileOps) pathExistsProbe(path string) *ExecuteResult {
	if s.nativeReadEnabled() {
		if _, err := os.Stat(s.nativePath(path)); err == nil {
			return &ExecuteResult{Stdout: "exists"}
		}
		return &ExecuteResult{Stdout: "not_found"}
	}
	return s.exec("test -e "+s.escapeShellArg(path, true)+" && echo exists || echo not_found", "", 0)
}

// Whether a no rg local root is the filesystem root, the home, or a home ancestor, links resolved
func (s *shellFileOps) isBroadLocalSearchRoot(path string) bool {
	if !EnvUsesHostFilesystem(s.terminalEnv) {
		return false
	}
	normalized := func(value string) string {
		if isWindowsHost {
			value = strings.ReplaceAll(msysToWindowsPath(value), `\`, "/")
		}
		if !filepath.IsAbs(value) {
			value = pathJoinOS(s.liveCwd(), value)
		}
		return guardNormcasePath(hostRealpath(value))
	}
	root := normalized(path)
	home := normalized(searchHome)
	drive := ""
	if isWindowsHost {
		drive, _, _ = ntSplitRoot(root)
	}
	anchor := hostAbspath(string(os.PathSeparator))
	if drive != "" {
		anchor = drive + string(os.PathSeparator)
	}
	if root == guardNormcasePath(anchor) {
		return true
	}
	common, ok := hostCommonpath(root, home)
	if !ok {
		return false
	}
	return root == home || common == root
}

// Drops hidden descendants of the root while keeping an explicitly chosen hidden root
func (s *shellFileOps) filterHiddenDescendants(paths []string, root string) []string {
	normalizedRoot := hostPathParts(hostRealpath(root), isWindowsHost)
	var filtered []string
	for _, itemPath := range paths {
		real := hostRealpath(itemPath)
		resolved := hostPathParts(real, isWindowsHost)
		var relParts []string
		if len(normalizedRoot) <= len(resolved) && slices.Equal(resolved[:len(normalizedRoot)], normalizedRoot) {
			relParts = hostPathParts(real, false)[len(normalizedRoot):]
		} else {
			relParts = hostPathParts(itemPath, false)
		}
		hidden := false
		for _, part := range relParts {
			if part != "." && part != ".." && strings.HasPrefix(part, ".") {
				hidden = true
				break
			}
		}
		if !hidden {
			filtered = append(filtered, itemPath)
		}
	}
	return filtered
}

// Drops paths git ignores when the root sits inside a git worktree
func (s *shellFileOps) filterGitignoredPaths(paths []string, root string) []string {
	if len(paths) == 0 {
		return paths
	}
	escapedRoot := s.escapeShellArg(root, true)
	probe := s.exec("git -C "+escapedRoot+" rev-parse --is-inside-work-tree >/dev/null 2>&1", "", 10*time.Second)
	if probe.ExitCode != 0 {
		return paths
	}
	stdin := strings.Join(paths, "\n")
	result := s.execStdin("git -C "+escapedRoot+" check-ignore --stdin", "", 10*time.Second, &stdin)
	ignored := setOf(pySplitLines(result.Stdout)...)
	var out []string
	for _, p := range paths {
		if !ignored[p] {
			out = append(out, p)
		}
	}
	return out
}

// Paths ordered newest first by backend stat times, vanished paths last in their prior order
func (s *shellFileOps) sortPathsByMtime(paths []string) []string {
	if len(paths) < 2 {
		return paths
	}
	script := `while IFS= read -r item || [ -n "$item" ]; do ` +
		`mtime="$(stat -c%Y "$item" 2>/dev/null ` +
		`|| stat -f%m "$item" 2>/dev/null || true)"; ` +
		`printf '%s\n' "${mtime:-0}"; ` +
		`done`
	stdin := strings.Join(paths, "\n")
	result := s.execStdin(script, "", 60*time.Second, &stdin)
	lines := pySplitLines(result.Stdout)
	mtimes := make([]float64, len(paths))
	for i := range paths {
		if i < len(lines) {
			if f, ok := pyFloatText(lines[i]); ok {
				mtimes[i] = f
			}
		}
	}
	idx := make([]int, len(paths))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return mtimes[idx[a]] > mtimes[idx[b]] })
	out := make([]string, len(paths))
	for i, j := range idx {
		out[i] = paths[j]
	}
	return out
}

// Matching descendant directories through find so empty directories stay discoverable
func (s *shellFileOps) searchDirectoriesWithFind(pattern, path string, fetchLimit int) ([]string, string) {
	if fetchLimit <= 0 || !s.hasCommand("find") {
		return nil, ""
	}
	escapedPath := s.escapeShellArg(path, true)
	escapedPattern := s.escapeShellArg(pattern, true)
	hiddenPrune := hiddenPruneExpr([]string{escapedPath})
	cmd := "find -H " + escapedPath + " -mindepth 1 " + hiddenPrune + " -o -type d " +
		"! -name '.*' -name " + escapedPattern + " -printf '%T@ %p\\n' " +
		"2>/dev/null | sort -rn"
	result := s.exec(cmd, "", 30*time.Second)
	stdout, limitReason := searchStdoutAndLimit(result)
	if pyStrip(stdout) == "" && limitReason == "" {
		cmdSimple := "find -H " + escapedPath + " -mindepth 1 " + hiddenPrune + " -o -type d " +
			"! -name '.*' -name " + escapedPattern + " 2>/dev/null"
		result = s.exec(cmdSimple, "", 30*time.Second)
		stdout, limitReason = searchStdoutAndLimit(result)
	}
	var dirs []string
	for _, line := range strings.Split(pyStrip(stdout), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 && pyIsDigitText(strings.ReplaceAll(parts[0], ".", "")) {
			dirs = append(dirs, parts[1])
		} else {
			dirs = append(dirs, line)
		}
	}
	if len(dirs) > 0 {
		if s.rootUnderHiddenDir(path) {
			dirs = s.filterHiddenDescendants(dirs, path)
		}
		dirs = s.filterGitignoredPaths(dirs, path)
	}
	return sliceWindow(s.sortPathsByMtime(dirs), 0, fetchLimit), limitReason
}

// Filename glob search across roots through rg files mode, else a bounded find
func (s *shellFileOps) searchFiles(pattern string, roots []string, rootList bool, limit, offset int, order string) *SearchResult {
	searchPattern := pattern
	if strings.HasPrefix(pattern, "**/") || strings.Contains(pattern, "/") {
		parts := strings.Split(pattern, "/")
		searchPattern = parts[len(parts)-1]
	}
	if len(roots) == 0 {
		return &SearchResult{Error: "File search requires at least one search root in 'path'."}
	}
	if s.hasCommand("rg") {
		rgExecutable := s.resolveCommand("rg")
		if rgExecutable == "" {
			rgExecutable = "rg"
		}
		if order == "modified" {
			if capabilityError := s.modifiedRgCapabilityError(rgExecutable); capabilityError != "" {
				return &SearchResult{Error: capabilityError}
			}
		}
		keys := filenameSearchRootKeys(s.terminalEnv, roots, s.cwd)
		if !s.acquireFilenameSearchRoots(keys) {
			return &SearchResult{Error: admissionInterruptedError}
		}
		defer releaseFilenameSearchRoots(keys)
		return s.searchFilesRg(searchPattern, roots, rootList, limit, offset, order, rgExecutable)
	}
	for _, root := range roots {
		if s.isBroadLocalSearchRoot(root) {
			return &SearchResult{Error: "Broad local file search without ripgrep is disabled because " +
				"find cannot keep this traversal safely bounded. Install " +
				"ripgrep or search a narrower directory."}
		}
	}
	if !s.hasCommand("find") {
		return &SearchResult{Error: "File search requires 'rg' (ripgrep) or 'find'. " +
			"Install ripgrep for best results: " +
			"https://github.com/BurntSushi/ripgrep#installation"}
	}
	findRoots := make([]string, len(roots))
	qRoots := make([]string, len(roots))
	for i, root := range roots {
		if strings.HasPrefix(root, "-") {
			root = "./" + root
		}
		findRoots[i] = root
		qRoots[i] = s.escapeShellArg(root, true)
	}
	hiddenPrune := " " + hiddenPruneExpr(qRoots) + " -o"
	var protectedPaths []string
	for _, ex := range s.effectiveMacosSearchExclusions(roots) {
		protectedPaths = append(protectedPaths, ex.absolute)
	}
	protectedPrune := ""
	if len(protectedPaths) > 0 {
		protectedPrune = " " + s.pruneExpr(protectedPaths) + " -o"
	}
	fetchLimit := offset + limit + 1
	var rootExemptions strings.Builder
	for _, root := range findLiteralPathExpressions(findRoots) {
		rootExemptions.WriteString(" ! -path " + s.escapeShellArg(root, true))
	}
	base := "find -H " + strings.Join(qRoots, " ") + protectedPrune + hiddenPrune + " " +
		`\( -type f -o \( -type d` + rootExemptions.String() + ` \) \) ` +
		"! -name '.*' -name " + s.escapeShellArg(searchPattern, true)
	var cmd string
	if order == "modified" {
		cmd = "set -o pipefail; " + base + " -printf '%T@ %p\\n' | sort -rn | head -n " + strconv.Itoa(fetchLimit)
	} else {
		cmd = "set -o pipefail; " + base + " -print | head -n " + strconv.Itoa(fetchLimit)
	}
	keys := filenameSearchRootKeys(s.terminalEnv, roots, s.cwd)
	if !s.acquireFilenameSearchRoots(keys) {
		return &SearchResult{Error: admissionInterruptedError}
	}
	result := func() *ExecuteResult {
		defer releaseFilenameSearchRoots(keys)
		return s.exec(cmd, "", 60*time.Second)
	}()
	stdout, limitReason := searchStdoutAndLimit(result)
	rawFiles, diagnostics := splitFindOutput(stdout, order)
	failure, partialWarning := findExitVerdict(result.ExitCode, rawFiles, diagnostics, fetchLimit, order)
	if failure != nil {
		return failure
	}
	if isWindowsHost && EnvUsesHostFilesystem(s.terminalEnv) {
		for i, f := range rawFiles {
			rawFiles[i] = msysToWindowsPath(f)
		}
	}
	return &SearchResult{Files: sliceWindow(rawFiles, offset, limit), TotalCount: len(rawFiles),
		Truncated: len(rawFiles) > offset+limit || limitReason != "", LimitReason: limitReason,
		Warning: partialWarning, SkippedPaths: extractUnreadablePaths(strings.Join(diagnostics, "\n"))}
}

// grep words in fixed order, the head, context, include glob, mode flag and the quoted pattern
func (s *shellFileOps) grepCmd(head []string, pattern, outputMode string, context int, fileGlob string) []string {
	parts := append([]string{}, head...)
	if context > 0 {
		parts = append(parts, "-C", strconv.Itoa(context))
	}
	if fileGlob != "" {
		parts = append(parts, "--include", s.escapeShellArg(fileGlob, true))
	}
	if flag, ok := outputModeFlags[outputMode]; ok {
		parts = append(parts, flag)
	}
	return append(parts, s.escapeShellArg(pattern, false))
}

// Recursive grep fallback skipping hidden directories, pruned through find when the root needs it
func (s *shellFileOps) searchWithGrep(pattern, path, fileGlob string, limit, offset int, outputMode string, context int) *SearchResult {
	protectedPaths := s.protectedPrunePaths(path)
	if len(protectedPaths) > 0 || s.rootUnderHiddenDir(path) {
		return s.searchWithGrepPruned(pattern, path, fileGlob, limit, offset, outputMode, context, protectedPaths)
	}
	cmdParts := s.grepCmd([]string{"grep", "-rnHE", "--exclude-dir='.*'"}, pattern, outputMode, context, fileGlob)
	var searchRoot string
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\\`) || windowsDriveRootRe.MatchString(path) {
		searchRoot = s.escapeShellArg(path, true)
	} else {
		relativePath := strings.TrimPrefix(path, "./")
		searchRoot = `"$PWD"`
		if relativePath != "" && relativePath != "." {
			searchRoot += "/" + s.escapeShellArg(relativePath, true)
		}
	}
	cmdParts = append(cmdParts, searchRoot)
	return s.runSearchPipeline(cmdParts, outputMode, limit, offset, context, "", true)
}

// grep through find with path scoped pruning for protected folders and roots under dot directories
func (s *shellFileOps) searchWithGrepPruned(pattern, path, fileGlob string, limit, offset int, outputMode string, context int, protectedPaths []string) *SearchResult {
	grepParts := s.grepCmd([]string{"grep", "-nHE"}, pattern, outputMode, context, "")
	root := path
	if root == "" {
		root = "."
	}
	qRoot := s.escapeShellArg(root, true)
	findParts := []string{"find", "-H", qRoot}
	if len(protectedPaths) > 0 {
		findParts = append(findParts, s.pruneExpr(protectedPaths), "-o")
	}
	findParts = append(findParts, hiddenPruneExpr([]string{qRoot}), "-o", "-type f")
	if fileGlob != "" {
		findParts = append(findParts, "-name", s.escapeShellArg(fileGlob, true))
	}
	findParts = append(findParts, "-exec")
	findParts = append(findParts, grepParts...)
	findParts = append(findParts, "{}", "+")
	return s.runSearchPipeline(findParts, outputMode, limit, offset, context, "", true)
}
