package pm

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

// PM runtime generation, published with a marker and lease managed unless told otherwise
func sttRuntimeGeneration(t *testing.T, root, name string, published, leaseManaged bool) string {
	t.Helper()
	generation := sttMkdir(t, filepath.Join(root, "generations", name))
	if leaseManaged {
		sttTouch(t, filepath.Join(generation, ".lease-managed"))
	}
	if published {
		writeFile(t, filepath.Join(generation, "pm-runtime.json"), []byte(`{"inputs": "`+name+`"}`), 0o666)
	}
	return generation
}

// Collects runtime generations failing the test on error
func sttCollectRuntime(t *testing.T, root string) []string {
	t.Helper()
	removed, err := CollectRuntimeGenerations(root)
	if err != nil {
		t.Fatal(err)
	}
	return removed
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

func TestCollectorKeepsSelectedLeasedAndPreLeaseGenerations(t *testing.T) {
	isolateHome(t)
	root := filepath.Join(t.TempDir(), "pm-runtime")
	selected := sttRuntimeGeneration(t, root, "selected", true, true)
	busy := sttRuntimeGeneration(t, root, "busy", true, true)
	idle := sttRuntimeGeneration(t, root, "idle", true, true)
	legacy := sttRuntimeGeneration(t, root, "legacy", true, false)
	aborted := sttRuntimeGeneration(t, root, "aborted", false, true)
	writeFile(t, filepath.Join(root, "selected.json"), []byte(`{"generation": "generations/selected"}`), 0o666)
	release := sttLease(t, busy)
	defer release()
	sttLease(t, idle)()

	removed := sttCollectRuntime(t, root)
	slices.Sort(removed)
	sttSamePaths(t, removed, []string{aborted, idle})
	for _, p := range []string{selected, busy, legacy} {
		if !sttIsDir(p) {
			t.Fatalf("%s was collected", p)
		}
	}
	release()
	sttSamePaths(t, sttCollectRuntime(t, root), []string{busy})
}

func TestCollectorYieldsToAnInFlightStage(t *testing.T) {
	isolateHome(t)
	root := filepath.Join(t.TempDir(), "pm-runtime")
	aborted := sttRuntimeGeneration(t, root, "aborted", false, true)
	lock := sttHold(t, filepath.Join(root, ".prepare.lock"))
	removed := sttCollectRuntime(t, root)
	lock.Close()
	sttSamePaths(t, removed, nil)
	if !sttIsDir(aborted) {
		t.Fatal("collector removed a generation while a stage held the prepare lock")
	}
	sttSamePaths(t, sttCollectRuntime(t, root), []string{aborted})
}

func TestNextReaderRemovesLeaseLeftByHardExit(t *testing.T) {
	isolateHome(t)
	generation := sttRuntimeGeneration(t, filepath.Join(t.TempDir(), "pm-runtime"), "selected", true, true)
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
	generation := sttRuntimeGeneration(t, filepath.Join(t.TempDir(), "pm-runtime"), "selected", true, true)
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
	generation := sttRuntimeGeneration(t, filepath.Join(t.TempDir(), "pm-runtime"), "selected", true, true)
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
