//go:build !windows

package selfupdate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Kills the owner child and reports whether its stalled descendant still runs under the checkout lock
func luKillOwnerKeepsCustody(t *testing.T, owner *exec.Cmd, pidFile string, root, tmp string) *updaterecord.UpdateLock {
	t.Helper()
	contender := updaterecord.NewUpdateLock(filepath.Join(tmp, "contender-marker"), root, true)
	stalled := 0
	t.Cleanup(func() {
		cuTouch(filepath.Join(tmp, "go"))
		if stalled != 0 && !cuGoneWithin(stalled, 20*time.Second) {
			_ = syscall.Kill(stalled, syscall.SIGKILL)
		}
		contender.Release(context.Background())
		_ = owner.Process.Kill()
		_ = owner.Wait()
	})
	stalled = cuWaitPID(t, pidFile)
	_ = owner.Process.Kill()
	_ = owner.Wait()
	if !cuAlive(stalled) {
		t.Fatal("the stalled descendant died with its owner")
	}
	if ok, err := contender.Acquire(context.Background()); ok || err != nil {
		t.Fatalf("a contender owns the checkout while the descendant still writes %v", err)
	}
	return contender
}

func TestLockGitKeepsCheckoutCustodyAfterItsUpdaterIsKilled(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	cuGitEnv(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "checkout")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cuGit(t, root, "init", "-q")
	cuGit(t, root, "config", "user.name", "probe")
	cuGit(t, root, "config", "user.email", "probe@invalid.local")
	cuWrite(t, filepath.Join(root, "tracked"), "before\n", 0o644)
	cuGit(t, root, "add", "tracked")
	cuGit(t, root, "commit", "-qm", "before")
	cuGit(t, root, "config", "filter.stall.clean", fmt.Sprintf("'%s' %s stall '%s' stalled pipe", os.Args[0], cuChildEntry, tmp))
	cuWrite(t, filepath.Join(root, ".git", "info", "attributes"), "tracked filter=stall\n", 0o644)
	cuWrite(t, filepath.Join(root, "tracked"), "after\n", 0o644)
	argv := cuChildArgv("git-add-owner", root)
	owner := exec.Command(argv[0], argv[1:]...)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	contender := luKillOwnerKeepsCustody(t, owner, filepath.Join(tmp, "stalled"), root, tmp)
	cuTouch(filepath.Join(tmp, "go"))
	end := time.Now().Add(30 * time.Second)
	for {
		if ok, _ := contender.Acquire(context.Background()); ok {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the checkout stayed locked after git exited")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestLockTheSourceBuildKeepsCheckoutCustodyAfterItsUpdaterIsKilled(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "checkout")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	argv := cuChildArgv("build-owner", append([]string{root, filepath.Join(root, "owner-marker")}, cuChildArgv("stall", tmp, "node")...)...)
	owner := exec.Command(argv[0], argv[1:]...)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	luKillOwnerKeepsCustody(t, owner, filepath.Join(tmp, "node"), root, tmp)
}
