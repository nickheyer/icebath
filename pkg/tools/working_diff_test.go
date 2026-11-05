package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Runs git in the repo with a hermetic identity and home
func wdGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = []string{"HOME=" + repo, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "PATH=" + os.Getenv("PATH")}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// Fresh repository with one committed tracked file
func wdRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git required for working-diff tests")
	}
	d := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	wdGit(t, d, "init", "-q")
	wdGit(t, d, "config", "core.autocrlf", "false")
	wdWrite(t, filepath.Join(d, "tracked.py"), "print('hello')\n")
	wdGit(t, d, "add", "-A")
	wdGit(t, d, "commit", "-q", "-m", "init")
	return d
}

func wdWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWorkingDiffCleanRepoReportsEmpty(t *testing.T) {
	repo := wdRepo(t)
	r := CollectWorkingDiff(repo, "working", nil)
	if !r.Success || !r.Empty || r.Diff != "" {
		t.Fatalf("got %+v", r)
	}
}

func TestWorkingDiffUnstagedChangeAppearsInDefaultMode(t *testing.T) {
	repo := wdRepo(t)
	wdWrite(t, filepath.Join(repo, "tracked.py"), "print('changed')\n")
	r := CollectWorkingDiff(repo, "", nil)
	if !r.Success || !strings.Contains(r.Diff, "-print('hello')") || !strings.Contains(r.Diff, "+print('changed')") || !strings.Contains(r.Stat, "tracked.py") {
		t.Fatalf("got %+v", r)
	}
}

func TestWorkingDiffUnknownModeRejected(t *testing.T) {
	repo := wdRepo(t)
	r := CollectWorkingDiff(repo, "bogus", nil)
	if r.Success || !strings.Contains(r.Error, "bogus") {
		t.Fatalf("got %+v", r)
	}
}

func TestWorkingDiffNonASCIIUntrackedFileDoesNotRaise(t *testing.T) {
	repo := wdRepo(t)
	wdWrite(t, filepath.Join(repo, "日本語ファイル.py"), "x = 'こんにちは'\n")
	r := CollectWorkingDiff(repo, "working", nil)
	if !r.Success {
		t.Fatalf("got %+v", r)
	}
	found := false
	for _, f := range r.Untracked {
		if strings.Contains(f, "日本語ファイル.py") {
			found = true
		}
	}
	if !found || !strings.Contains(r.Diff, "こんにちは") {
		t.Fatalf("got %+v", r)
	}
}

func TestWorkingDiffCp932ContentIsLossyButNeverRaises(t *testing.T) {
	repo := wdRepo(t)
	legacy := filepath.Join(repo, "legacy.txt")
	wdWrite(t, legacy, "before=\x93\x8c\x8b\x9e\n")
	wdGit(t, repo, "add", ".")
	wdGit(t, repo, "commit", "-q", "-m", "legacy")
	wdWrite(t, legacy, "after=\x93\xfa\x96\x7b\x8c\xea\n")
	r := CollectWorkingDiff(repo, "working", nil)
	if !r.Success || !strings.Contains(r.Diff, "legacy.txt") {
		t.Fatalf("got %+v", r)
	}
}

func TestWorkingDiffExternalDifferIsIgnored(t *testing.T) {
	repo := wdRepo(t)
	wdGit(t, repo, "config", "diff.external", "echo EXTERNAL-DIFF-GARBAGE")
	wdWrite(t, filepath.Join(repo, "tracked.py"), "print('changed')\n")
	wdWrite(t, filepath.Join(repo, "brand_new.py"), "print('new')\n")
	r := CollectWorkingDiff(repo, "working", nil)
	if !r.Success || strings.Contains(r.Diff, "EXTERNAL-DIFF-GARBAGE") {
		t.Fatalf("got %+v", r)
	}
	for _, want := range []string{"-print('hello')", "+print('changed')", "+print('new')"} {
		if !strings.Contains(r.Diff, want) {
			t.Fatalf("missing %q in %q", want, r.Diff)
		}
	}
	wdGit(t, repo, "add", "tracked.py")
	staged := CollectWorkingDiff(repo, "staged", nil)
	if !staged.Success || strings.Contains(staged.Diff, "EXTERNAL-DIFF-GARBAGE") || !strings.Contains(staged.Diff, "+print('changed')") {
		t.Fatalf("got %+v", staged)
	}
}

func TestHardenGitArgvInsertsAfterDiffSubcommandOnly(t *testing.T) {
	got := strings.Join(hardenGitArgv([]string{"-C", "diff", "diff", "--cached"}), " ")
	if got != "-C diff diff --no-ext-diff --no-textconv --cached" {
		t.Fatalf("got %q", got)
	}
	if got := strings.Join(hardenGitArgv([]string{"status", "--porcelain"}), " "); got != "status --porcelain" {
		t.Fatalf("got %q", got)
	}
}
