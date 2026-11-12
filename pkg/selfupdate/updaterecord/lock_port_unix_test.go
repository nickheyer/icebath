//go:build !windows

package updaterecord

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Waits until the child stops itself at its pause point
func lkWaitStopped(t *testing.T, p *pcProc) {
	t.Helper()
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(p.cmd.Process.Pid, &status, syscall.WUNTRACED, nil); err != nil || !status.Stopped() {
		t.Fatalf("child never stopped %v %v", err, status)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Signal(syscall.SIGCONT) })
}

func lkWaitPID(t *testing.T, path string, p *pcProc) int {
	t.Helper()
	end := time.Now().Add(30 * time.Second)
	for {
		text := strings.TrimSpace(lkWaitFor(t, path, p))
		if pid, err := strconv.Atoi(text); err == nil {
			return pid
		}
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for a pid in %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLockWithdrawingATornClaimNeverDeletesAReplacement(t *testing.T) {
	path := lcMarker(t)
	other := lcOtherPID(t)
	func() {
		lkSwap(t, &linkMarker, lkNoHardLinks)
		lkSwap(t, &writeMarker, lkTornWrite(func() {
			os.Remove(path)
			lcClaimV2(t, path, other, 0, 0)
		}))
		if lcAcquire(t, NewUpdateLock(path, "", true)) {
			t.Fatal("a torn claim acquired")
		}
	}()
	if raw, _ := os.ReadFile(path); !strings.HasPrefix(string(raw), fmt.Sprintf("%d\n", other)) {
		t.Fatalf("the withdrawal deleted a replacement %q", raw)
	}
}

func TestLockAStaleReclaimNeverDeletesAClaimPublishedAfterItsRead(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, MarkerName)
	os.WriteFile(path, []byte("0\n0\n"), 0o644)
	reader := pcStart(t, "lock_stale_reader", path)
	lkWaitStopped(t, reader)
	claimant := pcStart(t, "lock_claimant", path, filepath.Join(tmp, "claimed"))
	time.Sleep(time.Second)
	reader.cmd.Process.Signal(syscall.SIGCONT)
	if _, code := reader.rest(t); code != 0 {
		t.Fatal(code)
	}
	lkWaitFor(t, filepath.Join(tmp, "claimed"), claimant)
	if raw, _ := os.ReadFile(path); !strings.HasPrefix(string(raw), fmt.Sprintf("%d\n", claimant.cmd.Process.Pid)) {
		t.Fatalf("a stale reader deleted a live claim published after its read %q", raw)
	}
	if !lcExists(MarkerMutexPath(path)) {
		t.Fatal("the sidecar is never deleted")
	}
}

func TestLockAReaderReportsTheLiveClaimThatReplacedItsStaleSnapshot(t *testing.T) {
	tmp := t.TempDir()
	home, install := lkMkdir(t, filepath.Join(tmp, "home")), lkMkdir(t, filepath.Join(tmp, "install"))
	path := filepath.Join(home, MarkerName)
	lkDeadMarker(t, path)
	reader := pcStart(t, "lock_reclaim_paused_reader", path, install)
	lkWaitStopped(t, reader)
	claimant := pcStart(t, "lock_claimant", path, filepath.Join(tmp, "claimed"), install)
	lkWaitFor(t, filepath.Join(tmp, "claimed"), claimant)
	claim, _ := os.ReadFile(path)
	reader.cmd.Process.Signal(syscall.SIGCONT)
	out, code := reader.rest(t)
	if code != 0 || strings.TrimSpace(out) != "True" {
		t.Fatalf("the reader reported a live replacement claim as no update %d %q", code, out)
	}
	if raw, _ := os.ReadFile(path); string(raw) != string(claim) {
		t.Fatal("the live replacement claim was not preserved byte for byte")
	}
	if held, _ := CheckoutLockHeld(context.Background(), install); held {
		t.Fatal("the claim is marker only, no checkout lease answers for it")
	}
}

func TestAnOrphanedDelegateRemovesItsKilledOwnersMarker(t *testing.T) {
	tmp := t.TempDir()
	owner := pcStart(t, "lock_handoff_owner", tmp)
	child := lkWaitPID(t, filepath.Join(tmp, "delegate-written"), owner)
	lkWaitFor(t, filepath.Join(tmp, "ready"), owner)
	owner.kill(t)
	lkTouch(filepath.Join(tmp, "go"))
	end := time.Now().Add(30 * time.Second)
	for pidAlive(child) && time.Now().Before(end) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(child) {
		t.Fatal("the delegate never exited")
	}
	if lcExists(filepath.Join(tmp, "marker")) {
		t.Fatal("the orphaned update's exit left its dead claim behind")
	}
}

func TestLockOwnerReleaseHandsTheClaimToALiveDelegate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marker")
	delegate := lcOtherPID(t)
	lock := NewUpdateLock(path, "", true)
	if !lcAcquire(t, lock) || !lock.Acquired {
		t.Fatalf("%+v", lock.Holder)
	}
	ct := fmt.Sprintf("%.3f", *ProcessCreateTime(delegate))
	raw, _ := os.ReadFile(path)
	os.WriteFile(path, append(raw, fmt.Sprintf("delegate:%d ct:%s\nrun:desk-1\n", delegate, ct)...), 0o644)
	started := lcLines(t, path)[1]
	lock.Release(context.Background())
	if raw, _ := os.ReadFile(path); string(raw) != fmt.Sprintf("%d\n%s\nct:%s\nrun:desk-1\n", delegate, started, ct) {
		t.Fatalf("%q", raw)
	}
}

func TestLockARefusalOverADeadUpdatersLockUsesTheHeldWording(t *testing.T) {
	tmp := t.TempDir()
	install := lkMkdir(t, filepath.Join(tmp, "install"))
	updater := pcStart(t, "lock_orphaning_holder", install, filepath.Join(tmp, "child"))
	updater.rest(t)
	deadPID := updater.cmd.Process.Pid
	orphan := lkWaitPID(t, filepath.Join(tmp, "child"), nil)
	defer func() {
		lkTouch(filepath.Join(tmp, "release"))
		end := time.Now().Add(20 * time.Second)
		for pidAlive(orphan) && time.Now().Before(end) {
			time.Sleep(20 * time.Millisecond)
		}
	}()
	lock := NewUpdateLock(filepath.Join(tmp, MarkerName), install, true)
	if lcAcquire(t, lock) || lock.Holder == nil || !lock.Holder.Held || lock.Holder.PID != 0 {
		t.Fatalf("%+v", lock.Holder)
	}
	text := DescribeHolder(lock.Holder)
	if strings.Contains(text, fmt.Sprintf("process %d", deadPID)) || !strings.Contains(text, "still holds the checkout") {
		t.Fatal(text)
	}
}

func TestLockHeldProbeNeverFailsAConcurrentAcquire(t *testing.T) {
	tmp := t.TempDir()
	install := lkMkdir(t, filepath.Join(tmp, "install"))
	lockPath, _ := CheckoutLockPath(install)
	os.WriteFile(lockPath, nil, 0o644)
	prober := pcStart(t, "lock_prober", lockPath, "4", filepath.Join(tmp, "go"))
	lkWaitFor(t, filepath.Join(tmp, "go"), prober)
	done := make(chan struct{})
	go func() { prober.rest(t); close(done) }()
	attempts, refused := 0, 0
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
			attempts++
			if holder, err := acquireCheckout(install); holder != nil || err != nil {
				refused++
			} else {
				releaseCheckout()
			}
		}
	}
	if attempts <= 50 || refused != 0 {
		t.Fatalf("%d/%d acquires failed against a probe with no real holder", refused, attempts)
	}
}

func TestLockCheckoutCustodyNeverBorrowsALocalReadersDescriptor(t *testing.T) {
	for _, owner := range []string{"incidental-open", "status-reader"} {
		t.Run(owner, func(t *testing.T) {
			tmp := t.TempDir()
			root := lkMkdir(t, filepath.Join(tmp, "checkout"))
			lockPath, _ := CheckoutLockPath(root)
			os.WriteFile(lockPath, nil, 0o644)
			opened, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			resumeAll := func() { once.Do(func() { close(resume) }) }
			var wg sync.WaitGroup
			var readerFile *os.File
			if owner == "status-reader" {
				var first sync.Once
				lkSwap(t, &sharedProbe, func(f *os.File) (bool, error) {
					paused := false
					first.Do(func() { paused = true })
					if !paused {
						return trySharedLock(f)
					}
					close(opened)
					select {
					case <-resume:
					case <-time.After(20 * time.Second):
						panic("acquisition never released the status reader")
					}
					return trySharedLock(f)
				})
				wg.Add(1)
				go func() { defer wg.Done(); CheckoutLockHeld(context.Background(), root) }()
				select {
				case <-opened:
				case <-time.After(20 * time.Second):
					t.Fatal("status reader never opened its descriptor")
				}
			} else {
				readerFile, _ = os.Open(lockPath)
			}
			lock := NewUpdateLock(filepath.Join(tmp, "owner-marker"), root, true)
			defer func() { resumeAll(); wg.Wait(); lock.Release(context.Background()) }()
			if !lcAcquire(t, lock) {
				t.Fatalf("%+v", lock.Holder)
			}
			resumeAll()
			wg.Wait()
			if readerFile != nil {
				readerFile.Close()
			}
			out, err := pcCommand("lock_contender", filepath.Join(tmp, "other-home-marker"), root).Output()
			var result map[string]bool
			if err != nil || json.Unmarshal(out, &result) != nil || result["acquired"] {
				t.Fatalf("a contender entered while the first owner was alive %q %v", out, err)
			}
			lock.Release(context.Background())
			if held, _ := CheckoutLockHeld(context.Background(), root); held {
				t.Fatal("release must leave no orphaned custody")
			}
		})
	}
}

func TestExecChildReentersTheLockItExplicitlyInherited(t *testing.T) {
	tmp := t.TempDir()
	root := lkMkdir(t, filepath.Join(tmp, "checkout"))
	owner := NewUpdateLock(filepath.Join(tmp, "parent-marker"), root, true)
	contender := NewUpdateLock(filepath.Join(tmp, "contender-marker"), root, true)
	if !lcAcquire(t, owner) {
		t.Fatalf("%+v", owner.Holder)
	}
	defer owner.Release(context.Background())
	files, err := CheckoutLockFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	child := pcCommand("lock_exec_child", root)
	child.ExtraFiles = files
	stdin, _ := child.StdinPipe()
	stdout, _ := child.StdoutPipe()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if child.ProcessState == nil {
			child.Process.Kill()
			child.Wait()
		}
	}()
	proc := &pcProc{cmd: child, out: bufio.NewReader(stdout)}
	if line := proc.line(t); line != "JOINED" {
		t.Fatal(line)
	}
	owner.Release(context.Background())
	if !pidAlive(child.Process.Pid) {
		t.Fatal("the child exited with its owner's release")
	}
	if lcAcquire(t, contender) {
		t.Fatal("the inherited descriptor must outlive the original owner")
	}
	stdin.Write([]byte("exit\n"))
	if _, code := proc.rest(t); code != 0 {
		t.Fatal(code)
	}
	if !lcAcquire(t, contender) {
		t.Fatal("the last descendant's exit must release the lock")
	}
	contender.Release(context.Background())
}

func TestLockAnUnopenableCheckoutLockReadsHeldAndOnlyAMissingOneReadsFree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a mode 000 file")
	}
	root := lkMkdir(t, filepath.Join(t.TempDir(), "checkout"))
	lockPath, _ := CheckoutLockPath(root)
	if held, _ := CheckoutLockHeld(context.Background(), root); held {
		t.Fatal("no lock file, nothing can hold it")
	}
	os.WriteFile(lockPath, nil, 0o644)
	if held, _ := CheckoutLockHeld(context.Background(), root); held {
		t.Fatal("an openable unlocked file is free")
	}
	os.Chmod(lockPath, 0)
	defer os.Chmod(lockPath, 0o600)
	if held, _ := CheckoutLockHeld(context.Background(), root); !held {
		t.Fatal("a lock file that exists but cannot be opened answers held")
	}
}
