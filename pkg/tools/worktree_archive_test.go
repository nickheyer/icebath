package tools

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	worktreeRecordedMu sync.Mutex
	worktreeRecorded   []string
)

// Records registry writes in memory so worktree tests can assert the repos a restore registered
func installWorktreeDoubles() {
	WorktreeRecordRepo = func(repoRoot string) {
		worktreeRecordedMu.Lock()
		defer worktreeRecordedMu.Unlock()
		worktreeRecorded = append(worktreeRecorded, repoRoot)
	}
}

// Runs git with a fixed identity in dir and fails the test on a non zero exit
func waGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// Clone of a bare origin with a pushed main, crlf text attributes and an ignored worktrees dir
func waRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	origin := filepath.Join(tmp, "origin.git")
	waGit(t, tmp, "init", "-q", "--bare", "-b", "main", origin)
	clone := filepath.Join(tmp, "repo")
	waGit(t, tmp, "init", "-q", "-b", "main", clone)
	for name, body := range map[string]string{".gitignore": ".worktrees/\nnode_modules/\n", ".gitattributes": "*.txt text eol=crlf\n", "a.txt": "one\ntwo\n", "b.py": "x = 1\n"} {
		waWrite(t, filepath.Join(clone, name), body)
	}
	waGit(t, clone, "add", "-A")
	waGit(t, clone, "commit", "-qm", "init")
	waGit(t, clone, "remote", "add", "origin", origin)
	waGit(t, clone, "push", "-q", "origin", "main")
	waGit(t, clone, "fetch", "-q", "origin")
	if err := os.Mkdir(filepath.Join(clone, ".worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	return clone
}

func waWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Adds a tree on branch wt/name, optionally with an unpushed commit and an uncommitted file
func waTree(t *testing.T, repo, name string, dirty, unpushed bool) string {
	t.Helper()
	path := filepath.Join(repo, ".worktrees", name)
	waGit(t, repo, "worktree", "add", "-q", "-b", "wt/"+name, path, "HEAD")
	if unpushed {
		waWrite(t, filepath.Join(path, "feature.py"), "work for "+name+"\n")
		waGit(t, path, "add", "feature.py")
		waGit(t, path, "commit", "-qm", "wip")
	}
	if dirty {
		waWrite(t, filepath.Join(path, "notes.md"), "uncommitted\n")
	}
	return path
}

// Ages the tree and its activity markers so the sweep sees it idle for the given hours
func waIdle(t *testing.T, path string, hours float64) {
	t.Helper()
	stamp := time.Now().Add(-time.Duration(hours * float64(time.Hour)))
	gitDir := waGit(t, path, "rev-parse", "--absolute-git-dir")
	items := []string{}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && p != path {
			items = append(items, p)
		}
		return nil
	})
	items = append(items, filepath.Join(gitDir, "HEAD"), filepath.Join(gitDir, "logs", "HEAD"), path)
	for _, item := range items {
		if st, err := os.Lstat(item); err == nil && st.Mode()&os.ModeSymlink == 0 {
			if err := os.Chtimes(item, stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func waArchives(t *testing.T, repo string) []string {
	t.Helper()
	var refs []string
	for _, line := range strings.Split(waGit(t, repo, "for-each-ref", "--format=%(refname)", ArchivePrefix), "\n") {
		if line != "" {
			refs = append(refs, line)
		}
	}
	return refs
}

// Sha256 of every working file outside git metadata and node_modules keyed by relative path
func waDigest(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if slices.Contains(parts, ".git") || slices.Contains(parts, "node_modules") {
			return nil
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			data, _ := os.ReadFile(p)
			sum := sha256.Sum256(data)
			out[rel] = hex.EncodeToString(sum[:])
		}
		return nil
	})
	return out
}

// HEAD, staged, unstaged crlf and untracked bytes survive delete then restore and archiving never touches the tree
func TestRemovedDirtyTreeRestoresByteForByteAndArchivingNeverTouchesTheTree(t *testing.T) {
	repo := waRepo(t)
	waWrite(t, filepath.Join(repo, ".git", "info", "attributes"), "*.md text eol=crlf\n")
	waWrite(t, filepath.Join(repo, "attrs"), "*.cfg text eol=lf\n")
	waGit(t, repo, "config", "core.attributesFile", filepath.Join(repo, "attrs"))
	wt := waTree(t, repo, "lane-roundtrip", false, true)
	waWrite(t, filepath.Join(wt, "lf.md"), "lf only\n")
	waWrite(t, filepath.Join(wt, "crlf.cfg"), "keep\r\nthe CR\r\n")
	waWrite(t, filepath.Join(wt, "b.py"), "x = 2\n")
	waGit(t, wt, "add", "b.py")
	waWrite(t, filepath.Join(wt, "b.py"), "x = 3\n")
	waWrite(t, filepath.Join(wt, "a.txt"), "one\r\nTWO\r\n")
	blob := make([]byte, 4096)
	_, _ = rand.Read(blob)
	waWrite(t, filepath.Join(wt, "new dir", "blob.bin"), string(blob))
	waWrite(t, filepath.Join(wt, "node_modules", "dep.js"), "ignored, never archived\n")
	waWrite(t, filepath.Join(wt, "big.dat"), strings.Repeat("\x00", 2*1024*1024))
	gitDir := waGit(t, wt, "rev-parse", "--absolute-git-dir")
	indexBefore, _ := os.ReadFile(filepath.Join(gitDir, "index"))
	filesBefore := waDigest(t, wt)
	head, staged := waGit(t, wt, "rev-parse", "HEAD"), waGit(t, wt, "diff", "--cached")
	arc, err := SnapshotWorktree(repo, wt, "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	if indexAfter, _ := os.ReadFile(filepath.Join(gitDir, "index")); string(indexAfter) != string(indexBefore) {
		t.Fatal("archiving rewrote the real index")
	}
	if got := waDigest(t, wt); !maps.Equal(got, filesBefore) {
		t.Fatalf("archiving touched working files %v", got)
	}
	skipped, _ := arc.Meta.Get("skipped_untracked")
	list, _ := skipped.([]any)
	if len(list) != 1 {
		t.Fatalf("skipped %v", skipped)
	}
	if p, _ := list[0].(interface{ Get(string) (any, bool) }).Get("path"); p != "big.dat" {
		t.Fatalf("skipped %v", p)
	}
	if strings.Contains(waGit(t, repo, "ls-tree", "-r", "--name-only", arc.Sha), "node_modules/dep.js") {
		t.Fatal("ignored dependency archived")
	}
	waGit(t, repo, "worktree", "remove", "--force", wt)
	waGit(t, repo, "branch", "-D", "wt/lane-roundtrip")
	restored, err := RestoreArchive(repo, "lane-roundtrip", "")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Path != wt || restored.Branch != "wt/lane-roundtrip" {
		t.Fatalf("restored %+v", restored)
	}
	want := maps.Clone(filesBefore)
	delete(want, "big.dat")
	if got := waDigest(t, wt); !maps.Equal(got, want) {
		t.Fatalf("restored files %v want %v", got, want)
	}
	if got := waGit(t, wt, "rev-parse", "HEAD"); got != head {
		t.Fatalf("head %s want %s", got, head)
	}
	if got := waGit(t, wt, "diff", "--cached"); got != staged {
		t.Fatalf("staged %q want %q", got, staged)
	}
	worktreeRecordedMu.Lock()
	defer worktreeRecordedMu.Unlock()
	if !slices.Contains(worktreeRecorded, repo) {
		t.Fatalf("restore never registered %s", repo)
	}
}
