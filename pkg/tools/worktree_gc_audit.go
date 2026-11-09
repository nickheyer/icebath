package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Branches never considered for deletion, in any mode
var gcProtectedBranches = map[string]bool{"main": true, "master": true, "develop": true, "dev": true, "trunk": true}

// Trees owned by the kanban dispatcher gc, never touched
var gcKanbanRe = regexp.MustCompile(`^t_[0-9a-f]+$`)

// A branch this far ahead of upstream is a stale base lane, not merged scratch
const gcMaxCherryAhead = 50

// Stderr of a git call whose repository filter discovery failed
const gitFilterDiscoveryFailed = "git filter discovery failed"

// Verdicts the reclaim removes
var gcReapVerdicts = map[string]bool{"reap": true, "reap-archive": true, "reap-keep-branch": true}

// One tree under .worktrees with its reclaim verdict
type TreeRecord struct {
	Name      string   `json:"name"`
	Path      string   `json:"path"`
	Branch    string   `json:"branch"`
	AgeDays   float64  `json:"age_days"`
	SizeMB    *int     `json:"size_mb"`
	Verdict   string   `json:"verdict"`
	Reason    string   `json:"reason"`
	Untracked []string `json:"untracked"`
}

// One local branch with its delete or keep verdict
type BranchRecord struct {
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// A linked worktree registered outside .worktrees, reported for visibility and never touched
type ExternalTreeRecord struct {
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Locked  bool   `json:"locked"`
	Missing bool   `json:"missing"`
}

// Runs git with the noninteractive repo env, 124 on timeout, 1 with discovery text on unreadable filters
func gcGit(args []string, cwd string, timeout time.Duration) (*worktreeGitResult, error) {
	env := NoninteractiveRepoGitEnv(cwd, nil)
	if env == nil {
		return &worktreeGitResult{Returncode: 1, Stderr: gitFilterDiscoveryFailed}, nil
	}
	r, err := worktreeGit(args, cwd, timeout, env)
	if err != nil {
		var expired *pysubprocess.TimeoutExpired
		if errors.As(err, &expired) {
			return &worktreeGitResult{Returncode: 124, Stderr: fmt.Sprintf("timeout after %ds", int(timeout/time.Second))}, nil
		}
		return nil, err
	}
	return r, nil
}

// Whether tracked files changed and which paths are untracked, a failed status read as real work
func gcDirtySplit(path string) (bool, []string) {
	r, err := gcGit([]string{"status", "--porcelain", "--untracked-files=normal"}, path, 10*time.Second)
	if err != nil || r.Returncode != 0 {
		return true, nil
	}
	var lines, untracked []string
	for _, line := range strings.Split(r.Stdout, "\n") {
		if pystr.Strip(line) == "" {
			continue
		}
		lines = append(lines, line)
		if strings.HasPrefix(line, "??") {
			untracked = append(untracked, pystr.Strip(pystr.Slice(line, 3, pystr.Len(line))))
		}
	}
	if len(untracked) == len(lines) {
		edits, err := HiddenIndexEdits(path, nil)
		if err != nil || len(edits) > 0 {
			return true, nil
		}
	}
	return len(untracked) != len(lines), untracked
}

// Copies untracked files out of a doomed tree under the active home, empty on any failure
func gcArchiveUntracked(ctx context.Context, tree string, untracked []string) string {
	stamp := time.Now().Format("20060102-150405")
	dest := filepath.Join(home.Dir(ctx), "archive", "worktree-prune", filepath.Base(tree)+"-"+stamp)
	for _, rel := range untracked {
		src := filepath.Join(tree, rel)
		lst, err := os.Lstat(src)
		if err != nil || lst.Mode()&os.ModeSymlink != 0 {
			continue
		}
		target := filepath.Join(dest, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			toolsLogger(nil).Warn(fmt.Sprintf("Could not archive untracked files from %s: %s", tree, err))
			return ""
		}
		if lst.IsDir() {
			err = pyshutil.CopyTree(src, target, false, true)
		} else {
			err = pyshutil.Copy2(src, target)
		}
		if err != nil {
			toolsLogger(nil).Warn(fmt.Sprintf("Could not archive untracked files from %s: %s", tree, err))
			return ""
		}
	}
	if _, err := os.Stat(dest); err != nil {
		return ""
	}
	return dest
}

// Verdict, reason and untracked paths of one tree under .worktrees
func gcClassifyTree(repoRoot, entry string, mergeCache *mergeVerdicts, remoteHeads map[string]string) (string, string, []string) {
	if gcKanbanRe.MatchString(filepath.Base(entry)) {
		return "keep", "kanban task tree (owned by kanban gc)", nil
	}
	if worktreeLockIsLive(repoRoot, entry, 5*time.Second) == "live" {
		return "keep", "in use by a running icebath session", nil
	}
	trackedDirty, untracked := gcDirtySplit(entry)
	if trackedDirty {
		return "keep", "uncommitted tracked changes (real work)", nil
	}
	archiveNote := fmt.Sprintf("%d untracked file(s) will be archived", len(untracked))
	if worktreeHasUnpushedCommits(entry, 5*time.Second) && !worktreeCommitsAllMergedUpstream(entry, 30*time.Second, gcMaxCherryAhead, mergeCache) {
		if !worktreeBranchPushedExact(entry, remoteHeads, 10*time.Second) {
			return "keep", "unpushed commits not found upstream", nil
		}
		if blocked := RemovalBlocked(entry); blocked != nil {
			return "keep", blocked.Reason, nil
		}
		if len(untracked) > 0 {
			return "reap-keep-branch", "pushed to origin (open-PR lane); branch kept; " + archiveNote, untracked
		}
		return "reap-keep-branch", "pushed to origin (open-PR lane); branch kept", nil
	}
	if blocked := RemovalBlocked(entry); blocked != nil {
		return "keep", blocked.Reason, nil
	}
	if len(untracked) > 0 {
		return "reap-archive", "merged/pushed; " + archiveNote, untracked
	}
	return "reap", "clean and fully merged/pushed", nil
}

func gcRealpath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// Linked worktrees registered outside .worktrees, read only, missing directories flagged
func AuditExternalTrees(repoRoot string) ([]ExternalTreeRecord, error) {
	r, err := gcGit([]string{"worktree", "list", "--porcelain"}, repoRoot, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if r.Returncode != 0 {
		return nil, nil
	}
	managedRoot := gcRealpath(filepath.Join(repoRoot, ".worktrees"))
	mainRoot := gcRealpath(repoRoot)
	var records []ExternalTreeRecord
	current := map[string]string{}
	locked := false
	flush := func() {
		path := current["path"]
		if path == "" {
			return
		}
		real := gcRealpath(path)
		if real == mainRoot || real == managedRoot || strings.HasPrefix(real, managedRoot+string(os.PathSeparator)) {
			return
		}
		branch := current["branch"]
		if branch == "" && current["head"] != "" {
			branch = "detached @" + pystr.Slice(current["head"], 0, 10)
		}
		_, statErr := os.Stat(path)
		records = append(records, ExternalTreeRecord{Path: path, Branch: branch, Locked: locked, Missing: statErr != nil})
	}
	for _, line := range strings.Split(r.Stdout, "\n") {
		line = pystr.Rstrip(line)
		switch {
		case line == "":
			flush()
			current, locked = map[string]string{}, false
		case strings.HasPrefix(line, "worktree "):
			current["path"] = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch refs/heads/"):
			current["branch"] = strings.TrimPrefix(line, "branch refs/heads/")
		case strings.HasPrefix(line, "HEAD "):
			current["head"] = strings.TrimPrefix(line, "HEAD ")
		case line == "locked" || strings.HasPrefix(line, "locked "):
			locked = true
		}
	}
	flush()
	return records, nil
}

// Drops registrations whose directory no longer exists, metadata only
func PruneMissingRegistrations(repoRoot string, dryRun bool) ([]string, error) {
	external, err := AuditExternalTrees(repoRoot)
	if err != nil {
		return nil, err
	}
	var stale []ExternalTreeRecord
	for _, r := range external {
		if r.Missing && !r.Locked {
			stale = append(stale, r)
		}
	}
	if len(stale) == 0 {
		return nil, nil
	}
	actions := make([]string, 0, len(stale))
	if dryRun {
		for _, r := range stale {
			actions = append(actions, "would prune stale registration "+r.Path)
		}
		return actions, nil
	}
	r, err := gcGit([]string{"worktree", "prune"}, repoRoot, 15*time.Second)
	if err != nil {
		return nil, err
	}
	if r.Returncode != 0 {
		return []string{"failed to prune stale registrations: " + pystr.Strip(r.Stderr)}, nil
	}
	for _, s := range stale {
		actions = append(actions, "pruned stale registration "+s.Path)
	}
	return actions, nil
}

// Classifies every tree under .worktrees without mutating anything, the age gate only ever keeping young trees
func AuditWorktrees(ctx context.Context, repoRoot string, withSizes bool, olderThanDays *float64) ([]TreeRecord, error) {
	worktreesDir := filepath.Join(repoRoot, ".worktrees")
	if _, err := os.Stat(worktreesDir); err != nil {
		return nil, nil
	}
	if repoIsShallow(repoRoot, 5*time.Second) {
		deepenShallowRepo(repoRoot, 600*time.Second)
	}
	mergeCache := loadWorktreeMergeCache()
	cacheSizeBefore := len(mergeCache.keys)
	remoteHeads := fetchRemoteBranchHeads(repoRoot, 20*time.Second)
	now := time.Now()
	entries, err := os.ReadDir(worktreesDir)
	if err != nil {
		return nil, err
	}
	var records []TreeRecord
	for _, entry := range entries {
		path := filepath.Join(worktreesDir, entry.Name())
		st, err := os.Stat(path)
		if err != nil || !st.IsDir() {
			continue
		}
		ageDays := now.Sub(st.ModTime()).Seconds() / 86400.0
		branch := ""
		if r, err := gcGit([]string{"branch", "--show-current"}, path, 5*time.Second); err == nil {
			branch = pystr.Strip(r.Stdout)
		}
		verdict, reason, untracked := gcClassifyTree(repoRoot, path, mergeCache, remoteHeads)
		if olderThanDays != nil && gcReapVerdicts[verdict] && ageDays < *olderThanDays {
			threshold, _ := pyrepr.Format(*olderThanDays, "g")
			verdict, reason, untracked = "keep", fmt.Sprintf("reapable but only %.1fd old (--older-than %s)", ageDays, threshold), nil
		}
		var size *int
		if withSizes {
			size = TreeSizeMB(path, 30*time.Second)
		}
		if untracked == nil {
			untracked = []string{}
		}
		records = append(records, TreeRecord{Name: entry.Name(), Path: path, Branch: branch, AgeDays: ageDays, SizeMB: size,
			Verdict: verdict, Reason: reason, Untracked: untracked})
	}
	if len(mergeCache.keys) != cacheSizeBefore {
		saveWorktreeMergeCache(mergeCache)
	}
	return records, nil
}

// Removes every reap verdict tree of a frozen audit list, archiving untracked scratch first
func ReclaimWorktrees(ctx context.Context, repoRoot string, dryRun bool, records []TreeRecord) ([]string, error) {
	if records == nil {
		audited, err := AuditWorktrees(ctx, repoRoot, false, nil)
		if err != nil {
			return nil, err
		}
		records = audited
	}
	var actions []string
	for _, record := range records {
		if !gcReapVerdicts[record.Verdict] {
			continue
		}
		if dryRun {
			actions = append(actions, fmt.Sprintf("would remove %s (%s)", record.Name, record.Reason))
			continue
		}
		if len(record.Untracked) > 0 {
			archive := gcArchiveUntracked(ctx, record.Path, record.Untracked)
			if archive == "" {
				actions = append(actions, fmt.Sprintf("kept %s (archive of untracked files failed)", record.Name))
				continue
			}
			actions = append(actions, fmt.Sprintf("archived %d untracked file(s) → %s", len(record.Untracked), archive))
		}
		gcGit([]string{"worktree", "unlock", record.Path}, repoRoot, 10*time.Second)
		remove, err := gcGit([]string{"worktree", "remove", record.Path, "--force"}, repoRoot, 30*time.Second)
		if err != nil {
			actions = append(actions, fmt.Sprintf("failed to remove %s: %s", record.Name, err))
			continue
		}
		if remove.Returncode != 0 {
			actions = append(actions, fmt.Sprintf("failed to remove %s: %s", record.Name, pystr.Strip(remove.Stderr)))
			continue
		}
		if record.Verdict == "reap-keep-branch" {
			actions = append(actions, fmt.Sprintf("removed %s (branch %s kept — pushed open-PR lane)", record.Name, record.Branch))
			continue
		}
		if record.Branch != "" && !gcProtectedBranches[record.Branch] {
			gcGit([]string{"branch", "-D", record.Branch}, repoRoot, 10*time.Second)
		}
		actions = append(actions, "removed "+record.Name)
	}
	if !dryRun {
		gcGit([]string{"worktree", "prune"}, repoRoot, 15*time.Second)
	}
	return actions, nil
}

func gcLines(r *worktreeGitResult) []string {
	var out []string
	for _, line := range strings.Split(r.Stdout, "\n") {
		if s := pystr.Strip(line); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Classifies every local branch by content reachability, deletable when merged or patch equivalent upstream and not checked out
func AuditBranches(repoRoot string) ([]BranchRecord, error) {
	if repoIsShallow(repoRoot, 5*time.Second) {
		deepenShallowRepo(repoRoot, 600*time.Second)
	}
	upstream, err := worktreeMergeBaseRef(repoRoot, 5*time.Second)
	if err != nil || upstream == "" {
		return nil, nil
	}
	r, err := gcGit([]string{"branch", "--format=%(refname:short)"}, repoRoot, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if r.Returncode != 0 {
		return nil, nil
	}
	branches := gcLines(r)
	wt, err := gcGit([]string{"worktree", "list", "--porcelain"}, repoRoot, 10*time.Second)
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	for _, line := range strings.Split(wt.Stdout, "\n") {
		if strings.HasPrefix(line, "branch refs/heads/") {
			active[pystr.Strip(strings.TrimPrefix(line, "branch refs/heads/"))] = true
		}
	}
	mergedOut, err := gcGit([]string{"branch", "--merged", upstream, "--format=%(refname:short)"}, repoRoot, 15*time.Second)
	if err != nil {
		return nil, err
	}
	merged := map[string]bool{}
	for _, b := range gcLines(mergedOut) {
		merged[b] = true
	}
	classify := func(branch string) (BranchRecord, error) {
		if gcProtectedBranches[branch] || active[branch] {
			return BranchRecord{branch, "keep", "protected or checked out"}, nil
		}
		if merged[branch] {
			return BranchRecord{branch, "delete", "fully merged into " + upstream}, nil
		}
		ahead, err := gcGit([]string{"rev-list", "--count", upstream + ".." + branch}, repoRoot, 10*time.Second)
		if err != nil {
			return BranchRecord{}, err
		}
		aheadCount, convErr := strconv.Atoi(pystr.Strip(ahead.Stdout))
		if pystr.Strip(ahead.Stdout) == "" {
			aheadCount, convErr = 0, nil
		}
		if convErr != nil {
			aheadCount = gcMaxCherryAhead + 1
		}
		if aheadCount == 0 {
			return BranchRecord{branch, "delete", "no commits beyond " + upstream}, nil
		}
		if aheadCount > gcMaxCherryAhead {
			return BranchRecord{branch, "keep", fmt.Sprintf("%d commits ahead (stale-base lane)", aheadCount)}, nil
		}
		cherry, err := gcGit([]string{"cherry", upstream, branch}, repoRoot, 30*time.Second)
		if err != nil {
			return BranchRecord{}, err
		}
		if cherry.Returncode != 0 {
			return BranchRecord{branch, "keep", "could not verify (git cherry failed)"}, nil
		}
		lines := gcLines(cherry)
		allEquivalent := len(lines) > 0
		unique := 0
		for _, ln := range lines {
			if !strings.HasPrefix(ln, "-") {
				allEquivalent = false
			}
			if strings.HasPrefix(ln, "+") {
				unique++
			}
		}
		if allEquivalent {
			return BranchRecord{branch, "delete", "all commits patch-equivalent upstream"}, nil
		}
		return BranchRecord{branch, "keep", fmt.Sprintf("%d unique commit(s) not upstream", unique)}, nil
	}
	workers := max(1, min(8, runtime.NumCPU(), len(branches)))
	records := make([]BranchRecord, len(branches))
	errs := make([]error, len(branches))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, b := range branches {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, b string) {
			defer wg.Done()
			defer func() { <-sem }()
			records[i], errs[i] = classify(b)
		}(i, b)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return records, nil
}

// Deletes every delete verdict branch of a frozen audit list
func ReclaimBranches(repoRoot string, dryRun bool, records []BranchRecord) ([]string, error) {
	if records == nil {
		audited, err := AuditBranches(repoRoot)
		if err != nil {
			return nil, err
		}
		records = audited
	}
	var actions []string
	for _, record := range records {
		if record.Verdict != "delete" {
			continue
		}
		if dryRun {
			actions = append(actions, fmt.Sprintf("would delete branch %s (%s)", record.Name, record.Reason))
			continue
		}
		r, err := gcGit([]string{"branch", "-D", record.Name}, repoRoot, 10*time.Second)
		if err != nil {
			return nil, err
		}
		if r.Returncode == 0 {
			actions = append(actions, "deleted branch "+record.Name)
		} else {
			actions = append(actions, fmt.Sprintf("failed to delete %s: %s", record.Name, pystr.Strip(r.Stderr)))
		}
	}
	return actions, nil
}

// Tree records ordered by descending size for the list view
func sortTreeRecordsBySize(records []TreeRecord) []TreeRecord {
	out := append([]TreeRecord{}, records...)
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := 0, 0
		if out[i].SizeMB != nil {
			si = *out[i].SizeMB
		}
		if out[j].SizeMB != nil {
			sj = *out[j].SizeMB
		}
		return si > sj
	})
	return out
}
