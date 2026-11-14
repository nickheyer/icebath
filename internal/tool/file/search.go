package file

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/proc"
	"github.com/nickheyer/icebath/internal/shell"
	"github.com/nickheyer/icebath/internal/tool/env"
)

const (
	searchTimeout      = 60 * time.Second
	probeTimeout       = 30 * time.Second
	versionTimeout     = 10 * time.Second
	gitTimeout         = 10 * time.Second
	admissionPoll      = 50 * time.Millisecond
	ripgrepInstallHint = "https://github.com/BurntSushi/ripgrep#installation"
)

type SearchOptions struct {
	Admission *Admission
	Ripgrep   func(ctx context.Context) (string, error)
	RunEnv    func(ctx context.Context) ([]string, error)
}

type admissionKey struct {
	kind, root string
}

type Admission struct {
	mu     sync.Mutex
	active map[admissionKey]struct{}
	wake   chan struct{}
}

func NewAdmission() *Admission {
	return &Admission{active: make(map[admissionKey]struct{}), wake: make(chan struct{})}
}

func (a *Admission) acquire(ctx context.Context, keys []admissionKey) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		a.mu.Lock()
		busy := false
		for _, k := range keys {
			if _, held := a.active[k]; held {
				busy = true
				break
			}
		}
		if !busy {
			for _, k := range keys {
				a.active[k] = struct{}{}
			}
			a.mu.Unlock()
			return true
		}
		wake := a.wake
		a.mu.Unlock()
		timer := time.NewTimer(admissionPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (a *Admission) release(keys []admissionKey) {
	a.mu.Lock()
	for _, k := range keys {
		delete(a.active, k)
	}
	close(a.wake)
	a.wake = make(chan struct{})
	a.mu.Unlock()
}

var macProtectedDirs = [...]string{"Desktop", "Documents", "Downloads", "Library", "Movies", "Music", "Pictures"}

func macProtectedExclusions(root, cwd, userHome string) []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	root = home.ExpandUser(root)
	if !strings.HasPrefix(root, "/") {
		root = cwd + "/" + root
	}
	root = path.Clean(root)
	homePath := path.Clean(home.ExpandUser(userHome))
	var out []string
	for _, name := range macProtectedDirs {
		full := path.Join(homePath, name)
		switch {
		case root == "/":
			out = append(out, strings.TrimPrefix(full, "/"))
		case strings.HasPrefix(full, root+"/"):
			out = append(out, full[len(root)+1:])
		}
	}
	return out
}

func (o *Operations) controllerHome() string {
	return home.ExpandUser("~")
}

func (o *Operations) normalizedSearchRoot(root string) string {
	cwd := o.liveCwd()
	if o.env.HostFilesystem() {
		if runtime.GOOS == "windows" {
			root, cwd = env.MsysToWindowsPath(root), env.MsysToWindowsPath(cwd)
		}
		if !filepath.IsAbs(root) {
			root = filepath.Join(cwd, root)
		}
		abs, err := filepath.Abs(filepath.Clean(root))
		if err != nil {
			abs = filepath.Clean(root)
		}
		if runtime.GOOS == "windows" {
			abs = strings.ToLower(abs)
		}
		return abs
	}
	if !strings.HasPrefix(root, "/") {
		root = pathStyle{}.join(cwd, root)
	}
	return path.Clean(root)
}

func (o *Operations) admissionKeys(roots []string) []admissionKey {
	seen := make(map[admissionKey]struct{})
	var keys []admissionKey
	for _, root := range roots {
		k := admissionKey{kind: o.env.Kind(), root: o.normalizedSearchRoot(root)}
		if _, dup := seen[k]; !dup {
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].root < keys[j].root })
	return keys
}

func (o *Operations) resolveRipgrep(ctx context.Context) (string, error) {
	o.mu.Lock()
	cached := o.ripgrep
	o.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	remember := func(resolved string) (string, error) {
		o.mu.Lock()
		o.ripgrep = resolved
		o.mu.Unlock()
		return resolved, nil
	}
	onHost := o.env.HostFilesystem()
	if onHost {
		pinned, err := o.search.Ripgrep(ctx)
		if err != nil {
			return "", err
		}
		if pinned != "" {
			return remember(strings.ReplaceAll(pinned, `\`, "/"))
		}
	}
	res, err := o.exec(ctx, "command -v rg 2>/dev/null", nil)
	if err != nil {
		return "", err
	}
	if found := trimSpace(res.stdout); res.exitCode == 0 && found != "" {
		return remember(splitLines(found, false)[0])
	}
	if runtime.GOOS == "windows" && onHost {
		profile := os.Getenv("USERPROFILE")
		if profile == "" {
			profile = o.controllerHome()
		}
		scoop := os.Getenv("SCOOP")
		if scoop == "" {
			scoop = filepath.Join(profile, "scoop")
		}
		candidates := []string{
			filepath.Join(profile, ".cargo", "bin", "rg.exe"),
			filepath.Join(scoop, "shims", "rg.exe"),
		}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			candidates = append(candidates, filepath.Join(local, "Microsoft", "WinGet", "Links", "rg.exe"))
		}
		for _, candidate := range candidates {
			if fi, err := os.Stat(candidate); err == nil && fi.Mode().IsRegular() {
				return remember(strings.ReplaceAll(candidate, `\`, "/"))
			}
		}
	}
	return "", nil
}

func (o *Operations) quoteExecutable(executable string) string {
	if plainExecutable.MatchString(executable) {
		return executable
	}
	if o.env.HostFilesystem() {
		return nativeToolArg(executable)
	}
	return shellQuote(executable)
}

func (o *Operations) modifiedOrderError(ctx context.Context, executable string) (string, error) {
	o.mu.Lock()
	cached, known := o.modified[executable]
	o.mu.Unlock()
	if known {
		return cached, nil
	}
	res, err := o.execIn(ctx, o.quoteExecutable(executable)+" --version", "", versionTimeout, nil)
	if err != nil {
		return "", err
	}
	problem := ripgrepModifiedOrderError
	if m := ripgrepVersion.FindStringSubmatch(res.stdout); res.exitCode == 0 && m != nil {
		if major, convErr := strconv.Atoi(m[1]); convErr == nil && major >= 14 {
			problem = ""
		}
	}
	o.mu.Lock()
	o.modified[executable] = problem
	o.mu.Unlock()
	return problem, nil
}

type cappedStderr struct {
	kept      bytes.Buffer
	counted   int
	omitted   int
	keptOther bool
}

func (c *cappedStderr) drain(r *bufio.Reader) {
	for {
		raw, err := r.ReadBytes('\n')
		if len(raw) > 0 {
			if c.kept.Len() > 0 && c.counted+len(raw) > stderrRetainBytes {
				line := trimSpace(decodeLossy(raw))
				if !c.keptOther && line != "" && !permissionDenied.MatchString(line) {
					c.kept.Write(raw)
					c.keptOther = true
				} else {
					c.omitted++
				}
			} else {
				c.kept.Write(raw)
				c.counted += len(raw)
			}
		}
		if err != nil {
			return
		}
	}
}

func (o *Operations) runRipgrepNative(ctx context.Context, words []string, fetchLimit int, timeout time.Duration, mergeStderr bool) (execResult, error) {
	args, err := shell.Split(strings.Join(words, " "))
	if err != nil || len(args) == 0 {
		return execResult{stdout: fmt.Sprintf("rg: %v", err), exitCode: 2}, nil
	}
	environ, err := o.search.RunEnv(ctx)
	if err != nil {
		return execResult{}, err
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = o.liveCwd()
	cmd.Env = environ
	proc.NewGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return execResult{stdout: "rg: " + err.Error(), exitCode: 2}, nil
	}
	var stderr *cappedStderr
	var stderrPipe io.ReadCloser
	if mergeStderr {
		stderrPipe, err = cmd.StderrPipe()
		if err != nil {
			return execResult{stdout: "rg: " + err.Error(), exitCode: 2}, nil
		}
		stderr = &cappedStderr{}
	}
	if err := cmd.Start(); err != nil {
		return execResult{stdout: "rg: " + err.Error(), exitCode: 2}, nil
	}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		if stderr != nil {
			stderr.drain(bufio.NewReader(stderrPipe))
		}
	}()
	identity, identifyErr := proc.Identify(cmd.Process.Pid)
	kill := func() {
		if identifyErr != nil {
			_ = cmd.Process.Kill()
			return
		}
		if identity.Alive() {
			_ = proc.KillTree(context.WithoutCancel(ctx), identity, 0)
		}
	}

	var lines bytes.Buffer
	bounded := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		reader := bufio.NewReader(stdout)
		count := 0
		for {
			raw, err := reader.ReadBytes('\n')
			if len(raw) > 0 {
				lines.Write(raw)
				count++
				if count >= fetchLimit {
					close(bounded)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	status, hitBound := 0, false
	select {
	case <-bounded:
		hitBound = true
	case <-drained:
		select {
		case <-bounded:
			hitBound = true
		default:
		}
	case <-ctx.Done():
		status = 130
	case <-deadline.C:
		status = 124
	}
	natural := status == 0 && !hitBound
	if !natural {
		kill()
	}
	exited := make(chan struct{})
	go func() {
		<-drained
		<-stderrDone
		_ = cmd.Wait()
		close(exited)
	}()
	if natural {
		select {
		case <-exited:
		case <-ctx.Done():
			status = 130
			kill()
		case <-deadline.C:
			status = 124
			kill()
		}
	}
	<-exited
	out := decodeLossy(lines.Bytes())
	if stderr != nil {
		errText := decodeLossy(stderr.kept.Bytes())
		if stderr.omitted > 0 {
			errText = strings.TrimRight(errText, "\n") + "\n" + omittedDiagnosticsLine(stderr.omitted) + "\n"
		}
		out = errText + out
	}
	switch {
	case status == 124:
		return execResult{stdout: out + fmt.Sprintf("\n[Command timed out after %ds]", int(timeout/time.Second)), exitCode: 124}, nil
	case status == 130:
		return execResult{stdout: out + "\n[Command interrupted]", exitCode: 130}, nil
	case hitBound:
		return execResult{stdout: out}, nil
	}
	return execResult{stdout: out, exitCode: proc.ExitOf(cmd.ProcessState).Status()}, nil
}

func (o *Operations) runRipgrepBounded(ctx context.Context, words []string, fetchLimit int, timeout time.Duration, mergeStderr, nativeOK bool, shellPrefix string) (execResult, error) {
	if nativeOK && o.nativeEnabled() {
		return o.runRipgrepNative(ctx, words, fetchLimit, timeout, mergeStderr)
	}
	stderr := " 2>/dev/null"
	if mergeStderr {
		stderr = ""
	}
	return o.execIn(ctx, shellPrefix+strings.Join(words, " ")+stderr+" | head -n "+strconv.Itoa(fetchLimit), "", timeout, nil)
}

func (o *Operations) macSearchExclusions(root string) []string {
	if !o.env.Local() {
		return nil
	}
	return macProtectedExclusions(root, o.liveCwd(), o.controllerHome())
}

func (o *Operations) protectedPrunePaths(root string) []string {
	var out []string
	for _, item := range o.macSearchExclusions(root) {
		out = append(out, path.Clean(pathStyle{}.join(root, item)))
	}
	return out
}

func posixRoots(roots []string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	for _, root := range roots {
		if drivePath.MatchString(root) || strings.Contains(root, `\`) {
			return false
		}
	}
	return true
}

type macExclusion struct {
	root, relative, absolute string
}

func (o *Operations) effectiveMacExclusions(roots []string) []macExclusion {
	if runtime.GOOS != "darwin" {
		return nil
	}
	cwd := o.liveCwd()
	posix := posixRoots(roots)
	hostAbsolute := func(p string) string {
		abs, err := filepath.Abs(filepath.Clean(p))
		if err != nil {
			return filepath.Clean(p)
		}
		return abs
	}
	normalize := func(root string) string {
		switch {
		case !posix:
			return hostAbsolute(root)
		case strings.HasPrefix(root, "/"):
			return path.Clean(root)
		}
		return path.Clean(pathStyle{}.join(cwd, root))
	}
	explicit := make(map[string]struct{})
	normalized := make([]string, len(roots))
	for i, root := range roots {
		normalized[i] = normalize(root)
		explicit[normalized[i]] = struct{}{}
	}
	seen := make(map[string]struct{})
	var out []macExclusion
	for i, root := range roots {
		for _, relative := range o.macSearchExclusions(root) {
			absolute := path.Clean(pathStyle{}.join(normalized[i], relative))
			key := absolute
			if !posix {
				absolute = filepath.Clean(filepath.Join(root, relative))
				key = hostAbsolute(absolute)
			}
			if _, chosen := explicit[key]; chosen {
				continue
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, macExclusion{root: root, relative: relative, absolute: absolute})
		}
	}
	return out
}

func hiddenPruneExpr(quotedRoots []string) string {
	var exemptions strings.Builder
	for _, root := range quotedRoots {
		exemptions.WriteString(" ! -path " + root)
	}
	return `\( -type d -name '.*'` + exemptions.String() + ` \) -prune`
}

func pruneExpr(protected []string) string {
	terms := make([]string, len(protected))
	for i, item := range protected {
		terms[i] = "-path " + shellArg(item)
	}
	return `\( ` + strings.Join(terms, " -o ") + ` \) -prune`
}

func (o *Operations) rootUnderHiddenDir(root string) bool {
	if root == "" {
		root = "."
	}
	for _, part := range strings.Split(strings.ReplaceAll(o.normalizedSearchRoot(root), `\`, "/"), "/") {
		if strings.HasPrefix(part, ".") && part != "." && part != ".." {
			return true
		}
	}
	return false
}

func (o *Operations) ripgrepExclusionGlobs(root string) []string {
	var out []string
	for _, item := range o.macSearchExclusions(root) {
		out = append(out, "--glob", shellArg("!"+item+"/**"))
	}
	return out
}

func (o *Operations) pathExistsProbe(ctx context.Context, target string) (execResult, error) {
	if o.nativeEnabled() {
		if _, err := os.Stat(o.nativePath(target)); err == nil {
			return execResult{stdout: "exists"}, nil
		}
		return execResult{stdout: "not_found"}, nil
	}
	return o.exec(ctx, "test -e "+shellArg(target)+" && echo exists || echo not_found", nil)
}

func (o *Operations) pathNotFound(ctx context.Context, target string) (SearchResult, error) {
	style := o.paths()
	parent := style.dir(target)
	if parent == "" {
		parent = "."
	}
	query := style.base(target)
	hints := []string{"Path not found: " + target}
	check, err := o.exec(ctx, "test -d "+shellArg(parent)+" && echo yes || echo no", nil)
	if err != nil {
		return SearchResult{}, err
	}
	if strings.Contains(check.stdout, "yes") && query != "" {
		listing, err := o.exec(ctx, "ls -1 "+shellArg(parent)+" 2>/dev/null | head -20", nil)
		if err != nil {
			return SearchResult{}, err
		}
		if listing.exitCode == 0 && !isBlank(listing.stdout) {
			lq := strings.ToLower(query)
			var candidates []string
			for _, e := range strings.Split(trimSpace(listing.stdout), "\n") {
				le := strings.ToLower(e)
				if e != "" && (strings.Contains(le, lq) || strings.Contains(lq, le) || strings.HasPrefix(le, charPrefix(lq, 3))) {
					candidates = append(candidates, style.join(parent, e))
				}
			}
			if len(candidates) > 5 {
				candidates = candidates[:5]
			}
			if len(candidates) > 0 {
				hints = append(hints, "Similar paths: "+strings.Join(candidates, ", "))
			}
		}
	}
	return SearchResult{Error: strings.Join(hints, ". ")}, nil
}

type searchRequest struct {
	pattern, target, fileGlob, outputMode, order string
	limit, offset, context                       int
}

func (o *Operations) multiPathSearch(ctx context.Context, q searchRequest, target string) (SearchResult, bool, error) {
	var parts []string
	if strings.Contains(target, ",") {
		for _, part := range strings.Split(target, ",") {
			if s := trimSpace(part); s != "" {
				parts = append(parts, s)
			}
		}
	} else {
		parts = spaceFields(target)
	}
	if len(parts) < 2 {
		return SearchResult{}, false, nil
	}
	var existing, missing []string
	for _, p := range parts {
		expanded, err := o.expandPath(ctx, p)
		if err != nil {
			return SearchResult{}, false, err
		}
		probe, err := o.pathExistsProbe(ctx, expanded)
		if err != nil {
			return SearchResult{}, false, err
		}
		if strings.Contains(probe.stdout, "exists") {
			existing = append(existing, expanded)
		} else {
			missing = append(missing, expanded)
		}
	}
	if len(existing) == 0 {
		return SearchResult{}, false, nil
	}
	var merged SearchResult
	var children []SearchResult
	if q.target == "files" {
		found, err := o.searchFiles(ctx, q.pattern, existing, false, q.limit, q.offset, q.order)
		if err != nil {
			return SearchResult{}, false, err
		}
		merged, children = found, []SearchResult{found}
	} else {
		index := make(map[string]int)
		for _, root := range existing {
			sub, err := o.searchContent(ctx, q, root)
			if err != nil {
				return SearchResult{}, false, err
			}
			if sub.Error != "" {
				return sub, true, nil
			}
			children = append(children, sub)
			merged.Matches = append(merged.Matches, sub.Matches...)
			merged.Files = append(merged.Files, sub.Files...)
			for _, c := range sub.Counts {
				if i, seen := index[c.Path]; seen {
					merged.Counts[i].Count = c.Count
					continue
				}
				index[c.Path] = len(merged.Counts)
				merged.Counts = append(merged.Counts, c)
			}
			merged.TotalCount += sub.TotalCount
			merged.Truncated = merged.Truncated || sub.Truncated
		}
		merged.Matches = page(merged.Matches, 0, q.limit)
		merged.Files = page(merged.Files, 0, q.limit)
	}
	note := fmt.Sprintf("path contained %d entries; searched %d that exist", len(parts), len(existing))
	if len(missing) > 0 {
		note += "; skipped missing: " + strings.Join(page(missing, 0, 3), ", ")
		if len(missing) > 3 {
			note += fmt.Sprintf(" (+%d more)", len(missing)-3)
		}
	}
	hasResults := merged.TotalCount != 0 || len(merged.Matches) > 0 || len(merged.Files) > 0 || len(merged.Counts) > 0
	warnings := append(mergeChildWarnings(children, hasResults), note)
	if merged.Error == "" {
		var protected []string
		for _, e := range o.effectiveMacExclusions(existing) {
			protected = append(protected, e.absolute)
		}
		if len(protected) > 0 {
			warnings = append(warnings, macProtectedWarning(protected, o.paths()))
		}
	}
	merged.Warning = strings.Join(warnings, " ")
	return merged, true, nil
}

func (o *Operations) prunedGlobArgs() string {
	var globs []string
	for _, dir := range PrunedDirNames() {
		for _, prefix := range [...]string{"", "**/"} {
			globs = append(globs, "--glob", shellArg("!"+prefix+dir+"/**"))
		}
	}
	return strings.Join(globs, " ")
}

type zeroMatchProbe struct {
	flags, template string
}

var zeroMatchProbes = [...]zeroMatchProbe{
	{"-i", "0 exact matches, but %d case-insensitive match(es) in %d file(s): %s — the pattern's casing may be wrong."},
	{"--hidden --no-ignore", "0 matches in visible files, but %d match(es) in %d hidden or gitignored file(s): %s — these are excluded by default."},
	{"-F", "0 regex matches, but %d literal match(es) in %d file(s): %s — the pattern contains regex metacharacters that likely need escaping (or pass a simpler substring)."},
}

func (o *Operations) zeroMatchHint(ctx context.Context, pattern, root, fileGlob string) (string, error) {
	executable, err := o.resolveRipgrep(ctx)
	if err != nil || executable == "" {
		return "", err
	}
	rg := o.quoteExecutable(executable)
	hasMeta := regexMeta.MatchString(pattern)
	globExpr := ""
	if fileGlob != "" {
		globExpr = " --glob " + shellArg(fileGlob)
	}
	for _, probe := range zeroMatchProbes {
		if probe.flags == "-F" && !hasMeta {
			continue
		}
		probeGlob := globExpr
		if strings.HasPrefix(probe.flags, "--hidden") {
			probeGlob = globExpr + " " + o.prunedGlobArgs()
		}
		words := []string{rg, probe.flags, "--count-matches", probeGlob, o.shellText(pattern), nativeToolArg(root)}
		res, err := o.runRipgrepBounded(ctx, words, 50, probeTimeout, false, true, "")
		if err != nil {
			return "", err
		}
		total := 0
		var perFile []string
		for _, line := range splitLines(trimSpace(res.stdout), false) {
			name, digits := "", line
			if cut := strings.LastIndex(line, ":"); cut >= 0 {
				name, digits = line[:cut], line[cut+1:]
			}
			if !allDigits(digits) {
				continue
			}
			n, _ := strconv.Atoi(digits)
			total += n
			perFile = append(perFile, name)
		}
		if total > 0 {
			shown := strings.Join(page(perFile, 0, 5), ", ")
			if extra := len(perFile) - 5; extra > 0 {
				shown += fmt.Sprintf(" (+%d more)", extra)
			}
			return fmt.Sprintf(probe.template, total, len(perFile), shown), nil
		}
	}
	return "", nil
}

func (o *Operations) broadLocalRoot(root string) bool {
	if !o.env.HostFilesystem() {
		return false
	}
	normalize := func(value string) string {
		if runtime.GOOS == "windows" {
			value = strings.ReplaceAll(env.MsysToWindowsPath(value), `\`, "/")
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(o.liveCwd(), value)
		}
		resolved := realPath(value)
		if runtime.GOOS == "windows" {
			resolved = strings.ToLower(resolved)
		}
		return resolved
	}
	resolvedRoot := normalize(root)
	userHome := normalize(o.controllerHome())
	anchor := string(filepath.Separator)
	if volume := filepath.VolumeName(resolvedRoot); volume != "" {
		anchor = volume + string(filepath.Separator)
	}
	if samePath(resolvedRoot, anchor) {
		return true
	}
	return resolvedRoot == userHome || isUnder(userHome, resolvedRoot)
}

func filterHiddenDescendants(paths []string, root string) []string {
	resolvedRoot := realPath(root)
	var out []string
	for _, item := range paths {
		resolved := realPath(item)
		rel := item
		if isUnder(resolved, resolvedRoot) {
			rel = strings.TrimPrefix(resolved[len(resolvedRoot):], string(filepath.Separator))
		}
		hidden := false
		for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
			if part != "." && part != ".." && strings.HasPrefix(part, ".") {
				hidden = true
				break
			}
		}
		if !hidden {
			out = append(out, item)
		}
	}
	return out
}

func (o *Operations) filterGitignored(ctx context.Context, paths []string, root string) ([]string, error) {
	if len(paths) == 0 {
		return paths, nil
	}
	quoted := shellArg(root)
	probe, err := o.execIn(ctx, "git -C "+quoted+" rev-parse --is-inside-work-tree >/dev/null 2>&1", "", gitTimeout, nil)
	if err != nil {
		return nil, err
	}
	if probe.exitCode != 0 {
		return paths, nil
	}
	input := strings.Join(paths, "\n")
	res, err := o.execIn(ctx, "git -C "+quoted+" check-ignore --stdin", "", gitTimeout, &input)
	if err != nil {
		return nil, err
	}
	ignored := make(map[string]struct{})
	for _, line := range splitLines(res.stdout, false) {
		ignored[line] = struct{}{}
	}
	var out []string
	for _, p := range paths {
		if _, skip := ignored[p]; !skip {
			out = append(out, p)
		}
	}
	return out, nil
}

func (o *Operations) sortByModTime(ctx context.Context, paths []string) ([]string, error) {
	if len(paths) < 2 {
		return paths, nil
	}
	script := `while IFS= read -r item || [ -n "$item" ]; do ` +
		`mtime="$(stat -c%Y "$item" 2>/dev/null ` +
		`|| stat -f%m "$item" 2>/dev/null || true)"; ` +
		"printf '%s\\n' \"${mtime:-0}\"; " +
		"done"
	input := strings.Join(paths, "\n")
	res, err := o.execIn(ctx, script, "", searchTimeout, &input)
	if err != nil {
		return nil, err
	}
	stamps := make([]float64, len(paths))
	for i, line := range splitLines(res.stdout, false) {
		if i >= len(paths) {
			break
		}
		if v, convErr := strconv.ParseFloat(trimSpace(line), 64); convErr == nil {
			stamps[i] = v
		}
	}
	order := make([]int, len(paths))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return stamps[order[a]] > stamps[order[b]] })
	out := make([]string, len(paths))
	for i, idx := range order {
		out[i] = paths[idx]
	}
	return out, nil
}

func (o *Operations) directoriesWithFind(ctx context.Context, pattern, root string, fetchLimit int) ([]string, string, error) {
	if fetchLimit <= 0 {
		return nil, "", nil
	}
	hasFind, err := o.hasCommand(ctx, "find")
	if err != nil || !hasFind {
		return nil, "", err
	}
	quotedRoot := shellArg(root)
	quotedPattern := shellArg(pattern)
	hiddenPrune := hiddenPruneExpr([]string{quotedRoot})
	base := "find -H " + quotedRoot + " -mindepth 1 " + hiddenPrune + " -o -type d " +
		"! -name '.*' -name " + quotedPattern
	res, err := o.execIn(ctx, base+" -printf '%T@ %p\\n' 2>/dev/null | sort -rn", "", probeTimeout, nil)
	if err != nil {
		return nil, "", err
	}
	stdout, limitReason := searchStdoutAndLimit(res)
	if isBlank(stdout) && limitReason == "" {
		res, err = o.execIn(ctx, base+" 2>/dev/null", "", probeTimeout, nil)
		if err != nil {
			return nil, "", err
		}
		stdout, limitReason = searchStdoutAndLimit(res)
	}
	var dirs []string
	for _, line := range strings.Split(trimSpace(stdout), "\n") {
		if line == "" {
			continue
		}
		stamp, rest, found := strings.Cut(line, " ")
		if found && allDigits(strings.ReplaceAll(stamp, ".", "")) {
			dirs = append(dirs, rest)
		} else {
			dirs = append(dirs, line)
		}
	}
	if len(dirs) > 0 {
		if o.rootUnderHiddenDir(root) {
			dirs = filterHiddenDescendants(dirs, root)
		}
		dirs, err = o.filterGitignored(ctx, dirs, root)
		if err != nil {
			return nil, "", err
		}
	}
	sorted, err := o.sortByModTime(ctx, dirs)
	if err != nil {
		return nil, "", err
	}
	return page(sorted, 0, fetchLimit), limitReason, nil
}

func (o *Operations) searchFiles(ctx context.Context, pattern string, roots []string, single bool, limit, offset int, order string) (SearchResult, error) {
	searchPattern := pattern
	if strings.HasPrefix(pattern, "**/") || strings.Contains(pattern, "/") {
		searchPattern = pattern[strings.LastIndex(pattern, "/")+1:]
	}
	if len(roots) == 0 {
		return SearchResult{Error: "File search requires at least one search root in 'path'."}, nil
	}
	hasRipgrep, err := o.hasCommand(ctx, "rg")
	if err != nil {
		return SearchResult{}, err
	}
	if hasRipgrep {
		executable, err := o.resolveRipgrep(ctx)
		if err != nil {
			return SearchResult{}, err
		}
		if order == "modified" {
			problem, err := o.modifiedOrderError(ctx, executable)
			if err != nil {
				return SearchResult{}, err
			}
			if problem != "" {
				return SearchResult{Error: problem}, nil
			}
		}
		keys := o.admissionKeys(roots)
		if !o.search.Admission.acquire(ctx, keys) {
			return SearchResult{Error: admissionInterruptedError}, nil
		}
		defer o.search.Admission.release(keys)
		return o.searchFilesRipgrep(ctx, searchPattern, roots, single, limit, offset, order, executable)
	}
	for _, root := range roots {
		if o.broadLocalRoot(root) {
			return SearchResult{Error: "Broad local file search without ripgrep is disabled because " +
				"find cannot keep this traversal safely bounded. Install " +
				"ripgrep or search a narrower directory."}, nil
		}
	}
	hasFind, err := o.hasCommand(ctx, "find")
	if err != nil {
		return SearchResult{}, err
	}
	if !hasFind {
		return SearchResult{Error: "File search requires 'rg' (ripgrep) or 'find'. " +
			"Install ripgrep for best results: " + ripgrepInstallHint}, nil
	}
	findRoots := make([]string, len(roots))
	quotedRoots := make([]string, len(roots))
	for i, root := range roots {
		findRoots[i] = root
		if strings.HasPrefix(root, "-") {
			findRoots[i] = "./" + root
		}
		quotedRoots[i] = shellArg(findRoots[i])
	}
	hiddenPrune := " " + hiddenPruneExpr(quotedRoots) + " -o"
	var protected []string
	for _, e := range o.effectiveMacExclusions(roots) {
		protected = append(protected, e.absolute)
	}
	protectedPrune := ""
	if len(protected) > 0 {
		protectedPrune = " " + pruneExpr(protected) + " -o"
	}
	fetchLimit := offset + limit + 1
	var rootExemptions strings.Builder
	for _, root := range findLiteralPathExpressions(findRoots) {
		rootExemptions.WriteString(" ! -path " + shellArg(root))
	}
	base := "find -H " + strings.Join(quotedRoots, " ") + protectedPrune + hiddenPrune + " " +
		`\( -type f -o \( -type d` + rootExemptions.String() + ` \) \) ` +
		"! -name '.*' -name " + shellArg(searchPattern)
	command := "set -o pipefail; " + base + " -print | head -n " + strconv.Itoa(fetchLimit)
	if order == "modified" {
		command = "set -o pipefail; " + base + " -printf '%T@ %p\\n' | sort -rn | head -n " + strconv.Itoa(fetchLimit)
	}
	keys := o.admissionKeys(roots)
	if !o.search.Admission.acquire(ctx, keys) {
		return SearchResult{Error: admissionInterruptedError}, nil
	}
	res, err := o.execIn(ctx, command, "", searchTimeout, nil)
	o.search.Admission.release(keys)
	if err != nil {
		return SearchResult{}, err
	}
	stdout, limitReason := searchStdoutAndLimit(res)
	files, diagnostics := splitFindOutput(stdout, order)
	failure, partial := findExitVerdict(res.exitCode, files, diagnostics, fetchLimit, order)
	if failure != nil {
		return *failure, nil
	}
	if runtime.GOOS == "windows" && o.env.HostFilesystem() {
		for i, f := range files {
			files[i] = env.MsysToWindowsPath(f)
		}
	}
	return SearchResult{
		Files:        page(files, offset, limit),
		TotalCount:   len(files),
		Truncated:    len(files) > offset+limit || limitReason != "",
		LimitReason:  limitReason,
		Warning:      partial,
		SkippedPaths: unreadablePaths(strings.Join(diagnostics, "\n")),
	}, nil
}

func commonDir(paths []string) string {
	parts := strings.Split(paths[0], "/")
	for _, p := range paths[1:] {
		other := strings.Split(p, "/")
		n := 0
		for n < len(parts) && n < len(other) && parts[n] == other[n] {
			n++
		}
		parts = parts[:n]
	}
	joined := strings.Join(parts, "/")
	if joined == "" {
		return "/"
	}
	return joined
}

func relativeTo(target, base string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return filepath.ToSlash(rel)
}

func (o *Operations) searchFilesRipgrep(ctx context.Context, pattern string, roots []string, single bool, limit, offset int, order, executable string) (SearchResult, error) {
	globPattern := pattern
	if !strings.Contains(pattern, "/") && !strings.HasPrefix(pattern, "*") {
		globPattern = "*" + pattern
	}
	fetchLimit := limit + offset + 1
	exclusions := o.effectiveMacExclusions(roots)
	scopedCommon := ""
	commandRoots := roots
	var exclusionTerms []string
	if len(roots) > 1 && len(exclusions) > 0 && posixRoots(roots) {
		cwd := o.liveCwd()
		absolute := make([]string, len(roots))
		for i, root := range roots {
			if strings.HasPrefix(root, "/") {
				absolute[i] = path.Clean(root)
			} else {
				absolute[i] = path.Clean(pathStyle{}.join(cwd, root))
			}
		}
		scopedCommon = commonDir(absolute)
		commandRoots = make([]string, len(absolute))
		for i, root := range absolute {
			commandRoots[i] = relativeTo(root, scopedCommon)
		}
		for _, e := range exclusions {
			exclusionTerms = append(exclusionTerms, "--glob "+shellArg("!"+relativeTo(e.absolute, scopedCommon)+"/**"))
		}
	} else {
		for _, e := range exclusions {
			exclusionTerms = append(exclusionTerms, "--glob "+shellArg("!"+e.relative+"/**"))
		}
	}
	exclusionArgs := ""
	seenTerms := make(map[string]struct{})
	for _, term := range exclusionTerms {
		if _, dup := seenTerms[term]; !dup {
			seenTerms[term] = struct{}{}
			exclusionArgs += " " + term
		}
	}
	if executable == "" {
		return SearchResult{Error: "File search requires ripgrep (rg)."}, nil
	}
	rg := o.quoteExecutable(executable)
	sortArg := ""
	if order == "modified" {
		sortArg = " --sortr=modified"
	}
	rootArgs := make([]string, len(commandRoots))
	for i, root := range commandRoots {
		rootArgs[i] = nativeToolArg(root)
	}
	cdPrefix := ""
	if scopedCommon != "" {
		cdPrefix = "cd " + shellArg(scopedCommon) + " && "
	}
	command := rg + " --files" + sortArg + " -g " + shellArg(globPattern) + exclusionArgs + " -- " + strings.Join(rootArgs, " ")
	res, err := o.runRipgrepBounded(ctx, []string{command}, fetchLimit, searchTimeout, true, scopedCommon == "", "set -o pipefail; "+cdPrefix)
	if err != nil {
		return SearchResult{}, err
	}
	stdout, limitReason := searchStdoutAndLimit(res)
	var diagnosticLines, files []string
	for _, line := range splitLines(stdout, false) {
		switch {
		case hasToolPrefix(line, "rg", "grep"):
			diagnosticLines = append(diagnosticLines, line)
		case line != "":
			files = append(files, line)
		}
	}
	diagnostics := strings.Join(diagnosticLines, "\n")
	var skip skipped
	if permissionOnlyError(diagnostics) {
		skip = skippedFields(diagnostics)
	}
	permissionOnly := res.exitCode == 2 && len(skip.paths) > 0
	warning := prependSkippedWarning(skip, "")
	if scopedCommon != "" {
		for i, f := range files {
			if !strings.HasPrefix(f, "/") {
				files[i] = path.Clean(pathStyle{}.join(scopedCommon, f))
			}
		}
	}
	boundedPipe := res.exitCode == 141 && len(files) >= fetchLimit
	if res.exitCode != 0 && res.exitCode != 1 && res.exitCode != 124 && !boundedPipe && !permissionOnly {
		detail := trimSpace(diagnostics)
		if detail == "" {
			detail = "no diagnostics"
		}
		if order == "modified" {
			return SearchResult{Error: "Exact modification-time order search failed (ripgrep 14+ " +
				"required for --sortr=modified; the installed rg passed the " +
				"version check, so this is an invocation failure: " + detail + "). " +
				"Use order='discovery'."}, nil
		}
		return SearchResult{Error: "File search failed while running ripgrep: " + detail}, nil
	}
	if limitReason != limitTimeout && single {
		hasFind, err := o.hasCommand(ctx, "find")
		if err != nil {
			return SearchResult{}, err
		}
		if hasFind {
			dirs, dirLimit, err := o.directoriesWithFind(ctx, globPattern, roots[0], fetchLimit)
			if err != nil {
				return SearchResult{}, err
			}
			if len(dirs) > 0 {
				existing := make(map[string]struct{}, len(files))
				for _, f := range files {
					existing[f] = struct{}{}
				}
				merged := append([]string(nil), files...)
				for _, d := range dirs {
					if _, dup := existing[d]; !dup {
						merged = append(merged, d)
					}
				}
				files, err = o.sortByModTime(ctx, merged)
				if err != nil {
					return SearchResult{}, err
				}
				if limitReason == "" {
					limitReason = dirLimit
				}
			}
		}
	}
	if permissionOnly && len(files) == 0 {
		return SearchResult{Warning: warning + " " + permissionSearchHint, SkippedPaths: skip.paths, SkippedOmitted: skip.omitted}, nil
	}
	return SearchResult{
		Files:          page(files, offset, limit),
		TotalCount:     len(files),
		Truncated:      len(files) > offset+limit || limitReason != "",
		LimitReason:    limitReason,
		Warning:        warning,
		SkippedPaths:   skip.paths,
		SkippedOmitted: skip.omitted,
	}, nil
}

func (o *Operations) searchContent(ctx context.Context, q searchRequest, root string) (SearchResult, error) {
	usedRipgrep, err := o.hasCommand(ctx, "rg")
	if err != nil {
		return SearchResult{}, err
	}
	var result SearchResult
	if usedRipgrep {
		result, err = o.searchWithRipgrep(ctx, q, root)
	} else {
		hasGrep, grepErr := o.hasCommand(ctx, "grep")
		if grepErr != nil {
			return SearchResult{}, grepErr
		}
		if !hasGrep {
			return SearchResult{Error: "Content search requires ripgrep (rg) or grep. " +
				"Install ripgrep: " + ripgrepInstallHint}, nil
		}
		result, err = o.searchWithGrep(ctx, q, root)
	}
	if err != nil {
		return SearchResult{}, err
	}
	if result.Error == "" && result.TotalCount == 0 && len(result.Matches) == 0 && len(result.Files) == 0 && len(result.Counts) == 0 {
		if hint, hintErr := o.zeroMatchHint(ctx, q.pattern, root, q.fileGlob); hintErr == nil && hint != "" {
			if result.Warning == "" {
				result.Warning = hint
			} else {
				result.Warning += " " + hint
			}
		}
	}
	if usedRipgrep {
		return result, nil
	}
	return warnLineOrientedNewline(result, q.pattern), nil
}

func (o *Operations) runSearchPipeline(ctx context.Context, parts []string, q searchRequest, warning string, lineCap bool) (SearchResult, error) {
	fetchLimit := q.limit + q.offset
	if q.context > 0 {
		fetchLimit += 200
	}
	var res execResult
	var err error
	if lineCap {
		command := append(append([]string(nil), parts...), "|", "head", "-n", strconv.Itoa(fetchLimit))
		if q.outputMode != "files_only" && q.outputMode != "count" {
			command = append(command, "|", "cut", "-c1-2000")
		}
		res, err = o.execIn(ctx, "set -o pipefail; "+strings.Join(command, " "), "", searchTimeout, nil)
	} else {
		res, err = o.runRipgrepBounded(ctx, parts, fetchLimit, searchTimeout, true, true, "set -o pipefail; ")
	}
	if err != nil {
		return SearchResult{}, err
	}
	return parseSearchOutput(res, q.outputMode, q.limit, q.offset, q.context, warning), nil
}

func (o *Operations) searchWithRipgrep(ctx context.Context, q searchRequest, root string) (SearchResult, error) {
	executable, err := o.resolveRipgrep(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	if executable == "" {
		return SearchResult{Error: "Content search requires ripgrep (rg)."}, nil
	}
	parts := []string{o.quoteExecutable(executable), "--line-number", "--no-heading", "--with-filename"}
	if q.outputMode != "files_only" && q.outputMode != "count" {
		parts = append(parts, "--max-columns", "2000", "--max-columns-preview")
	}
	multiline := patternHasRegexNewline(q.pattern)
	if multiline {
		parts = append(parts, "--multiline")
	}
	if q.context > 0 {
		parts = append(parts, "-C", strconv.Itoa(q.context))
	}
	parts = append(parts, o.ripgrepExclusionGlobs(root)...)
	if q.fileGlob != "" {
		parts = append(parts, "--glob", shellArg(q.fileGlob))
	}
	if flag, ok := outputModeFlag(q.outputMode); ok {
		parts = append(parts, flag)
	}
	parts = append(parts, o.shellText(q.pattern), nativeToolArg(root))
	note := ""
	if multiline {
		note = "Pattern contains \\n — multiline mode (-U) was enabled automatically " +
			"so the regex can match across line boundaries."
	}
	return o.runSearchPipeline(ctx, parts, q, note, false)
}

func (o *Operations) grepCommand(head []string, q searchRequest, fileGlob string) []string {
	parts := append([]string(nil), head...)
	if q.context > 0 {
		parts = append(parts, "-C", strconv.Itoa(q.context))
	}
	if fileGlob != "" {
		parts = append(parts, "--include", shellArg(fileGlob))
	}
	if flag, ok := outputModeFlag(q.outputMode); ok {
		parts = append(parts, flag)
	}
	return append(parts, o.shellText(q.pattern))
}

func (o *Operations) searchWithGrep(ctx context.Context, q searchRequest, root string) (SearchResult, error) {
	protected := o.protectedPrunePaths(root)
	if len(protected) > 0 || o.rootUnderHiddenDir(root) {
		return o.searchWithGrepPruned(ctx, q, root, protected)
	}
	parts := o.grepCommand([]string{"grep", "-rnHE", "--exclude-dir='.*'"}, q, q.fileGlob)
	absolute := strings.HasPrefix(root, "/") || strings.HasPrefix(root, `\\`) || drivePath.MatchString(root)
	searchRoot := shellArg(root)
	if !absolute {
		relative := strings.TrimPrefix(root, "./")
		searchRoot = `"$PWD"`
		if relative != "" && relative != "." {
			searchRoot += "/" + shellArg(relative)
		}
	}
	return o.runSearchPipeline(ctx, append(parts, searchRoot), q, "", true)
}

func (o *Operations) searchWithGrepPruned(ctx context.Context, q searchRequest, root string, protected []string) (SearchResult, error) {
	grepParts := o.grepCommand([]string{"grep", "-nHE"}, q, "")
	if root == "" {
		root = "."
	}
	quotedRoot := shellArg(root)
	findParts := []string{"find", "-H", quotedRoot}
	if len(protected) > 0 {
		findParts = append(findParts, pruneExpr(protected), "-o")
	}
	findParts = append(findParts, hiddenPruneExpr([]string{quotedRoot}), "-o", "-type f")
	if q.fileGlob != "" {
		findParts = append(findParts, "-name", shellArg(q.fileGlob))
	}
	findParts = append(findParts, "-exec")
	findParts = append(findParts, grepParts...)
	findParts = append(findParts, "{}", "+")
	return o.runSearchPipeline(ctx, findParts, q, "", true)
}

type SearchQuery struct {
	Pattern    string
	Path       string
	Target     string
	FileGlob   string
	Limit      int
	Offset     int
	OutputMode string
	Context    int
	Order      string
}

func (o *Operations) Search(ctx context.Context, query SearchQuery) (SearchResult, error) {
	offset, limit := SearchPage(query.Offset, query.Limit)
	pattern, kind, order := query.Pattern, query.Target, query.Order
	if kind == "files" && order != "discovery" && order != "modified" {
		return SearchResult{Error: "Invalid file search order " + quote(order) + "; expected " +
			"'discovery' or 'modified'."}, nil
	}
	q := searchRequest{pattern: pattern, target: kind, fileGlob: query.FileGlob, outputMode: query.OutputMode, order: order, limit: limit, offset: offset, context: query.Context}
	target, err := o.expandPath(ctx, query.Path)
	if err != nil {
		return SearchResult{}, err
	}
	probe, err := o.pathExistsProbe(ctx, target)
	if err != nil {
		return SearchResult{}, err
	}
	if probe.cwdError != "" {
		return SearchResult{Error: probe.cwdError}, nil
	}
	exists, missing := strings.Contains(probe.stdout, "exists"), strings.Contains(probe.stdout, "not_found")
	if !exists && !missing {
		return SearchResult{Error: unavailableText(target)}, nil
	}
	if missing {
		multi, handled, err := o.multiPathSearch(ctx, q, target)
		if err != nil {
			return SearchResult{}, err
		}
		if handled {
			return multi, nil
		}
		return o.pathNotFound(ctx, target)
	}
	var result SearchResult
	if kind == "files" {
		result, err = o.searchFiles(ctx, pattern, []string{target}, true, limit, offset, order)
	} else {
		result, err = o.searchContent(ctx, q, target)
	}
	if err != nil {
		return SearchResult{}, err
	}
	if exclusions := o.macSearchExclusions(target); len(exclusions) > 0 && result.Error == "" {
		names := make([]string, len(exclusions))
		for i, item := range exclusions {
			names[i] = item[strings.LastIndex(item, "/")+1:]
		}
		protected := "Skipped macOS protected folders during broad search to avoid " +
			"an unattended privacy prompt: " + strings.Join(names, ", ") + ". Search a protected " +
			"folder directly when access is intentional."
		if result.Warning != "" {
			result.Warning += " " + protected
		} else {
			result.Warning = protected
		}
	}
	return result, nil
}
