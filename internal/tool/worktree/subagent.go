package worktree

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/home"
)

const subagentGitTimeout = 30 * time.Second

type SubagentTree struct {
	Path       string
	Branch     string
	RepoRoot   string
	BaseCommit string
}

type SubagentResult struct {
	Path             string `json:"path"`
	Branch           string `json:"branch"`
	Commits          int    `json:"commits"`
	Dirty            bool   `json:"dirty"`
	Pruned           bool   `json:"pruned"`
	InspectionFailed bool   `json:"inspection_failed,omitempty"`
	Note             string `json:"note,omitempty"`
	Archive          string `json:"archive,omitempty"`
}

func (m *Manager) LocalBackendActive(ctx context.Context) (bool, error) {
	cfg, err := m.config.LoadReadonly(ctx)
	if err != nil {
		return false, err
	}
	backend := strings.ToLower(strings.TrimSpace(string(cfg.Terminal.Backend)))
	return backend == "" || backend == "local", nil
}

func (m *Manager) subagentGit(ctx context.Context, cwd string, env map[string]string, args ...string) (*result, error) {
	if env == nil {
		env = m.noninteractiveEnv(ctx)
	}
	return m.git(ctx, cwd, subagentGitTimeout, env, args...)
}

func (m *Manager) SubagentRepoRoot(ctx context.Context, path string) (string, error) {
	if path == "" {
		return "", nil
	}
	candidate, err := filepath.Abs(home.ExpandUser(path))
	if err != nil || !isDir(candidate) {
		return "", nil
	}
	r, err := m.subagentGit(ctx, candidate, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		if fatal(ctx, err) {
			return "", err
		}
		slog.Debug("subagent worktree: rev-parse failed", "error", err)
		return "", nil
	}
	if !r.ok() {
		return "", nil
	}
	return r.out(), nil
}

func (m *Manager) CreateSubagent(ctx context.Context, parentCwd, subagentID string) (*SubagentTree, error) {
	repoRoot, err := m.SubagentRepoRoot(ctx, parentCwd)
	if err != nil || repoRoot == "" {
		return nil, err
	}
	id := subagentID
	if id == "" {
		if id, err = randomHex(8); err != nil {
			return nil, err
		}
	}
	name := "subagent-" + strings.ReplaceAll(id, "/", "-")
	branch := "icebath-subagent/" + name
	wtPath := filepath.Join(repoRoot, ".worktrees", name)
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o777); err != nil {
		slog.Warn("subagent worktree: creation failed", "error", err)
		return nil, nil
	}
	ensureWorktreesGitignored(repoRoot)
	base, err := m.subagentGit(ctx, repoRoot, nil, "rev-parse", "HEAD")
	if err != nil {
		if fatal(ctx, err) {
			return nil, err
		}
		slog.Warn("subagent worktree: creation failed", "error", err)
		return nil, nil
	}
	baseCommit := ""
	if base.ok() {
		baseCommit = base.out()
	}
	addEnv, trusted, err := m.repoEnv(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	if !trusted {
		slog.Warn("subagent worktree: filter discovery failed; not creating a worktree")
		return nil, nil
	}
	r, err := m.subagentGit(ctx, repoRoot, addEnv, "worktree", "add", wtPath, "-b", branch, "HEAD")
	if err != nil {
		if fatal(ctx, err) {
			return nil, err
		}
		slog.Warn("subagent worktree: creation failed", "error", err)
		return nil, nil
	}
	if !r.ok() {
		slog.Warn("subagent worktree: git worktree add failed", "stderr", r.errText())
		return nil, nil
	}
	slog.Info("subagent worktree created", "path", wtPath, "branch", branch)
	if err := m.recordQuietly(ctx, repoRoot); err != nil {
		return nil, err
	}
	return &SubagentTree{Path: wtPath, Branch: branch, RepoRoot: repoRoot, BaseCommit: baseCommit}, nil
}

func basePayload(t *SubagentTree) *SubagentResult {
	return &SubagentResult{Path: t.Path, Branch: t.Branch}
}

func markUnproven(payload *SubagentResult, reason, unmeasured string) *SubagentResult {
	payload.InspectionFailed = true
	payload.Note = fmt.Sprintf("git inspection failed (%s): %s UNKNOWN — not proven zero/clean. The worktree and branch were preserved — inspect %s (branch %s) before assuming no work.", reason, unmeasured, payload.Path, payload.Branch)
	slog.Warn("subagent worktree: git inspection failed, keeping it for manual review", "reason", reason, "path", payload.Path, "branch", payload.Branch)
	return payload
}

func UnprovenSubagentResult(t *SubagentTree, reason string) *SubagentResult {
	return markUnproven(basePayload(t), reason, "commits/dirty")
}

func (m *Manager) FinalizeSubagent(ctx context.Context, t *SubagentTree, prune bool) (*SubagentResult, error) {
	payload := basePayload(t)
	if t.Path == "" || !isDir(t.Path) {
		payload.Pruned = true
		return payload, nil
	}
	if t.BaseCommit == "" {
		return markUnproven(payload, "no base_commit recorded — commit count unmeasurable", "commits"), nil
	}
	probeEnv, trusted, err := m.repoEnv(ctx, t.Path)
	if err != nil {
		return nil, err
	}
	if !trusted {
		return markUnproven(payload, "filter discovery failed", "dirty"), nil
	}
	var failed, unmeasured []string
	commits, err := m.subagentGit(ctx, t.Path, probeEnv, "rev-list", "--count", t.BaseCommit+"..HEAD")
	if err != nil {
		if fatal(ctx, err) {
			return nil, err
		}
		return markUnproven(payload, "inspection raised: "+err.Error(), "commits/dirty"), nil
	}
	if commits.ok() {
		text := commits.out()
		if text == "" {
			text = "0"
		}
		n, convErr := strconv.Atoi(text)
		if convErr != nil {
			return markUnproven(payload, "inspection raised: "+convErr.Error(), "commits/dirty"), nil
		}
		payload.Commits = n
	} else {
		failed = append(failed, fmt.Sprintf("rev-list exit %d: %s", commits.code, truncate(commits.errText(), 200)))
		unmeasured = append(unmeasured, "commits")
	}
	status, err := m.subagentGit(ctx, t.Path, probeEnv, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		if fatal(ctx, err) {
			return nil, err
		}
		return markUnproven(payload, "inspection raised: "+err.Error(), "commits/dirty"), nil
	}
	if status.ok() {
		payload.Dirty = status.out() != ""
	} else {
		failed = append(failed, fmt.Sprintf("status exit %d: %s", status.code, truncate(status.errText(), 200)))
		unmeasured = append(unmeasured, "dirty")
	}
	if len(failed) > 0 {
		return markUnproven(payload, strings.Join(failed, "; "), strings.Join(unmeasured, "/")), nil
	}
	if !payload.Dirty {
		edits, err := m.hiddenIndexEdits(ctx, t.Path, nil)
		if err != nil {
			if fatal(ctx, err) {
				return nil, err
			}
			return markUnproven(payload, "index flag probe failed: "+err.Error(), "dirty"), nil
		}
		payload.Dirty = len(edits) > 0
	}
	if prune && (payload.Commits != 0 || payload.Dirty) {
		return m.archiveFinishedSubagent(ctx, t, payload)
	}
	if prune {
		if err := m.pruneIdleSubagent(ctx, t, payload); err != nil {
			if fatal(ctx, err) {
				return nil, err
			}
			slog.Debug("subagent worktree: prune failed", "error", err)
		}
	}
	return payload, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (m *Manager) pruneIdleSubagent(ctx context.Context, t *SubagentTree, payload *SubagentResult) error {
	cwd := t.RepoRoot
	if cwd == "" {
		cwd = t.Path
	}
	blocked, err := m.RemovalBlocked(ctx, t.Path)
	if err != nil {
		return err
	}
	if blocked != nil {
		payload.Note = fmt.Sprintf("Worktree kept at %s: %s.", t.Path, blocked.Reason)
		return nil
	}
	if err := m.releaseLSP(ctx, t.Path); err != nil {
		return err
	}
	removed, err := m.subagentGit(ctx, cwd, nil, "worktree", "remove", "--force", t.Path)
	if err != nil {
		return err
	}
	if !removed.ok() {
		slog.Debug("subagent worktree: prune failed", "stderr", removed.errText())
		return nil
	}
	if _, err := m.subagentGit(ctx, cwd, nil, "branch", "-D", t.Branch); err != nil {
		return err
	}
	payload.Pruned = true
	slog.Info("subagent worktree pruned (no work)", "path", t.Path)
	return nil
}

func (m *Manager) archiveFinishedSubagent(ctx context.Context, t *SubagentTree, payload *SubagentResult) (*SubagentResult, error) {
	settings, err := m.Settings(ctx)
	if err != nil {
		return nil, err
	}
	if !settings.ArchiveDirty {
		return payload, nil
	}
	live, err := m.LivePIDs([]string{t.Path}, []int{os.Getpid()})
	if err != nil {
		return nil, err
	}
	if len(live[t.Path]) > 0 {
		slog.Info("subagent worktree: a live process works in the tree; keeping it", "path", t.Path)
		return payload, nil
	}
	repo := t.RepoRoot
	if repo == "" {
		repo = t.Path
	}
	res, err := m.ArchiveThenRemove(ctx, repo, t.Path, ArchiveRemoveOptions{
		Reason: "subagent finished", MaxUntrackedMB: settings.ArchiveMaxFileMB, KeepBranch: true, ExcludePIDs: []int{os.Getpid()},
	})
	if err != nil {
		if fatal(ctx, err) {
			return nil, err
		}
		slog.Warn("subagent worktree: archive failed, keeping it", "path", t.Path, "error", err)
		return payload, nil
	}
	payload.Archive = res.Ref
	payload.Pruned = res.Removed
	if res.Removed {
		payload.Note = fmt.Sprintf("Worktree archived to %s and removed; branch %s kept. Restore the full tree (including uncommitted edits) with `icebath worktree restore %s`.", res.Ref, t.Branch, res.Ref[strings.LastIndex(res.Ref, "/")+1:])
	} else {
		payload.Note = fmt.Sprintf("Worktree archived to %s but kept at %s (%s); branch %s kept.", res.Ref, t.Path, res.WhyKept, t.Branch)
	}
	return payload, nil
}

func SubagentContextNote(t *SubagentTree) string {
	return "\n\n[WORKTREE ISOLATION] You are working in an isolated git worktree " +
		"at: " + t.Path + "\n" +
		"Your dedicated branch is: " + t.Branch + "\n" +
		"All file edits and shell commands must happen inside this worktree directory (your " +
		"terminal already starts there). Do NOT cd to the main repository checkout. Commit your " +
		"changes to your branch when done; the parent agent will review and merge your branch. If " +
		"you make no commits and leave the tree clean, the worktree is discarded automatically; " +
		"otherwise it is archived (restorable) and removed when you finish."
}
