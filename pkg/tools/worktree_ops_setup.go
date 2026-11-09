package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Colored CLI line printer of the worktree setup, the cli _cprint the app replaces with its renderer
var WorktreeCprint = func(text string) { fmt.Println(text) }

// Sweeps the partial dir, the locked admin entry and the branch a failed worktree creation left behind
func cleanupFailedWorktreeAdd(repoRoot, wtPath, branchName string) {
	worktreeGitQuiet([]string{"worktree", "unlock", wtPath}, repoRoot, 15*time.Second, "", nil)
	worktreeGitQuiet([]string{"worktree", "remove", "--force", wtPath}, repoRoot, 15*time.Second, "", nil)
	if _, err := os.Lstat(wtPath); err == nil {
		if err := os.RemoveAll(wtPath); err != nil {
			toolsLogger(nil).Debug(fmt.Sprintf("cleanup after failed worktree add: %s", err))
		}
	}
	worktreeGitQuiet([]string{"worktree", "prune"}, repoRoot, 15*time.Second, "", NoninteractiveGitEnv(nil))
	worktreeGitQuiet([]string{"branch", "-D", branchName}, repoRoot, 15*time.Second, "", NoninteractiveGitEnv(nil))
}

// Pack sprawl threshold, repack timeout, slot interval and slot lock name, the module attributes hermes tests patch
var (
	packSprawlThreshold = 15
	repackTimeout       = 1800.0
	repackMinInterval   = 6 * 3600.0
	repackLock          = "icebath-repack.lock"
)

// Exactly one process per clone gets to repack per interval
func claimRepackSlot(gitDir string) (bool, error) {
	return claimMaintenanceSlot(gitDir, repackLock, repackMinInterval)
}

// Repack step of the pack health pass, the module attribute hermes tests patch
var runBoundedRepackFn = runBoundedRepack

// Incremental geometric repack whose whole process tree dies with the timeout or with this process
func runBoundedRepack(repoRoot string) {
	runBoundedGit(repoRoot, []string{"repack", "-d", "--geometric=2", "--write-midx", "--quiet"}, repackTimeout, nil)
}

// Repacks on pack sprawl and keeps the commit graph fresh, every failure logged at debug
func maintainPackHealth(repoRoot string) {
	if err := maintainPackHealthErr(repoRoot); err != nil {
		toolsLogger(nil).Debug(fmt.Sprintf("pack maintenance skipped: %s", err))
	}
}

// MaintainPackHealth repacks a repository once its refs are final
func MaintainPackHealth(repoRoot string) { maintainPackHealth(repoRoot) }

// Pack health pass returning the first failure instead of logging it
func maintainPackHealthErr(repoRoot string) error {
	gitDir, err := commonGitDir(repoRoot)
	if err != nil {
		return err
	}
	if gitDir == "" {
		return nil
	}
	packDir := filepath.Join(gitDir, "objects", "pack")
	if !isDirFollow(packDir) {
		return nil
	}
	packs, err := filepath.Glob(filepath.Join(packDir, "*.pack"))
	if err != nil {
		return err
	}
	if len(packs) >= packSprawlThreshold {
		claimed, err := claimRepackSlot(gitDir)
		if err != nil {
			return err
		}
		if claimed {
			ClearStaleTmpPacks(gitDir, nil)
			toolsLogger(nil).Info(fmt.Sprintf("git pack sprawl (%d packs) — repacking in background", len(packs)))
			runBoundedRepackFn(repoRoot)
			if _, err := worktreeGit([]string{"worktree", "prune"}, repoRoot, 60*time.Second, NoninteractiveGitEnv(nil)); err != nil {
				return err
			}
		}
	}
	claimed, err := claimCommitGraphRefresh(gitDir)
	if err != nil {
		return err
	}
	if claimed {
		return refreshCommitGraph(repoRoot, gitDir)
	}
	return nil
}

// Git runner of the base resolution, the subprocess.run hermes tests spy on and stall
var resolveBaseGitFn = worktreeGit

// Freshest base ref to branch a new worktree from with its banner label, one capped fetch at most
func resolveWorktreeBase(repoRoot string, fetchTimeout, freshnessWindow float64) (string, string) {
	run := func(args []string, timeout float64) (*worktreeGitResult, error) {
		return resolveBaseGitFn(args, repoRoot, time.Duration(timeout*float64(time.Second)), NoninteractiveGitEnv(nil))
	}
	refExists := func(ref string) bool {
		r, err := run([]string{"rev-parse", "--verify", "--quiet", ref + "^{commit}"}, 20)
		return err == nil && r.Returncode == 0
	}
	fetchHeadAge := func() (float64, bool) {
		gd, err := run([]string{"rev-parse", "--git-dir"}, 20)
		if err != nil || gd.Returncode != 0 {
			return 0, false
		}
		gitDir := pystr.Strip(gd.Stdout)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(repoRoot, gitDir)
		}
		st, err := os.Stat(filepath.Join(gitDir, "FETCH_HEAD"))
		if err != nil {
			return 0, false
		}
		return max(0.0, nowSeconds()-float64(st.ModTime().UnixNano())/1e9), true
	}
	refresh := func(remote, branch, ref string) (string, string) {
		if age, ok := fetchHeadAge(); ok && age < freshnessWindow && refExists(ref) {
			return ref, fmt.Sprintf("%s (fetched %ds ago)", ref, int(age))
		}
		var reason string
		fetched, err := run([]string{"fetch", remote, trackingRefspec(remote, branch)}, fetchTimeout)
		var timeout *pysubprocess.TimeoutExpired
		switch {
		case err == nil && fetched.Returncode == 0:
			return ref, fmt.Sprintf("%s (fetched)", ref)
		case err == nil:
			reason = "fetch failed"
		case errors.As(err, &timeout):
			reason = fmt.Sprintf("fetch timed out after %ss", strconv.FormatFloat(fetchTimeout, 'g', -1, 64))
		default:
			reason = fmt.Sprintf("fetch error: %s", err)
		}
		if refExists(ref) {
			toolsLogger(nil).Debug(fmt.Sprintf("worktree base: %s — using cached %s", reason, ref))
			return ref, fmt.Sprintf("%s (cached — %s)", ref, reason)
		}
		return "HEAD", fmt.Sprintf("HEAD (local — %s, no cached %s)", reason, ref)
	}
	up, err := run([]string{"rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"}, 20)
	if err != nil {
		toolsLogger(nil).Debug(fmt.Sprintf("worktree base: upstream resolution failed: %s", err))
	} else if up.Returncode == 0 {
		if upstream := pystr.Strip(up.Stdout); upstream != "" && strings.Contains(upstream, "/") {
			remote, branch, _ := strings.Cut(upstream, "/")
			return refresh(remote, branch, upstream)
		}
	}
	headRef, err := run([]string{"symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"}, 20)
	if err != nil {
		toolsLogger(nil).Debug(fmt.Sprintf("worktree base: default-branch resolution failed: %s", err))
		return "HEAD", "HEAD (local — could not reach remote)"
	}
	defaultRef := ""
	if headRef.Returncode == 0 {
		defaultRef = strings.Replace(pystr.Strip(headRef.Stdout), "refs/remotes/", "", 1)
	}
	if defaultRef == "" {
		show, err := run([]string{"remote", "show", "origin"}, max(fetchTimeout, 5))
		if err != nil {
			toolsLogger(nil).Debug(fmt.Sprintf("worktree base: default-branch resolution failed: %s", err))
			return "HEAD", "HEAD (local — could not reach remote)"
		}
		for _, line := range pystr.Splitlines(show.Stdout, false) {
			line = pystr.Strip(line)
			if strings.HasPrefix(line, "HEAD branch:") {
				_, rest, _ := strings.Cut(line, ":")
				if branch := pystr.Strip(rest); branch != "" && branch != "(unknown)" {
					defaultRef = "origin/" + branch
				}
				break
			}
		}
	}
	if defaultRef != "" && strings.Contains(defaultRef, "/") {
		remote, branch, _ := strings.Cut(defaultRef, "/")
		return refresh(remote, branch, defaultRef)
	}
	return "HEAD", "HEAD (local — could not reach remote)"
}

// Refspec that always writes the remote tracking ref, whatever the remote fetch config says
func trackingRefspec(remote, branch string) string {
	return "+refs/heads/" + branch + ":refs/remotes/" + remote + "/" + branch
}

// Appends .worktrees/ to the repo gitignore when missing, failures logged at debug
func ensureWorktreesGitignored(repoRoot string) {
	gitignore := filepath.Join(repoRoot, ".gitignore")
	existing := ""
	if raw, err := os.ReadFile(gitignore); err == nil {
		existing = pystr.DecodeUTF8Replace(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))
	} else if !errors.Is(err, os.ErrNotExist) {
		toolsLogger(nil).Debug(fmt.Sprintf("Could not update .gitignore: %s", err))
		return
	}
	for _, line := range pystr.Splitlines(existing, false) {
		if line == ".worktrees/" {
			return
		}
	}
	f, err := os.OpenFile(gitignore, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		toolsLogger(nil).Debug(fmt.Sprintf("Could not update .gitignore: %s", err))
		return
	}
	defer f.Close()
	text := ".worktrees/\n"
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		text = "\n" + text
	}
	if _, err := f.WriteString(text); err != nil {
		toolsLogger(nil).Debug(fmt.Sprintf("Could not update .gitignore: %s", err))
	}
}

// Creates the worktree with a local HEAD retry, every failed attempt swept, false when it failed
func worktreeAdd(repoRoot, wtPath, branchName, baseRef, baseLabel string) (string, string, bool) {
	env := NoninteractiveRepoGitEnv(repoRoot, nil)
	if env == nil {
		WorktreeCprint("\x1b[31m✗ Failed to create worktree: could not read the repository's filter config\x1b[0m")
		return "", "", false
	}
	create := func(cfg []string) (*worktreeGitResult, error) {
		args := append(append([]string{}, cfg...), "worktree", "add", wtPath, "-b", branchName, baseRef)
		return worktreeGit(args, repoRoot, 120*time.Second, env)
	}
	result, err := create([]string{"-c", "checkout.workers=8", "-c", "checkout.thresholdForParallelism=100"})
	if err == nil && result.Returncode != 0 {
		if baseRef != "HEAD" {
			toolsLogger(nil).Warn(fmt.Sprintf("worktree add from %s failed (%s); retrying from local HEAD", baseRef, pystr.Strip(result.Stderr)))
			cleanupFailedWorktreeAdd(repoRoot, wtPath, branchName)
			baseRef, baseLabel = "HEAD", "HEAD (fallback — remote base failed)"
			result, err = create(nil)
		}
		if err == nil && result.Returncode != 0 {
			cleanupFailedWorktreeAdd(repoRoot, wtPath, branchName)
			WorktreeCprint(fmt.Sprintf("\x1b[31m✗ Failed to create worktree: %s\x1b[0m", pystr.Strip(result.Stderr)))
			return "", "", false
		}
	}
	if err != nil {
		cleanupFailedWorktreeAdd(repoRoot, wtPath, branchName)
		WorktreeCprint(fmt.Sprintf("\x1b[31m✗ Failed to create worktree: %s\x1b[0m", err))
		return "", "", false
	}
	return baseRef, baseLabel, true
}

var worktreeNameUnsafeRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Creates an isolated git worktree and returns its path, branch, repo root and base, nil on failure
func SetupWorktree(ctx context.Context, repoRoot string, syncBase bool, name string, retainDB *string) map[string]string {
	if repoRoot == "" {
		repoRoot = gitRepoRoot()
	}
	if repoRoot == "" {
		WorktreeCprint("\x1b[31m✗ --worktree requires being inside a git repository.\x1b[0m")
		fmt.Println("  cd into your project repo first, then run icebath -w")
		return nil
	}
	wtName := ""
	if name != "" {
		wtName = worktreeHead(strings.Trim(worktreeNameUnsafeRe.ReplaceAllString(name, "-"), "-._"), 40)
	}
	if wtName == "" {
		wtName = "icebath-" + strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	}
	branchName := "icebath/" + wtName
	worktreesDir := filepath.Join(repoRoot, ".worktrees")
	if err := os.MkdirAll(worktreesDir, 0o777); err != nil {
		WorktreeCprint(fmt.Sprintf("\x1b[31m✗ Failed to create worktree: %s\x1b[0m", err))
		return nil
	}
	wtPath := filepath.Join(worktreesDir, wtName)
	if _, err := os.Lstat(wtPath); name != "" && err == nil {
		WorktreeCprint(fmt.Sprintf("\x1b[31m✗ Worktree already exists: %s\x1b[0m", wtPath))
		fmt.Printf("  Pick a different name, or remove it with: git worktree remove %s\n", wtPath)
		return nil
	}
	ensureWorktreesGitignored(repoRoot)
	baseRef, baseLabel := "HEAD", "HEAD (local — worktree_sync disabled)"
	if syncBase {
		baseRef, baseLabel = resolveWorktreeBase(repoRoot, 5, 300)
	}
	baseRef, baseLabel, ok := worktreeAdd(repoRoot, wtPath, branchName, baseRef, baseLabel)
	if !ok {
		return nil
	}
	copyWorktreeIncludes(repoRoot, wtPath)
	reason := fmt.Sprintf("icebath pid=%d", os.Getpid())
	if retainDB != nil {
		reason += " db=" + *retainDB
	}
	if _, err := worktreeGit([]string{"worktree", "lock", "--reason", reason, wtPath}, repoRoot, 10*time.Second, nil); err != nil {
		toolsLogger(nil).Debug(fmt.Sprintf("git worktree lock failed (non-fatal): %s", err))
	} else {
		toolsLogger(nil).Debug(fmt.Sprintf("Worktree locked: %s (pid=%d)", wtPath, os.Getpid()))
	}
	WorktreeRecordRepo(repoRoot)
	if retainDB != nil {
		RecordTUIWorktree(ctx, map[string]string{"path": wtPath, "branch": branchName, "repo_root": repoRoot})
	}
	WorktreeCprint(fmt.Sprintf("\x1b[32m✓ Worktree created:\x1b[0m %s", wtPath))
	fmt.Printf("  Branch: %s\n", branchName)
	fmt.Printf("  Base:   %s\n", baseLabel)
	return map[string]string{"path": wtPath, "branch": branchName, "repo_root": repoRoot, "base": baseRef}
}
