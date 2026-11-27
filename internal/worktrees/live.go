package worktrees

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/terminal"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Lock of a tree icebath made: by the process using it, or kept for the session working in it, with the folder of the icebath whose
// sessions say whether it is still worked in
var icebathLock = regexp.MustCompile(`(?s)^icebath (?:pid=(\d+)|session=\S+)(?: db=(.+))?$`)

// Lock reason of a tree the process uses
func (w *Worktrees) pidLock(pid int) string {
	return fmt.Sprintf("icebath pid=%d db=%s", pid, w.config.Dir())
}

// Lock reason of a tree kept for the session working in it
func (w *Worktrees) sessionLock(session string) string {
	return fmt.Sprintf("icebath session=%s db=%s", session, w.config.Dir())
}

// Locks the tree again for the session working in it, once the process that made it ends
func (w *Worktrees) Retain(repo, tree, session string) bool {
	quiet(repo, nil, 10*time.Second, "worktree unlock before retain failed", "worktree", "unlock", tree)
	result, err := run(repo, nil, 10*time.Second, "worktree", "lock", "--reason", w.sessionLock(session), tree)
	if err != nil {
		slog.Debug("worktree retain lock failed", "tree", tree, "error", err)
		return false
	}
	return result.Code == 0
}

// Whether the tree is one of the repository's worktrees, and its lock reason, nil when it is not locked, read from its admin folder
func lockReason(repo, tree string, timeout time.Duration) (bool, *string, error) {
	common, ok, err := output(repo, timeout, "rev-parse", "--git-common-dir")
	if err != nil {
		return false, nil, err
	}
	if !ok || common == "" {
		return false, nil, errors.New("The folder is not a git repository.")
	}
	dotGit := filepath.Join(tree, ".git")
	if info, err := os.Stat(dotGit); err != nil || !info.Mode().IsRegular() {
		return false, nil, nil
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return false, nil, err
	}
	link, ok := strings.CutPrefix(strings.TrimPrefix(string(data), bom), "gitdir:")
	if !ok {
		return false, nil, fmt.Errorf("The .git file in %s cannot be read.", tree)
	}
	target := strings.TrimSpace(link)
	if !filepath.IsAbs(target) {
		target = filepath.Join(tree, target)
	}
	admin, err := filepath.EvalSymlinks(target)
	if err != nil {
		return false, nil, err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(repo, common)
	}
	trees, err := filepath.EvalSymlinks(filepath.Join(common, "worktrees"))
	if err != nil {
		return false, nil, err
	}
	if filepath.Dir(admin) != trees {
		return false, nil, nil
	}
	locked, err := os.ReadFile(filepath.Join(admin, "locked"))
	if os.IsNotExist(err) {
		return true, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	reason := strings.Trim(strings.TrimPrefix(strings.ToValidUTF8(string(locked), "�"), bom), " \t\n\v\f\r")
	return true, &reason, nil
}

// How the tree's lock stands: live while the process that made it runs or a resumable session works in it, dead once neither does or
// for a lock icebath did not make, empty for a tree not locked
func (w *Worktrees) lockState(repo, tree string, timeout time.Duration) string {
	found, reason, err := lockReason(repo, tree, timeout)
	switch {
	case err != nil:
		return "live"
	case !found || reason == nil:
		return ""
	case !strings.HasPrefix(*reason, "icebath "):
		return "dead"
	}
	m := icebathLock.FindStringSubmatch(*reason)
	if m == nil {
		return "live"
	}
	pid, db := m[1], m[2]
	if pid != "" {
		n, err := strconv.Atoi(pid)
		if err != nil || n == os.Getpid() || n > 1<<31-1 || terminal.Running(n) {
			return "live"
		}
	}
	if db != "" {
		if db != w.config.Dir() || w.retained(tree, false) {
			return "live"
		}
		return "dead"
	}
	if pid != "" {
		return "dead"
	}
	return "live"
}

// Sessions of the store whose folder is the tree or lies under it, by the tree as given and as its links resolve
func (w *Worktrees) inside(ctx context.Context, tree string) ([]*sessionsv1.Session, error) {
	store, err := w.sessions.Current()
	if err != nil {
		return nil, err
	}
	folders := []string{filepath.Clean(tree)}
	if resolved, err := filepath.EvalSymlinks(tree); err == nil && resolved != folders[0] {
		folders = append(folders, resolved)
	}
	var found []*sessionsv1.Session
	for _, folder := range folders {
		listed, _, err := store.List(ctx, &sessionsv1.Filter{Directory: folder, IncludeArchived: true, IncludeHidden: true, IncludeChildren: true,
			IncludeEmpty: true, IncludeOpen: true, IncludePinned: true})
		if err != nil {
			return nil, err
		}
		for _, session := range listed {
			if !slices.ContainsFunc(found, func(have *sessionsv1.Session) bool { return have.GetId() == session.GetId() }) {
				found = append(found, session)
			}
		}
	}
	return found, nil
}

// Whether a session that can be resumed works in the tree: one with messages, or with messages waiting for their turn; pendingOnly
// counts the waiting messages alone. True when the store can't tell
func (w *Worktrees) retained(tree string, pendingOnly bool) bool {
	ctx := context.Background()
	found, err := w.inside(ctx, tree)
	if err != nil {
		slog.Debug("retained worktree session check failed; keeping tree", "error", err)
		return true
	}
	store, err := w.sessions.Current()
	if err != nil {
		return true
	}
	for _, session := range found {
		if !pendingOnly && session.GetMessages() > 0 {
			return true
		}
		pending, err := store.Pending(ctx, session.GetId())
		if err != nil || len(pending) > 0 {
			return true
		}
	}
	return false
}

// Why the tree is still worked in: a session there in the middle of a turn, one a messaging chat, a schedule or an agent keeps going, the
// store unreadable, or what a feature that works in trees says; empty when nothing serves it. A session of the app or the command line
// left idle as the app ends is no reason
func (w *Worktrees) served(tree string) string {
	ctx := context.Background()
	found, err := w.inside(ctx, tree)
	if err != nil {
		slog.Debug("retained worktree serve check failed; keeping tree", "error", err)
		return "unreadable"
	}
	for _, session := range found {
		if session.GetEnded() != nil {
			continue
		}
		if source := strings.ToLower(session.GetSource()); session.GetActivity() != "" || source != "" && source != "tui" && source != "cli" {
			return "session"
		}
	}
	if holds := w.Hooks().Holds; holds != nil {
		return holds(ctx, tree)
	}
	return ""
}

// Root among the roots the path is or lies under, empty for none
func within(path string, roots []string) string {
	for _, root := range roots {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return root
		}
	}
	return ""
}

// Processes working in each of the paths, by the path as given: their folder, or on Linux a file they hold open, lies in it. The
// excluded processes are left out
func Live(paths []string, exclude ...int) map[string][]int {
	roots := map[string]string{}
	for _, path := range paths {
		root, err := filepath.EvalSymlinks(path)
		if err != nil {
			root = filepath.Clean(path)
		}
		roots[root] = path
	}
	out := map[string][]int{}
	if len(roots) == 0 {
		return out
	}
	ordered := make([]string, 0, len(roots))
	for root := range roots {
		ordered = append(ordered, root)
	}
	slices.SortFunc(ordered, func(a, b string) int { return len(b) - len(a) })
	scan(func(pid int, place string) bool {
		if slices.Contains(exclude, pid) {
			return true
		}
		root := within(place, ordered)
		if root != "" {
			out[roots[root]] = append(out[roots[root]], pid)
		}
		return root != ""
	})
	for path := range out {
		slices.Sort(out[path])
	}
	return out
}

// Whether a process other than this one still works in the tree once the grace passed
func StillInUse(tree string, grace time.Duration, exclude ...int) bool {
	deadline := time.Now().Add(grace)
	for len(Live([]string{tree}, append(exclude, os.Getpid())...)[tree]) > 0 {
		if !time.Now().Before(deadline) {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// First processes of a list, as a reason names them
func pids(found []int) string {
	parts := make([]string, 0, 5)
	for _, pid := range found[:min(5, len(found))] {
		parts = append(parts, strconv.Itoa(pid))
	}
	return strings.Join(parts, ", ")
}

// Latest time anything happened in the tree: its HEAD and that log moved, a changed or new file was written, or the folder of a deleted
// one changed; now when git can't tell
func lastActivity(tree string) time.Time {
	env := repoEnv(tree)
	if env == nil {
		return time.Now()
	}
	env["GIT_OPTIONAL_LOCKS"] = "0"
	gitDir, err := run(tree, env, 10*time.Second, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return time.Now()
	}
	status, err := run(tree, env, time.Minute, "status", "--porcelain", "-z", "-uall", "--no-renames")
	if err != nil || status.Code != 0 {
		return time.Now()
	}
	var candidates []string
	if dir := strings.TrimSpace(gitDir.Stdout); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "HEAD"), filepath.Join(dir, "logs", "HEAD"))
	}
	var changed []string
	for _, entry := range strings.Split(status.Stdout, "\x00") {
		if len(entry) > 3 {
			changed = append(changed, filepath.Join(tree, entry[3:]))
		}
	}
	candidates = append(candidates, changed...)
	root := filepath.Clean(tree)
	for _, item := range changed {
		parent := filepath.Dir(item)
		for parent != root && strings.HasPrefix(parent, root+string(filepath.Separator)) {
			if _, err := os.Lstat(parent); err == nil {
				break
			}
			parent = filepath.Dir(parent)
		}
		candidates = append(candidates, parent)
	}
	var newest time.Time
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		if info, err := os.Lstat(candidate); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

// Why the tree must not be removed now: a process working in it, or a change at or after the time; empty while it sits idle
func inUseSince(tree string, since time.Time, exclude ...int) string {
	if found := Live([]string{tree}, exclude...)[tree]; len(found) > 0 {
		return "it is in use by process " + pids(found)
	}
	if active := lastActivity(tree); !active.Before(since) {
		return "it changed " + strs.Span(max(0, time.Since(active)).Truncate(time.Second)) + " ago"
	}
	return ""
}
