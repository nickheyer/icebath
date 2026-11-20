package updaterecord

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"golang.org/x/sys/windows"
)

func TestLockReadOnlyLockFileIsRefusedOnWindows(t *testing.T) {
	tmp := t.TempDir()
	root := lkMkdir(t, filepath.Join(tmp, "install"))
	lockPath, _ := CheckoutLockPath(root)
	os.WriteFile(lockPath, []byte("0\n0\n"), 0o644)
	os.Chmod(lockPath, 0o444)
	defer os.Chmod(lockPath, 0o666)
	path := filepath.Join(tmp, "home", MarkerName)
	lock := NewUpdateLock(path, root, true)
	if lcAcquire(t, lock) || lock.Holder == nil || lock.Holder.Reason == "" || lcExists(path) {
		t.Fatalf("%+v", lock.Holder)
	}
}

// Warm creation time and parent probes keep no object alive per probe, the Go form of no new ctypes objects
func TestLockRepeatedWindowsProcessProbesRetainNoObjects(t *testing.T) {
	created, err := proc.Birth(os.Getpid())
	parent, ok := proc.PPID(os.Getpid())
	if err != nil || created == 0 || !ok || parent == 0 {
		t.Fatal(err, created, parent)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const probes = 200
	for range probes {
		if again, _ := proc.Birth(os.Getpid()); again != created {
			t.Fatal(again)
		}
		if again, _ := proc.PPID(os.Getpid()); again != parent {
			t.Fatal(again)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if after.HeapObjects >= before.HeapObjects+probes {
		t.Fatalf("probes retained %d objects", after.HeapObjects-before.HeapObjects)
	}
}

// Polls until pid is gone or the deadline passes, reporting whether it still runs
func lkStillAlive(pid int, within time.Duration) bool {
	end := time.Now().Add(within)
	for pidAlive(pid) && time.Now().Before(end) {
		time.Sleep(200 * time.Millisecond)
	}
	return pidAlive(pid)
}

func TestLockWindowsKilledOwnerTakesItsTreeDownAndFreesTheLock(t *testing.T) {
	tmp := t.TempDir()
	install := lkMkdir(t, filepath.Join(tmp, "checkout"))
	owner := pcStart(t, "lock_win_owner", install, filepath.Join(tmp, "m"))
	child, err := strconv.Atoi(owner.line(t))
	if err != nil || !pidAlive(child) {
		t.Fatal(err, child)
	}
	if in, _ := UpdateInProgress(context.Background(), install); !in {
		t.Fatal("the owner's lock is not visible")
	}
	if lcAcquire(t, NewUpdateLock(filepath.Join(tmp, "other-home-marker"), install, true)) {
		t.Fatal("a second update of one checkout was not refused")
	}
	owner.kill(t)
	if lkStillAlive(child, 15*time.Second) {
		t.Fatal("the update tree child outlived its killed owner")
	}
	if in, _ := UpdateInProgress(context.Background(), install); in {
		t.Fatal("the lock stayed held after the whole tree died")
	}
	fresh := NewUpdateLock(filepath.Join(tmp, "other-home-marker"), install, true)
	if !lcAcquire(t, fresh) {
		t.Fatalf("%+v", fresh.Holder)
	}
	fresh.Release(context.Background())
}

func TestLockWindowsAChildTheJobRefusedKeepsTheCheckoutBusyAfterItsOwnerDies(t *testing.T) {
	tmp := t.TempDir()
	install := lkMkdir(t, filepath.Join(tmp, "checkout"))
	owner := pcStart(t, "lock_win_unbound_owner", install, filepath.Join(tmp, "m"))
	child, err := strconv.Atoi(owner.line(t))
	if err != nil {
		t.Fatal(err)
	}
	killChild := func() {
		if process, err := os.FindProcess(child); err == nil {
			_ = process.Kill()
		}
	}
	defer killChild()
	owner.kill(t)
	if !pidAlive(child) {
		t.Fatal("the unbound child must outlive its owner")
	}
	if in, _ := UpdateInProgress(context.Background(), install); !in {
		t.Fatal("the checkout reads free while the refused child still runs")
	}
	if lcAcquire(t, NewUpdateLock(filepath.Join(tmp, "other-home-marker"), install, true)) {
		t.Fatal("a contender entered beside the refused child")
	}
	killChild()
	lkStillAlive(child, 15*time.Second)
	fresh := NewUpdateLock(filepath.Join(tmp, "other-home-marker"), install, true)
	if !lcAcquire(t, fresh) {
		t.Fatal("the lease outlived the child that held it")
	}
	fresh.Release(context.Background())
}

func TestSwapUpdateTreeJob(t *testing.T) {
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	restore := SwapUpdateTreeJob(event)
	job, err := UpdateTreeJob()
	restore()
	if err != nil || job != event {
		t.Fatalf("swapped job %v %v", job, err)
	}
}
