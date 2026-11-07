package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Age past which a lock is presumed abandoned, a healthy fetch completes in minutes
const StaleLockMinAgeSeconds = 10 * 60

// Age floor of the aborted transfer temp pack sweep
const StaleTmpPackMinAgeSeconds = StaleLockMinAgeSeconds

// Interrupted git operation locks the stale sweep removes from a checkout
var LockNames = []string{"shallow.lock", "index.lock", "HEAD.lock", "MERGE_HEAD.lock"}

// Object store index locks a killed git leaves, each one failing every later write of its file
var ObjectStoreLockNames = []string{
	"objects/info/commit-graph.lock",
	"objects/info/commit-graphs/commit-graph-chain.lock",
	"objects/pack/multi-pack-index.lock",
	"objects/pack/multi-pack-index.d/multi-pack-index-chain.lock",
}

// Temp prefixes git writes in objects/pack and renames away on success
var tmpPackPrefixes = []string{"tmp_pack_", "tmp_idx_", "tmp_rev_", "tmp_mtimes_", ".tmp-"}

// Temp a split commit graph write killed before its rename strands in objects/info/commit-graphs
const tmpGraphPrefix = "tmp_graph_"

// Name pattern pgrep matches for every form git runs as
const pgrepGit = `^git(-.*|\.exe)?$`

// Updater git runner update_custody.run_git, pkg/selfupdate installs its RunGit here at init
var GitlockRunGit func(ctx context.Context, gitCmd, args []string, o pysubprocess.Options) (*pysubprocess.Completed, error)

// The subprocess.run gitlock spawns its probes and treeless conversion through, swapped by tests
var gitlockSubprocessRun = pysubprocess.RunWith

// Whether psutil is importable, the process scan the stale sweeps trust outside Windows
var gitlockPsutil = true

// Whether os.name is nt for the git process probes, swapped by tests
var gitlockWindows = runtime.GOOS == "windows"

// The windows_hide_flags creation flags as process attributes, nil outside Windows
var gitlockHideAttr = windowsHideAttr

// Standard output the treeless conversion step prints its verdict line on
var gitlockStdout io.Writer = os.Stdout

// Unlink of the debris sweeps, swapped by tests
var gitlockRemove = os.Remove

// Git process probe guarding the debris sweeps, swapped by tests
var checkpointGitProcRunning = gitProcRunning

// Runs one updater git call, panicking when the process wiring never installed the runner
func gitlockRunGit(ctx context.Context, gitCmd, args []string, o pysubprocess.Options) (*pysubprocess.Completed, error) {
	if GitlockRunGit == nil {
		panic("tools.GitlockRunGit is not installed: pkg/selfupdate wires update_custody.run_git at init")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return GitlockRunGit(ctx, gitCmd, args, o)
}

// Adds the console hiding flags to attr as run_kwargs creationflags or windows_hide_flags does
func withHideFlags(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	if attr == nil {
		return gitlockHideAttr()
	}
	return mergeHideAttr(attr)
}

// Names git uses as a process, plain, dashed helpers and the exe spelling, empty for anything else
func gitProgramName(arg0 string) string {
	name := strings.TrimSpace(arg0)
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".exe")
	if name == "git" || strings.HasPrefix(name, "git-") {
		return name
	}
	return ""
}

// Reports whether any git process may be running, answering true whenever the probe cannot tell
func gitProcRunning(env *Env) bool {
	if !gitlockWindows && gitlockPsutil {
		return psutilGitRunning(env)
	}
	argv := []string{"pgrep", pgrepGit}
	if gitlockWindows {
		argv = []string{"tasklist", "/FO", "CSV", "/NH"}
	}
	res, err := gitlockSubprocessRun(context.Background(), argv, pysubprocess.Options{Timeout: 10 * time.Second, Attr: gitlockHideAttr()})
	if err != nil {
		toolsLogger(env).Debug("git process probe failed; assuming git may be running")
		return true
	}
	if gitlockWindows {
		if res.Returncode != 0 {
			return true
		}
		for _, line := range pystr.Splitlines(pystr.DecodeUTF8Replace(res.Stdout), false) {
			first, _, _ := strings.Cut(line, ",")
			if gitProgramName(strings.Trim(first, `"`)) != "" {
				return true
			}
		}
		return false
	}
	return res.Returncode != 1
}

// Scans the process table for a git, a failed scan or an unreadable name answering true
func psutilGitRunning(env *Env) bool {
	pids, err := proc.PIDs()
	if err != nil {
		toolsLogger(env).Debug("git process scan failed; assuming git may be running")
		return true
	}
	for _, pid := range pids {
		name, err := proc.Name(pid)
		if err != nil {
			if errors.Is(err, proc.ErrNoSuchProcess) || errors.Is(err, fs.ErrNotExist) || !proc.Exists(pid) {
				continue
			}
			toolsLogger(env).Debug("git process scan failed; assuming git may be running")
			return true
		}
		if gitProgramName(name) != "" {
			return true
		}
	}
	return false
}

// Removes aged debris files listed by the candidate function, skipping entirely while a git process runs
func sweepStaleGitDebris(env *Env, directory string, candidates func() []string, minAge *int, defaultAge int, skipMsg string,
	logRemoved func(string, int64)) []string {
	if !isDirFollow(directory) {
		return nil
	}
	if checkpointGitProcRunning(env) {
		toolsLogger(env).Debug(skipMsg)
		return nil
	}
	age := defaultAge
	if minAge != nil {
		age = *minAge
	}
	cutoff := float64(time.Now().UnixNano())/1e9 - float64(age)
	var removed []string
	for _, entry := range candidates() {
		st, err := os.Stat(entry)
		if err != nil {
			if !checkpointPathMissing(err) {
				toolsLogger(env).Warn(fmt.Sprintf("Could not clear %s (skipping): %v", entry, err))
			}
			continue
		}
		if !st.Mode().IsRegular() || statMtime(st) >= cutoff {
			continue
		}
		if runtime.GOOS == "windows" {
			os.Chmod(entry, 0o666)
		}
		if err := gitlockRemove(entry); err != nil {
			toolsLogger(env).Warn(fmt.Sprintf("Could not clear %s (skipping): %v", entry, err))
			continue
		}
		removed = append(removed, entry)
		logRemoved(entry, st.Size())
	}
	return removed
}

// Joins the slash separated git relative names under dir
func gitJoinAll(dir string, names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = filepath.Join(dir, filepath.FromSlash(n))
	}
	return out
}

// Sweeps the named locks under gitDir with the stale lock floor
func sweepGitLocks(env *Env, gitDir string, candidates func() []string, minAge *int) []string {
	return sweepStaleGitDebris(env, gitDir, candidates, minAge, StaleLockMinAgeSeconds,
		"git process running; skipping stale-lock sweep",
		func(p string, _ int64) { toolsLogger(env).Info("Removed stale git lock " + p) })
}

// Removes abandoned locks under the checkout git dir older than the floor while no git runs, returning their paths
func ClearStaleGitLocks(repoRoot string, minAge *int) []string {
	gitDir := filepath.Join(repoRoot, ".git")
	return sweepGitLocks(nil, gitDir, func() []string {
		names := append(append([]string{}, LockNames...), ObjectStoreLockNames...)
		return append(gitJoinAll(gitDir, names), RefLockFiles(gitDir)...)
	}, minAge)
}

// Removes only the stale object store locks under a git dir Hermes does not own
func ClearStaleObjectStoreLocks(gitDir string) []string {
	return sweepGitLocks(nil, gitDir, func() []string { return gitJoinAll(gitDir, ObjectStoreLockNames) }, nil)
}

// The packed-refs lock then every refs lock file in path component order
func RefLockFiles(gitDir string) []string {
	out := []string{filepath.Join(gitDir, "packed-refs.lock")}
	refs := filepath.Join(gitDir, "refs")
	if !isDirFollow(refs) {
		return out
	}
	var found []string
	filepath.WalkDir(refs, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == refs {
			return nil
		}
		name := d.Name()
		if runtime.GOOS == "windows" {
			name = strings.ToLower(name)
		}
		if strings.HasSuffix(name, ".lock") {
			found = append(found, p)
		}
		return nil
	})
	sort.SliceStable(found, func(i, j int) bool {
		return slices.Compare(strings.Split(found[i], string(filepath.Separator)), strings.Split(found[j], string(filepath.Separator))) < 0
	})
	return append(out, found...)
}

// Pack directory of a checkout or a bare repository
func gitPackDir(repoRoot string) string {
	gitDir := filepath.Join(repoRoot, ".git")
	if isDirFollow(gitDir) {
		return filepath.Join(gitDir, "objects", "pack")
	}
	return filepath.Join(repoRoot, "objects", "pack")
}

// Gives every pack without a promisor marker one and returns how many were marked
func MarkUnmarkedPacksPromisor(repoRoot string) int {
	marked := 0
	packs, _ := filepath.Glob(filepath.Join(gitPackDir(repoRoot), "pack-*.pack"))
	for _, pack := range packs {
		marker := strings.TrimSuffix(pack, filepath.Ext(pack)) + ".promisor"
		f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if err != nil {
			if !errors.Is(err, fs.ErrExist) {
				toolsLogger(nil).Warn(fmt.Sprintf("Could not mark %s as a partial-clone pack: %v", pack, err))
			}
			continue
		}
		f.Close()
		marked++
	}
	if marked > 0 {
		toolsLogger(nil).Info(fmt.Sprintf("Marked %d pack(s) in %s as partial-clone packs", marked, repoRoot))
	}
	return marked
}

// Commit graph writers a partial clone keeps off, from gc, fetch and maintenance
var tree0MaintenanceOff = [][2]string{
	{"maintenance.commit-graph.enabled", "false"},
	{"gc.writeCommitGraph", "false"},
	{"fetch.writeCommitGraph", "false"},
}

// Runs a run_git config call in repo with captured output and the console hidden
func gitlockConfigRun(ctx context.Context, repo string, check bool, args ...string) (*pysubprocess.Completed, error) {
	return gitlockRunGit(ctx, []string{"git"}, args, pysubprocess.Options{Dir: repo, Timeout: 30 * time.Second, Check: check, Attr: gitlockHideAttr()})
}

// Keeps git from writing a commit graph in a partial clone, idempotent and never failing
func DisableTree0AutoMaintenance(ctx context.Context, repoRoot string) {
	migrateEarlierMaintenanceKeys(ctx, repoRoot)
	for _, kv := range tree0MaintenanceOff {
		res, err := gitlockConfigRun(ctx, repoRoot, false, "config", "--local", "--get", kv[0])
		if err == nil {
			if pystr.Strip(pystr.DecodeUTF8Replace(res.Stdout)) == kv[1] {
				continue
			}
			_, err = gitlockConfigRun(ctx, repoRoot, true, "config", "--local", kv[0], kv[1])
		}
		if err != nil {
			toolsLogger(nil).Warn(fmt.Sprintf("Could not set %s=%s in %s", kv[0], kv[1], repoRoot))
		}
	}
}

// Removes the maintenance keys earlier cuts persisted, only under their fingerprint
func migrateEarlierMaintenanceKeys(ctx context.Context, repoRoot string) {
	fail := func() { toolsLogger(nil).Warn("Could not migrate earlier maintenance keys in " + repoRoot) }
	local := map[string]string{}
	for _, line := range GitlockStdoutLines(ctx, repoRoot, []string{"config", "--local", "--get-regexp",
		`^(maintenance\.auto|maintenance\.commit-graph\.enabled|fetch\.writecommitgraph|gc\.auto)$`}) {
		parts := pystr.SplitWhitespace(line, 1)
		if len(parts) != 2 {
			fail()
			return
		}
		local[parts[0]] = parts[1]
	}
	auto, hasAuto := local["maintenance.auto"]
	graph, hasGraph := local["fetch.writecommitgraph"]
	if _, enabled := local["maintenance.commit-graph.enabled"]; !hasAuto || auto != "false" || !hasGraph || graph != "false" || enabled {
		return
	}
	keys := []string{"maintenance.auto"}
	if v, ok := local["gc.auto"]; ok && v == "0" {
		keys = append(keys, "gc.auto")
	}
	for _, key := range keys {
		if _, err := gitlockConfigRun(ctx, repoRoot, true, "config", "--local", "--unset", key); err != nil {
			fail()
			return
		}
	}
}

// Removes aborted transfer temp packs and killed commit graph temps from a checkout or bare repository
func clearStaleTmpPacks(env *Env, repoRoot string, minAge *int) []string {
	packDir := gitPackDir(repoRoot)
	graphDir := filepath.Join(filepath.Dir(packDir), "info", "commit-graphs")
	candidates := func() []string {
		var found []string
		for _, spec := range []struct {
			dir      string
			prefixes []string
		}{{packDir, tmpPackPrefixes}, {graphDir, []string{tmpGraphPrefix}}} {
			ents, err := os.ReadDir(spec.dir)
			if err != nil {
				continue
			}
			for _, e := range ents {
				if slices.ContainsFunc(spec.prefixes, func(p string) bool { return strings.HasPrefix(e.Name(), p) }) {
					found = append(found, filepath.Join(spec.dir, e.Name()))
				}
			}
		}
		return found
	}
	return sweepStaleGitDebris(env, packDir, candidates, minAge, StaleTmpPackMinAgeSeconds,
		"git process running; skipping tmp-pack sweep",
		func(p string, size int64) {
			toolsLogger(env).Info(fmt.Sprintf("Removed aborted-write git debris %s (%d bytes)", p, size))
		})
}

// Removes aborted transfer temp packs older than the floor while no git runs, returning their paths
func ClearStaleTmpPacks(repoRoot string, minAge *int) []string {
	return clearStaleTmpPacks(nil, repoRoot, minAge)
}

// Nonblank stripped stdout lines of a read only git query in repo, empty on any failure
func GitlockStdoutLines(ctx context.Context, repoRoot string, args []string) []string {
	res, err := gitlockRunGit(ctx, []string{"git"}, args, pysubprocess.Options{Dir: repoRoot, Timeout: 10 * time.Second, Attr: gitlockHideAttr()})
	if err != nil {
		toolsLogger(nil).Debug(fmt.Sprintf("git query failed: %s", pyrepr.Repr(anySlice(args))))
		return nil
	}
	if res.Returncode != 0 {
		return nil
	}
	var out []string
	for _, line := range pystr.Splitlines(pystr.DecodeUTF8Replace(res.Stdout), false) {
		if s := pystr.Strip(line); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Strings as a list of any for repr
func anySlice(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// Fields split on ASCII whitespace as bytes.split does
func bytesFields(b []byte) [][]byte {
	return bytes.FieldsFunc(b, func(r rune) bool { return r == ' ' || (r >= '\t' && r <= '\r') })
}

// Local commits whose parents are missing, read from each commit header only, empty on any failure
func batchMissingParents(ctx context.Context, repoRoot string, candidates []string) map[string]bool {
	if len(candidates) == 0 {
		return nil
	}
	result, err := batchMissingParentsErr(ctx, repoRoot, candidates)
	if err != nil {
		toolsLogger(nil).Debug("parent-object probe failed for " + repoRoot)
		return nil
	}
	return result
}

func batchMissingParentsErr(ctx context.Context, repoRoot string, candidates []string) (map[string]bool, error) {
	res, err := gitlockRunGit(ctx, []string{"git"}, []string{"cat-file", "--batch"}, pysubprocess.Options{
		Dir: repoRoot, Input: []byte(strings.Join(candidates, "\n") + "\n"), Timeout: 30 * time.Second, Attr: gitlockHideAttr()})
	if err != nil {
		return nil, err
	}
	if res.Returncode != 0 {
		return nil, nil
	}
	data := res.Stdout
	cursor := 0
	parentsBy := map[string]map[string]bool{}
	parents := map[string]bool{}
	for _, candidate := range candidates {
		headerEnd := bytes.IndexByte(data[min(cursor, len(data)):], '\n')
		if headerEnd < 0 {
			return nil, nil
		}
		headerEnd += cursor
		header := bytesFields(data[cursor:headerEnd])
		cursor = headerEnd + 1
		if len(header) >= 3 && string(header[1]) == "commit" {
			size, err := strconv.Atoi(strings.TrimSpace(string(header[2])))
			if err != nil {
				return nil, err
			}
			end := min(cursor+size, len(data))
			body := data[min(cursor, len(data)):end]
			cursor += size
			if cursor >= len(data) || data[cursor] != '\n' {
				return nil, nil
			}
			cursor++
			own := map[string]bool{}
			for _, line := range bytes.Split(body, []byte("\n")) {
				if len(line) == 0 {
					break
				}
				fields := bytesFields(line)
				if len(fields) >= 2 && string(fields[0]) == "parent" {
					sha, err := pystr.DecodeUTF8(fields[1])
					if err != nil {
						return nil, err
					}
					own[sha] = true
				}
			}
			parentsBy[candidate] = own
			for p := range own {
				parents[p] = true
			}
		} else if len(header) < 2 || string(header[1]) != "missing" {
			return nil, nil
		}
	}
	if len(parents) == 0 {
		return nil, nil
	}
	sorted := sortedSet(parents)
	check, err := gitlockRunGit(ctx, []string{"git"}, []string{"cat-file", "--batch-check"}, pysubprocess.Options{
		Dir: repoRoot, Input: []byte(strings.Join(sorted, "\n") + "\n"), Timeout: 10 * time.Second, Attr: gitlockHideAttr()})
	if err != nil {
		return nil, err
	}
	if check.Returncode != 0 {
		return nil, nil
	}
	missing := map[string]bool{}
	for _, line := range pystr.Splitlines(pystr.DecodeUTF8Replace(check.Stdout), false) {
		if strings.HasSuffix(line, " missing") {
			missing[pystr.SplitWhitespace(line, -1)[0]] = true
		}
	}
	out := map[string]bool{}
	for commit, own := range parentsBy {
		for p := range own {
			if missing[p] {
				out[commit] = true
				break
			}
		}
	}
	return out, nil
}

// Shallow file git resolves for repo, empty when the repository has none
func shallowFilePath(ctx context.Context, repoRoot string) string {
	rel := GitlockStdoutLines(ctx, repoRoot, []string{"rev-parse", "--git-path", "shallow"})
	if len(rel) == 0 {
		return ""
	}
	p := rel[0]
	if !filepath.IsAbs(p) {
		p = filepath.Join(repoRoot, rel[0])
	}
	if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
		return p
	}
	return ""
}

// Git's own shallow.lock held open for the duration so a concurrent fetch fails fast instead of being clobbered
type shallowLock struct {
	lockPath string
	f        *os.File
}

// Takes the shallow lock beside shallowPath
func acquireShallowLock(shallowPath string) (*shallowLock, error) {
	lockPath := shallowPath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o777)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("shallow lock held: %s", lockPath)
		}
		return nil, fmt.Errorf("cannot create shallow lock: %w", err)
	}
	if _, err := f.Write([]byte("icebath shallow maintenance\n")); err != nil {
		f.Close()
		os.Remove(lockPath)
		return nil, fmt.Errorf("cannot write shallow lock: %w", err)
	}
	return &shallowLock{lockPath: lockPath, f: f}, nil
}

// Closes and removes the lock, an already missing lock file is fine
func (l *shallowLock) release() error {
	l.f.Close()
	if err := os.Remove(l.lockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Runs body under the shallow lock, a failed release replacing body's result like a raising exit
func withShallowLock(shallowPath string, body func() (int, error)) (int, error) {
	lock, err := acquireShallowLock(shallowPath)
	if err != nil {
		return 0, err
	}
	n, err := body()
	if rerr := lock.release(); rerr != nil && err == nil {
		return 0, rerr
	}
	return n, err
}

// Atomically replaces the shallow file through a temp file in text mode
func writeShallow(shallowPath, content, suffix string) error {
	tmp := shallowPath + suffix
	if runtime.GOOS == "windows" {
		content = strings.ReplaceAll(content, "\n", "\r\n")
	}
	if err := os.WriteFile(tmp, []byte(content), 0o666); err != nil {
		return err
	}
	return os.Rename(tmp, shallowPath)
}

// Lines of the shallow file bytes, decoded as utf-8-sig and split like str.splitlines
func shallowLines(original []byte) ([]string, error) {
	text, err := pystr.DecodeUTF8Sig(original)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range pystr.Splitlines(text, false) {
		if line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

// Re-appends shallow boundaries for reflog reachable commits whose parents were never fetched, never failing
func RepairBrokenShallowBoundaries(ctx context.Context, repoRoot string) int {
	n, err := repairBrokenShallowBoundaries(ctx, repoRoot)
	if err != nil {
		toolsLogger(nil).Debug("shallow boundary repair failed for " + repoRoot)
		return 0
	}
	return n
}

func repairBrokenShallowBoundaries(ctx context.Context, repoRoot string) (int, error) {
	shallowPath := shallowFilePath(ctx, repoRoot)
	if shallowPath == "" {
		return 0, nil
	}
	probe, err := gitlockRunGit(ctx, []string{"git"}, []string{"rev-list", "--count", "--all", "--reflog"},
		pysubprocess.Options{Dir: repoRoot, Timeout: 10 * time.Second, Attr: gitlockHideAttr()})
	if err != nil {
		return 0, err
	}
	if probe.Returncode == 0 {
		return 0, nil
	}
	repaired := 0
	n, err := withShallowLock(shallowPath, func() (int, error) {
		original, err := os.ReadFile(shallowPath)
		if err != nil {
			return 0, err
		}
		lines, err := shallowLines(original)
		if err != nil {
			return 0, err
		}
		existing := map[string]bool{}
		for _, l := range lines {
			existing[l] = true
		}
		if len(existing) == 0 {
			return 0, nil
		}
		set := map[string]bool{}
		for _, entry := range GitlockStdoutLines(ctx, repoRoot, []string{"reflog", "show", "--all", "--format=%H%x00%gD"}) {
			sha, selector, ok := strings.Cut(entry, "\x00")
			if !ok {
				return 0, fmt.Errorf("not enough values to unpack (expected 2, got 1)")
			}
			if strings.HasPrefix(selector, "refs/remotes/") {
				set[sha] = true
			}
		}
		broken := batchMissingParents(ctx, repoRoot, sortedSet(set))
		all := map[string]bool{}
		for k := range existing {
			all[k] = true
		}
		for k := range broken {
			if !existing[k] {
				repaired++
				all[k] = true
			}
		}
		if repaired == 0 {
			return 0, nil
		}
		if err := writeShallow(shallowPath, strings.Join(sortedSet(all), "\n")+"\n", ".icebath-repair"); err != nil {
			return 0, err
		}
		if len(GitlockStdoutLines(ctx, repoRoot, []string{"rev-list", "--count", "--all", "--reflog"})) == 0 {
			if err := os.WriteFile(shallowPath, original, 0o666); err != nil {
				return 0, err
			}
			toolsLogger(nil).Debug("shallow boundary repair self-check failed; file restored")
			repaired = 0
			return 0, nil
		}
		return repaired, nil
	})
	if err != nil || n == 0 {
		return 0, err
	}
	toolsLogger(nil).Info(fmt.Sprintf("Restored %d broken shallow boundary(ies) in %s", n, repoRoot))
	return n, nil
}

// Drops shallow graft lines no live ref or reflog still points at, restoring the file when history stops walking
func PruneStaleShallowGrafts(ctx context.Context, repoRoot string) int {
	n, err := pruneStaleShallowGrafts(ctx, repoRoot)
	if err != nil {
		toolsLogger(nil).Debug("shallow graft prune failed for " + repoRoot)
		return 0
	}
	return n
}

func pruneStaleShallowGrafts(ctx context.Context, repoRoot string) (int, error) {
	shallowPath := shallowFilePath(ctx, repoRoot)
	if shallowPath == "" {
		return 0, nil
	}
	n, err := withShallowLock(shallowPath, func() (int, error) {
		original, err := os.ReadFile(shallowPath)
		if err != nil {
			return 0, err
		}
		lines, err := shallowLines(original)
		if err != nil {
			return 0, err
		}
		if len(lines) == 0 {
			return 0, nil
		}
		live := map[string]bool{}
		for _, args := range [][]string{{"rev-parse", "HEAD"}, {"rev-parse", "--verify", "--quiet", "FETCH_HEAD"}, {"for-each-ref", "--format=%(objectname)"}} {
			for _, sha := range GitlockStdoutLines(ctx, repoRoot, args) {
				live[sha] = true
			}
		}
		keep, dropped := map[string]bool{}, map[string]bool{}
		for _, l := range lines {
			if live[l] {
				keep[l] = true
			}
		}
		if len(keep) == len(lines) {
			return 0, nil
		}
		for _, l := range lines {
			if !keep[l] {
				dropped[l] = true
			}
		}
		for _, ref := range GitlockStdoutLines(ctx, repoRoot, []string{"for-each-ref", "--format=%(refname)", "refs/remotes/"}) {
			pins := false
			for _, sha := range GitlockStdoutLines(ctx, repoRoot, []string{"reflog", "show", "--format=%H", ref}) {
				pins = pins || dropped[sha]
			}
			if pins {
				if _, err := gitlockRunGit(ctx, []string{"git"}, []string{"reflog", "expire", "--expire=now", ref},
					pysubprocess.Options{Dir: repoRoot, Timeout: 10 * time.Second, Attr: gitlockHideAttr()}); err != nil {
					return 0, err
				}
			}
		}
		if err := writeShallow(shallowPath, strings.Join(sortedSet(keep), "\n")+"\n", ".icebath-prune"); err != nil {
			return 0, err
		}
		walks := len(GitlockStdoutLines(ctx, repoRoot, []string{"rev-list", "--count", "HEAD"})) > 0 &&
			len(GitlockStdoutLines(ctx, repoRoot, []string{"rev-list", "--count", "--all"})) > 0 &&
			len(GitlockStdoutLines(ctx, repoRoot, []string{"rev-list", "--count", "--all", "--reflog"})) > 0
		if !walks {
			if err := os.WriteFile(shallowPath, original, 0o666); err != nil {
				return 0, err
			}
			toolsLogger(nil).Debug("shallow prune self-check failed; grafts restored")
			return 0, nil
		}
		return len(lines) - len(keep), nil
	})
	if err != nil || n == 0 {
		return 0, err
	}
	toolsLogger(nil).Info(fmt.Sprintf("Pruned %d stale shallow graft(s) in %s", n, repoRoot))
	return n, nil
}

// Runs a run_git text query in repo with the caller's run keywords and a 30 second timeout
func gitlockKwRun(ctx context.Context, repo string, kw pysubprocess.Options, args ...string) (*pysubprocess.Completed, error) {
	o := kw
	o.Dir, o.Timeout = repo, 30*time.Second
	return gitlockRunGit(ctx, []string{"git"}, args, o)
}

// The checkout's own partial clone filter, empty for a clone that is not partial
func PartialCloneFilter(ctx context.Context, repoRoot string, kw pysubprocess.Options) (string, error) {
	res, err := gitlockKwRun(ctx, repoRoot, kw, "config", "--get", "remote.origin.promisor")
	if err != nil {
		return "", err
	}
	if res.Returncode != 0 || pystr.Lower(pystr.Strip(pystr.DecodeUTF8Replace(res.Stdout))) != "true" {
		return "", nil
	}
	configured, err := gitlockKwRun(ctx, repoRoot, kw, "config", "--get", "remote.origin.partialclonefilter")
	if err != nil {
		return "", err
	}
	return pystr.Strip(pystr.DecodeUTF8Replace(configured.Stdout)), nil
}

// Unshallows a stale installer checkout before the bounded fetch, reporting whether it was shallow
func HealShallowHistory(ctx context.Context, repoRoot, branch string, kw pysubprocess.Options) (bool, error) {
	if shallowFilePath(ctx, repoRoot) == "" {
		return false, nil
	}
	return FetchFullCommitGraph(ctx, repoRoot, []string{branch}, kw)
}

// Timeout of the release tag refresh fetch
const tagRefreshTimeout = 30 * time.Second

// Timeout of the history filling unshallow fetch
const historyFetchTimeout = 900 * time.Second

// Refreshes release tags and fills shallow history keeping the clone's mode, reporting whether it unshallowed
func FetchFullCommitGraph(ctx context.Context, repoRoot string, extraRefspecs []string, kw pysubprocess.Options) (shallow bool, err error) {
	shallowPath := shallowFilePath(ctx, repoRoot)
	shallow = shallowPath != ""
	kw.Attr = withHideFlags(kw.Attr)
	fetchFilter, err := PartialCloneFilter(ctx, repoRoot, kw)
	if err != nil {
		return false, err
	}
	converts := false
	if fetchFilter == "" && shallow {
		raw, err := os.ReadFile(shallowPath)
		if err != nil {
			return false, err
		}
		text, err := pystr.DecodeUTF8Sig(raw)
		if err != nil {
			return false, err
		}
		converts = len(batchMissingParents(ctx, repoRoot, pystr.SplitWhitespace(text, -1))) > 0
	}
	if converts {
		fetchFilter = "blob:none"
	}
	defer func() {
		if converts {
			MarkUnmarkedPacksPromisor(repoRoot)
			DisableTree0AutoMaintenance(ctx, repoRoot)
		}
	}()
	args := []string{"fetch", "--quiet"}
	if shallow {
		args = append(args, "--unshallow")
	}
	if fetchFilter != "" {
		args = append(args, "--filter="+fetchFilter)
	}
	args = append(append(args, "--no-tags", "origin", "refs/tags/v*:refs/tags/v*"), extraRefspecs...)
	o := kw
	o.Dir, o.Check, o.Timeout = repoRoot, true, tagRefreshTimeout
	if shallow {
		o.Timeout = historyFetchTimeout
	}
	if _, err := gitlockRunGit(ctx, []string{"git"}, args, o); err != nil {
		return false, err
	}
	return shallow, nil
}

// Line of the git 2.53 partial clone pack-objects BUG
const packObjectsCrashFingerprint = "should_include_obj should only be called on existing objects"

// Lines one of which accompanies the fingerprint when the BUG killed the fetch
var packObjectsCrashTerminators = []string{
	"pack-objects died of signal 6",
	"could not finish pack-objects to repack local links",
	"fetch-pack: invalid index-pack output",
}

// Whether a fetch failure is the git 2.53 partial clone pack-objects BUG
func IsPartialClonePackObjectsCrash(stderr string) bool {
	if !strings.Contains(stderr, packObjectsCrashFingerprint) {
		return false
	}
	return slices.ContainsFunc(packObjectsCrashTerminators, func(t string) bool { return strings.Contains(stderr, t) })
}

// Runs a fetch and on the pack-objects BUG marks the unmarked packs and retries it once
func FetchWithPartialCloneRecovery(runner func(gitCmd, args []string) (*pysubprocess.Completed, error), gitCmd, fetchArgs []string,
	repoRoot string) (*pysubprocess.Completed, error) {
	result, err := runner(gitCmd, fetchArgs)
	if err != nil {
		return nil, err
	}
	if result.Returncode == 0 || !IsPartialClonePackObjectsCrash(pystr.DecodeUTF8Replace(result.Stderr)) {
		return result, nil
	}
	MarkUnmarkedPacksPromisor(repoRoot)
	toolsLogger(nil).Info("pack-objects crash on a partial clone; retrying the fetch")
	return runner(gitCmd, fetchArgs)
}

// Turns a treeless checkout into the blobless layout, recording the filter only once its history is whole
func ConvertTreelessCheckout(ctx context.Context, repoRoot string, kw pysubprocess.Options) (bool, error) {
	kw.Attr = withHideFlags(kw.Attr)
	filter, err := PartialCloneFilter(ctx, repoRoot, kw)
	if err != nil {
		return false, err
	}
	if filter != "tree:0" {
		return false, nil
	}
	major, minor, err := gitlockGitVersion(ctx, kw)
	if err != nil {
		return false, err
	}
	if major < 2 || (major == 2 && minor < 36) {
		return false, nil
	}
	refetch := []string{"git", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "fetch", "--quiet", "--refetch",
		"--filter=blob:none", "origin"}
	fetchKw := kw
	fetchKw.Dir, fetchKw.Timeout = repoRoot, 900*time.Second
	if _, err := gitlockSubprocessRun(ctx, append(slices.Clone(refetch), "--tags"), fetchKw); err != nil {
		return false, err
	}
	complete, err := historyTreesComplete(ctx, repoRoot, kw)
	if err != nil {
		return false, err
	}
	if !complete {
		headKw := kw
		headKw.Dir, headKw.Check, headKw.Timeout = repoRoot, true, 30*time.Second
		res, err := gitlockSubprocessRun(ctx, []string{"git", "rev-parse", "HEAD"}, headKw)
		if err != nil {
			return false, err
		}
		head := pystr.Strip(pystr.DecodeUTF8Replace(res.Stdout))
		checked := fetchKw
		checked.Check = true
		if _, err := gitlockSubprocessRun(ctx, append(slices.Clone(refetch), head), checked); err != nil {
			return false, err
		}
		if complete, err = historyTreesComplete(ctx, repoRoot, kw); err != nil {
			return false, err
		}
		if !complete {
			return false, &pysubprocess.CalledProcessError{Cmd: refetch, Returncode: 1, Stderr: []byte("the checked-out history is still missing trees")}
		}
	}
	cfgKw := kw
	cfgKw.Dir, cfgKw.Check, cfgKw.Timeout = repoRoot, true, 30*time.Second
	if _, err := gitlockSubprocessRun(ctx, []string{"git", "config", "remote.origin.partialclonefilter", "blob:none"}, cfgKw); err != nil {
		return false, err
	}
	return true, nil
}

// Run keywords of no_prompt_git_kwargs, the prompt disabled and the console hidden
func noPromptGitKwargs() pysubprocess.Options {
	env := os.Environ()
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=Never")
	return pysubprocess.Options{Env: dedupeEnviron(env), Attr: gitlockHideAttr()}
}

// Keeps the last assignment of every environment name
func dedupeEnviron(env []string) []string {
	m := environToMap(env)
	return mapToEnviron(m)
}

// Runs the one time treeless conversion as the update's first new code step and prints its verdict, never failing
func ConvertTreelessCheckoutFirst(ctx context.Context, repoRoot string) {
	converted, err := ConvertTreelessCheckout(ctx, repoRoot, noPromptGitKwargs())
	if err == nil {
		if converted {
			fmt.Fprintln(gitlockStdout, "  ✓ Fetched this checkout's directory history once; updates stop re-downloading it")
		}
		return
	}
	text := err.Error()
	var cpe *pysubprocess.CalledProcessError
	var te *pysubprocess.TimeoutExpired
	name := "OSError"
	if errors.As(err, &cpe) {
		name = "CalledProcessError"
		if len(cpe.Stderr) > 0 {
			text = pystr.DecodeUTF8Replace(cpe.Stderr)
		}
	} else if errors.As(err, &te) {
		name = "TimeoutExpired"
		if len(te.Stderr) > 0 {
			text = pystr.DecodeUTF8Replace(te.Stderr)
		}
	}
	lines := pystr.Splitlines(pystr.Strip(text), false)
	detail := name
	if len(lines) > 0 {
		detail = lines[len(lines)-1]
	}
	fmt.Fprintf(gitlockStdout, "  ⚠ Could not fetch this checkout's directory history (%s); retrying next update\n", detail)
}

// Version pattern of git --version output
var gitVersionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// Major and minor of the git on PATH, zeros when the output names none
func gitlockGitVersion(ctx context.Context, kw pysubprocess.Options) (int, int, error) {
	o := kw
	o.Timeout = 30 * time.Second
	res, err := gitlockSubprocessRun(ctx, []string{"git", "--version"}, o)
	if err != nil {
		return 0, 0, err
	}
	m := gitVersionRe.FindStringSubmatch(pystr.DecodeUTF8Replace(res.Stdout))
	if m == nil {
		return 0, 0, nil
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major, minor, nil
}

// Whether every tree in HEAD's history is local, checked without lazy fetching
func historyTreesComplete(ctx context.Context, repoRoot string, kw pysubprocess.Options) (bool, error) {
	m := noninteractiveGitEnv(nil)
	if len(kw.Env) > 0 {
		m = environToMap(kw.Env)
	}
	m["GIT_NO_LAZY_FETCH"] = "1"
	o := kw
	o.Dir, o.Timeout, o.Env = repoRoot, 300*time.Second, mapToEnviron(m)
	walk, err := gitlockSubprocessRun(ctx, []string{"git", "rev-list", "--objects", "--filter=blob:none", "--missing=print", "HEAD"}, o)
	if err != nil {
		return false, err
	}
	if walk.Returncode != 0 {
		return false, nil
	}
	for _, line := range pystr.Splitlines(pystr.DecodeUTF8Replace(walk.Stdout), false) {
		if strings.HasPrefix(line, "?") {
			return false, nil
		}
	}
	return true, nil
}

// Persists the commit graph off keys on a partial clone while a full clone keeps stock maintenance, never failing
func SettlePartialCloneMaintenance(ctx context.Context, repoRoot string) {
	filter, err := PartialCloneFilter(ctx, repoRoot, pysubprocess.Options{Attr: gitlockHideAttr()})
	if err != nil {
		toolsLogger(nil).Debug("partial-clone maintenance settings failed for " + repoRoot)
		return
	}
	if filter != "" {
		DisableTree0AutoMaintenance(ctx, repoRoot)
	}
}
