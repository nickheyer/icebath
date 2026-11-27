package worktrees

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/git"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Branches never deleted, in any mode
var protected = map[string]bool{"main": true, "master": true, "develop": true, "dev": true, "trunk": true}

// Trees the kanban board makes and cleans up itself, never touched here
var kanbanTree = regexp.MustCompile(`^t_[0-9a-f]+$`)

// A branch this far ahead of upstream is a lane on a stale base, not merged scratch
const cherryAhead = 50

// Errors of a git run refused because the repository's filter drivers could not be found with certainty
const FilterDiscoveryFailed = "The git filters of the repository could not be found for certain."

// Verdicts whose trees a reclaim removes
var reapVerdicts = map[string]bool{"reap": true, "reap-archive": true, "reap-keep-branch": true}

// One tree under .worktrees with its reclaim verdict
type TreeRecord struct {
	Name    string  `json:"name"`
	Path    string  `json:"path"`
	Branch  string  `json:"branch"`
	AgeDays float64 `json:"age_days"`
	SizeMB  *int    `json:"size_mb"`
	Verdict string  `json:"verdict"`
	Reason  string  `json:"reason"`
	// Untracked files archived before the tree goes
	Untracked []string `json:"untracked"`
}

// One local branch with its verdict, delete or keep
type BranchRecord struct {
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// A worktree registered outside .worktrees, shown and never touched
type ExternalTree struct {
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Locked  bool   `json:"locked"`
	Missing bool   `json:"missing"`
}

// Runs git in the folder hardened for its repository: exit status 124 for a run that timed out, 1 for a repository whose filter drivers
// could not be found with certainty
func gcGit(dir string, timeout time.Duration, args ...string) (*git.Result, error) {
	env := repoEnv(dir)
	if env == nil {
		return &git.Result{Code: 1, Stderr: FilterDiscoveryFailed}, nil
	}
	result, err := run(dir, env, timeout, args...)
	if _, ok := errors.AsType[*git.TimeoutError](err); ok {
		return &git.Result{Code: 124, Stderr: "git took longer than " + strs.Span(timeout) + "."}, nil
	}
	return result, err
}

// Whether tracked files changed, and the untracked paths; a status that failed counts as real work
func dirtySplit(path string) (bool, []string) {
	result, err := gcGit(path, 10*time.Second, "status", "--porcelain", "--untracked-files=normal")
	if err != nil || result.Code != 0 {
		return true, nil
	}
	var lines, others []string
	for line := range strings.Lines(result.Stdout) {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
		if rest, ok := strings.CutPrefix(line, "??"); ok {
			others = append(others, strings.TrimSpace(rest))
		}
	}
	if len(others) == len(lines) {
		if edits, err := hiddenEdits(path, nil); err != nil || len(edits) > 0 {
			return true, nil
		}
	}
	return len(others) != len(lines), others
}

// Copies the untracked files of a tree about to go into the profile's archive folder: where, empty on any failure
func archiveUntracked(cfg *config.Config, tree string, paths []string) string {
	dest := filepath.Join(profileDir(cfg), "archive", "worktree-prune", filepath.Base(tree)+"-"+time.Now().Format("20060102-150405"))
	for _, rel := range paths {
		src := filepath.Join(tree, rel)
		info, err := os.Lstat(src)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		target := filepath.Join(dest, rel)
		if err = os.MkdirAll(filepath.Dir(target), 0o777); err == nil {
			if info.IsDir() {
				err = copyTree(src, target)
			} else {
				err = copyFile(src, target)
			}
		}
		if err != nil {
			slog.Warn("Could not archive untracked files from "+tree, "error", err)
			return ""
		}
	}
	if _, err := os.Stat(dest); err != nil {
		return ""
	}
	return dest
}

// Verdict, reason and untracked paths of one tree under .worktrees
func (w *Worktrees) classifyTree(repo, entry string, cache *verdicts, heads map[string]string) (string, string, []string) {
	if kanbanTree.MatchString(filepath.Base(entry)) {
		return "keep", "a kanban task tree, which the kanban cleanup removes", nil
	}
	if w.lockState(repo, entry, 5*time.Second) == "live" {
		return "keep", "in use by a running icebath session", nil
	}
	tracked, others := dirtySplit(entry)
	if tracked {
		return "keep", "uncommitted changes to tracked files", nil
	}
	note := strs.Count(len(others), "untracked file") + " to archive first"
	if Unpushed(entry, 5*time.Second) && !allMerged(entry, 30*time.Second, cherryAhead, cache) {
		if !pushedExact(entry, heads, 10*time.Second) {
			return "keep", "commits not found upstream", nil
		}
		if blocked := Blocked(entry); blocked != nil {
			return "keep", blocked.Reason, nil
		}
		if len(others) > 0 {
			return "reap-keep-branch", "pushed to origin for a pull request, branch kept, " + note, others
		}
		return "reap-keep-branch", "pushed to origin for a pull request, branch kept", nil
	}
	if blocked := Blocked(entry); blocked != nil {
		return "keep", blocked.Reason, nil
	}
	if len(others) > 0 {
		return "reap-archive", "merged or pushed, " + note, others
	}
	return "reap", "clean, and fully merged or pushed", nil
}

// Path with its links resolved, absolute, as it is when that fails
func realOr(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// Worktrees of the repository registered outside .worktrees, those whose folder is gone flagged
func External(repo string) ([]ExternalTree, error) {
	result, err := gcGit(repo, 10*time.Second, "worktree", "list", "--porcelain")
	if err != nil || result.Code != 0 {
		return nil, err
	}
	managed, main := realOr(filepath.Join(repo, ".worktrees")), realOr(repo)
	var records []ExternalTree
	current := map[string]string{}
	locked := false
	flush := func() {
		path := current["path"]
		if path == "" {
			return
		}
		if resolved := realOr(path); resolved == main || resolved == managed || strings.HasPrefix(resolved, managed+string(os.PathSeparator)) {
			return
		}
		branch := current["branch"]
		if branch == "" && current["head"] != "" {
			branch = "detached @" + current["head"][:min(10, len(current["head"]))]
		}
		_, err := os.Stat(path)
		records = append(records, ExternalTree{Path: path, Branch: branch, Locked: locked, Missing: err != nil})
	}
	for line := range strings.Lines(result.Stdout) {
		line = strings.TrimRight(line, " \t\r\n")
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

// Drops the registrations of worktrees whose folder is gone, metadata only, or names them for a dry run
func PruneMissing(repo string, dryRun bool) ([]string, error) {
	external, err := External(repo)
	if err != nil {
		return nil, err
	}
	var stale []ExternalTree
	for _, tree := range external {
		if tree.Missing && !tree.Locked {
			stale = append(stale, tree)
		}
	}
	if len(stale) == 0 {
		return nil, nil
	}
	var actions []string
	if dryRun {
		for _, tree := range stale {
			actions = append(actions, "Would forget the missing worktree "+tree.Path+".")
		}
		return actions, nil
	}
	result, err := gcGit(repo, 15*time.Second, "worktree", "prune")
	if err != nil {
		return nil, err
	}
	if result.Code != 0 {
		return []string{"The missing worktrees could not be forgotten. git said: " + strings.TrimSpace(result.Stderr)}, nil
	}
	for _, tree := range stale {
		actions = append(actions, "Forgot the missing worktree "+tree.Path+".")
	}
	return actions, nil
}

// Classifies every tree under .worktrees without changing anything; olderThanDays, when given, only keeps reapable trees younger than it
func (w *Worktrees) Audit(cfg *config.Config, repo string, sizes bool, olderThanDays *float64) ([]TreeRecord, error) {
	dir := filepath.Join(repo, ".worktrees")
	if _, err := os.Stat(dir); err != nil {
		return nil, nil
	}
	if Shallow(repo, 5*time.Second) {
		deepen(repo, 10*time.Minute)
	}
	cache := loadVerdicts(cfg)
	heads := remoteHeads(repo, 20*time.Second)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var records []TreeRecord
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		age := time.Since(info.ModTime()).Hours() / 24
		branch := ""
		if result, err := gcGit(path, 5*time.Second, "branch", "--show-current"); err == nil {
			branch = strings.TrimSpace(result.Stdout)
		}
		verdict, reason, others := w.classifyTree(repo, path, cache, heads)
		if olderThanDays != nil && reapVerdicts[verdict] && age < *olderThanDays {
			verdict, reason, others = "keep", fmt.Sprintf("could go, but is only %s old, younger than --older-than %s", strs.Counted(math.Round(age*10)/10, "days"),
				strconv.FormatFloat(*olderThanDays, 'f', -1, 64)), nil
		}
		var mb *int
		if sizes {
			mb = SizeMB(path, 30*time.Second)
		}
		if others == nil {
			others = []string{}
		}
		records = append(records, TreeRecord{Name: entry.Name(), Path: path, Branch: branch, AgeDays: age, SizeMB: mb, Verdict: verdict, Reason: reason,
			Untracked: others})
	}
	cache.save()
	return records, nil
}

// Removes every tree of an audit whose verdict reaps it, its untracked scratch archived first, or names them for a dry run
func (w *Worktrees) Reclaim(cfg *config.Config, repo string, dryRun bool, records []TreeRecord) []string {
	var actions []string
	for _, record := range records {
		if !reapVerdicts[record.Verdict] {
			continue
		}
		if dryRun {
			actions = append(actions, fmt.Sprintf("Would remove %s: %s.", record.Name, record.Reason))
			continue
		}
		if len(record.Untracked) > 0 {
			archive := archiveUntracked(cfg, record.Path, record.Untracked)
			if archive == "" {
				actions = append(actions, fmt.Sprintf("Kept %s, because its untracked files could not be archived.", record.Name))
				continue
			}
			actions = append(actions, fmt.Sprintf("Archived %s of %s to %s.", strs.Count(len(record.Untracked), "untracked file"), record.Name, archive))
		}
		w.Hooks().Release(record.Path)
		gcGit(repo, 10*time.Second, "worktree", "unlock", record.Path)
		removed, err := gcGit(repo, 30*time.Second, "worktree", "remove", record.Path, "--force")
		switch {
		case err != nil:
			actions = append(actions, fmt.Sprintf("%s could not be removed: %s", record.Name, err))
			continue
		case removed.Code != 0:
			actions = append(actions, fmt.Sprintf("%s could not be removed. git said: %s", record.Name, strings.TrimSpace(removed.Stderr)))
			continue
		case record.Verdict == "reap-keep-branch":
			actions = append(actions, fmt.Sprintf("Removed %s and kept its branch %s, which is pushed for a pull request.", record.Name, record.Branch))
			continue
		}
		if record.Branch != "" && !protected[record.Branch] {
			gcGit(repo, 10*time.Second, "branch", "-D", record.Branch)
		}
		actions = append(actions, "Removed "+record.Name+".")
	}
	if !dryRun {
		gcGit(repo, 15*time.Second, "worktree", "prune")
	}
	return actions
}

// Classifies every local branch by what of it upstream holds: deleted when merged or every commit is the same change as one upstream, and
// not checked out anywhere
func Branches(repo string) ([]BranchRecord, error) {
	if Shallow(repo, 5*time.Second) {
		deepen(repo, 10*time.Minute)
	}
	upstream, err := mergeBase(repo, 5*time.Second)
	if err != nil || upstream == "" {
		return nil, nil
	}
	listed, err := gcGit(repo, 10*time.Second, "branch", "--format=%(refname:short)")
	if err != nil || listed.Code != 0 {
		return nil, err
	}
	branches := git.Lines(listed.Stdout)
	trees, err := gcGit(repo, 10*time.Second, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	for line := range strings.Lines(trees.Stdout) {
		if branch, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "branch refs/heads/"); ok {
			active[strings.TrimSpace(branch)] = true
		}
	}
	mergedOut, err := gcGit(repo, 15*time.Second, "branch", "--merged", upstream, "--format=%(refname:short)")
	if err != nil {
		return nil, err
	}
	merged := map[string]bool{}
	for _, branch := range git.Lines(mergedOut.Stdout) {
		merged[branch] = true
	}
	classify := func(branch string) (BranchRecord, error) {
		switch {
		case protected[branch] || active[branch]:
			return BranchRecord{branch, "keep", "protected or checked out"}, nil
		case merged[branch]:
			return BranchRecord{branch, "delete", "fully merged into " + upstream}, nil
		}
		ahead, err := gcGit(repo, 10*time.Second, "rev-list", "--count", upstream+".."+branch)
		if err != nil {
			return BranchRecord{}, err
		}
		count, err := strconv.Atoi(cmp.Or(strings.TrimSpace(ahead.Stdout), "0"))
		if err != nil {
			count = cherryAhead + 1
		}
		switch {
		case count == 0:
			return BranchRecord{branch, "delete", "no commits beyond " + upstream}, nil
		case count > cherryAhead:
			return BranchRecord{branch, "keep", strs.Count(count, "commit") + " ahead, on an old base"}, nil
		}
		cherry, err := gcGit(repo, 30*time.Second, "cherry", upstream, branch)
		if err != nil {
			return BranchRecord{}, err
		}
		if cherry.Code != 0 {
			return BranchRecord{branch, "keep", "could not be checked, because git cherry failed"}, nil
		}
		lines := git.Lines(cherry.Stdout)
		unique := 0
		for _, line := range lines {
			if strings.HasPrefix(line, "+") {
				unique++
			}
		}
		if len(lines) > 0 && !slices.ContainsFunc(lines, func(line string) bool { return !strings.HasPrefix(line, "-") }) {
			return BranchRecord{branch, "delete", "every commit has the same change upstream"}, nil
		}
		return BranchRecord{branch, "keep", strs.Count(unique, "commit") + " not upstream"}, nil
	}
	records := make([]BranchRecord, len(branches))
	errs := make([]error, len(branches))
	slots := make(chan struct{}, max(1, min(8, runtime.NumCPU(), len(branches))))
	var wg sync.WaitGroup
	for i, branch := range branches {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			records[i], errs[i] = classify(branch)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return records, nil
}

// Deletes every branch of an audit whose verdict deletes it, or names them for a dry run
func ReclaimBranches(repo string, dryRun bool, records []BranchRecord) ([]string, error) {
	var actions []string
	for _, record := range records {
		if record.Verdict != "delete" {
			continue
		}
		if dryRun {
			actions = append(actions, fmt.Sprintf("Would delete the branch %s: %s.", record.Name, record.Reason))
			continue
		}
		result, err := gcGit(repo, 10*time.Second, "branch", "-D", record.Name)
		if err != nil {
			return nil, err
		}
		if result.Code == 0 {
			actions = append(actions, "Deleted the branch "+record.Name+".")
		} else {
			actions = append(actions, fmt.Sprintf("The branch %s could not be deleted. git said: %s", record.Name, strings.TrimSpace(result.Stderr)))
		}
	}
	return actions, nil
}

// Megabytes a folder takes, through du where there is one, else by walking it; nil when it could not be measured in time
func SizeMB(path string, timeout time.Duration) *int {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if du, err := exec.LookPath("du"); err == nil {
		out, err := exec.CommandContext(ctx, du, "-sm", path).Output()
		fields := strings.Fields(string(out))
		if err != nil || len(fields) == 0 {
			return nil
		}
		mb, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil
		}
		return &mb
	}
	var total int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		return nil
	}
	mb := int((total + 1024*1024 - 1) / (1024 * 1024))
	return &mb
}

// How many trees .worktrees holds and the megabytes they take, not measured once they are at least sizeBelow
func Summary(repo string, sizeBelow int) (int, *int) {
	dir := filepath.Join(repo, ".worktrees")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, nil
	}
	count := 0
	for _, entry := range entries {
		if fsx.IsDir(filepath.Join(dir, entry.Name())) {
			count++
		}
	}
	if sizeBelow > 0 && count >= sizeBelow {
		return count, nil
	}
	return count, SizeMB(dir, 20*time.Second)
}
