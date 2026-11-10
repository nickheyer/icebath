package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const subagentGitTimeout = 30 * time.Second

func subagentWorktreeLog() *slog.Logger {
	return slog.Default().With("logger", "tools.subagent_worktree")
}

type gitRunResult struct {
	code           int
	stdout, stderr string
}

// Runs hardened git with captured output, never failing on a non zero exit
func subagentRunGit(args []string, cwd string, env map[string]string) (gitRunResult, error) {
	if env == nil {
		env = tools.NoninteractiveGitEnv(nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), subagentGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", tools.HardenGitArgv(args)...)
	cmd.Dir = cwd
	cmd.Stdin = nil
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	err := cmd.Run()
	if ctx.Err() != nil {
		return gitRunResult{}, fmt.Errorf("Command '%v' timed out after %d seconds", append([]string{"git"}, args...), int(subagentGitTimeout.Seconds()))
	}
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			return gitRunResult{}, err
		}
		code = ee.ExitCode()
	}
	return gitRunResult{code: code, stdout: strings.ToValidUTF8(out.String(), "�"), stderr: strings.ToValidUTF8(errBuf.String(), "�")}, nil
}

// Reports a local terminal backend where host worktrees are visible to tools
func subagentLocalBackendActive(ctx context.Context) bool {
	terminal, _ := provider.LoadConfigSafe(home.Dir(ctx))["terminal"].(map[string]any)
	backend := "local"
	if b := terminal["backend"]; pyTruthy(b) {
		backend = fmt.Sprint(b)
	}
	b := strings.ToLower(pystr.Strip(backend))
	return b == "" || b == "local"
}

// Git toplevel of a path, empty outside a work tree
func resolveRepoRoot(path string) string {
	if path == "" {
		return ""
	}
	candidate, err := filepath.Abs(expandUserPath(path))
	if err != nil {
		return ""
	}
	if st, err := os.Stat(candidate); err != nil || !st.IsDir() {
		return ""
	}
	res, err := subagentRunGit([]string{"rev-parse", "--show-toplevel"}, candidate, nil)
	if err != nil {
		subagentWorktreeLog().Debug(fmt.Sprintf("subagent worktree: rev-parse failed: %v", err))
		return ""
	}
	if res.code != 0 {
		return ""
	}
	return strings.TrimSpace(res.stdout)
}

// Keeps the worktrees directory out of git status
func ensureWorktreesGitignore(repoRoot string) {
	gitignore := filepath.Join(repoRoot, ".gitignore")
	existing := ""
	if b, err := os.ReadFile(gitignore); err == nil {
		existing = strings.TrimPrefix(strings.ToValidUTF8(string(b), "�"), "\uFEFF")
	} else if !os.IsNotExist(err) {
		subagentWorktreeLog().Debug(fmt.Sprintf("subagent worktree: could not update .gitignore: %v", err))
		return
	}
	for _, line := range pystr.Splitlines(existing, false) {
		if line == ".worktrees/" {
			return
		}
	}
	f, err := os.OpenFile(gitignore, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		subagentWorktreeLog().Debug(fmt.Sprintf("subagent worktree: could not update .gitignore: %v", err))
		return
	}
	defer f.Close()
	sep := ""
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		sep = "\n"
	}
	if _, err := f.WriteString(sep + ".worktrees/\n"); err != nil {
		subagentWorktreeLog().Debug(fmt.Sprintf("subagent worktree: could not update .gitignore: %v", err))
	}
}

// Isolated worktree of one child off the parent's HEAD, nil outside git or on failure
func createSubagentWorktree(ctx context.Context, parentCwd, subagentID string) map[string]string {
	repoRoot := resolveRepoRoot(parentCwd)
	if repoRoot == "" {
		return nil
	}
	id := subagentID
	if id == "" {
		id = strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	}
	wtName := "subagent-" + strings.ReplaceAll(id, "/", "-")
	branch := "icebath-subagent/" + wtName
	wtPath := filepath.Join(repoRoot, ".worktrees", wtName)
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		subagentWorktreeLog().Warn(fmt.Sprintf("subagent worktree: creation failed: %v", err))
		return nil
	}
	ensureWorktreesGitignore(repoRoot)
	base, err := subagentRunGit([]string{"rev-parse", "HEAD"}, repoRoot, nil)
	if err != nil {
		subagentWorktreeLog().Warn(fmt.Sprintf("subagent worktree: creation failed: %v", err))
		return nil
	}
	baseCommit := ""
	if base.code == 0 {
		baseCommit = strings.TrimSpace(base.stdout)
	}
	addEnv := tools.NoninteractiveRepoGitEnv(repoRoot, nil)
	if addEnv == nil {
		subagentWorktreeLog().Warn("subagent worktree: filter discovery failed; not creating a worktree")
		return nil
	}
	res, err := subagentRunGit([]string{"worktree", "add", wtPath, "-b", branch, "HEAD"}, repoRoot, addEnv)
	if err != nil {
		subagentWorktreeLog().Warn(fmt.Sprintf("subagent worktree: creation failed: %v", err))
		return nil
	}
	if res.code != 0 {
		subagentWorktreeLog().Warn("subagent worktree: git worktree add failed: " + strings.TrimSpace(res.stderr))
		return nil
	}
	subagentWorktreeLog().Info(fmt.Sprintf("subagent worktree created: %s (branch %s)", wtPath, branch))
	cron.RecordWorktreeRepo(ctx, home.Dir(ctx), repoRoot)
	return map[string]string{"path": wtPath, "branch": branch, "repo_root": repoRoot, "base_commit": baseCommit}
}

func worktreeBasePayload(info map[string]string) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("path", info["path"])
	o.Set("branch", info["branch"])
	o.Set("commits", 0)
	o.Set("dirty", false)
	o.Set("pruned", false)
	return o
}

// Flags a worktree payload as uninspected so the parent never reads defaults as no work
func markWorktreePayloadUnproven(payload *pyjson.Object, reason, unmeasured string) *pyjson.Object {
	path, _ := payload.Get("path")
	branch, _ := payload.Get("branch")
	payload.Set("inspection_failed", true)
	payload.Set("note", fmt.Sprintf("git inspection failed (%s): %s UNKNOWN — not proven "+
		"zero/clean. The worktree and branch were preserved — inspect %v "+
		"(branch %v) before assuming no work.", reason, unmeasured, path, branch))
	subagentWorktreeLog().Warn(fmt.Sprintf("subagent worktree: git inspection failed (%s) — keeping %v (branch %v) "+
		"for manual review", reason, path, branch))
	return payload
}

// Complete uninspected payload for a finalize that raised
func unprovenWorktreePayload(info map[string]string, reason string) *pyjson.Object {
	return markWorktreePayloadUnproven(worktreeBasePayload(info), reason, "commits/dirty")
}

// Inspects a finished child worktree, pruning an empty clean tree and archiving one with work
func finalizeSubagentWorktree(info map[string]string, prune bool) (*pyjson.Object, error) {
	path, branch, baseCommit := info["path"], info["branch"], info["base_commit"]
	payload := worktreeBasePayload(info)
	if st, err := os.Stat(path); path == "" || err != nil || !st.IsDir() {
		payload.Set("pruned", true)
		return payload, nil
	}
	if baseCommit == "" {
		return markWorktreePayloadUnproven(payload, "no base_commit recorded — commit count unmeasurable", "commits"), nil
	}
	probeEnv := tools.NoninteractiveRepoGitEnv(path, nil)
	if probeEnv == nil {
		return markWorktreePayloadUnproven(payload, "filter discovery failed", "dirty"), nil
	}
	var failed, unmeasured []string
	probes := []struct {
		field, label string
		args         []string
	}{
		{"commits", "rev-list", []string{"rev-list", "--count", baseCommit + "..HEAD"}},
		{"dirty", "status", []string{"status", "--porcelain", "--untracked-files=normal"}},
	}
	for _, p := range probes {
		res, err := subagentRunGit(p.args, path, probeEnv)
		if err != nil {
			return markWorktreePayloadUnproven(payload, fmt.Sprintf("inspection raised: %v", err), "commits/dirty"), nil
		}
		if res.code != 0 {
			failed = append(failed, fmt.Sprintf("%s exit %d: %s", p.label, res.code, pystr.Slice(strings.TrimSpace(res.stderr), 0, 200)))
			unmeasured = append(unmeasured, p.field)
			continue
		}
		out := strings.TrimSpace(res.stdout)
		if p.field == "commits" {
			n := 0
			if out != "" {
				v, err := strconv.Atoi(out)
				if err != nil {
					return markWorktreePayloadUnproven(payload, fmt.Sprintf("inspection raised: invalid literal for int() with base 10: '%s'", out), "commits/dirty"), nil
				}
				n = v
			}
			payload.Set("commits", n)
		} else {
			payload.Set("dirty", out != "")
		}
	}
	if len(failed) > 0 {
		return markWorktreePayloadUnproven(payload, strings.Join(failed, "; "), strings.Join(unmeasured, "/")), nil
	}
	dirty, _ := payload.Get("dirty")
	if dirty == false {
		edits, err := tools.HiddenIndexEdits(path, nil)
		if err != nil {
			return markWorktreePayloadUnproven(payload, fmt.Sprintf("index flag probe failed: %v", err), "dirty"), nil
		}
		payload.Set("dirty", len(edits) > 0)
	}
	commitsV, _ := payload.Get("commits")
	commits, _ := commitsV.(int)
	dirtyV, _ := payload.Get("dirty")
	isDirty, _ := dirtyV.(bool)
	if prune && (commits != 0 || isDirty) {
		return archiveFinishedWorktree(info, payload), nil
	}
	if prune && commits == 0 && !isDirty {
		cwd := info["repo_root"]
		if cwd == "" {
			cwd = path
		}
		if blocked := tools.RemovalBlocked(path); blocked != nil {
			payload.Set("note", fmt.Sprintf("Worktree kept at %s: %s.", path, blocked.Reason))
			return payload, nil
		}
		tools.ReleaseLspClients(path)
		removed, err := subagentRunGit([]string{"worktree", "remove", "--force", path}, cwd, nil)
		switch {
		case err != nil:
			subagentWorktreeLog().Debug(fmt.Sprintf("subagent worktree: prune failed: %v", err))
		case removed.code == 0:
			if _, err := subagentRunGit([]string{"branch", "-D", branch}, cwd, nil); err != nil {
				subagentWorktreeLog().Debug(fmt.Sprintf("subagent worktree: prune failed: %v", err))
				return payload, nil
			}
			payload.Set("pruned", true)
			subagentWorktreeLog().Info("subagent worktree pruned (no work): " + path)
		default:
			subagentWorktreeLog().Debug("subagent worktree: prune failed: " + strings.TrimSpace(removed.stderr))
		}
	}
	return payload, nil
}

// Snapshots a finished tree with work into an archive ref and removes it, keeping the branch
func archiveFinishedWorktree(info map[string]string, payload *pyjson.Object) *pyjson.Object {
	settings := tools.SweepSettings()
	if !settings.ArchiveDirty {
		return payload
	}
	path := info["path"]
	if len(tools.LiveProcessPids([]string{path}, []int{os.Getpid()})[path]) > 0 {
		subagentWorktreeLog().Info(fmt.Sprintf("subagent worktree: a live process works in %s; keeping it", path))
		return payload
	}
	repoRoot := info["repo_root"]
	if repoRoot == "" {
		repoRoot = path
	}
	res, err := tools.ArchiveThenRemove(repoRoot, path, "subagent finished", settings.ArchiveMaxFileMB, false, nil, true, []int{os.Getpid()})
	if err != nil {
		subagentWorktreeLog().Warn(fmt.Sprintf("subagent worktree: archive failed, keeping %s: %v", path, err))
		return payload
	}
	payload.Set("archive", res.Ref)
	payload.Set("pruned", res.Removed)
	if res.Removed {
		short := res.Ref[strings.LastIndex(res.Ref, "/")+1:]
		payload.Set("note", fmt.Sprintf("Worktree archived to %s and removed; branch %s kept. "+
			"Restore the full tree (including uncommitted edits) with "+
			"`icebath worktree restore %s`.", res.Ref, info["branch"], short))
	} else {
		payload.Set("note", fmt.Sprintf("Worktree archived to %s but kept at %s (%s); "+
			"branch %s kept.", res.Ref, path, res.WhyKept, info["branch"]))
	}
	return payload
}

// Context block telling the child to work inside its isolated worktree
func buildWorktreeContextNote(info map[string]string) string {
	return "\n\n[WORKTREE ISOLATION] You are working in an isolated git worktree " +
		"at: " + info["path"] + "\n" +
		"Your dedicated branch is: " + info["branch"] + "\n" +
		"All file edits and shell commands must happen inside this worktree directory (your " +
		"terminal already starts there). Do NOT cd to the main repository checkout. Commit your " +
		"changes to your branch when done; the parent agent will review and merge your branch. If " +
		"you make no commits and leave the tree clean, the worktree is discarded automatically; " +
		"otherwise it is archived (restorable) and removed when you finish."
}
