package cron

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

func sweepGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func sweepRepo(t *testing.T, tmp string) string {
	t.Helper()
	origin := filepath.Join(tmp, "origin.git")
	sweepGit(t, tmp, "init", "-q", "--bare", "-b", "main", origin)
	clone := filepath.Join(tmp, "repo")
	sweepGit(t, tmp, "init", "-q", "-b", "main", clone)
	for name, body := range map[string]string{".gitignore": ".worktrees/\nnode_modules/\n", ".gitattributes": "*.txt text eol=crlf\n", "a.txt": "one\ntwo\n", "b.py": "x = 1\n"} {
		if err := os.WriteFile(filepath.Join(clone, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sweepGit(t, clone, "add", "-A")
	sweepGit(t, clone, "commit", "-qm", "init")
	sweepGit(t, clone, "remote", "add", "origin", origin)
	sweepGit(t, clone, "push", "-q", "origin", "main")
	sweepGit(t, clone, "fetch", "-q", "origin")
	if err := os.Mkdir(filepath.Join(clone, ".worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	return clone
}

func realpath(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return resolved
}

func TestCronSweepVisitsOnlyTheActiveProfilesRegisteredRepos(t *testing.T) {
	tmp := t.TempDir()
	repo := sweepRepo(t, tmp)
	homeA, homeB := filepath.Join(tmp, "home-a"), filepath.Join(tmp, "home-b")
	for _, h := range []string{homeA, homeB} {
		if err := os.Mkdir(h, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", homeA)
	if info := tools.SetupWorktree(ctx, repo, false, "reg", nil); info == nil {
		t.Fatal("worktree setup failed")
	}
	gone := filepath.Join(tmp, "gone")
	sweepGit(t, tmp, "init", "-q", gone)
	if err := os.Mkdir(filepath.Join(gone, ".worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	RecordWorktreeRepo(ctx, homeA, gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	reposA := openCronStore(homeA, time.Now).worktreeMaintenanceRepos(ctx)
	t.Setenv("ICEBATH_HOME", homeB)
	reposB := openCronStore(homeB, time.Now).worktreeMaintenanceRepos(ctx)
	if !slices.Contains(reposA, realpath(t, repo)) {
		t.Fatalf("profile A does not sweep its registered repo: %v", reposA)
	}
	if slices.Contains(reposA, gone) || slices.Contains(reposA, realpath(t, gone)) {
		t.Fatalf("a deleted repo stayed registered: %v", reposA)
	}
	if slices.Contains(reposB, realpath(t, repo)) {
		t.Fatalf("profile B sweeps a repo profile A registered: %v", reposB)
	}
}
