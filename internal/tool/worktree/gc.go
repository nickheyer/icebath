package worktree

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

const maxCherryAhead = 50

var reapable = map[string]bool{"reap": true, "reap-archive": true, "reap-keep-branch": true}

type TreeRecord struct {
	Name      string   `json:"name"`
	Path      string   `json:"path"`
	Branch    string   `json:"branch"`
	AgeDays   float64  `json:"age_days"`
	SizeMB    *int64   `json:"size_mb"`
	Verdict   string   `json:"verdict"`
	Reason    string   `json:"reason"`
	Untracked []string `json:"untracked"`
}

type BranchRecord struct {
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

type ExternalTreeRecord struct {
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Locked  bool   `json:"locked"`
	Missing bool   `json:"missing"`
}

func (m *Manager) gcGit(ctx context.Context, cwd string, timeout time.Duration, args ...string) (*result, error) {
	env, trusted, err := m.repoEnv(ctx, cwd)
	if err != nil {
		return nil, err
	}
	if !trusted {
		return &result{args: args, code: 1, stderr: []byte(filterDiscoveryText)}, nil
	}
	r, err := m.git(ctx, cwd, timeout, env, args...)
	var timedOut *DeadlineError
	if errors.As(err, &timedOut) {
		return &result{args: args, code: 124, stderr: []byte(fmt.Sprintf("timeout after %gs", timeout.Seconds()))}, nil
	}
	return r, err
}

func nonBlankLines(text string) []string {
	var lines []string
	for _, ln := range strings.Split(text, "\n") {
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	return lines
}

func (m *Manager) dirtySplit(ctx context.Context, path string) (bool, []string, error) {
	r, err := m.gcGit(ctx, path, 10*time.Second, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		if fatal(ctx, err) {
			return false, nil, err
		}
		return true, nil, nil
	}
	if !r.ok() {
		return true, nil, nil
	}
	lines := nonBlankLines(r.text())
	var untracked []string
	for _, line := range lines {
		if strings.HasPrefix(line, "??") {
			rest := ""
			if len(line) > 3 {
				rest = line[3:]
			}
			untracked = append(untracked, strings.TrimSpace(rest))
		}
	}
	if len(untracked) == len(lines) {
		edits, err := m.hiddenIndexEdits(ctx, path, nil)
		if err != nil {
			if fatal(ctx, err) {
				return false, nil, err
			}
			return true, nil, nil
		}
		if len(edits) > 0 {
			return true, nil, nil
		}
	}
	return len(untracked) != len(lines), untracked, nil
}

func unquoteCPath(s string) string {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	body := s[1 : len(s)-1]
	var out []byte
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' || i+1 >= len(body) {
			out = append(out, c)
			continue
		}
		i++
		switch e := body[i]; e {
		case 'a':
			out = append(out, 7)
		case 'b':
			out = append(out, 8)
		case 't':
			out = append(out, '\t')
		case 'n':
			out = append(out, '\n')
		case 'v':
			out = append(out, 11)
		case 'f':
			out = append(out, 12)
		case 'r':
			out = append(out, '\r')
		default:
			if e >= '0' && e <= '7' && i+2 < len(body) {
				if v, err := strconv.ParseUint(body[i:i+3], 8, 8); err == nil {
					out = append(out, byte(v))
					i += 2
					continue
				}
			}
			out = append(out, e)
		}
	}
	return string(out)
}

func (m *Manager) archiveUntracked(ctx context.Context, tree string, untracked []string) (string, error) {
	h, err := m.home(ctx)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(home.ArchivedWorktrees.In(h), filepath.Base(tree)+"-"+time.Now().Format("20060102-150405"))
	for _, quoted := range untracked {
		rel := unquoteCPath(quoted)
		src := filepath.Join(tree, rel)
		info, err := os.Lstat(src)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if st, err := os.Stat(src); err != nil {
			continue
		} else {
			info = st
		}
		target := filepath.Join(dest, rel)
		if err := m.homes.Mkdir(ctx, filepath.Dir(target)); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			slog.Warn("Could not archive untracked files", "tree", tree, "error", err)
			return "", nil
		}
		if info.IsDir() {
			err = fileutil.CopyTree(src, target, fileutil.CopyTreeOptions{MergeInto: true})
		} else {
			err = fileutil.CopyFile(src, target)
		}
		if err != nil {
			slog.Warn("Could not archive untracked files", "tree", tree, "error", err)
			return "", nil
		}
	}
	if _, err := os.Stat(dest); err != nil {
		return "", nil
	}
	return dest, nil
}

type remoteHeadsView struct {
	heads map[string]string
	known bool
}

func (m *Manager) classifyTree(ctx context.Context, repoRoot, path, name string, cache *verdictCache, remote remoteHeadsView) (string, string, []string, error) {
	if kanbanTree.MatchString(name) {
		return "keep", "kanban task tree (owned by kanban gc)", nil, nil
	}
	lock, err := m.lockState(ctx, repoRoot, path, 5*time.Second)
	if err != nil {
		return "", "", nil, err
	}
	if lock == lockLive {
		return "keep", "in use by a running icebath session", nil, nil
	}
	trackedDirty, untracked, err := m.dirtySplit(ctx, path)
	if err != nil {
		return "", "", nil, err
	}
	if trackedDirty {
		return "keep", "uncommitted tracked changes (real work)", nil, nil
	}
	note := fmt.Sprintf("%d untracked file(s) will be archived", len(untracked))
	unpushed, err := m.hasUnpushedCommits(ctx, path, 5*time.Second)
	if err != nil {
		return "", "", nil, err
	}
	merged := true
	if unpushed {
		if merged, err = m.commitsAllMergedUpstream(ctx, path, 30*time.Second, maxCherryAhead, cache); err != nil {
			return "", "", nil, err
		}
	}
	if unpushed && !merged {
		heads := remote.heads
		if !remote.known {
			heads = nil
		}
		exact, err := m.branchPushedExact(ctx, path, heads, 10*time.Second)
		if err != nil {
			return "", "", nil, err
		}
		if !exact {
			return "keep", "unpushed commits not found upstream", nil, nil
		}
		blocked, err := m.RemovalBlocked(ctx, path)
		if err != nil {
			return "", "", nil, err
		}
		if blocked != nil {
			return "keep", blocked.Reason, nil, nil
		}
		if len(untracked) > 0 {
			return "reap-keep-branch", "pushed to origin (open-PR lane); branch kept; " + note, untracked, nil
		}
		return "reap-keep-branch", "pushed to origin (open-PR lane); branch kept", nil, nil
	}
	blocked, err := m.RemovalBlocked(ctx, path)
	if err != nil {
		return "", "", nil, err
	}
	if blocked != nil {
		return "keep", blocked.Reason, nil, nil
	}
	if len(untracked) > 0 {
		return "reap-archive", "merged/pushed; " + note, untracked, nil
	}
	return "reap", "clean and fully merged/pushed", nil, nil
}

func (m *Manager) AuditExternalTrees(ctx context.Context, repoRoot string) ([]*ExternalTreeRecord, error) {
	r, err := m.gcGit(ctx, repoRoot, 10*time.Second, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	if !r.ok() {
		return nil, nil
	}
	managed := resolvePath(filepath.Join(repoRoot, ".worktrees"))
	mainRoot := resolvePath(repoRoot)
	var records []*ExternalTreeRecord
	current := map[string]string{}
	locked := false
	flush := func() {
		path := current["path"]
		if path == "" {
			return
		}
		real := resolvePath(path)
		if real == mainRoot || real == managed || strings.HasPrefix(real, managed+string(filepath.Separator)) {
			return
		}
		branch := current["branch"]
		if branch == "" && current["head"] != "" {
			head := current["head"]
			if len(head) > 10 {
				head = head[:10]
			}
			branch = "detached @" + head
		}
		_, statErr := os.Stat(path)
		records = append(records, &ExternalTreeRecord{Path: path, Branch: branch, Locked: locked, Missing: statErr != nil})
	}
	for _, line := range strings.Split(r.text(), "\n") {
		line = strings.TrimRight(line, " \t\r\n\v\f")
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

func (m *Manager) PruneMissingRegistrations(ctx context.Context, repoRoot string, dryRun bool) ([]string, error) {
	external, err := m.AuditExternalTrees(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	var stale []*ExternalTreeRecord
	for _, r := range external {
		if r.Missing && !r.Locked {
			stale = append(stale, r)
		}
	}
	if len(stale) == 0 {
		return nil, nil
	}
	var actions []string
	if dryRun {
		for _, r := range stale {
			actions = append(actions, "would prune stale registration "+r.Path)
		}
		return actions, nil
	}
	res, err := m.gcGit(ctx, repoRoot, 15*time.Second, "worktree", "prune")
	if err != nil {
		return nil, err
	}
	if !res.ok() {
		return []string{"failed to prune stale registrations: " + res.errText()}, nil
	}
	for _, r := range stale {
		actions = append(actions, "pruned stale registration "+r.Path)
	}
	return actions, nil
}

func (m *Manager) AuditWorktrees(ctx context.Context, repoRoot string, withSizes bool, olderThanDays *float64) ([]*TreeRecord, error) {
	worktreesDir := filepath.Join(repoRoot, ".worktrees")
	if _, err := os.Stat(worktreesDir); err != nil {
		return nil, nil
	}
	shallow, err := m.repoIsShallow(ctx, repoRoot, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if shallow {
		if _, err := m.deepenShallowRepo(ctx, repoRoot, 600*time.Second); err != nil {
			return nil, err
		}
	}
	cache, err := m.loadMergeCache(ctx)
	if err != nil {
		return nil, err
	}
	sizeBefore := cache.size()
	heads, known, err := m.fetchRemoteHeads(ctx, repoRoot, 20*time.Second)
	if err != nil {
		return nil, err
	}
	remote := remoteHeadsView{heads: heads, known: known}
	now := epoch(time.Now())
	names, err := sortedEntries(worktreesDir)
	if err != nil {
		return nil, err
	}
	var records []*TreeRecord
	for _, name := range names {
		path := filepath.Join(worktreesDir, name)
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		ageDays := (now - epoch(info.ModTime())) / 86400
		branch := ""
		if br, err := m.gcGit(ctx, path, 5*time.Second, "branch", "--show-current"); err == nil {
			branch = br.out()
		} else if fatal(ctx, err) {
			return nil, err
		}
		verdict, reason, untracked, err := m.classifyTree(ctx, repoRoot, path, name, cache, remote)
		if err != nil {
			return nil, err
		}
		if olderThanDays != nil && reapable[verdict] && ageDays < *olderThanDays {
			verdict, reason, untracked = "keep", fmt.Sprintf("reapable but only %.1fd old (--older-than %g)", ageDays, *olderThanDays), nil
		}
		var size *int64
		if withSizes {
			if size, err = m.treeSizeMB(ctx, path, 30*time.Second); err != nil {
				return nil, err
			}
		}
		if untracked == nil {
			untracked = []string{}
		}
		records = append(records, &TreeRecord{Name: name, Path: path, Branch: branch, AgeDays: ageDays, SizeMB: size, Verdict: verdict, Reason: reason, Untracked: untracked})
	}
	if cache.size() != sizeBefore {
		if err := m.saveMergeCache(ctx, cache); err != nil {
			return nil, err
		}
	}
	return records, nil
}

func (m *Manager) ReclaimWorktrees(ctx context.Context, repoRoot string, dryRun bool, records []*TreeRecord) ([]string, error) {
	var actions []string
	for _, record := range records {
		if !reapable[record.Verdict] {
			continue
		}
		if dryRun {
			actions = append(actions, fmt.Sprintf("would remove %s (%s)", record.Name, record.Reason))
			continue
		}
		if len(record.Untracked) > 0 {
			archive, err := m.archiveUntracked(ctx, record.Path, record.Untracked)
			if err != nil {
				return actions, err
			}
			if archive == "" {
				actions = append(actions, fmt.Sprintf("kept %s (archive of untracked files failed)", record.Name))
				continue
			}
			actions = append(actions, fmt.Sprintf("archived %d untracked file(s) → %s", len(record.Untracked), archive))
		}
		if _, err := m.gcGit(ctx, repoRoot, 10*time.Second, "worktree", "unlock", record.Path); err != nil && fatal(ctx, err) {
			return actions, err
		}
		line, err := m.reclaimOne(ctx, repoRoot, record)
		if err != nil {
			if fatal(ctx, err) {
				return actions, err
			}
			line = fmt.Sprintf("failed to remove %s: %s", record.Name, err)
		}
		actions = append(actions, line)
	}
	if !dryRun {
		if _, err := m.gcGit(ctx, repoRoot, 15*time.Second, "worktree", "prune"); err != nil && fatal(ctx, err) {
			return actions, err
		}
	}
	return actions, nil
}

func (m *Manager) reclaimOne(ctx context.Context, repoRoot string, record *TreeRecord) (string, error) {
	removed, err := m.gcGit(ctx, repoRoot, 30*time.Second, "worktree", "remove", record.Path, "--force")
	if err != nil {
		return "", err
	}
	if !removed.ok() {
		return fmt.Sprintf("failed to remove %s: %s", record.Name, removed.errText()), nil
	}
	if record.Verdict == "reap-keep-branch" {
		return fmt.Sprintf("removed %s (branch %s kept — pushed open-PR lane)", record.Name, record.Branch), nil
	}
	if record.Branch != "" && !protectedBranches[record.Branch] {
		if _, err := m.gcGit(ctx, repoRoot, 10*time.Second, "branch", "-D", record.Branch); err != nil {
			return "", err
		}
	}
	return "removed " + record.Name, nil
}

func (m *Manager) AuditBranches(ctx context.Context, repoRoot string) ([]*BranchRecord, error) {
	shallow, err := m.repoIsShallow(ctx, repoRoot, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if shallow {
		if _, err := m.deepenShallowRepo(ctx, repoRoot, 600*time.Second); err != nil {
			return nil, err
		}
	}
	upstream, found, err := m.mergeBaseRef(ctx, repoRoot, 5*time.Second)
	if err != nil {
		if fatal(ctx, err) {
			return nil, err
		}
		found = false
	}
	if !found {
		return nil, nil
	}
	listing, err := m.gcGit(ctx, repoRoot, 10*time.Second, "branch", "--format=%(refname:short)")
	if err != nil {
		return nil, err
	}
	if !listing.ok() {
		return nil, nil
	}
	branches := trimmedLines(listing.text())
	wt, err := m.gcGit(ctx, repoRoot, 10*time.Second, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	for _, line := range strings.Split(wt.text(), "\n") {
		if strings.HasPrefix(line, "branch refs/heads/") {
			active[strings.TrimSpace(strings.TrimPrefix(line, "branch refs/heads/"))] = true
		}
	}
	mergedOut, err := m.gcGit(ctx, repoRoot, 15*time.Second, "branch", "--merged", upstream, "--format=%(refname:short)")
	if err != nil {
		return nil, err
	}
	merged := map[string]bool{}
	for _, b := range trimmedLines(mergedOut.text()) {
		merged[b] = true
	}
	records := make([]*BranchRecord, len(branches))
	errs := make([]error, len(branches))
	workers := max(1, min(8, runtime.NumCPU(), len(branches)))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, branch := range branches {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			records[i], errs[i] = m.classifyBranch(ctx, repoRoot, upstream, branch, active, merged)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return records, nil
}

func trimmedLines(text string) []string {
	var out []string
	for _, ln := range strings.Split(text, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

func (m *Manager) classifyBranch(ctx context.Context, repoRoot, upstream, branch string, active, merged map[string]bool) (*BranchRecord, error) {
	if protectedBranches[branch] || active[branch] {
		return &BranchRecord{Name: branch, Verdict: "keep", Reason: "protected or checked out"}, nil
	}
	if merged[branch] {
		return &BranchRecord{Name: branch, Verdict: "delete", Reason: "fully merged into " + upstream}, nil
	}
	ahead, err := m.gcGit(ctx, repoRoot, 10*time.Second, "rev-list", "--count", upstream+".."+branch)
	if err != nil {
		return nil, err
	}
	if !ahead.ok() {
		return &BranchRecord{Name: branch, Verdict: "keep", Reason: "could not verify (git rev-list failed)"}, nil
	}
	countText := ahead.out()
	if countText == "" {
		countText = "0"
	}
	count, convErr := strconv.Atoi(countText)
	if convErr != nil {
		count = maxCherryAhead + 1
	}
	if count == 0 {
		return &BranchRecord{Name: branch, Verdict: "delete", Reason: "no commits beyond " + upstream}, nil
	}
	if count > maxCherryAhead {
		return &BranchRecord{Name: branch, Verdict: "keep", Reason: fmt.Sprintf("%d commits ahead (stale-base lane)", count)}, nil
	}
	cherry, err := m.gcGit(ctx, repoRoot, 30*time.Second, "cherry", upstream, branch)
	if err != nil {
		return nil, err
	}
	if !cherry.ok() {
		return &BranchRecord{Name: branch, Verdict: "keep", Reason: "could not verify (git cherry failed)"}, nil
	}
	lines := trimmedLines(cherry.text())
	all := len(lines) > 0
	unique := 0
	for _, ln := range lines {
		if !strings.HasPrefix(ln, "-") {
			all = false
		}
		if strings.HasPrefix(ln, "+") {
			unique++
		}
	}
	if all {
		return &BranchRecord{Name: branch, Verdict: "delete", Reason: "all commits patch-equivalent upstream"}, nil
	}
	return &BranchRecord{Name: branch, Verdict: "keep", Reason: fmt.Sprintf("%d unique commit(s) not upstream", unique)}, nil
}

func (m *Manager) ReclaimBranches(ctx context.Context, repoRoot string, dryRun bool, records []*BranchRecord) ([]string, error) {
	var actions []string
	for _, record := range records {
		if record.Verdict != "delete" {
			continue
		}
		if dryRun {
			actions = append(actions, fmt.Sprintf("would delete branch %s (%s)", record.Name, record.Reason))
			continue
		}
		r, err := m.gcGit(ctx, repoRoot, 10*time.Second, "branch", "-D", record.Name)
		if err != nil {
			return actions, err
		}
		if r.ok() {
			actions = append(actions, "deleted branch "+record.Name)
		} else {
			actions = append(actions, fmt.Sprintf("failed to delete %s: %s", record.Name, r.errText()))
		}
	}
	return actions, nil
}
