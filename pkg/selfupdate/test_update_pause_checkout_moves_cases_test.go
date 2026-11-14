//go:build unix

package selfupdate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Empty checkout on main whose project root and pause install root are the checkout
func cmRepo(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores read-only dirs")
	}
	root := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ucGit(t, root, "init", "-q", "-b", "main")
	ucGit(t, root, "config", "user.name", "Fixture")
	ucGit(t, root, "config", "user.email", "fixture@example.com")
	ucPatchRoot(t, root)
	t.Cleanup(pm.PatchRepoRoot(root))
	t.Cleanup(func() {
		if locked := filepath.Join(root, "locked"); mhExists(locked) {
			_ = os.Chmod(locked, 0o755)
		}
	})
	return root
}

// Holds the restore claim as another launch would, so the in-process repair gives up and keeps the marker
func cmRepairBlocked(t *testing.T, root string) func() {
	t.Helper()
	saved := restoreClaimWaitSeconds
	restoreClaimWaitSeconds = 0.2
	marker, err := pm.InterruptedPullMarkerPath(root)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(marker), restoreClaimName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if !lockClaimFile(f, true) {
		f.Close()
		t.Fatal("the restore claim could not be held")
	}
	return func() {
		lockClaimFile(f, false)
		f.Close()
		restoreClaimWaitSeconds = saved
	}
}

// Bare upstream cloned from root with main at tip, added to root as the upstream remote
func cmBareUpstream(t *testing.T, root, dir, tip string) string {
	t.Helper()
	remote := filepath.Join(dir, "upstream.git")
	ucGit(t, dir, "clone", "-q", "--bare", root, remote)
	ucGit(t, remote, "update-ref", "refs/heads/main", tip)
	ucGit(t, root, "remote", "add", "upstream", remote)
	return remote
}

func TestATornFirstBranchSwitchStaysHeldAfterARefOnlyFetch(t *testing.T) {
	repo := cmRepo(t)
	a := mhCommit(t, repo, "A", map[string]string{"a.py": "A=1\n"})
	ucGit(t, repo, "checkout", "-q", "-b", "feature")
	ucGit(t, repo, "checkout", "-q", "main")
	b := mhCommit(t, repo, "B", map[string]string{"a.py": "A=2\n"})
	ucGit(t, repo, "update-ref", "refs/remotes/origin/main", b)
	ucGit(t, repo, "checkout", "-q", "feature")
	ucGit(t, repo, "branch", "-q", "-D", "main")
	token := mhPause(t, repo)
	heads := filepath.Join(repo, ".git", "refs", "heads")
	if err := os.Chmod(heads, 0o555); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = os.Chmod(heads, 0o755) }()
		release := cmRepairBlocked(t, repo)
		defer release()
		_, err := prepareCheckoutForUpdate(context.Background(), io.Discard, []string{"git"}, "main", "feature",
			prepareOptions{AssumeYes: true, WindowsGatewayResume: token})
		var exit *pm.SystemExit
		if !errors.As(err, &exit) {
			t.Fatalf("the torn switch did not exit the update: %v", err)
		}
	}()
	if head := ucGit(t, repo, "rev-parse", "HEAD"); head != a || mhRead(t, filepath.Join(repo, "a.py")) != "A=2\n" {
		t.Fatalf("fixture: not torn (HEAD %s)", head)
	}
	c := ucGit(t, repo, "commit-tree", "-p", a, "-m", "C", ucGit(t, repo, "rev-parse", a+"^{tree}"))
	ucGit(t, repo, "update-ref", "refs/remotes/origin/main", c)
	if mhGate(t, repo) {
		t.Fatal("a torn switch was admitted once a fetch moved the refs")
	}
}

func TestATornEarlyUpstreamSyncStaysHeldAfterARefOnlyFetch(t *testing.T) {
	repo := cmRepo(t)
	dir := t.TempDir()
	a := mhCommit(t, repo, "A", map[string]string{"a.py": "A=1\n", "locked__z.py": "Z=1\n"})
	b := mhCommit(t, repo, "B", map[string]string{"a.py": "A=2\n", "locked__z.py": "Z=2\n"})
	remote := cmBareUpstream(t, repo, dir, b)
	ucGit(t, repo, "reset", "-q", "--hard", a)
	ucGit(t, repo, "update-ref", "refs/remotes/origin/main", a)
	token := mhPause(t, repo)
	if err := os.Chmod(filepath.Join(repo, "locked"), 0o555); err != nil {
		t.Fatal(err)
	}
	func() {
		release := cmRepairBlocked(t, repo)
		defer release()
		if _, err := prepareCheckoutForUpdate(context.Background(), io.Discard, []string{"git"}, "main", "main",
			prepareOptions{IsFork: true, AssumeYes: true, WindowsGatewayResume: token}); err != nil {
			t.Fatal(err)
		}
	}()
	if head := ucGit(t, repo, "rev-parse", "HEAD"); head != a || mhRead(t, filepath.Join(repo, "a.py")) != "A=2\n" {
		t.Fatalf("fixture: not torn (HEAD %s)", head)
	}
	ucGit(t, remote, "update-ref", "refs/heads/main", a)
	ucGit(t, repo, "fetch", "-q", "upstream", "+refs/heads/main:refs/remotes/upstream/main")
	if mhGate(t, repo) {
		t.Fatal("a torn upstream sync was admitted once a fetch moved the refs")
	}
}

func TestASecondMoveIsGatedFromTheHeadTheFirstOneReached(t *testing.T) {
	for _, secondMove := range []string{"torn", "refused", "complete"} {
		t.Run(secondMove, func(t *testing.T) {
			repo := cmRepo(t)
			dir := t.TempDir()
			mhCommit(t, repo, "A", map[string]string{"a.py": "A=1\n", "locked__z.py": "Z=1\n", "notes.txt": "mine\n"})
			o := mhCommit(t, repo, "O", map[string]string{"origin.py": "O=1\n"})
			files := map[string]string{"a.py": "A=2\n", "locked__z.py": "Z=2\n", "new.py": "N=1\n"}
			if secondMove == "refused" {
				files["notes.txt"] = "upstream\n"
			}
			u := mhCommit(t, repo, "U", files)
			cmBareUpstream(t, repo, dir, u)
			ucGit(t, repo, "reset", "-q", "--hard", "HEAD~2")
			ucGit(t, repo, "update-ref", "refs/remotes/origin/main", o)
			notes := filepath.Join(repo, "notes.txt")
			if err := os.WriteFile(notes, []byte("my edit\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			token := mhPause(t, repo)
			switch secondMove {
			case "torn":
				if err := os.Chmod(filepath.Join(repo, "locked"), 0o555); err != nil {
					t.Fatal(err)
				}
			case "refused":
				if err := os.WriteFile(filepath.Join(repo, "new.py"), []byte("in the way\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			func() {
				if secondMove == "torn" {
					defer cmRepairBlocked(t, repo)()
				}
				if _, err := pullUpdates(context.Background(), io.Discard, []string{"git"}, "main", "", pullOptions{
					SyncUpstream: true, AssumeYes: true, WindowsGatewayResume: token}); err != nil {
					t.Fatal(err)
				}
			}()
			head := ucGit(t, repo, "rev-parse", "HEAD")
			if secondMove == "torn" {
				if head != o || mhRead(t, filepath.Join(repo, "a.py")) != "A=2\n" {
					t.Fatalf("fixture: not torn (HEAD %s)", head)
				}
				if mhGate(t, repo) {
					t.Fatal("the failed second move's torn tree was admitted at the first move's HEAD")
				}
				return
			}
			want := u
			if secondMove == "refused" {
				want = o
			}
			if head != want {
				t.Fatalf("HEAD %s, want %s", head, want)
			}
			if err := os.WriteFile(notes, []byte("my later edit\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if !mhGate(t, repo) {
				t.Fatal("a move that left the tree whole held the paused set")
			}
		})
	}
}

func mhExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
