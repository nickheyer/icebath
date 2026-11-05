package pm

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Lease managed generation with an empty leases dir
func sttLeaseGenerationDir(t *testing.T, root string) string {
	t.Helper()
	sttMkdir(t, root)
	sttTouch(t, filepath.Join(root, ".lease-managed"))
	sttMkdir(t, filepath.Join(root, ".leases"))
	return root
}

func TestPrunerRemovesAbandonedFileWhileHoldingItsLock(t *testing.T) {
	isolateHome(t)
	generation := sttLeaseGenerationDir(t, filepath.Join(t.TempDir(), "generation"))
	lease := sttTouch(t, filepath.Join(generation, ".leases", "abandoned"))
	realUnlink := unlinkLease
	t.Cleanup(func() { unlinkLease = realUnlink })
	var lockedAtDelete []bool
	unlinkLease = func(path string) error {
		if path == lease {
			probe, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Error(err)
			} else {
				locked, err := LockFD(probe, false, 0)
				if err != nil {
					t.Error(err)
				}
				lockedAtDelete = append(lockedAtDelete, !locked)
				probe.Close()
			}
		}
		return realUnlink(path)
	}
	held, err := LeasesHeld(generation)
	if err != nil || held {
		t.Fatalf("held %v err %v", held, err)
	}
	if sttExists(lease) {
		t.Fatal("abandoned lease kept")
	}
	if len(lockedAtDelete) != 1 || !lockedAtDelete[0] {
		t.Fatalf("locked at delete %v", lockedAtDelete)
	}
}

func TestPrunerKeepsAWindowsCreatorBeforeItsFirstLock(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("platforms windows")
	}
	isolateHome(t)
	generation := sttLeaseGenerationDir(t, filepath.Join(t.TempDir(), "generation"))
	realLock := leaseLock
	t.Cleanup(func() { leaseLock = realLock })
	type observation struct {
		held    bool
		created []string
	}
	var observed []observation
	leaseLock = func(f *os.File, wait bool, timeout time.Duration) (bool, error) {
		if wait && len(observed) == 0 {
			created := sttChildren(t, filepath.Join(generation, ".leases"))
			held, err := LeasesHeld(generation)
			if err != nil {
				t.Error(err)
			}
			observed = append(observed, observation{held, created})
		}
		return realLock(f, wait, timeout)
	}
	release := sttLease(t, generation)
	func() {
		defer release()
		if len(observed) == 0 {
			t.Fatal("the creator never waited for its lock")
		}
		if !observed[0].held {
			t.Fatal("An unlocked but still-open creator is not an abandoned owner")
		}
		if len(observed[0].created) != 1 || !sttExists(observed[0].created[0]) {
			t.Fatalf("created %q", observed[0].created)
		}
		if held, err := LeasesHeld(generation); err != nil || !held {
			t.Fatalf("held %v err %v", held, err)
		}
	}()
	sttSamePaths(t, sttChildren(t, filepath.Join(generation, ".leases")), nil)
}

func TestUnremovableLeaseKeepsItsGeneration(t *testing.T) {
	t.Run("runtime", func(t *testing.T) {
		isolateHome(t)
		root := filepath.Join(t.TempDir(), "runtime")
		generation := sttLeaseGenerationDir(t, filepath.Join(root, "generations", "obsolete"))
		writeFile(t, filepath.Join(generation, "pm-runtime.json"), []byte("{}"), 0o666)
		lease := sttTouch(t, filepath.Join(generation, ".leases", "abandoned"))
		realUnlink := unlinkLease
		t.Cleanup(func() { unlinkLease = realUnlink })
		unlinkLease = func(path string) error {
			if path == lease {
				return &fs.PathError{Op: "unlink", Path: path, Err: errors.New("Synthetic lease deletion refusal")}
			}
			return realUnlink(path)
		}
		held, err := LeasesHeld(generation)
		if err != nil || !held {
			t.Fatalf("held %v err %v", held, err)
		}
		sttSamePaths(t, sttCollectRuntime(t, root), nil)
		if !sttIsDir(generation) || !sttExists(lease) {
			t.Fatal("a generation whose lease could not be removed was collected")
		}
		unlinkLease = realUnlink
		sttSamePaths(t, sttCollectRuntime(t, root), []string{generation})
		if sttExists(generation) {
			t.Fatal("collected generation still exists")
		}
	})
}
