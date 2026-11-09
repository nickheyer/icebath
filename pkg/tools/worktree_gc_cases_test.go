package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Runs git with a fixed author, failing the test on a non zero exit, and returns the stripped stdout
func gcTestGit(t *testing.T, dir string, env map[string]string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("git %v failed: %v\n%s", args, err, stderr)
	}
	return strings.TrimSpace(string(out))
}

// Bare origin plus a clone holding .worktrees, the home redirected under the temp dir
func gcTestRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	homeDir := filepath.Join(tmp, "home")
	if err := os.Mkdir(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("ICEBATH_HOME", filepath.Join(homeDir, ".icebath"))
	origin := filepath.Join(tmp, "origin.git")
	if err := os.Mkdir(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	gcTestGit(t, origin, nil, "init", "--bare", "-b", "main")
	clone := filepath.Join(tmp, "repo")
	gcTestGit(t, tmp, nil, "clone", origin, clone)
	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gcTestGit(t, clone, nil, "add", ".")
	gcTestGit(t, clone, nil, "commit", "-m", "init")
	gcTestGit(t, clone, nil, "push", "origin", "main")
	gcTestGit(t, clone, nil, "remote", "set-head", "origin", "main")
	if err := os.Mkdir(filepath.Join(clone, ".worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	return clone
}

// A repository with no remote at all, the home redirected under the temp dir
func gcTestLocalRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	homeDir := filepath.Join(tmp, "home")
	if err := os.Mkdir(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("ICEBATH_HOME", filepath.Join(homeDir, ".icebath"))
	repo := filepath.Join(tmp, "local-repo")
	gcTestGit(t, tmp, nil, "init", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gcTestGit(t, repo, nil, "add", ".")
	gcTestGit(t, repo, nil, "commit", "-m", "init")
	if err := os.Mkdir(filepath.Join(repo, ".worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	return repo
}

func gcAddWorktree(t *testing.T, repo, name, branch string) (string, string) {
	t.Helper()
	tree := filepath.Join(repo, ".worktrees", name)
	if branch == "" {
		branch = "icebath/" + name
	}
	gcTestGit(t, repo, nil, "worktree", "add", tree, "-b", branch)
	return tree, branch
}

func gcVerdict(t *testing.T, records []TreeRecord, name string) TreeRecord {
	t.Helper()
	for _, r := range records {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no record for %s", name)
	return TreeRecord{}
}

func gcAudit(t *testing.T, repo string, olderThan *float64) []TreeRecord {
	t.Helper()
	records, err := AuditWorktrees(context.Background(), repo, false, olderThan)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func gcReclaim(t *testing.T, repo string, dryRun bool, records []TreeRecord) []string {
	t.Helper()
	actions, err := ReclaimWorktrees(context.Background(), repo, dryRun, records)
	if err != nil {
		t.Fatal(err)
	}
	return actions
}

func gcBranchesByName(t *testing.T, repo string) (map[string]BranchRecord, []BranchRecord) {
	t.Helper()
	records, err := AuditBranches(repo)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]BranchRecord{}
	for _, r := range records {
		byName[r.Name] = r
	}
	return byName, records
}

func gcWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gcExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func gcAny(actions []string, needle string) bool {
	for _, a := range actions {
		if strings.Contains(a, needle) {
			return true
		}
	}
	return false
}

func TestAuditVerdictsCleanMergedTreeReaps(t *testing.T) {
	repo := gcTestRepo(t)
	gcAddWorktree(t, repo, "icebath-clean", "")
	if v := gcVerdict(t, gcAudit(t, repo, nil), "icebath-clean"); v.Verdict != "reap" {
		t.Fatalf("verdict %+v", v)
	}
}

func TestAuditVerdictsTrackedModificationsKeep(t *testing.T) {
	repo := gcTestRepo(t)
	tree, _ := gcAddWorktree(t, repo, "icebath-dirty", "")
	gcWrite(t, filepath.Join(tree, "README.md"), "edited\n")
	record := gcVerdict(t, gcAudit(t, repo, nil), "icebath-dirty")
	if record.Verdict != "keep" || !strings.Contains(record.Reason, "tracked") {
		t.Fatalf("record %+v", record)
	}
}

func TestAuditVerdictsUntrackedOnlyIsReapArchive(t *testing.T) {
	repo := gcTestRepo(t)
	tree, _ := gcAddWorktree(t, repo, "icebath-scratch", "")
	gcWrite(t, filepath.Join(tree, "PR_BODY_DRAFT.md"), "draft\n")
	record := gcVerdict(t, gcAudit(t, repo, nil), "icebath-scratch")
	if record.Verdict != "reap-archive" || len(record.Untracked) != 1 || record.Untracked[0] != "PR_BODY_DRAFT.md" {
		t.Fatalf("record %+v", record)
	}
}

func TestAuditVerdictsUniqueUnpushedCommitsKeep(t *testing.T) {
	repo := gcTestRepo(t)
	tree, _ := gcAddWorktree(t, repo, "icebath-work", "")
	gcWrite(t, filepath.Join(tree, "new.py"), "x = 1\n")
	gcTestGit(t, tree, nil, "add", ".")
	gcTestGit(t, tree, nil, "commit", "-m", "unique work")
	record := gcVerdict(t, gcAudit(t, repo, nil), "icebath-work")
	if record.Verdict != "keep" || !strings.Contains(record.Reason, "unpushed") {
		t.Fatalf("record %+v", record)
	}
}

func TestAuditVerdictsUniqueCommitsWithoutARemoteKeepTreeAndBranch(t *testing.T) {
	repo := gcTestLocalRepo(t)
	tree, branch := gcAddWorktree(t, repo, "icebath-local-work", "")
	gcWrite(t, filepath.Join(tree, "new.py"), "x = 1\n")
	gcTestGit(t, tree, nil, "add", ".")
	gcTestGit(t, tree, nil, "commit", "-m", "unique local work")
	records := gcAudit(t, repo, nil)
	record := gcVerdict(t, records, "icebath-local-work")
	if record.Verdict != "keep" || !strings.Contains(record.Reason, "unpushed") {
		t.Fatalf("record %+v", record)
	}
	if actions := gcReclaim(t, repo, false, records); len(actions) != 0 {
		t.Fatalf("actions %v", actions)
	}
	if !gcExists(tree) {
		t.Fatal("tree removed")
	}
	gcTestGit(t, repo, nil, "rev-parse", "--verify", branch)
}

func TestAuditVerdictsWithoutARemoteLocalTrunkIsTheBaseline(t *testing.T) {
	repo := gcTestLocalRepo(t)
	cleanTree, _ := gcAddWorktree(t, repo, "icebath-clean", "")
	squashedTree, _ := gcAddWorktree(t, repo, "icebath-squashed", "")
	gcWrite(t, filepath.Join(squashedTree, "k.py"), "k = 1\n")
	gcTestGit(t, squashedTree, nil, "add", ".")
	gcTestGit(t, squashedTree, nil, "commit", "-m", "squash me")
	gcTestGit(t, repo, nil, "merge", "--squash", "icebath/icebath-squashed")
	gcTestGit(t, repo, nil, "commit", "-m", "squash merge")
	gcTestGit(t, repo, nil, "branch", "orphan-unique", "main")
	gcTestGit(t, repo, nil, "checkout", "-q", "orphan-unique")
	gcWrite(t, filepath.Join(repo, "o.py"), "o = 1\n")
	gcTestGit(t, repo, nil, "add", ".")
	gcTestGit(t, repo, nil, "commit", "-m", "unique, no worktree")
	gcTestGit(t, repo, nil, "checkout", "-q", "main")
	records := gcAudit(t, repo, nil)
	if gcVerdict(t, records, "icebath-clean").Verdict != "reap" || gcVerdict(t, records, "icebath-squashed").Verdict != "reap" {
		t.Fatalf("records %+v", records)
	}
	gcReclaim(t, repo, false, records)
	if gcExists(cleanTree) || gcExists(squashedTree) {
		t.Fatal("reaped trees still exist")
	}
	byName, _ := gcBranchesByName(t, repo)
	if byName["orphan-unique"].Verdict != "keep" || !strings.Contains(byName["orphan-unique"].Reason, "unique") || byName["main"].Verdict != "keep" {
		t.Fatalf("branches %+v", byName)
	}
	if _, present := byName["icebath/icebath-squashed"]; present {
		t.Fatal("squashed branch survived its tree")
	}
}

func TestAuditVerdictsPatchEquivalentCommitsReap(t *testing.T) {
	repo := gcTestRepo(t)
	tree, _ := gcAddWorktree(t, repo, "icebath-merged", "")
	gcWrite(t, filepath.Join(tree, "feat.py"), "y = 2\n")
	gcTestGit(t, tree, nil, "add", ".")
	gcTestGit(t, tree, nil, "commit", "-m", "feat")
	sha := gcTestGit(t, tree, nil, "rev-parse", "HEAD")
	gcTestGit(t, repo, map[string]string{"GIT_COMMITTER_NAME": "other", "GIT_COMMITTER_EMAIL": "o@o"}, "cherry-pick", sha)
	gcTestGit(t, repo, nil, "push", "origin", "main")
	if v := gcVerdict(t, gcAudit(t, repo, nil), "icebath-merged"); v.Verdict != "reap" {
		t.Fatalf("verdict %+v", v)
	}
}

func TestAuditVerdictsLiveLockedTreeKeeps(t *testing.T) {
	repo := gcTestRepo(t)
	tree, _ := gcAddWorktree(t, repo, "icebath-live", "")
	gcTestGit(t, repo, nil, "worktree", "lock", tree, "--reason", "icebath pid="+strconv.Itoa(os.Getpid()))
	record := gcVerdict(t, gcAudit(t, repo, nil), "icebath-live")
	if record.Verdict != "keep" || !strings.Contains(record.Reason, "in use") {
		t.Fatalf("record %+v", record)
	}
}

func TestAuditVerdictsKanbanTreeUntouched(t *testing.T) {
	repo := gcTestRepo(t)
	gcAddWorktree(t, repo, "t_deadbeef", "kanban/t_deadbeef")
	record := gcVerdict(t, gcAudit(t, repo, nil), "t_deadbeef")
	if record.Verdict != "keep" || !strings.Contains(record.Reason, "kanban") {
		t.Fatalf("record %+v", record)
	}
}

func TestReclaimReapRemovesTreeAndBranch(t *testing.T) {
	repo := gcTestRepo(t)
	tree, branch := gcAddWorktree(t, repo, "icebath-clean", "")
	actions := gcReclaim(t, repo, false, gcAudit(t, repo, nil))
	if !gcAny(actions, "removed icebath-clean") || gcExists(tree) {
		t.Fatalf("actions %v exists %v", actions, gcExists(tree))
	}
	probe := exec.Command("git", "rev-parse", "--verify", "--quiet", branch)
	probe.Dir = repo
	if err := probe.Run(); err == nil {
		t.Fatal("branch should be gone with its tree")
	}
}

func TestReclaimUntrackedFilesArchivedUnderTheActiveProfileHome(t *testing.T) {
	repo := gcTestRepo(t)
	tmp := t.TempDir()
	nativeHome := filepath.Join(tmp, "native")
	profileHome := filepath.Join(tmp, "root", "profiles", "work")
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", nativeHome)
	t.Setenv("ICEBATH_HOME", profileHome)
	tree, _ := gcAddWorktree(t, repo, "icebath-scratch", "")
	gcWrite(t, filepath.Join(tree, "NOTES.md"), "important scribbles\n")
	gcReclaim(t, repo, false, gcAudit(t, repo, nil))
	if gcExists(tree) {
		t.Fatal("tree still exists")
	}
	var archived []string
	filepath.Walk(filepath.Join(profileHome, "archive", "worktree-prune"), func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Name() == "NOTES.md" {
			archived = append(archived, p)
		}
		return nil
	})
	if len(archived) == 0 {
		t.Fatal("untracked file must be archived under the profile home, not destroyed")
	}
	if body, _ := os.ReadFile(archived[0]); string(body) != "important scribbles\n" {
		t.Fatalf("archived body %q", body)
	}
	if gcExists(filepath.Join(nativeHome, ".icebath")) {
		t.Fatal("archive leaked into the native home")
	}
}

func TestReclaimDryRunChangesNothing(t *testing.T) {
	repo := gcTestRepo(t)
	tree, _ := gcAddWorktree(t, repo, "icebath-clean", "")
	actions := gcReclaim(t, repo, true, gcAudit(t, repo, nil))
	if !gcAny(actions, "would remove") || !gcExists(tree) {
		t.Fatalf("actions %v exists %v", actions, gcExists(tree))
	}
}

func TestReclaimFrozenListIgnoresTreesCreatedAfterAudit(t *testing.T) {
	repo := gcTestRepo(t)
	gcAddWorktree(t, repo, "icebath-old", "")
	records := gcAudit(t, repo, nil)
	lateTree, _ := gcAddWorktree(t, repo, "icebath-late", "")
	gcReclaim(t, repo, false, records)
	if !gcExists(lateTree) {
		t.Fatal("tree created after the audit must survive")
	}
}

func TestReclaimDeadLockedTreeIsUnlockedAndReaped(t *testing.T) {
	repo := gcTestRepo(t)
	tree, _ := gcAddWorktree(t, repo, "icebath-zombie", "")
	gcTestGit(t, repo, nil, "worktree", "lock", tree, "--reason", "icebath pid=999999999")
	records := gcAudit(t, repo, nil)
	if gcVerdict(t, records, "icebath-zombie").Verdict != "reap" {
		t.Fatalf("records %+v", records)
	}
	gcReclaim(t, repo, false, records)
	if gcExists(tree) {
		t.Fatal("dead locked tree survived")
	}
}

func TestBranchGCMergedBranchDeletedAnyName(t *testing.T) {
	repo := gcTestRepo(t)
	gcTestGit(t, repo, nil, "branch", "salv-12345", "main")
	gcTestGit(t, repo, nil, "branch", "feat/some-old-thing", "main")
	byName, records := gcBranchesByName(t, repo)
	if byName["salv-12345"].Verdict != "delete" || byName["feat/some-old-thing"].Verdict != "delete" {
		t.Fatalf("branches %+v", byName)
	}
	if _, err := ReclaimBranches(repo, false, records); err != nil {
		t.Fatal(err)
	}
	out := gcTestGit(t, repo, nil, "branch", "--format=%(refname:short)")
	if strings.Contains(out, "salv-12345") || strings.Contains(out, "feat/some-old-thing") {
		t.Fatalf("branches remain: %s", out)
	}
}

func TestBranchGCUniqueCommitBranchKept(t *testing.T) {
	repo := gcTestRepo(t)
	gcTestGit(t, repo, nil, "checkout", "-b", "feat/real-work")
	gcWrite(t, filepath.Join(repo, "wip.py"), "z = 3\n")
	gcTestGit(t, repo, nil, "add", ".")
	gcTestGit(t, repo, nil, "commit", "-m", "wip")
	gcTestGit(t, repo, nil, "checkout", "main")
	byName, _ := gcBranchesByName(t, repo)
	if byName["feat/real-work"].Verdict != "keep" || !strings.Contains(byName["feat/real-work"].Reason, "unique") {
		t.Fatalf("branches %+v", byName)
	}
}

func TestBranchGCPatchEquivalentBranchDeleted(t *testing.T) {
	repo := gcTestRepo(t)
	gcTestGit(t, repo, nil, "checkout", "-b", "fix/landed")
	gcWrite(t, filepath.Join(repo, "fix.py"), "a = 4\n")
	gcTestGit(t, repo, nil, "add", ".")
	gcTestGit(t, repo, nil, "commit", "-m", "fix")
	sha := gcTestGit(t, repo, nil, "rev-parse", "HEAD")
	gcTestGit(t, repo, nil, "checkout", "main")
	gcTestGit(t, repo, map[string]string{"GIT_COMMITTER_NAME": "other", "GIT_COMMITTER_EMAIL": "o@o"}, "cherry-pick", sha)
	gcTestGit(t, repo, nil, "push", "origin", "main")
	byName, _ := gcBranchesByName(t, repo)
	if byName["fix/landed"].Verdict != "delete" || !strings.Contains(byName["fix/landed"].Reason, "patch-equivalent") {
		t.Fatalf("branches %+v", byName)
	}
}

func TestBranchGCCheckedOutAndProtectedKept(t *testing.T) {
	repo := gcTestRepo(t)
	_, branch := gcAddWorktree(t, repo, "icebath-active", "")
	byName, _ := gcBranchesByName(t, repo)
	if byName["main"].Verdict != "keep" || byName[branch].Verdict != "keep" {
		t.Fatalf("branches %+v", byName)
	}
}

func olderThan(days float64) *float64 { return &days }

func TestOlderThanGateYoungReapableTreeKeptUnderOlderThan(t *testing.T) {
	repo := gcTestRepo(t)
	gcAddWorktree(t, repo, "icebath-young", "")
	record := gcVerdict(t, gcAudit(t, repo, olderThan(7)), "icebath-young")
	if record.Verdict != "keep" || !strings.Contains(record.Reason, "older-than") {
		t.Fatalf("record %+v", record)
	}
}

func TestOlderThanGateAgedReapableTreeStillReaps(t *testing.T) {
	repo := gcTestRepo(t)
	tree, _ := gcAddWorktree(t, repo, "icebath-old", "")
	old := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(tree, old, old); err != nil {
		t.Fatal(err)
	}
	if v := gcVerdict(t, gcAudit(t, repo, olderThan(7)), "icebath-old"); v.Verdict != "reap" {
		t.Fatalf("verdict %+v", v)
	}
}

func TestOlderThanGateOlderThanNeverWidensEligibility(t *testing.T) {
	repo := gcTestRepo(t)
	tree, _ := gcAddWorktree(t, repo, "icebath-old-work", "")
	gcWrite(t, filepath.Join(tree, "README.md"), "edited\n")
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(tree, old, old); err != nil {
		t.Fatal(err)
	}
	record := gcVerdict(t, gcAudit(t, repo, olderThan(7)), "icebath-old-work")
	if record.Verdict != "keep" || !strings.Contains(record.Reason, "tracked") {
		t.Fatalf("record %+v", record)
	}
}

func gcExternal(t *testing.T, repo string) []ExternalTreeRecord {
	t.Helper()
	records, err := AuditExternalTrees(repo)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestExternalTreesExternalTreeReportedNeverReaped(t *testing.T) {
	repo := gcTestRepo(t)
	ext := filepath.Join(t.TempDir(), "elsewhere-tree")
	gcTestGit(t, repo, nil, "worktree", "add", ext, "-b", "ext/branch")
	gcWrite(t, filepath.Join(ext, "WIP.txt"), "outside work\n")
	var record *ExternalTreeRecord
	for _, r := range gcExternal(t, repo) {
		if strings.Contains(r.Path, "elsewhere-tree") {
			r := r
			record = &r
		}
	}
	if record == nil || record.Branch != "ext/branch" || record.Missing {
		t.Fatalf("record %+v", record)
	}
	records := gcAudit(t, repo, nil)
	for _, r := range records {
		if strings.Contains(r.Name, "elsewhere-tree") {
			t.Fatalf("external tree audited as managed: %+v", r)
		}
	}
	gcReclaim(t, repo, false, records)
	if !gcExists(ext) || !gcExists(filepath.Join(ext, "WIP.txt")) {
		t.Fatal("external tree touched by the reclaim")
	}
}

func TestExternalTreesManagedTreesNotReportedAsExternal(t *testing.T) {
	repo := gcTestRepo(t)
	gcAddWorktree(t, repo, "icebath-managed", "")
	for _, r := range gcExternal(t, repo) {
		if strings.Contains(r.Path, "icebath-managed") {
			t.Fatalf("managed tree reported external: %+v", r)
		}
	}
}

func TestExternalTreesMissingRegistrationFlaggedAndPruned(t *testing.T) {
	repo := gcTestRepo(t)
	ext := filepath.Join(t.TempDir(), "vanished-tree")
	gcTestGit(t, repo, nil, "worktree", "add", ext, "-b", "ext/vanished")
	if err := os.RemoveAll(ext); err != nil {
		t.Fatal(err)
	}
	missing := false
	for _, r := range gcExternal(t, repo) {
		if strings.Contains(r.Path, "vanished-tree") {
			missing = r.Missing
		}
	}
	if !missing {
		t.Fatal("vanished registration not flagged missing")
	}
	planned, err := PruneMissingRegistrations(repo, true)
	if err != nil || !gcAny(planned, "vanished-tree") {
		t.Fatalf("planned %v %v", planned, err)
	}
	stillMissing := false
	for _, r := range gcExternal(t, repo) {
		stillMissing = stillMissing || r.Missing
	}
	if !stillMissing {
		t.Fatal("dry run pruned the registration")
	}
	done, err := PruneMissingRegistrations(repo, false)
	if err != nil || !gcAny(done, "pruned") {
		t.Fatalf("done %v %v", done, err)
	}
	for _, r := range gcExternal(t, repo) {
		if strings.Contains(r.Path, "vanished-tree") {
			t.Fatalf("registration survived the prune: %+v", r)
		}
	}
}

func TestGitFilterDiscoveryFailedExportMatchesTheRefusalText(t *testing.T) {
	if GitFilterDiscoveryFailed != gitFilterDiscoveryFailed {
		t.Fatalf("GitFilterDiscoveryFailed = %q", GitFilterDiscoveryFailed)
	}
	if GitFilterDiscoveryFailed != "git filter discovery failed" {
		t.Fatalf("GitFilterDiscoveryFailed = %q", GitFilterDiscoveryFailed)
	}
}
