package pm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

func TestGitProgramRecognisesEveryFormOfGit(t *testing.T) {
	for arg0, want := range map[string]string{
		"git": "git", "/usr/bin/git": "git", "GIT.EXE": "git", `C:\Git\mingw64\libexec\git-core\GIT-COMMIT.EXE`: "git-commit",
		"/usr/lib/git-core/git-commit": "git-commit", " git ": "git", "gitk": "", "digit": "", "": "", "sh": "",
	} {
		if got := GitProgram(arg0); got != want {
			t.Fatalf("GitProgram(%q) = %q, want %q", arg0, got, want)
		}
	}
}

func TestGitSubcommandOfSkipsValueOptionsAndReadsDashedForms(t *testing.T) {
	if got := GitSubcommandOf(GitArgvBytes([]byte("/usr/lib/git-core/git-commit\x00-a\x00"))); got != "commit" {
		t.Fatalf("dashed bytes form %q", got)
	}
	if got := GitSubcommandOf([]string{`C:\Git\mingw64\libexec\git-core\GIT-COMMIT.EXE`}); got != "commit" {
		t.Fatalf("exe form %q", got)
	}
	if got := GitSubcommandOf([]string{"git", "-C", "x", "-c", "k=v", "--no-pager", "save", "-a"}); got != "save" {
		t.Fatalf("value options %q", got)
	}
	if got := GitSubcommandOf([]string{"git", "--version"}); got != "" {
		t.Fatalf("no subcommand %q", got)
	}
	if got := GitSubcommandOf([]string{"sh", "commit"}); got != "" {
		t.Fatalf("not git %q", got)
	}
	if !GitNeverLocks("fsmonitor--daemon") || GitNeverLocks("commit") {
		t.Fatal("daemon rule")
	}
}

func TestCheckoutPlacesAndGitWorksInCompareByPathComponents(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "icebath")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	places := CheckoutPlaces(root, filepath.Join(root, ".git"))
	if !slices.IsSorted(places) || len(places) < 2 {
		t.Fatalf("places %v", places)
	}
	if GitWorksIn([]string{"git", "commit"}, filepath.Join(tmp, "icebath-backup"), nil, places) {
		t.Fatal("a sibling directory is not this checkout")
	}
	if !GitWorksIn([]string{"git", "commit"}, filepath.Join(root, "sub"), nil, places) {
		t.Fatal("a subdirectory cwd works in the checkout")
	}
	if !GitWorksIn([]string{"git", "--git-dir=" + filepath.Join(root, ".git"), "fetch"}, tmp, nil, places) {
		t.Fatal("an option valued path works in the checkout")
	}
	if !GitWorksIn([]string{"git", "add", "icebath/a.txt"}, tmp, nil, places) {
		t.Fatal("a relative path argument is joined to the cwd")
	}
	if GitWorksIn([]string{"git", "add", "icebath/a.txt"}, "", nil, places) {
		t.Fatal("a relative path without a cwd places nothing")
	}
	if !GitWorksIn([]string{"git", "status"}, tmp, map[string]string{"GIT_DIR": filepath.Join(root, ".git")}, places) {
		t.Fatal("GIT_DIR works in the checkout")
	}
	if common, ok := commonPath(filepath.Join(root, "a", "b"), filepath.Join(root, "a", "c")); !ok || common != filepath.Join(root, "a") {
		t.Fatalf("commonPath %q %v", common, ok)
	}
	if runtime.GOOS != "windows" {
		if common, ok := commonPath("/x/y", "/"); !ok || common != "/" {
			t.Fatalf("root commonPath %q %v", common, ok)
		}
	}
}

func TestLockHoldAnswersAndTheProbeSeams(t *testing.T) {
	if !lockFree.Free() || lockFree.Held() || lockUnknown.Free() || lockUnknown.Unknown == false {
		t.Fatal("answers")
	}
	held := lockHolder("4242", "git commit")
	if !held.Held() || held.Free() || held.Holder != "pid 4242 (git commit)" {
		t.Fatalf("holder %+v", held)
	}
	prev := SwapCountsAsHolder(func([]string, bool) bool { return false })
	if countsAsHolderFn([]string{"git", "commit"}, false) {
		t.Fatal("swapped rule")
	}
	SwapCountsAsHolder(prev)
	if !countsAsHolderFn([]string{"git", "commit"}, false) || countsAsHolderFn([]string{"git", "log"}, false) || !countsAsHolderFn([]string{"git", "log"}, true) || countsAsHolderFn([]string{"git", "fsmonitor--daemon"}, true) {
		t.Fatal("holder rule")
	}
	prevWhich := SwapLockWhich(func(string) (string, bool) { return "", false })
	t.Cleanup(func() { SwapLockWhich(prevWhich) })
	prevRun := SwapLockRunner(func(context.Context, []string, pysubprocess.Options) (*pysubprocess.Completed, error) {
		return nil, errors.New("[Errno 2] No such file or directory: 'lsof'")
	})
	t.Cleanup(func() { SwapLockRunner(prevRun) })
	lock := filepath.Join(t.TempDir(), "index.lock")
	if got := HeldOpenLsof(lock, "", false); !got.Unknown {
		t.Fatalf("a failing lsof answers unknown, got %+v", got)
	}
	SwapLockWhich(func(name string) (string, bool) { return name, true })
	SwapLockRunner(func(_ context.Context, argv []string, _ pysubprocess.Options) (*pysubprocess.Completed, error) {
		return &pysubprocess.Completed{Args: argv, Returncode: 1}, nil
	})
	if got := HeldOpenLsof(lock, "", false); !got.Free() {
		t.Fatalf("an empty lsof without a root is free, got %+v", got)
	}
}

func TestReleaseDeadLockDropsAnUnheldLockAndKeepsNothingAbsent(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(gitDir, "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		prevWhich := SwapLockWhich(func(name string) (string, bool) { return name, true })
		t.Cleanup(func() { SwapLockWhich(prevWhich) })
		prevRun := SwapLockRunner(func(_ context.Context, argv []string, _ pysubprocess.Options) (*pysubprocess.Completed, error) {
			return &pysubprocess.Completed{Args: argv, Returncode: 1}, nil
		})
		t.Cleanup(func() { SwapLockRunner(prevRun) })
	}
	released, err := ReleaseDeadLock(lock, root, false)
	if err != nil || !released {
		t.Fatalf("release %v %v", released, err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("lock stays")
	}
	released, err = ReleaseDeadLock(lock, root, false)
	if err != nil || !released {
		t.Fatalf("absent lock %v %v", released, err)
	}
	if got, err := CheckoutGitDir(root); err != nil || got != gitDir {
		t.Fatalf("git dir %q %v", got, err)
	}
	if got := IndexLockHolder(gitDir, root); got != "a running git" {
		t.Fatalf("holder text %q", got)
	}
}
