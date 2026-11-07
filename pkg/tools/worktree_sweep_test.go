package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func wsExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func wsSetWorktreesConfig(t *testing.T, text string) {
	t.Helper()
	waWrite(t, filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), text)
}

func wsPrune(t *testing.T, repo string) {
	t.Helper()
	if err := PruneStaleWorktrees(context.Background(), repo, 24); err != nil {
		t.Fatal(err)
	}
}

// Idle work is archived then removed while live, kanban, young and unarchivable trees are never touched
func TestSweepArchivesIdleWorkAndNeverTouchesLiveKanbanOrYoungTrees(t *testing.T) {
	repo := waRepo(t)
	clean := waTree(t, repo, "lane-clean", false, false)
	dirty := waTree(t, repo, "lane-dirty", true, false)
	unpushed := waTree(t, repo, "icebath-unpushed", false, true)
	liveCwd := waTree(t, repo, "lane-live-cwd", true, false)
	liveLock := waTree(t, repo, "icebath-live-lock", true, false)
	waGit(t, repo, "worktree", "lock", "--reason", fmt.Sprintf("icebath pid=%d", os.Getpid()), liveLock)
	kanban := waTree(t, repo, "t_0abc12", true, false)
	young := waTree(t, repo, "lane-young", true, false)
	unarchivable := waTree(t, repo, "lane-embedded", true, false)
	waGit(t, repo, "init", "-q", filepath.Join(unarchivable, "inner"))
	waWrite(t, filepath.Join(unarchivable, "inner", "f"), "x")
	subagent := waTree(t, repo, "subagent-done", true, false)
	deleted := waTree(t, repo, "lane-deleted", false, false)
	waWrite(t, filepath.Join(deleted, "sub", "x.py"), "x\n")
	waWrite(t, filepath.Join(deleted, "sub", "y.py"), "y\n")
	waGit(t, deleted, "add", "sub")
	waGit(t, deleted, "commit", "-qm", "sub")
	for _, tree := range []string{clean, dirty, unpushed, liveCwd, liveLock, kanban, unarchivable, deleted} {
		waIdle(t, tree, 100)
	}
	waIdle(t, subagent, 30)
	if err := os.Remove(filepath.Join(deleted, "sub", "x.py")); err != nil {
		t.Fatal(err)
	}
	sleeper := exec.Command("sleep", "120")
	sleeper.Dir = liveCwd
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()
		wsPrune(t, repo)
	}()
	if wsExists(clean) {
		t.Fatal("clean tree kept")
	}
	for _, gone := range []string{dirty, unpushed} {
		if wsExists(gone) {
			t.Fatalf("idle %s should be archived then removed", filepath.Base(gone))
		}
	}
	if wsExists(subagent) {
		t.Fatal("subagent-* trees age on the 24h tier")
	}
	archived := map[string]bool{}
	for _, ref := range waArchives(t, repo) {
		archived[ref[strings.LastIndex(ref, "/")+1:]] = true
	}
	if len(archived) != 3 || !archived["lane-dirty"] || !archived["icebath-unpushed"] || !archived["subagent-done"] {
		t.Fatalf("archived %v", archived)
	}
	if !wsExists(filepath.Join(deleted, "sub", "y.py")) {
		t.Fatal("a fresh deletion is activity: the tree is not idle")
	}
	if got := waGit(t, repo, "show", ArchivePrefix+"lane-dirty:notes.md"); got != "uncommitted" {
		t.Fatalf("archived notes %q", got)
	}
	for _, kept := range []string{liveCwd, liveLock, kanban, young, unarchivable} {
		data, err := os.ReadFile(filepath.Join(kept, "notes.md"))
		if err != nil || string(data) != "uncommitted\n" {
			t.Fatalf("%s touched: %v %q", filepath.Base(kept), err, data)
		}
	}
}

func wsBig(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(2 * 1024 * 1024); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("UNIQUE BYTES"), 0); err != nil {
		t.Fatal(err)
	}
}

func wsPrivateCommit(t *testing.T, dir string) string {
	t.Helper()
	waGit(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	waWrite(t, filepath.Join(dir, "x"), "PRIVATE\n")
	waGit(t, dir, "add", "-A")
	waGit(t, dir, "commit", "-qm", "private")
	return waGit(t, dir, "rev-parse", "HEAD")
}

type wsCommit struct{ where, sha string }

type wsCase func(t *testing.T, repo, wt string) (map[string][]byte, *wsCommit)

var wsCases = []struct {
	name  string
	setup wsCase
}{
	{"large", func(t *testing.T, repo, wt string) (map[string][]byte, *wsCommit) {
		waWrite(t, filepath.Join(wt, "notes.md"), "dirty\n")
		wsBig(t, filepath.Join(wt, "dataset.bin"))
		return map[string][]byte{"dataset.bin": nil}, nil
	}},
	{"ignored_repo", func(t *testing.T, repo, wt string) (map[string][]byte, *wsCommit) {
		waWrite(t, filepath.Join(wt, "notes.md"), "dirty\n")
		dir := filepath.Join(wt, "build", "tool")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return map[string][]byte{}, &wsCommit{dir, wsPrivateCommit(t, dir)}
	}},
	{"untracked_hidden", func(t *testing.T, repo, wt string) (map[string][]byte, *wsCommit) {
		waGit(t, repo, "config", "status.showUntrackedFiles", "no")
		waWrite(t, filepath.Join(wt, "new_work.py"), "UNTRACKED WORK\n")
		return map[string][]byte{"new_work.py": []byte("UNTRACKED WORK\n")}, nil
	}},
	{"skip_worktree", func(t *testing.T, repo, wt string) (map[string][]byte, *wsCommit) {
		waGit(t, wt, "update-index", "--skip-worktree", "b.py")
		waGit(t, wt, "update-index", "--assume-unchanged", "a.txt")
		waWrite(t, filepath.Join(wt, "b.py"), "SKIP-WORKTREE EDIT\n")
		return map[string][]byte{"b.py": []byte("SKIP-WORKTREE EDIT\n")}, nil
	}},
	{"worktree_ref", func(t *testing.T, repo, wt string) (map[string][]byte, *wsCommit) {
		waWrite(t, filepath.Join(wt, "b.py"), "private\n")
		waGit(t, wt, "commit", "-qam", "private")
		sha := waGit(t, wt, "rev-parse", "HEAD")
		waGit(t, wt, "update-ref", "refs/worktree/keep", sha)
		waGit(t, wt, "update-ref", "refs/bisect/bad", sha)
		waGit(t, wt, "reset", "-q", "--hard", "origin/main")
		return map[string][]byte{}, &wsCommit{repo, sha}
	}},
}

func wsCopy(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	st, _ := in.Stat()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(dst, st.ModTime(), st.ModTime())
}

// Every byte and commit git cannot regenerate survives in the tree or its archive, ignored files never hold a tree back
func TestThePrunerRemovesATreeOnlyWhenEverythingIrreproducibleIsArchived(t *testing.T) {
	for _, mode := range []string{"archive", "classic"} {
		for _, c := range wsCases {
			t.Run(mode+"_"+c.name, func(t *testing.T) {
				repo := waRepo(t)
				if mode == "classic" {
					wsSetWorktreesConfig(t, "worktrees:\n  archive_dirty: false\n")
				} else {
					wsSetWorktreesConfig(t, "worktrees:\n  archive_max_file_mb: 1\n")
				}
				waWrite(t, filepath.Join(repo, ".git", "info", "exclude"), "build/\n*.secret\n.env\n__pycache__/\ntest_durations.json\n")
				waWrite(t, filepath.Join(repo, ".env"), "SHARED=1\n")
				wt, regen := waTree(t, repo, "lane-case", false, false), waTree(t, repo, "lane-regen", true, false)
				for _, tree := range []string{wt, regen} {
					wsCopy(t, filepath.Join(repo, ".env"), filepath.Join(tree, ".env"))
					waWrite(t, filepath.Join(tree, "node_modules", "dep", "index.js"), "x\n")
					waWrite(t, filepath.Join(tree, "pkg", "__pycache__", "m.pyc"), "\x00")
				}
				waWrite(t, filepath.Join(regen, ".env"), "LOCAL EDIT=1\n")
				waWrite(t, filepath.Join(regen, "test_durations.json"), "{}\n")
				waWrite(t, filepath.Join(regen, "notes.secret"), "scratch\n")
				precious, commit := c.setup(t, repo, wt)
				for rel, data := range precious {
					if data == nil {
						b, err := os.ReadFile(filepath.Join(wt, rel))
						if err != nil {
							t.Fatal(err)
						}
						precious[rel] = b
					}
				}
				for _, tree := range []string{wt, regen} {
					waIdle(t, tree, 100)
				}
				wsPrune(t, repo)
				archives := waArchives(t, repo)
				for rel, data := range precious {
					got, err := os.ReadFile(filepath.Join(wt, rel))
					found := err == nil && string(got) == string(data)
					for _, ref := range archives {
						if found || !strings.HasSuffix(ref, "/lane-case") {
							continue
						}
						blob, _ := exec.Command("git", "-C", repo, "cat-file", "blob", ref+":"+rel).Output()
						found = string(blob) == string(data)
					}
					if !found {
						t.Fatalf("%s lost", rel)
					}
				}
				if commit != nil {
					if !wsExists(commit.where) || waGit(t, commit.where, "cat-file", "-t", commit.sha) != "commit" {
						t.Fatalf("commit %s lost", commit.sha)
					}
					if commit.where == repo && !wsExists(wt) {
						t.Fatal("tree holding a private per-worktree ref removed")
					}
				}
				if mode == "archive" {
					if wsExists(regen) {
						t.Fatal("ignored files are disposable and must not hold a tree back")
					}
					hit := false
					for _, ref := range archives {
						hit = hit || strings.HasSuffix(ref, "/lane-regen")
					}
					if !hit {
						t.Fatal("the dirty tree is archived first")
					}
				}
			})
		}
	}
}

// The archive_dirty opt out removes exactly what the classic pruner removes and archives nothing
func TestArchiveDirtyFalseKeepsTheClassicNeverDeleteDirtyPruner(t *testing.T) {
	repo := waRepo(t)
	wsSetWorktreesConfig(t, "worktrees:\n  archive_dirty: false\n")
	clean := waTree(t, repo, "lane-clean", false, false)
	dirty := waTree(t, repo, "lane-dirty", true, false)
	unpushed := waTree(t, repo, "icebath-unpushed", false, true)
	for _, tree := range []string{clean, dirty, unpushed} {
		waIdle(t, tree, 100)
	}
	wsPrune(t, repo)
	if wsExists(clean) || !wsExists(dirty) || !wsExists(unpushed) {
		t.Fatalf("clean %v dirty %v unpushed %v", wsExists(clean), wsExists(dirty), wsExists(unpushed))
	}
	if a := waArchives(t, repo); len(a) != 0 {
		t.Fatalf("archives %v", a)
	}
}
