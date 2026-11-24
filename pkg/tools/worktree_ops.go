package tools

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	executil "github.com/nickheyer/icebath/pkg/utils/exec"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"

	"modernc.org/sqlite"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Decoded result of one worktree git call
type worktreeGitResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Runs git in cwd capturing utf-8 text with replacement, env nil inherits the process environment
func worktreeGit(args []string, cwd string, timeout time.Duration, env map[string]string) (*worktreeGitResult, error) {
	o := executil.Options{Dir: cwd, Timeout: timeout}
	if env != nil {
		o.Env = mapToEnviron(env)
	}
	r, err := executil.Run(context.Background(), append([]string{"git"}, args...), o)
	if err != nil {
		return nil, err
	}
	return &worktreeGitResult{r.ExitCode, strings.ToValidUTF8(string(r.Stdout), "\uFFFD"), strings.ToValidUTF8(string(r.Stderr), "\uFFFD")}, nil
}

// Stripped stdout with ok false on a non zero exit, errors raise like the git call
func worktreeGitOut(args []string, cwd string, timeout time.Duration) (string, bool, error) {
	r, err := worktreeGit(args, cwd, timeout, nil)
	if err != nil {
		return "", false, err
	}
	if r.ExitCode != 0 {
		return "", false, nil
	}
	return strings.TrimSpace(r.Stdout), true, nil
}

// Runs git swallowing every error, logging it at debug under the given prefix
func worktreeGitQuiet(args []string, cwd string, timeout time.Duration, log string, env map[string]string) {
	if _, err := worktreeGit(args, cwd, timeout, env); err != nil && log != "" {
		envLog(nil).Debug(fmt.Sprintf("%s: %s", log, err))
	}
}

// WorktreeGitQuiet runs git swallowing every error, logging it at debug under the given prefix
func WorktreeGitQuiet(args []string, cwd string, timeout time.Duration, log string, env map[string]string) {
	worktreeGitQuiet(args, cwd, timeout, log, env)
}

// Non blank non comment entries of the repository worktreeinclude file, empty when absent
func worktreeincludeEntries(repoRoot string) []string {
	p := filepath.Join(repoRoot, ".worktreeinclude")
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	text := strings.ToValidUTF8(string(raw), "\uFFFD")
	text = strings.TrimPrefix(text, "\ufeff")
	var entries []string
	for _, line := range textutil.Lines(text) {
		if entry := strings.TrimSpace(line); entry != "" && !strings.HasPrefix(entry, "#") {
			entries = append(entries, entry)
		}
	}
	return entries
}

var (
	remoteTrunkCandidates = []string{"origin/HEAD", "origin/main", "origin/master"}
	localTrunkCandidates  = []string{"main", "master"}
)

// Local trunk of a repo without remote tracking refs, main or master or the main worktree branch, empty when none
func worktreeLocalTrunk(path string, timeout time.Duration) (string, error) {
	for _, name := range localTrunkCandidates {
		out, ok, err := worktreeGitOut([]string{"rev-parse", "--verify", "--quiet", "refs/heads/" + name}, path, timeout)
		if err != nil {
			return "", err
		}
		if ok && out != "" {
			return name, nil
		}
	}
	porcelain, _, err := worktreeGitOut([]string{"worktree", "list", "--porcelain"}, path, timeout)
	if err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(porcelain, "\n\n")
	for _, line := range textutil.Lines(first) {
		if strings.HasPrefix(line, "branch refs/heads/") {
			return strings.TrimSpace(line[len("branch refs/heads/"):]), nil
		}
	}
	return "", nil
}

// Ref merged work is judged against, origin trunk or the local trunk of a remoteless repo, empty when nothing compares
func worktreeMergeBaseRef(path string, timeout time.Duration) (string, error) {
	for _, cand := range remoteTrunkCandidates {
		out, ok, err := worktreeGitOut([]string{"rev-parse", "--verify", "--quiet", cand}, path, timeout)
		if err != nil {
			return "", err
		}
		if ok && out != "" {
			return cand, nil
		}
	}
	out, ok, err := worktreeGitOut([]string{"for-each-ref", "--format=%(refname)", "refs/remotes"}, path, timeout)
	if err != nil {
		return "", err
	}
	if ok && out == "" {
		return worktreeLocalTrunk(path, timeout)
	}
	return "", nil
}

// Whether the tree has commits no remote branch reaches, failing safe toward true
func worktreeHasUnpushedCommits(worktreePath string, timeout time.Duration) bool {
	remoteRefs, ok, err := worktreeGitOut([]string{"for-each-ref", "--format=%(refname)", "refs/remotes"}, worktreePath, timeout)
	if err != nil || !ok {
		return true
	}
	baseline := []string{"--remotes"}
	if remoteRefs == "" {
		trunk, err := worktreeLocalTrunk(worktreePath, timeout)
		if err != nil || trunk == "" {
			return true
		}
		baseline = []string{trunk}
	}
	unpushed, ok, err := worktreeGitOut(append([]string{"log", "--oneline", "HEAD", "--not"}, baseline...), worktreePath, timeout)
	return err != nil || !ok || unpushed != ""
}

// WorktreeHasUnpushedCommits reports commits the worktree branch has not pushed
func WorktreeHasUnpushedCommits(worktreePath string, timeout time.Duration) bool {
	return worktreeHasUnpushedCommits(worktreePath, timeout)
}

// Copies the files and links the directories .worktreeinclude lists into the worktree, escaping entries skipped
func copyWorktreeIncludes(repoRoot, wtPath string) {
	entries := worktreeincludeEntries(repoRoot)
	if len(entries) == 0 {
		return
	}
	log := envLog(nil)
	repoResolved, err := pathutil.Resolve(repoRoot)
	if err != nil {
		log.Debug(fmt.Sprintf("Error copying .worktreeinclude entries: %s", errchain.OSText(err)))
		return
	}
	wtResolved, err := pathutil.Resolve(wtPath)
	if err != nil {
		log.Debug(fmt.Sprintf("Error copying .worktreeinclude entries: %s", errchain.OSText(err)))
		return
	}
	for _, entry := range entries {
		src, dst := filepath.Join(repoRoot, entry), filepath.Join(wtPath, entry)
		srcResolved, err1 := pathutil.Resolve(src)
		dstResolved, err2 := pathutil.Resolve(dst)
		if err1 != nil || err2 != nil {
			log.Debug(fmt.Sprintf("Skipping invalid .worktreeinclude entry: %s", entry))
			continue
		}
		if _, inside := pathutil.RelativeTo(srcResolved, repoResolved); !inside {
			log.Warn(fmt.Sprintf("Skipping .worktreeinclude entry outside repo root: %s", entry))
			continue
		}
		if _, inside := pathutil.RelativeTo(dstResolved, wtResolved); !inside {
			log.Warn(fmt.Sprintf("Skipping .worktreeinclude entry that escapes worktree: %s", entry))
			continue
		}
		st, err := os.Stat(src)
		if err != nil {
			continue
		}
		if st.Mode().IsRegular() {
			if err := os.MkdirAll(filepath.Dir(dst), 0o777); err == nil {
				err = fsutil.CopyFile(src, dst)
			}
			if err != nil {
				log.Debug(fmt.Sprintf("Error copying .worktreeinclude entries: %s", errchain.OSText(err)))
				return
			}
			continue
		}
		if _, exists := os.Lstat(dst); !st.IsDir() || exists == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
			log.Debug(fmt.Sprintf("Error copying .worktreeinclude entries: %s", errchain.OSText(err)))
			return
		}
		if err := os.Symlink(srcResolved, dst); err != nil {
			if runtime.GOOS != "windows" {
				log.Debug(fmt.Sprintf("Error copying .worktreeinclude entries: %s", errchain.OSText(err)))
				return
			}
			log.Info(fmt.Sprintf(".worktreeinclude: symlink failed (%s) \u2014 falling back to copytree on Windows.", errchain.OSText(err)))
			if err := fsutil.CopyTree(srcResolved, dst, fsutil.CopyTreeOptions{Symlinks: true}); err != nil {
				log.Warn(fmt.Sprintf(".worktreeinclude: copy fallback also failed for %s -> %s: %s", src, dst, err))
			}
		}
	}
}

// Relative worktreeinclude entries of the tree that are symlinks back to the same entry of the main checkout
func includeSymlinkPaths(worktreePath, repoRoot string) map[string]bool {
	paths := map[string]bool{}
	for _, entry := range worktreeincludeEntries(repoRoot) {
		dst := filepath.Join(worktreePath, entry)
		if !isSymlink(dst) {
			continue
		}
		a, errA := pathutil.Resolve(dst)
		b, errB := pathutil.Resolve(filepath.Join(repoRoot, entry))
		if errA == nil && errB == nil && a == b {
			paths[strings.TrimRight(filepath.ToSlash(filepath.Clean(entry)), "/")] = true
		}
	}
	return paths
}

// Whether the tree has staged, unstaged, untracked or hidden index changes, failing safe toward true
func worktreeIsDirty(worktreePath, repoRoot string, timeout time.Duration) bool {
	env := noninteractiveRepoGitEnv(worktreePath, nil)
	if env == nil {
		return true
	}
	r, err := worktreeGit([]string{"status", "--porcelain", "-z", "--untracked-files=normal"}, worktreePath, timeout, env)
	if err != nil || r.ExitCode != 0 {
		return true
	}
	var entries []string
	for _, e := range strings.Split(r.Stdout, "\x00") {
		if e != "" {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		edits, err := HiddenIndexEdits(worktreePath, nil)
		return err != nil || len(edits) > 0
	}
	for _, e := range entries {
		if !strings.HasPrefix(e, "?? ") {
			return true
		}
	}
	links := includeSymlinkPaths(worktreePath, repoRoot)
	for _, e := range entries {
		if !links[strings.TrimRight(e[3:], "/")] {
			return true
		}
	}
	return false
}

// WorktreeIsDirty reports uncommitted work in a worktree
func WorktreeIsDirty(worktreePath, repoRoot string, timeout time.Duration) bool {
	return worktreeIsDirty(worktreePath, repoRoot, timeout)
}

// Whether the repository is a shallow clone, false when git cannot tell
func repoIsShallow(repoPath string, timeout time.Duration) bool {
	out, ok, err := worktreeGitOut([]string{"rev-parse", "--is-shallow-repository"}, repoPath, timeout)
	return err == nil && ok && out == "true"
}

// RepoIsShallow reports a shallow clone whose unpushed verdict is unreliable
func RepoIsShallow(repoPath string, timeout time.Duration) bool {
	return repoIsShallow(repoPath, timeout)
}

// Blobless unshallow so history verdicts hold, a plain unshallow when filters are refused, reports non shallow after
func deepenShallowRepo(repoRoot string, timeout time.Duration) bool {
	if !repoIsShallow(repoRoot, 5*time.Second) {
		return true
	}
	remotes, ok, err := worktreeGitOut([]string{"remote"}, repoRoot, 10*time.Second)
	if err != nil {
		envLog(nil).Debug(fmt.Sprintf("Deepening shallow repo failed (non-fatal): %s", err))
		return false
	}
	if !ok || remotes == "" {
		return false
	}
	var names []string
	for _, r := range textutil.Lines(remotes) {
		if r = strings.TrimSpace(r); r != "" {
			names = append(names, r)
		}
	}
	remote := names[0]
	if slices.Contains(names, "origin") {
		remote = "origin"
	}
	failed := func() bool {
		defer MarkUnmarkedPacksPromisor(repoRoot)
		for _, extra := range [][]string{{"--filter=blob:none"}, {}} {
			r, err := worktreeGit(append([]string{"fetch", remote, "--unshallow"}, extra...), repoRoot, timeout, NoninteractiveGitEnv(nil))
			if err != nil {
				if _, isTimeout := err.(*executil.TimeoutError); isTimeout {
					return true
				}
				envLog(nil).Debug(fmt.Sprintf("Deepening shallow repo failed (non-fatal): %s", err))
				return true
			}
			if r.ExitCode == 0 {
				break
			}
			label := ""
			if len(extra) > 0 {
				label = " " + strings.Join(extra, " ")
			}
			stderr := strings.TrimSpace(r.Stderr)
			envLog(nil).Debug(fmt.Sprintf("git fetch --unshallow%s failed: %s", label, textutil.Last(stderr, 500)))
		}
		return false
	}()
	if failed {
		return false
	}
	deepened := !repoIsShallow(repoRoot, 5*time.Second)
	if deepened {
		envLog(nil).Info(fmt.Sprintf("Deepened shallow clone at %s so worktree cleanup can verify push state", repoRoot))
	}
	return deepened
}

// Retained git cherry verdicts kept on disk, the module attribute tests patch
var worktreeMergeCacheMax = 1000

// Insertion ordered verdict map with dict update semantics
type mergeVerdicts struct {
	keys []string
	vals map[string]bool
}

func newMergeVerdicts() *mergeVerdicts { return &mergeVerdicts{vals: map[string]bool{}} }

func (m *mergeVerdicts) get(k string) (bool, bool) { v, ok := m.vals[k]; return v, ok }

func (m *mergeVerdicts) set(k string, v bool) {
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

func (m *mergeVerdicts) clone() *mergeVerdicts {
	c := &mergeVerdicts{keys: slices.Clone(m.keys), vals: make(map[string]bool, len(m.vals))}
	for k, v := range m.vals {
		c.vals[k] = v
	}
	return c
}

func (m *mergeVerdicts) update(o *mergeVerdicts) {
	for _, k := range o.keys {
		m.set(k, o.vals[k])
	}
}

// Path of the patch equivalence verdict cache of the active profile, swapped by tests
var worktreeMergeCachePath = func() string {
	return filepath.Join(home.Process(), "cache", "worktree_merge_verdicts.json")
}

// Loads the cherry verdict cache keeping only boolean verdicts, empty when missing or corrupt
func loadWorktreeMergeCache() *mergeVerdicts {
	out := newMergeVerdicts()
	raw, err := os.ReadFile(worktreeMergeCachePath())
	if err != nil {
		return out
	}
	text, err := textutil.DecodeUTF8(textutil.TrimBOM(raw))
	if err != nil {
		return out
	}
	v, err := jsonutil.Decode(text)
	obj, ok := v.(*jsonutil.Object)
	if err != nil || !ok {
		return out
	}
	entries, _ := obj.Get("verdicts")
	if eo, ok := entries.(*jsonutil.Object); ok {
		for _, k := range eo.Keys() {
			if b, ok := eo.Value(k).(bool); ok {
				out.set(k, b)
			}
		}
	}
	return out
}

// Atomically persists the newest verdicts up to the cache cap, never failing the caller
func saveWorktreeMergeCache(ctx context.Context, verdicts *mergeVerdicts) {
	keys := verdicts.keys[max(0, len(verdicts.keys)-worktreeMergeCacheMax):]
	inner := jsonutil.NewObject()
	for _, k := range keys {
		inner.Set(k, verdicts.vals[k])
	}
	payload := jsonutil.NewObject()
	payload.Set("version", int64(1))
	payload.Set("verdicts", inner)
	path := worktreeMergeCachePath()
	err := os.MkdirAll(filepath.Dir(path), 0o777)
	if err == nil {
		err = config.AtomicJSONWriteWith(ctx, path, payload, jsonutil.Spaced())
	}
	if err != nil {
		envLog(nil).Debug(fmt.Sprintf("Could not persist worktree merge cache: %s", err))
	}
}

// Whether every local only commit is patch equivalent to upstream, memoized on base, head and the ahead cap
func worktreeCommitsAllMergedUpstream(worktreePath string, timeout time.Duration, maxAhead int, cache *mergeVerdicts) bool {
	base, err := worktreeMergeBaseRef(worktreePath, timeout)
	if err != nil || base == "" {
		return false
	}
	cacheKey := ""
	if cache != nil {
		revs, _, err := worktreeGitOut([]string{"rev-parse", base + "^{commit}", "HEAD^{commit}"}, worktreePath, timeout)
		if err != nil {
			return false
		}
		if shas := strings.Fields(revs); len(shas) == 2 {
			cacheKey = fmt.Sprintf("%s..%s:%d", shas[0], shas[1], maxAhead)
			if v, ok := cache.get(cacheKey); ok {
				return v
			}
		}
	}
	memo := func(verdict bool) bool {
		if cache != nil && cacheKey != "" {
			cache.set(cacheKey, verdict)
		}
		return verdict
	}
	ahead, ok, err := worktreeGitOut([]string{"rev-list", "--count", base + "..HEAD"}, worktreePath, timeout)
	if err != nil || !ok {
		return false
	}
	if ahead == "" {
		ahead = "0"
	}
	n, err := strconv.ParseInt(strings.TrimSpace(ahead), 10, 64)
	if err != nil {
		return false
	}
	count := n
	if count == 0 {
		return memo(true)
	}
	if count > int64(maxAhead) {
		return memo(false)
	}
	cherry, err := worktreeGit([]string{"cherry", base, "HEAD"}, worktreePath, timeout, nil)
	if err != nil || cherry.ExitCode != 0 {
		return false
	}
	var lines []string
	for _, ln := range textutil.Lines(cherry.Stdout) {
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	all := len(lines) > 0
	for _, ln := range lines {
		all = all && strings.HasPrefix(ln, "-")
	}
	return memo(all)
}

// Checked out branch name, empty when detached or git fails
func worktreeCurrentBranch(worktreePath string, timeout time.Duration) (string, error) {
	branch, ok, err := worktreeGitOut([]string{"rev-parse", "--abbrev-ref", "HEAD"}, worktreePath, timeout)
	if err != nil || !ok || branch == "HEAD" {
		return "", err
	}
	return branch, nil
}

// Runs the gh cli for the merged pull request probe, swapped by tests
var worktreeGhRun = func(args []string, cwd string, timeout time.Duration) (*worktreeGitResult, error) {
	r, err := executil.Run(context.Background(), append([]string{"gh"}, args...), executil.Options{Dir: cwd, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	return &worktreeGitResult{r.ExitCode, strings.ToValidUTF8(string(r.Stdout), "\uFFFD"), strings.ToValidUTF8(string(r.Stderr), "\uFFFD")}, nil
}

// Whether the branch pull request is merged on GitHub, caching only true verdicts per branch and head
func worktreeBranchPRMerged(worktreePath string, timeout time.Duration, cache *mergeVerdicts) bool {
	branch, err := worktreeCurrentBranch(worktreePath, timeout)
	if err != nil || branch == "" {
		return false
	}
	cacheKey := ""
	if cache != nil {
		sha, _, err := worktreeGitOut([]string{"rev-parse", "HEAD"}, worktreePath, timeout)
		if err != nil {
			return false
		}
		if sha != "" {
			cacheKey = "pr-merged:" + branch + ":" + sha
			if v, ok := cache.get(cacheKey); ok && v {
				return true
			}
		}
	}
	r, err := worktreeGhRun([]string{"pr", "list", "--head", branch, "--state", "merged", "--json", "number", "--limit", "1"}, worktreePath, timeout)
	if err != nil || r.ExitCode != 0 {
		return false
	}
	text := r.Stdout
	if text == "" {
		text = "[]"
	}
	v, err := jsonutil.Decode(text)
	if err != nil {
		return false
	}
	prs, isList := v.([]any)
	merged := isList && len(prs) > 0
	if merged && cache != nil && cacheKey != "" {
		cache.set(cacheKey, true)
	}
	return merged
}

// Branch to sha map of every origin branch from one ls-remote, nil when origin cannot be read
func fetchRemoteBranchHeads(repoRoot string, timeout time.Duration) map[string]string {
	r, err := worktreeGit([]string{"ls-remote", "--heads", "origin"}, repoRoot, timeout, NoninteractiveGitEnv(nil))
	if err != nil || r.ExitCode != 0 {
		return nil
	}
	heads := map[string]string{}
	for _, line := range textutil.Lines(r.Stdout) {
		sha, ref, ok := strings.Cut(line, "\t")
		if ok && strings.HasPrefix(ref, "refs/heads/") {
			heads[strings.TrimSpace(ref[len("refs/heads/"):])] = strings.TrimSpace(sha)
		}
	}
	return heads
}

// Whether the branch head is exactly the sha origin holds for it
func worktreeBranchPushedExact(worktreePath string, remoteHeads map[string]string, timeout time.Duration) bool {
	if len(remoteHeads) == 0 {
		return false
	}
	branch, err := worktreeCurrentBranch(worktreePath, timeout)
	if err != nil || branch == "" {
		return false
	}
	remoteSha := remoteHeads[branch]
	if remoteSha == "" {
		return false
	}
	head, ok, err := worktreeGitOut([]string{"rev-parse", "HEAD"}, worktreePath, timeout)
	return err == nil && ok && head == remoteSha
}

var icebathLockRe = regexp.MustCompile(`(?s)^icebath (?:pid=(\p{Nd}+)|session=[^\t\n\v\f\r\x1c-\x1f\x85\p{Z}]+)(?: db=(.+))?$`)

// Relocks a tree its exiting launcher retains under the session that uses it
func retainWorktreeForSession(repoRoot, worktreePath, sessionID, dbPath string) bool {
	worktreeGitQuiet([]string{"worktree", "unlock", worktreePath}, repoRoot, 10*time.Second, "worktree unlock before retain failed", nil)
	r, err := worktreeGit([]string{"worktree", "lock", "--reason", fmt.Sprintf("icebath session=%s db=%s", sessionID, dbPath), worktreePath}, repoRoot, 10*time.Second, nil)
	if err != nil {
		envLog(nil).Debug(fmt.Sprintf("worktree retain lock failed for %s: %s", worktreePath, err))
		return false
	}
	return r.ExitCode == 0
}

// Native path identity of a spelling, the os.path.normcase tests patch
var insideTreeNormcase = pathutil.FoldCase

// Core count the classification pool is sized from, the os.cpu_count tests patch
var worktreeCPUCount = runtime.NumCPU

// Comparable key of a path under the native identity with forward slashes and no trailing slash
func insideTreeKey(value string) string {
	return strings.TrimRight(strings.ReplaceAll(insideTreeNormcase(value), `\`, "/"), "/")
}

// Reports whether cwd is the tree or under it by native path identity of its spelling and its resolved form
func insideTree(worktreePath string) func(cwd any) bool {
	roots := []string{insideTreeKey(filepath.Clean(worktreePath))}
	if resolved, err := pathutil.Resolve(worktreePath); err == nil {
		roots = append(roots, insideTreeKey(resolved))
	}
	return func(cwd any) bool {
		if !anyutil.IsTrue(cwd) {
			return false
		}
		var s string
		switch v := cwd.(type) {
		case string:
			s = v
		case []byte:
			s = string(v)
		default:
			s = anyutil.String(cwd)
		}
		k := insideTreeKey(s)
		for _, r := range roots {
			if k == r || strings.HasPrefix(k, r+"/") {
				return true
			}
		}
		return false
	}
}

var registerInsideTree = sync.OnceValue(func() error {
	return sqlite.RegisterDeterministicScalarFunction("icebath_inside_tree", 2, func(ctx *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		tree, _ := args[1].(string)
		if insideTree(tree)(args[0]) {
			return int64(1), nil
		}
		return int64(0), nil
	})
})

const servedTreeSQL = "SELECT 1 FROM sessions s WHERE icebath_inside_tree(s.cwd, ?1) AND (s.ended_at IS NULL OR EXISTS (" +
	"SELECT 1 FROM state_meta m WHERE m.key IN ('loop:'||s.id, 'heartbeat:'||s.id, 'goal:'||s.id) " +
	"AND (json_valid(m.value)=0 OR COALESCE(json_extract(m.value,'$.status'),'active')='active'))) LIMIT 1"

// Percent quotes a path the way urllib quote does with slash kept
func sqliteURIQuote(p string) string {
	var b strings.Builder
	for _, c := range []byte(p) {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("_.-~/", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Opens a session store read only with a two second busy timeout and the tree membership function
func openReadonlyStore(dbPath string) (*sql.DB, error) {
	if err := registerInsideTree(); err != nil {
		return nil, err
	}
	return sql.Open("sqlite", "file:"+sqliteURIQuote(dbPath)+"?mode=ro&_pragma=busy_timeout(2000)")
}

// Why the gateway still needs a retained tree, session, cron or unreadable, empty when nothing serves it
func treeStillServed(dbPath, worktreePath string) string {
	chats := map[string]bool{}
	if isRegularFile(dbPath) {
		err := func() error {
			db, err := openReadonlyStore(dbPath)
			if err != nil {
				return err
			}
			defer db.Close()
			var one int
			switch err := db.QueryRow(servedTreeSQL, worktreePath).Scan(&one); err {
			case nil:
				return errServed
			case sql.ErrNoRows:
			default:
				return err
			}
			rows, err := db.Query("SELECT chat_id FROM sessions WHERE icebath_inside_tree(cwd, ?1) AND chat_id IS NOT NULL", worktreePath)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var chat any
				if err := rows.Scan(&chat); err != nil {
					return err
				}
				chats[sqlText(chat)] = true
			}
			return rows.Err()
		}()
		if err == errServed {
			return "session"
		}
		if err != nil {
			envLog(nil).Debug(fmt.Sprintf("retained worktree serve check failed (%s); keeping tree", err))
			return "unreadable"
		}
	}
	if cronBoundToTree(worktreePath, chats) {
		return "cron"
	}
	return ""
}

var errServed = fmt.Errorf("served")

func sqlText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	}
	return anyutil.String(v)
}

// Installed by pkg/cron at init over the current store, the jobs.json path, its parsed payload and the runnable non terminal test
var WorktreeCronStore struct {
	JobsFile  func() string
	ParseJobs func(path string) (any, error)
	JobActive func(job *jsonutil.Object) bool
}

// Whether an enabled non terminal cron job runs inside the tree or is bound to a local chat of a session there
func cronBoundToTree(worktreePath string, chats map[string]bool) bool {
	if WorktreeCronStore.JobsFile == nil || WorktreeCronStore.ParseJobs == nil || WorktreeCronStore.JobActive == nil {
		return false
	}
	jobsFile := WorktreeCronStore.JobsFile()
	if _, err := os.Stat(jobsFile); err != nil {
		if os.IsNotExist(err) {
			return false
		}
		envLog(nil).Debug(fmt.Sprintf("cron store unreadable (%s); keeping tree", err))
		return true
	}
	data, err := WorktreeCronStore.ParseJobs(jobsFile)
	if err != nil {
		envLog(nil).Debug(fmt.Sprintf("cron store unreadable (%s); keeping tree", err))
		return true
	}
	jobs := data
	if o, ok := data.(*jsonutil.Object); ok {
		jobs, _ = o.Get("jobs")
	}
	list, _ := jobs.([]any)
	inside := insideTree(worktreePath)
	for _, j := range list {
		job, ok := j.(*jsonutil.Object)
		if !ok || !WorktreeCronStore.JobActive(job) {
			continue
		}
		originV, _ := job.Get("origin")
		origin, ok := originV.(*jsonutil.Object)
		if !ok {
			origin = jsonutil.NewObject()
		}
		workdir, _ := job.Get("workdir")
		if inside(workdir) {
			return true
		}
		platform, _ := origin.Get("platform")
		chat, _ := origin.Get("chat_id")
		if strings.ToLower(jsonStrOr(platform)) == "local" && chats[jsonStrOr(chat)] {
			return true
		}
	}
	return false
}

// Text of str(value or "") for a decoded JSON value
func jsonStrOr(v any) string {
	if !anyutil.IsTrue(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return anyutil.String(v)
}

// Whether a resumable session has its cwd inside the retained tree, pendingOnly counts pending gateway work alone
func retainedTreeIsLive(dbPath, worktreePath string, pendingOnly bool) bool {
	if !isRegularFile(dbPath) {
		return false
	}
	history := "s.message_count>0 OR "
	if pendingOnly {
		history = ""
	}
	query := "SELECT 1 FROM sessions s WHERE icebath_inside_tree(s.cwd, ?1) AND (" + history +
		"EXISTS (SELECT 1 FROM session_admissions a WHERE a.target_session_id=s.id AND a.status!='terminal')) LIMIT 1"
	db, err := openReadonlyStore(dbPath)
	if err == nil {
		defer db.Close()
		var one int
		err = db.QueryRow(query, worktreePath).Scan(&one)
		if err == nil {
			return true
		}
		if err == sql.ErrNoRows {
			return false
		}
	}
	envLog(nil).Debug(fmt.Sprintf("retained worktree session check failed (%s); keeping tree", err))
	return true
}

// Found flag and lock reason read raw from the tree admin dir, reason nil when unlocked
func worktreeLockReason(repoRoot, worktreePath string, timeout time.Duration) (bool, *string, error) {
	common, ok, err := worktreeGitOut([]string{"rev-parse", "--git-common-dir"}, repoRoot, timeout)
	if err != nil {
		return false, nil, err
	}
	if !ok || common == "" {
		return false, nil, fmt.Errorf("not a git repository")
	}
	dotGit := filepath.Join(worktreePath, ".git")
	if !isRegularFile(dotGit) {
		return false, nil, nil
	}
	raw, err := os.ReadFile(dotGit)
	if err != nil {
		return false, nil, err
	}
	link, err := textutil.DecodeUTF8(textutil.TrimBOM(raw))
	if err != nil {
		return false, nil, err
	}
	if !strings.HasPrefix(link, "gitdir:") {
		return false, nil, fmt.Errorf("unreadable .git file in %s", worktreePath)
	}
	target := strings.TrimSpace(link[len("gitdir:"):])
	if !filepath.IsAbs(target) {
		target = filepath.Join(worktreePath, target)
	}
	admin, err := pathutil.Resolve(target)
	if err != nil {
		return false, nil, err
	}
	commonPath := common
	if !filepath.IsAbs(commonPath) {
		commonPath = filepath.Join(repoRoot, commonPath)
	}
	worktrees, err := pathutil.Resolve(filepath.Join(commonPath, "worktrees"))
	if err != nil {
		return false, nil, err
	}
	if filepath.Dir(admin) != worktrees {
		return false, nil, nil
	}
	data, err := os.ReadFile(filepath.Join(admin, "locked"))
	if os.IsNotExist(err) {
		return true, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	text := strings.TrimPrefix(strings.ToValidUTF8(string(data), "\uFFFD"), "\ufeff")
	reason := strings.Trim(text, " \t\n\v\f\r")
	return true, &reason, nil
}

// Lock state of a tree, live while its owner pid runs or a resumable session works there, dead otherwise, empty when unlocked
func worktreeLockIsLive(repoRoot, worktreePath string, timeout time.Duration) string {
	found, reason, err := worktreeLockReason(repoRoot, worktreePath, timeout)
	if err != nil {
		return "live"
	}
	if !found || reason == nil {
		return ""
	}
	if !strings.HasPrefix(*reason, "icebath ") {
		return "dead"
	}
	m := icebathLockRe.FindStringSubmatchIndex(*reason)
	if m == nil {
		return "live"
	}
	hasPid, hasDB := m[2] >= 0, m[4] >= 0
	if hasPid {
		n, err := strconv.ParseInt(strings.TrimSpace((*reason)[m[2]:m[3]]), 10, 64)
		if err != nil {
			return "live"
		}
		pid := n
		if pid == int64(os.Getpid()) {
			return "live"
		}
		if pid > int64(^uint32(0)>>1) || proc.Exists(int(pid)) {
			return "live"
		}
	}
	if hasDB {
		if retainedTreeIsLive((*reason)[m[4]:m[5]], worktreePath, false) {
			return "live"
		}
		return "dead"
	}
	if hasPid {
		return "dead"
	}
	return "live"
}

var autoTreePrefixes = []string{"icebath-", "subagent-"}

// Idle hours before a tree may be reclaimed, the base tier for icebath made trees and three times that otherwise
func PruneTierHours(name string, maxAgeHours float64) float64 {
	for _, p := range autoTreePrefixes {
		if strings.HasPrefix(name, p) {
			return maxAgeHours
		}
	}
	return maxAgeHours * 3
}

// Tree past its soft cutoff with its mtime and whether it reached the hard tier
type pruneCandidate struct {
	entry string
	mtime float64
	force bool
}

var kanbanTreeRe = regexp.MustCompile(`^t_[0-9a-f]+`)

// Stat only age filter over the worktrees dir, kanban trees excluded and named trees aged on three times the tier
func pruneCandidates(worktreesDir string, maxAgeHours int, now float64) []pruneCandidate {
	ents, err := os.ReadDir(worktreesDir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var out []pruneCandidate
	for _, name := range names {
		entry := filepath.Join(worktreesDir, name)
		st, err := os.Stat(entry)
		if err != nil || !st.IsDir() || kanbanTreeRe.MatchString(name) {
			continue
		}
		tier := PruneTierHours(name, float64(maxAgeHours))
		mtime := float64(st.ModTime().UnixNano()) / 1e9
		if mtime > now-tier*3600 {
			continue
		}
		out = append(out, pruneCandidate{entry, mtime, mtime <= now-tier*3*3600})
	}
	return out
}

// Classified prune candidate with its verdict and lock state
type pruneVerdict struct {
	pruneCandidate
	verdict   string
	lockState string
}

// Parallel read only classification into dirty, unpushed, locked live, reap or reap keep branch with memoized cherry verdicts
func classifyPruneCandidates(ctx context.Context, repoRoot string, candidates []pruneCandidate) []pruneVerdict {
	mergeCache := loadWorktreeMergeCache()
	sizeBefore := len(mergeCache.keys)
	var cacheMu sync.Mutex
	getRemoteHeads := sync.OnceValue(func() map[string]string { return fetchRemoteBranchHeads(repoRoot, 10*time.Second) })
	classify := func(c pruneCandidate) pruneVerdict {
		if worktreeIsDirty(c.entry, repoRoot, 5*time.Second) {
			return pruneVerdict{c, "dirty", ""}
		}
		keepBranch := false
		if worktreeHasUnpushedCommits(c.entry, 5*time.Second) {
			cacheMu.Lock()
			snapshot := mergeCache.clone()
			cacheMu.Unlock()
			merged := worktreeCommitsAllMergedUpstream(c.entry, 30*time.Second, 20, snapshot)
			if !merged {
				merged = worktreeBranchPRMerged(c.entry, 15*time.Second, snapshot)
			}
			cacheMu.Lock()
			mergeCache.update(snapshot)
			cacheMu.Unlock()
			if !merged && !worktreeBranchPushedExact(c.entry, getRemoteHeads(), 10*time.Second) {
				return pruneVerdict{c, "unpushed", ""}
			}
			keepBranch = !merged
		}
		lockState := worktreeLockIsLive(repoRoot, c.entry, 5*time.Second)
		if lockState == "live" {
			return pruneVerdict{c, "locked-live", ""}
		}
		if keepBranch {
			return pruneVerdict{c, "reap-keep-branch", lockState}
		}
		return pruneVerdict{c, "reap", lockState}
	}
	verdicts := make([]pruneVerdict, len(candidates))
	workers := max(1, min(8, worktreeCPUCount(), len(candidates)))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, c := range candidates {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			verdicts[i] = classify(c)
		}()
	}
	wg.Wait()
	if len(mergeCache.keys) != sizeBefore {
		saveWorktreeMergeCache(ctx, mergeCache)
	}
	return verdicts
}

var preserveReasons = map[string]string{"dirty": "uncommitted changes", "unpushed": "unpushed commits"}

// Keep action and reason when deleting the clean tree would lose something no branch holds
func RemovalBlocked(path string) *RemovalBlock { return RemovalBlocker(path, nil, false) }

// Serial unlock, remove and branch delete of the reapable verdicts, returning preserved stale trees and kept branches
func reapPruneVerdicts(repoRoot string, verdicts []pruneVerdict, staleWorkCutoff float64) ([]string, map[string]bool) {
	var preservedStale []string
	keptBranches := map[string]bool{}
	for _, v := range verdicts {
		name := filepath.Base(v.entry)
		if reason, ok := preserveReasons[v.verdict]; ok {
			if v.mtime <= staleWorkCutoff {
				preservedStale = append(preservedStale, fmt.Sprintf("%s (%s)", name, reason))
			}
			continue
		}
		if v.verdict == "locked-live" {
			envLog(nil).Debug("Skipping live-locked worktree: " + name)
			continue
		}
		if blocked := RemovalBlocked(v.entry); blocked != nil {
			preservedStale = append(preservedStale, fmt.Sprintf("%s (%s)", name, blocked.Reason))
			continue
		}
		if v.lockState == "dead" {
			worktreeGitQuiet([]string{"worktree", "unlock", v.entry}, repoRoot, 10*time.Second, "Failed to unlock dead worktree "+name, nil)
		}
		err := func() error {
			br, err := worktreeGit([]string{"branch", "--show-current"}, v.entry, 5*time.Second, nil)
			if err != nil {
				return err
			}
			branch := strings.TrimSpace(br.Stdout)
			rm, err := worktreeGit([]string{"worktree", "remove", v.entry, "--force"}, repoRoot, 15*time.Second, nil)
			if err != nil {
				return err
			}
			if rm.ExitCode != 0 {
				envLog(nil).Debug(fmt.Sprintf("Failed to remove worktree %s: %s", name, strings.TrimSpace(rm.Stderr)))
				return nil
			}
			if branch != "" && v.verdict == "reap-keep-branch" {
				keptBranches[branch] = true
			} else if branch != "" {
				if _, err := worktreeGit([]string{"branch", "-D", branch}, repoRoot, 10*time.Second, NoninteractiveGitEnv(nil)); err != nil {
					return err
				}
			}
			envLog(nil).Debug(fmt.Sprintf("Pruned stale worktree: %s (force=%s)", name, anyutil.String(v.force)))
			return nil
		}()
		if err != nil {
			envLog(nil).Debug(fmt.Sprintf("Failed to prune worktree %s: %s", name, err))
		}
	}
	return preservedStale, keptBranches
}

// Removes stale worktrees and orphaned branches, then reclaims the dependency state of deleted checkouts
func PruneStaleWorktrees(ctx context.Context, repoRoot string, maxAgeHours int) error {
	defer reclaimOrphanInstallStates(ctx)
	return pruneStaleWorktreeTrees(ctx, repoRoot, maxAgeHours)
}

// Orphan install state reclaimer, swapped by tests
var collectOrphanInstallStates = func(ctx context.Context) ([]string, error) {
	return pm.CollectOrphanInstallStates(pm.InstallsRoot(ctx))
}

// Reclaims the dependency state dirs of checkouts removed by any path, never blocking a launch
func reclaimOrphanInstallStates(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			envLog(nil).Debug(fmt.Sprintf("Orphan install-state reclaim failed: %v", r))
		}
	}()
	reclaimed, err := collectOrphanInstallStates(ctx)
	if err != nil {
		envLog(nil).Debug(fmt.Sprintf("Orphan install-state reclaim failed: %s", err))
		return
	}
	if len(reclaimed) > 0 {
		envLog(nil).Info(fmt.Sprintf("Reclaimed %d dependency state dir(s) of deleted checkouts", len(reclaimed)))
	}
}

func pruneStaleWorktreeTrees(ctx context.Context, repoRoot string, maxAgeHours int) error {
	worktreesDir := filepath.Join(repoRoot, ".worktrees")
	if _, err := os.Stat(worktreesDir); err != nil {
		pruneOrphanedBranches(repoRoot, nil)
		return nil
	}
	if repoIsShallow(repoRoot, 5*time.Second) {
		deepenShallowRepo(repoRoot, 600*time.Second)
	}
	settings := SweepSettings()
	if !ClassicMode(settings) {
		if err := RunSweep(ctx, repoRoot, maxAgeHours, settings); err != nil {
			return err
		}
		warnWorktreeFootprint(repoRoot)
		return nil
	}
	now := float64(time.Now().UnixNano()) / 1e9
	candidates := pruneCandidates(worktreesDir, maxAgeHours, now)
	if len(candidates) == 0 {
		pruneOrphanedBranches(repoRoot, nil)
		return nil
	}
	verdicts := classifyPruneCandidates(ctx, repoRoot, candidates)
	preservedStale, keptBranches := reapPruneVerdicts(repoRoot, verdicts, now-7*24*3600)
	if len(preservedStale) > 0 {
		sorted := slices.Clone(preservedStale)
		sort.Strings(sorted)
		envLog(nil).Warn(fmt.Sprintf("Preserving %d worktree(s) older than 7 days with unmerged work "+
			"(run `icebath worktree prune` to review and reclaim): %s", len(preservedStale), strings.Join(sorted, ", ")))
	}
	pruneOrphanedBranches(repoRoot, keptBranches)
	warnWorktreeFootprint(repoRoot)
	return nil
}

// Warns once per launch when the worktrees dir holds ten or more trees or five gigabytes
func warnWorktreeFootprint(repoRoot string) {
	below := 10
	count, sizeMB := WorktreesSummary(repoRoot, &below)
	if count >= 10 || (sizeMB != nil && *sizeMB >= 5120) {
		sizeTxt := ""
		if sizeMB != nil && *sizeMB != 0 {
			sizeTxt = fmt.Sprintf(" (%.1fGB)", float64(*sizeMB)/1024)
		}
		envLog(nil).Warn(fmt.Sprintf(".worktrees/ holds %d tree(s)%s — run `icebath worktree list` "+
			"to audit and `icebath worktree prune` to reclaim safely.", count, sizeTxt))
	}
}

// Deletes local icebath and pr branches with no worktree, except the protected ones
func pruneOrphanedBranches(repoRoot string, protect map[string]bool) {
	listing, ok, err := worktreeGitOut([]string{"branch", "--format=%(refname:short)"}, repoRoot, 10*time.Second)
	if err != nil || !ok {
		return
	}
	var all []string
	for _, b := range strings.Split(listing, "\n") {
		if b = strings.TrimSpace(b); b != "" {
			all = append(all, b)
		}
	}
	active := map[string]bool{}
	wt, err := worktreeGit([]string{"worktree", "list", "--porcelain"}, repoRoot, 10*time.Second, nil)
	if err != nil {
		return
	}
	for _, line := range strings.Split(wt.Stdout, "\n") {
		if strings.HasPrefix(line, "branch refs/heads/") {
			_, after, _ := strings.Cut(line, "branch refs/heads/")
			active[strings.TrimSpace(after)] = true
		}
	}
	if cur, err := worktreeGit([]string{"branch", "--show-current"}, repoRoot, 5*time.Second, nil); err == nil {
		if c := strings.TrimSpace(cur.Stdout); c != "" {
			active[c] = true
		}
	}
	active["main"] = true
	var orphaned []string
	for _, b := range all {
		if !active[b] && !protect[b] && (strings.HasPrefix(b, "icebath/icebath-") || strings.HasPrefix(b, "pr-")) {
			orphaned = append(orphaned, b)
		}
	}
	if len(orphaned) == 0 {
		return
	}
	for i := 0; i < len(orphaned); i += 50 {
		worktreeGitQuiet(append([]string{"branch", "-D"}, orphaned[i:min(i+50, len(orphaned))]...), repoRoot, 30*time.Second,
			"Failed to prune orphaned branches", NoninteractiveGitEnv(nil))
	}
	envLog(nil).Debug(fmt.Sprintf("Pruned %d orphaned branches", len(orphaned)))
}
