//go:build !icebath_core && !icebath_noextras

package pm

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

// Lease managed generation directory under a root
func sttLeasedGeneration(t *testing.T, root, name string) string {
	t.Helper()
	generation := sttMkdir(t, filepath.Join(root, "generations", name))
	sttTouch(t, filepath.Join(generation, ".lease-managed"))
	return generation
}

// Takes a lease failing the test on error
func sttLease(t *testing.T, generation string) func() {
	t.Helper()
	release, err := LeaseDirectory(generation)
	if err != nil {
		t.Fatal(err)
	}
	return release
}

func TestNextReaderRemovesLeaseLeftByHardExit(t *testing.T) {
	isolateHome(t)
	generation := sttLeasedGeneration(t, filepath.Join(t.TempDir(), "runtime"), "selected")
	if out, err := sttHelperCommand("lease", generation).CombinedOutput(); err != nil {
		t.Fatalf("hard exit child failed: %v\n%s", err, out)
	}
	leases := filepath.Join(generation, ".leases")
	stale := sttChildren(t, leases)
	if len(stale) != 1 {
		t.Fatalf("stale leases %q", stale)
	}
	release := sttLease(t, generation)
	active := sttChildren(t, leases)
	if len(active) != 1 || slices.Contains(stale, active[0]) {
		release()
		t.Fatalf("active leases %q after stale %q", active, stale)
	}
	release()
	sttSamePaths(t, sttChildren(t, leases), nil)
}

func TestPruneKeepsUnopenableSiblingLease(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	if os.Geteuid() == 0 {
		t.Skip("root opens mode-0 files")
	}
	isolateHome(t)
	generation := sttLeasedGeneration(t, filepath.Join(t.TempDir(), "runtime"), "selected")
	leases := sttMkdir(t, filepath.Join(generation, ".leases"))
	foreign := sttTouch(t, filepath.Join(leases, "foreign"))
	if err := os.Chmod(foreign, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(foreign, 0o600)
	sttLease(t, generation)()
	if !sttExists(foreign) {
		t.Fatal("an unopenable lease was pruned")
	}
	held, err := LeasesHeld(generation)
	if err != nil || !held {
		t.Fatalf("held %v err %v", held, err)
	}
}

func TestLeaseTakenWhileAPeerPrunedItIsRetaken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	isolateHome(t)
	generation := sttLeasedGeneration(t, filepath.Join(t.TempDir(), "runtime"), "selected")
	leases := filepath.Join(generation, ".leases")
	realLock := leaseLock
	t.Cleanup(func() { leaseLock = realLock })
	var raced []*os.File
	leaseLock = func(f *os.File, wait bool, timeout time.Duration) (bool, error) {
		if wait && len(raced) == 0 {
			for _, stray := range sttChildren(t, leases) {
				if err := os.Remove(stray); err != nil {
					t.Error(err)
				}
			}
			raced = append(raced, f)
		}
		return realLock(f, wait, timeout)
	}
	release := sttLease(t, generation)
	if len(raced) == 0 {
		release()
		t.Fatal("the peer prune never ran")
	}
	if got := sttChildren(t, leases); len(got) != 1 {
		release()
		t.Fatalf("leases %q", got)
	}
	held, err := LeasesHeld(generation)
	release()
	if err != nil || !held {
		t.Fatalf("held %v err %v", held, err)
	}
	sttSamePaths(t, sttChildren(t, leases), nil)
}
