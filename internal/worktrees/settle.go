package worktrees

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// What the user may do with a tree holding work as they are done with it
const (
	ExitKeep    = "keep"
	ExitArchive = "archive"
	ExitDiscard = "discard"
)

// How a tree stands as its session ends: dirty, unpushed, or empty for one clean and pushed, or gone
func State(tree Tree) string {
	if !fsx.IsDir(tree.Path) {
		return ""
	}
	if Dirty(tree.Path, tree.Repo, 10*time.Second) {
		return "dirty"
	}
	if Unpushed(tree.Path, 10*time.Second) {
		return "unpushed"
	}
	return ""
}

// Removes the tree and its changes for good, its branch too once the tree is gone
func (w *Worktrees) discard(tree Tree, say func(string)) {
	w.Hooks().Release(tree.Path)
	quiet(tree.Repo, nil, 10*time.Second, "git worktree unlock failed (non-fatal)", "worktree", "unlock", tree.Path)
	quiet(tree.Repo, nil, time.Minute, "Failed to remove worktree", "worktree", "remove", tree.Path, "--force")
	if tree.Branch != "" && !fsx.IsDir(tree.Path) {
		quiet(tree.Repo, plain(), 10*time.Second, "Failed to delete branch "+tree.Branch, "branch", "-D", tree.Branch)
	}
	say("✓ Removed the worktree " + tree.Path + " and threw away its changes.")
}

// Archives the tree, then removes it unless something keeps it, saying how it went
func (w *Worktrees) archive(cfg *config.Config, tree Tree, say func(string), exclude ...int) {
	say("Archiving " + tree.Path + "… A large worktree takes a while.")
	removal, err := w.ArchiveThenRemove(tree.Repo, tree.Path, "session exit", w.Settings(cfg).ArchiveMaxFileMB, true, nil, false, exclude...)
	if err != nil {
		say("✗ " + tree.Path + " could not be archived, so it is kept. " + strs.Full(cmp.Or(err.Error(), "The archive was interrupted.")))
		return
	}
	if removal.Removed {
		say("✓ Archived the worktree " + tree.Path + " to " + removal.Ref + " and removed it.")
	} else {
		say("✓ Archived the worktree " + tree.Path + " to " + removal.Ref + ", but kept it: " + cmp.Or(removal.WhyKept, "the reason is not known") + ".")
	}
	say("  Restore it with `icebath worktree restore " + removal.Ref[strings.LastIndex(removal.Ref, "/")+1:] + "`.")
}

// Carries out what the user chose for a tree they are done with, else removes it with its branch when it is clean and pushed and nothing
// of it would be lost, saying why one is kept. The excluded processes don't count as working in it
func (w *Worktrees) Settle(cfg *config.Config, tree Tree, choice string, say func(string), exclude ...int) {
	if _, err := os.Stat(tree.Path); err != nil {
		return
	}
	switch choice {
	case ExitArchive:
		w.archive(cfg, tree, say, exclude...)
		return
	case ExitDiscard:
		w.discard(tree, say)
		return
	}
	switch {
	case Dirty(tree.Path, tree.Repo, 10*time.Second):
		say("⚠ Kept the worktree " + tree.Path + ", because it has uncommitted changes.")
		return
	case Unpushed(tree.Path, 10*time.Second) && Shallow(tree.Repo, 10*time.Second):
		say("⚠ Kept the worktree " + tree.Path + ", because the clone is shallow, so icebath cannot check what is pushed.")
		say("  The next `icebath -w` session deepens the clone and removes merged worktrees on its own.")
		return
	case Unpushed(tree.Path, 10*time.Second):
		say("⚠ Kept the worktree " + tree.Path + ", because it has commits that are not pushed.")
		say("  To remove it yourself, run `git worktree remove --force " + tree.Path + "`.")
		return
	}
	if blocked := Blocked(tree.Path); blocked != nil {
		say("⚠ Kept the worktree " + tree.Path + ": " + blocked.Reason + ".")
		return
	}
	w.Hooks().Release(tree.Path)
	quiet(tree.Repo, nil, 10*time.Second, "git worktree unlock failed (non-fatal)", "worktree", "unlock", tree.Path)
	quiet(tree.Repo, nil, 15*time.Second, "Failed to remove worktree", "worktree", "remove", tree.Path, "--force")
	quiet(tree.Repo, nil, 10*time.Second, "Failed to delete branch "+tree.Branch, "branch", "-D", tree.Branch)
	say("✓ Removed the worktree " + tree.Path + ".")
}

// What becomes of a launch's own tree as the launch ends, after what the user chose: settled when no session waits to work in it, no
// other process works in it and nothing else serves it, else kept for the session that worked in it last, saying so. Whether it is gone
func (w *Worktrees) Leave(cfg *config.Config, tree Tree, choice string, say func(string), exclude ...int) bool {
	if !fsx.IsDir(tree.Path) {
		return true
	}
	if choice == ExitArchive || choice == ExitDiscard {
		w.Settle(cfg, tree, choice, say, exclude...)
		if !fsx.IsDir(tree.Path) {
			return true
		}
	} else if !w.retained(tree.Path, true) && !StillInUse(tree.Path, 5*time.Second, exclude...) && w.served(tree.Path) == "" {
		w.Settle(cfg, tree, "", say, exclude...)
		if _, err := os.Lstat(tree.Path); os.IsNotExist(err) {
			return true
		}
	}
	if session := w.latest(tree.Path); session != "" {
		w.Retain(tree.Repo, tree.Path, session)
	}
	say("Kept the worktree " + tree.Path + " for this session.")
	return false
}

// Newest session working in the tree, empty when there is none
func (w *Worktrees) latest(tree string) string {
	found, err := w.inside(context.Background(), tree)
	if err != nil || len(found) == 0 {
		return ""
	}
	newest := slices.MaxFunc(found, func(a, b *sessionsv1.Session) int { return a.GetUpdated().AsTime().Compare(b.GetUpdated().AsTime()) })
	return newest.GetId()
}

// Trees made for launches and those sessions moved into, kept so a session resumed after its tree was removed gets it again
const recordName = "worktrees.json"

// Most trees the record keeps, the oldest going first
const recordMax = 500

// Path of the profile's record of trees
func recordPath(cfg *config.Config) string { return filepath.Join(profileDir(cfg), recordName) }

// Key of a tree in the record: its path with links resolved, case folded where the system folds case
func recordKey(path string) string {
	key := realOr(path)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key
}

// Trees of the record, oldest first
func readRecord(cfg *config.Config) []Tree {
	var saved struct {
		Trees []Tree `json:"trees"`
	}
	data, err := os.ReadFile(recordPath(cfg))
	if err != nil || json.Unmarshal(data, &saved) != nil {
		return nil
	}
	return saved.Trees
}

// Remembers a tree a launch made or a session moved into, never failing: the trees of repositories gone are dropped
func (w *Worktrees) Record(cfg *config.Config, tree Tree) {
	path := recordPath(cfg)
	release, err := fsx.Lock(context.Background(), path+".lock", 5*time.Second)
	if err != nil {
		slog.Debug("Worktree record failed", "tree", tree.Path, "error", err)
		return
	}
	defer release()
	key := recordKey(tree.Path)
	var kept []Tree
	for _, entry := range readRecord(cfg) {
		if _, err := os.Stat(filepath.Join(entry.Repo, ".git")); err == nil && recordKey(entry.Path) != key {
			kept = append(kept, entry)
		}
	}
	kept = append(kept, Tree{Path: tree.Path, Branch: tree.Branch, Repo: tree.Repo})
	kept = kept[max(0, len(kept)-recordMax):]
	data, err := json.MarshalIndent(struct {
		Version int    `json:"version"`
		Trees   []Tree `json:"trees"`
	}{1, kept}, "", "  ")
	if err == nil {
		err = fsx.Write(path, data, 0o600)
	}
	if err != nil {
		slog.Debug("Worktree record failed", "tree", tree.Path, "error", err)
	}
}

// Recorded tree the folder is or lies in, nil when none was recorded there or its record no longer holds
func recorded(cfg *config.Config, cwd string) *Tree {
	trees := readRecord(cfg)
	for candidate := filepath.Clean(cwd); ; candidate = filepath.Dir(candidate) {
		at := slices.IndexFunc(trees, func(tree Tree) bool { return recordKey(tree.Path) == recordKey(candidate) })
		if at >= 0 {
			tree := trees[at]
			if _, err := os.Stat(filepath.Join(tree.Repo, ".git")); tree.Branch == "" || err != nil ||
				recordKey(filepath.Dir(tree.Path)) != recordKey(filepath.Join(tree.Repo, ".worktrees")) {
				return nil
			}
			return &tree
		}
		if filepath.Dir(candidate) == candidate {
			return nil
		}
	}
}

// Makes again the recorded tree a resumed session's missing folder names, on its branch when that is still there, and keeps it for the
// session, the newest session working in it when none is named: the tree, empty when it can't
func (w *Worktrees) Recreate(cfg *config.Config, cwd, session string) string {
	if cwd == "" || fsx.IsDir(cwd) {
		return ""
	}
	tree := recorded(cfg, cwd)
	if tree == nil {
		return ""
	}
	if _, err := os.Lstat(tree.Path); err == nil {
		return ""
	}
	env := repoEnv(tree.Repo)
	if env == nil {
		return ""
	}
	quiet(tree.Repo, env, 15*time.Second, "", "worktree", "prune")
	if err := os.MkdirAll(filepath.Dir(tree.Path), 0o777); err != nil {
		slog.Warn("could not recreate worktree "+tree.Path, "error", err)
		return ""
	}
	args := []string{"worktree", "add", tree.Path, "-b", tree.Branch, "HEAD"}
	if result, err := run(tree.Repo, nil, 0, "rev-parse", "--verify", "--quiet", "refs/heads/"+tree.Branch); err == nil && result.Code == 0 {
		args = []string{"worktree", "add", tree.Path, tree.Branch}
	}
	result, err := run(tree.Repo, env, 2*time.Minute, args...)
	if err != nil {
		slog.Warn("could not recreate worktree "+tree.Path, "error", err)
		return ""
	}
	if result.Code != 0 {
		slog.Warn("could not recreate worktree "+tree.Path, "error", strings.TrimSpace(result.Stderr))
		return ""
	}
	copyIncludes(tree.Repo, tree.Path)
	if session == "" {
		session = w.latest(tree.Path)
	}
	if session != "" {
		w.Retain(tree.Repo, tree.Path, session)
	}
	slog.Info("Recreated the removed worktree a session works in", "tree", tree.Path)
	return tree.Path
}
