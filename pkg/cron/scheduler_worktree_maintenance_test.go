package cron

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs git with a fixed identity in dir and fails the test on a non zero exit
func wmGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Ages a tree and its git activity markers so the sweep sees it idle for the given hours
func wmIdle(t *testing.T, path string, hours float64) {
	t.Helper()
	stamp := time.Now().Add(-time.Duration(hours * float64(time.Hour)))
	gitDir := wmGit(t, path, "rev-parse", "--absolute-git-dir")
	items := []string{filepath.Join(gitDir, "HEAD"), filepath.Join(gitDir, "logs", "HEAD"), path}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && p != path {
			items = append(items, p)
		}
		return nil
	})
	for _, item := range items {
		if st, err := os.Lstat(item); err == nil && st.Mode()&os.ModeSymlink == 0 {
			if err := os.Chtimes(item, stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The production prune seam reaches the worktree port with the 24 hour age so a day old tree goes and a young one stays
func TestProductionWorktreePruneUsesTheTwentyFourHourAge(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	origin := filepath.Join(tmp, "origin.git")
	wmGit(t, tmp, "init", "-q", "--bare", "-b", "main", origin)
	repo := filepath.Join(tmp, "repo")
	wmGit(t, tmp, "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wmGit(t, repo, "add", "-A")
	wmGit(t, repo, "commit", "-qm", "init")
	wmGit(t, repo, "remote", "add", "origin", origin)
	wmGit(t, repo, "push", "-q", "origin", "main")
	wmGit(t, repo, "fetch", "-q", "origin")
	trees := map[string]float64{"subagent-old": 30, "subagent-young": 10}
	for name, hours := range trees {
		path := filepath.Join(repo, ".worktrees", name)
		wmGit(t, repo, "worktree", "add", "-q", "-b", "wt/"+name, path, "HEAD")
		wmIdle(t, path, hours)
	}
	if err := realWorktreePrune(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(repo, ".worktrees", "subagent-old")); err == nil {
		t.Fatal("tree idle past 24 hours kept")
	}
	if _, err := os.Lstat(filepath.Join(repo, ".worktrees", "subagent-young")); err != nil {
		t.Fatalf("tree idle 10 hours pruned: %v", err)
	}
}

// The registry hook records into the process home that pkg/cron reads its maintenance repos from
func TestWorktreeRecordRepoHookRecordsIntoTheProcessHome(t *testing.T) {
	s, _ := newTestStore(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	tools.WorktreeRecordRepo(repo)
	want, err := pypath.Realpath(repo, pypath.Lax)
	if err != nil {
		t.Fatal(err)
	}
	if got := worktreeRegisteredRepos(s.Home()); !slices.Contains(got, want) {
		t.Fatalf("registry %v lacks %s", got, want)
	}
}

// The cron store hooks name the active home's jobs.json, decode it, and judge a stored dict as the scheduler does
func TestWorktreeCronStoreHooksReadTheActiveHomesJobs(t *testing.T) {
	s, _ := newTestStore(t)
	if got := tools.WorktreeCronStore.JobsFile(); got != s.JobsFile() {
		t.Fatalf("jobs file %q, want %q", got, s.JobsFile())
	}
	body := `{"jobs": [{"id": "a", "workdir": "/w"}], "updated_at": "x"}`
	if err := os.WriteFile(s.JobsFile(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := tools.WorktreeCronStore.ParseJobs(s.JobsFile())
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		t.Fatalf("decoded %T", data)
	}
	if jobs, _ := obj.Get("jobs"); len(jobs.([]any)) != 1 {
		t.Fatalf("jobs %v", jobs)
	}
	if err := os.WriteFile(s.JobsFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.WorktreeCronStore.ParseJobs(s.JobsFile()); err == nil {
		t.Fatal("undecodable store read without an error")
	}
	cases := []struct {
		body   string
		active bool
	}{
		{`{}`, true},
		{`{"enabled": true, "state": "scheduled"}`, true},
		{`{"enabled": 1}`, true},
		{`{"enabled": false}`, false},
		{`{"enabled": null}`, false},
		{`{"enabled": 0}`, false},
		{`{"state": " paused "}`, false},
		{`{"paused_at": "2026-01-01T00:00:00"}`, false},
		{`{"paused_at": ""}`, true},
		{`{"state": "completed"}`, false},
		{`{"state": "error"}`, false},
		{`{"state": "running"}`, true},
		{`{"state": null}`, true},
	}
	for _, c := range cases {
		v, err := pyjson.Loads(c.body)
		if err != nil {
			t.Fatal(err)
		}
		if got := tools.WorktreeCronStore.JobActive(v.(*pyjson.Object)); got != c.active {
			t.Errorf("%s active %v, want %v", c.body, got, c.active)
		}
	}
}
