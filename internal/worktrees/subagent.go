package worktrees

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/git"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Longest one git step of a subagent's tree may take
const subagentGit = 30 * time.Second

// Tree a subagent works in apart from the checkout of the agent that delegated to it: .worktrees/subagent-<id> of the checkout's repository
// on a branch icebath-subagent/subagent-<id> off its HEAD; nil outside git or when it could not be made
func (w *Worktrees) Subagent(ctx context.Context, cwd, id string) *Tree {
	repo := git.Root(ctx, cwd)
	if repo == "" {
		return nil
	}
	if id == "" {
		id = strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	}
	name := "subagent-" + strings.ReplaceAll(id, "/", "-")
	branch := "icebath-subagent/" + name
	path := filepath.Join(repo, ".worktrees", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Warn(fmt.Sprintf("subagent worktree: creation failed: %v", err))
		return nil
	}
	ignoreTrees(repo)
	base, _, err := output(repo, subagentGit, "rev-parse", "HEAD")
	if err != nil {
		slog.Warn(fmt.Sprintf("subagent worktree: creation failed: %v", err))
		return nil
	}
	env := repoEnv(repo)
	if env == nil {
		slog.Warn("subagent worktree: filter discovery failed; not creating a worktree")
		return nil
	}
	result, err := run(repo, env, subagentGit, "worktree", "add", path, "-b", branch, "HEAD")
	if err != nil {
		slog.Warn(fmt.Sprintf("subagent worktree: creation failed: %v", err))
		return nil
	}
	if result.Code != 0 {
		slog.Warn("subagent worktree: git worktree add failed: " + strings.TrimSpace(result.Stderr))
		return nil
	}
	slog.Info(fmt.Sprintf("subagent worktree created: %s (branch %s)", path, branch))
	w.Hooks().Recorded(ctx, repo)
	return &Tree{Path: path, Branch: branch, Repo: repo, Base: base}
}

// What a subagent left in its tree once it finished: the commits it made, whether the tree holds uncommitted work, and whether the tree
// was removed, archived first when it held work
type Leftover struct {
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Commits int    `json:"commits"`
	Dirty   bool   `json:"dirty"`
	Pruned  bool   `json:"pruned"`
	Archive string `json:"archive,omitempty"`
	// What git could not tell, so the zero counts are no proof the subagent did nothing
	Failed bool   `json:"inspection_failed,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Marks the leftover uninspected, so the agent never reads its zero counts as no work, the tree and branch kept for review
func (l *Leftover) unproven(reason, unmeasured string) Leftover {
	l.Failed = true
	l.Note = fmt.Sprintf("git inspection failed (%s): %s UNKNOWN — not proven zero/clean. The worktree and branch were preserved — inspect %s (branch %s) "+
		"before assuming no work.", reason, unmeasured, l.Path, l.Branch)
	slog.Warn(fmt.Sprintf("subagent worktree: git inspection failed (%s) — keeping %s (branch %s) for manual review", reason, l.Path, l.Branch))
	return *l
}

// Inspects a finished subagent's tree: an empty clean one is removed with its branch, one holding work archived and removed with its
// branch kept, as the profile's settings allow
func (w *Worktrees) FinishSubagent(cfg *config.Config, tree Tree) Leftover {
	l := Leftover{Path: tree.Path, Branch: tree.Branch}
	if info, err := os.Stat(tree.Path); tree.Path == "" || err != nil || !info.IsDir() {
		l.Pruned = true
		return l
	}
	if tree.Base == "" {
		return l.unproven("no base_commit recorded — commit count unmeasurable", "commits")
	}
	env := repoEnv(tree.Path)
	if env == nil {
		return l.unproven("filter discovery failed", "dirty")
	}
	var failed, unmeasured []string
	for _, probe := range []struct {
		field, label string
		args         []string
	}{
		{"commits", "rev-list", []string{"rev-list", "--count", tree.Base + "..HEAD"}},
		{"dirty", "status", []string{"status", "--porcelain", "--untracked-files=normal"}},
	} {
		result, err := run(tree.Path, env, subagentGit, probe.args...)
		if err != nil {
			return l.unproven(fmt.Sprintf("git %s could not run: %v", probe.label, err), "commits/dirty")
		}
		if result.Code != 0 {
			failed = append(failed, fmt.Sprintf("%s exit %d: %s", probe.label, result.Code, strs.Head(strings.TrimSpace(result.Stderr), 200)))
			unmeasured = append(unmeasured, probe.field)
			continue
		}
		out := strings.TrimSpace(result.Stdout)
		if probe.field == "dirty" {
			l.Dirty = out != ""
			continue
		}
		if out != "" {
			n, err := strconv.Atoi(out)
			if err != nil {
				return l.unproven(fmt.Sprintf("git rev-list printed %s where a commit count was expected", out), "commits/dirty")
			}
			l.Commits = n
		}
	}
	if len(failed) > 0 {
		return l.unproven(strings.Join(failed, "; "), strings.Join(unmeasured, "/"))
	}
	if !l.Dirty {
		edits, err := hiddenEdits(tree.Path, nil)
		if err != nil {
			return l.unproven(fmt.Sprintf("index flag probe failed: %v", err), "dirty")
		}
		l.Dirty = len(edits) > 0
	}
	if l.Commits != 0 || l.Dirty {
		return w.archiveSubagent(cfg, tree, l)
	}
	repo := cmp.Or(tree.Repo, tree.Path)
	if blocked := Blocked(tree.Path); blocked != nil {
		l.Note = fmt.Sprintf("Worktree kept at %s: %s.", tree.Path, blocked.Reason)
		return l
	}
	w.Hooks().Release(tree.Path)
	removed, err := run(repo, nil, subagentGit, "worktree", "remove", "--force", tree.Path)
	switch {
	case err != nil:
		slog.Debug(fmt.Sprintf("subagent worktree: prune failed: %v", err))
	case removed.Code == 0:
		if _, err := run(repo, nil, subagentGit, "branch", "-D", tree.Branch); err != nil {
			slog.Debug(fmt.Sprintf("subagent worktree: prune failed: %v", err))
			return l
		}
		l.Pruned = true
		slog.Info("subagent worktree pruned (no work): " + tree.Path)
	default:
		slog.Debug("subagent worktree: prune failed: " + strings.TrimSpace(removed.Stderr))
	}
	return l
}

// Snapshots a finished tree holding work into an archive ref and removes it, its branch kept, unless the settings keep such trees or a
// process still works in it
func (w *Worktrees) archiveSubagent(cfg *config.Config, tree Tree, l Leftover) Leftover {
	settings := w.Settings(cfg)
	if !settings.ArchiveDirty {
		return l
	}
	if len(Live([]string{tree.Path}, os.Getpid())[tree.Path]) > 0 {
		slog.Info(fmt.Sprintf("subagent worktree: a live process works in %s; keeping it", tree.Path))
		return l
	}
	removal, err := w.ArchiveThenRemove(cmp.Or(tree.Repo, tree.Path), tree.Path, "subagent finished", settings.ArchiveMaxFileMB, false, nil, true, os.Getpid())
	if err != nil {
		slog.Warn(fmt.Sprintf("subagent worktree: archive failed, keeping %s: %v", tree.Path, err))
		return l
	}
	l.Archive, l.Pruned = removal.Ref, removal.Removed
	if removal.Removed {
		short := removal.Ref[strings.LastIndex(removal.Ref, "/")+1:]
		l.Note = fmt.Sprintf("Worktree archived to %s and removed; branch %s kept. Restore the full tree (including uncommitted edits) with "+
			"`icebath worktree restore %s`.", removal.Ref, tree.Branch, short)
	} else {
		l.Note = fmt.Sprintf("Worktree archived to %s but kept at %s (%s); branch %s kept.", removal.Ref, tree.Path, removal.WhyKept, tree.Branch)
	}
	return l
}

// What the subagent is told of the tree it works in
func (t Tree) SubagentNote() string {
	return "\n\n[WORKTREE ISOLATION] You are working in an isolated git worktree at: " + t.Path + "\n" +
		"Your dedicated branch is: " + t.Branch + "\n" +
		"All file edits and shell commands must happen inside this worktree directory (your terminal already starts there). Do NOT cd to the main " +
		"repository checkout. Commit your changes to your branch when done; the parent agent will review and merge your branch. If you make no " +
		"commits and leave the tree clean, the worktree is discarded automatically; otherwise it is archived (restorable) and removed when you finish."
}
