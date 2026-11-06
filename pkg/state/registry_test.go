package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Substitutes the registry open for the test and closes every shared handle afterwards
func withRegistryOpen(t *testing.T, open func(string) (*DB, error)) {
	t.Helper()
	prev := openSessionDB
	if open != nil {
		openSessionDB = open
	}
	t.Cleanup(func() {
		CloseAll()
		openSessionDB = prev
	})
}

func mustAcquire(t *testing.T, path string) *DB {
	t.Helper()
	d, err := Acquire(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Copies the live file aside, unlinks it and renames the copy into place so the path names a new inode
func replaceDBFile(t *testing.T, path string) {
	t.Helper()
	tmp := strings.TrimSuffix(path, filepath.Ext(path)) + ".replacement.tmp"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, data, st.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func TestLiveHoldersKeepWorkingHandleAcrossReplacement(t *testing.T) {
	withRegistryOpen(t, nil)
	path := filepath.Join(t.TempDir(), "state.db")
	old := mustAcquire(t, path)
	replaceDBFile(t, path)
	next := mustAcquire(t, path)
	if next == old {
		t.Fatal("a replaced file was lent the retired generation")
	}
	if stats := RegistryStats(); stats["live_generations"] != 1 || stats["retired_generations"] != 1 {
		t.Fatalf("stats after replacement %v", stats)
	}
	if old.closedForTest() {
		t.Fatal("retired generation closed while holders live")
	}
	var replaced *StateDBReplacedError
	if err := old.SetMeta(context.Background(), "k", "v"); !errors.As(err, &replaced) {
		t.Fatalf("retired holder write was not refused as replaced: %v", err)
	}
	if err := next.SetMeta(context.Background(), "k", "v"); err != nil {
		t.Fatalf("new generation write failed: %v", err)
	}
	if again := mustAcquire(t, path); again != next {
		t.Fatal("the retired generation was lent again")
	}
	if !Release(old) || !old.closedForTest() {
		t.Fatal("the last holder of a retired generation did not tear it down")
	}
	if next.closedForTest() || RegistryStats()["retired_generations"] != 0 {
		t.Fatalf("retired drain disturbed the live generation %v", RegistryStats())
	}
}

func TestOpenFailureAfterReplacementLeavesNoStaleEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	var fail atomic.Bool
	withRegistryOpen(t, func(p string) (*DB, error) {
		if fail.Load() {
			return nil, errors.New("disk I/O error")
		}
		return Open(p, Options{})
	})
	old := mustAcquire(t, path)
	replaceDBFile(t, path)
	fail.Store(true)
	if _, err := Acquire(t.Context(), path); err == nil {
		t.Fatal("a failed replacement open succeeded")
	}
	if stats := RegistryStats(); stats["live_generations"] != 0 || stats["retired_generations"] != 1 {
		t.Fatalf("stale entry after a failed open %v", stats)
	}
	Release(old)
	fail.Store(false)
	if d := mustAcquire(t, path); d == old {
		t.Fatal("the retired generation came back after the failed open")
	}
}

func TestConcurrentColdAcquireOpensOneWriter(t *testing.T) {
	var opens atomic.Int32
	withRegistryOpen(t, func(p string) (*DB, error) {
		opens.Add(1)
		time.Sleep(50 * time.Millisecond)
		return Open(p, Options{})
	})
	path := filepath.Join(t.TempDir(), "state.db")
	var wg sync.WaitGroup
	got := make([]*DB, 8)
	for i := range got {
		wg.Go(func() { got[i] = mustAcquire(t, path) })
	}
	wg.Wait()
	for _, d := range got {
		if d != got[0] {
			t.Fatal("concurrent cold acquires got different generations")
		}
	}
	if opens.Load() != 1 || RegistryStats()["total_refcounts"] != 8 {
		t.Fatalf("opens %d stats %v", opens.Load(), RegistryStats())
	}
}

func TestWaiterRetriesAfterColdOpenFailure(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	withRegistryOpen(t, func(p string) (*DB, error) {
		if calls.Add(1) == 1 {
			<-release
			return nil, errors.New("first open failed")
		}
		return Open(p, Options{})
	})
	path := filepath.Join(t.TempDir(), "state.db")
	firstErr := make(chan error, 1)
	go func() {
		_, err := Acquire(t.Context(), path)
		firstErr <- err
	}()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	waiter := make(chan *DB, 1)
	go func() {
		d, _ := Acquire(t.Context(), path)
		waiter <- d
	}()
	close(release)
	if err := <-firstErr; err == nil {
		t.Fatal("the failing opener reported success")
	}
	if d := <-waiter; d == nil || calls.Load() != 2 {
		t.Fatalf("waiter did not retry the open, calls %d", calls.Load())
	}
}

func TestEquivalentPathSpellingsShareGeneration(t *testing.T) {
	withRegistryOpen(t, nil)
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	a := mustAcquire(t, filepath.Join(dir, "state.db"))
	b := mustAcquire(t, filepath.Join(link, "sub", "..", "state.db"))
	if a != b || RegistryStats()["total_refcounts"] != 2 {
		t.Fatalf("spellings split the generation %v", RegistryStats())
	}
}

func TestFinalReleaseDoesNotHoldRegistryLockDuringClose(t *testing.T) {
	withRegistryOpen(t, nil)
	slow := mustAcquire(t, filepath.Join(t.TempDir(), "state.db"))
	other := filepath.Join(t.TempDir(), "state.db")
	lifecycle := func() *sync.Mutex {
		regMu.Lock()
		defer regMu.Unlock()
		return pathLifecycleLockLocked(slow.Path())
	}()
	lifecycle.Lock()
	done := make(chan struct{})
	go func() {
		Release(slow)
		close(done)
	}()
	for {
		regMu.Lock()
		admitted := tearingDown[slow.Path()] != nil
		regMu.Unlock()
		if admitted {
			break
		}
		time.Sleep(time.Millisecond)
	}
	acquired := make(chan struct{})
	go func() {
		mustAcquire(t, other)
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("an unrelated acquire stalled behind a final close")
	}
	lifecycle.Unlock()
	<-done
}

func TestConcurrentAcquireAndReleaseNoDeadlock(t *testing.T) {
	withRegistryOpen(t, nil)
	path := filepath.Join(t.TempDir(), "state.db")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				d, err := Acquire(t.Context(), path)
				if err != nil {
					t.Error(err)
					return
				}
				Release(d)
			}
		})
	}
	wg.Wait()
	if stats := RegistryStats(); stats["live_generations"] != 0 || stats["total_refcounts"] != 0 {
		t.Fatalf("leftover generations %v", stats)
	}
}

func TestMaintenanceBorrowPinsConnectionUntilScopeExits(t *testing.T) {
	withRegistryOpen(t, nil)
	d := mustAcquire(t, filepath.Join(t.TempDir(), "state.db"))
	err := BorrowLiveSharedSessionDBs(func(borrowed []*DB) error {
		if len(borrowed) != 1 || borrowed[0] != d {
			t.Fatalf("borrowed %v", borrowed)
		}
		Release(d)
		if d.closedForTest() {
			t.Fatal("the owner's release closed a borrowed handle")
		}
		return errors.New("maintenance failed")
	})
	if err == nil || !d.closedForTest() {
		t.Fatalf("borrow err %v closed %v", err, d.closedForTest())
	}
}

func TestCloseOnSharedInstanceReleasesOneRefcount(t *testing.T) {
	withRegistryOpen(t, nil)
	path := filepath.Join(t.TempDir(), "state.db")
	a := mustAcquire(t, path)
	mustAcquire(t, path)
	if err := a.Close(); err != nil || a.closedForTest() {
		t.Fatalf("close of a shared handle tore it down: %v", err)
	}
	if RegistryStats()["total_refcounts"] != 1 {
		t.Fatalf("stats %v", RegistryStats())
	}
	a.Close()
	if !a.closedForTest() || RegistryStats()["live_generations"] != 0 {
		t.Fatal("the final close did not tear the generation down")
	}
}

func TestReleaseOrCloseClosesAnUnmanagedHandle(t *testing.T) {
	withRegistryOpen(t, nil)
	bare, err := Open(filepath.Join(t.TempDir(), "state.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if Release(bare) || Release(nil) {
		t.Fatal("an unmanaged handle was released")
	}
	ReleaseOrClose(bare)
	if !bare.closedForTest() {
		t.Fatal("ReleaseOrClose left an unmanaged handle open")
	}
}

func TestRetiredDrainDoesNotLiftAPendingCurrentTeardown(t *testing.T) {
	withRegistryOpen(t, nil)
	path := filepath.Join(t.TempDir(), "state.db")
	old := mustAcquire(t, path)
	replaceDBFile(t, path)
	current := mustAcquire(t, path)
	lifecycle := func() *sync.Mutex {
		regMu.Lock()
		defer regMu.Unlock()
		return pathLifecycleLockLocked(current.Path())
	}()
	lifecycle.Lock()
	go Release(current)
	go Release(old)
	for {
		regMu.Lock()
		barrier := tearingDown[current.Path()]
		pending := 0
		if barrier != nil {
			pending = barrier.pending
		}
		regMu.Unlock()
		if pending == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	published := make(chan *DB, 1)
	go func() { published <- mustAcquire(t, path) }()
	lifecycle.Unlock()
	d := <-published
	if !old.closedForTest() || !current.closedForTest() || d == current || d == old {
		t.Fatal("a replacement was published before both admitted closes settled")
	}
}

func TestCloseAllUnderClosesConnectionsInsideDirectoryOnly(t *testing.T) {
	withRegistryOpen(t, nil)
	inside := filepath.Join(t.TempDir(), "profiles", "doomed")
	outside := mustAcquire(t, filepath.Join(t.TempDir(), "state.db"))
	doomed := mustAcquire(t, filepath.Join(inside, "state.db"))
	if n := CloseAllUnder(inside); n != 1 {
		t.Fatalf("closed %d", n)
	}
	if !doomed.closedForTest() || outside.closedForTest() {
		t.Fatal("close_all_under crossed the directory boundary")
	}
	if n := CloseAllUnder(t.TempDir()); n != 0 {
		t.Fatalf("closed %d in a directory holding nothing", n)
	}
	if got := OtherGenerationsForPath(outside.Path(), nil); len(got) != 1 || got[0] != "in-process live SessionDB generation (refcount 1)" {
		t.Fatalf("other generations %q", got)
	}
	if got := OtherGenerationsForPath(outside.Path(), outside); len(got) != 0 {
		t.Fatalf("excluded handle still listed %q", got)
	}
	if live := LiveSharedSessionDBs(); len(live) != 1 || live[0] != outside {
		t.Fatalf("live shared %v", live)
	}
	if n := CloseAll(); n != 1 || !outside.closedForTest() {
		t.Fatalf("close_all closed %d", n)
	}
}

func TestCloseAllUnderWaitsForAdmittedTeardownAfterGenerationIsGone(t *testing.T) {
	withRegistryOpen(t, nil)
	dir := t.TempDir()
	d := mustAcquire(t, filepath.Join(dir, "state.db"))
	lifecycle := func() *sync.Mutex {
		regMu.Lock()
		defer regMu.Unlock()
		return pathLifecycleLockLocked(d.Path())
	}()
	lifecycle.Lock()
	go Release(d)
	for RegistryStats()["live_generations"] != 0 {
		time.Sleep(time.Millisecond)
	}
	swept := make(chan int, 1)
	go func() { swept <- CloseAllUnder(dir) }()
	select {
	case <-swept:
		t.Fatal("close_all_under returned while an admitted close was still running")
	case <-time.After(100 * time.Millisecond):
	}
	lifecycle.Unlock()
	if n := <-swept; n != 0 || !d.closedForTest() {
		t.Fatalf("swept %d closed %v", n, d.closedForTest())
	}
}

func TestFailedTeardownStillSettlesTheBarrier(t *testing.T) {
	withRegistryOpen(t, nil)
	path := filepath.Join(t.TempDir(), "state.db")
	d := mustAcquire(t, path)
	real := d.conn
	defer real.Close()
	d.conn = &TrackedConn{}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the broken close did not fail")
			}
		}()
		Release(d)
	}()
	regMu.Lock()
	_, pending := tearingDown[d.Path()]
	regMu.Unlock()
	if pending {
		t.Fatal("a failed close left the path barrier up")
	}
	if again := mustAcquire(t, path); again == d {
		t.Fatal("the torn down generation was lent again")
	}
}
