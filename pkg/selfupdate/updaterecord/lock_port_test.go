package updaterecord

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func lkNoHardLinks(oldname, newname string) error {
	return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
}

// A claim write that lands half its bytes, runs after, then fails like a full disk
func lkTornWrite(after func()) func(*os.File, []byte) (int, error) {
	return func(f *os.File, data []byte) (int, error) {
		n, _ := f.Write(data[:len(data)/2])
		if after != nil {
			after()
		}
		return n, &os.PathError{Op: "write", Path: f.Name(), Err: syscall.ENOSPC}
	}
}

func lkSwap[T any](t *testing.T, target *T, value T) {
	t.Helper()
	saved := *target
	*target = value
	t.Cleanup(func() { *target = saved })
}

func lkDeadPID(t *testing.T) int {
	t.Helper()
	cmd := pcCommand("lock_wait_file", t.TempDir())
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func lkDeadMarker(t *testing.T, path string) []byte {
	t.Helper()
	body := []byte(fmt.Sprintf("%d\n%d\nct:1.5\n", lkDeadPID(t), time.Now().Unix()))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return body
}

func lkWaitFor(t *testing.T, path string, p *pcProc) string {
	t.Helper()
	end := time.Now().Add(30 * time.Second)
	for !lcExists(path) {
		if p != nil && p.cmd.ProcessState != nil {
			t.Fatalf("child exited before %s", path)
		}
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, _ := os.ReadFile(path)
	return string(raw)
}

func lkMkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLockMarkerPathIsTheProfileTreeRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "profiles", "work"))
	if got := UpdateMarkerPath(); got != filepath.Join(root, MarkerName) {
		t.Fatal(got)
	}
	t.Setenv("ICEBATH_HOME", root)
	if got := UpdateMarkerPath(); got != filepath.Join(root, MarkerName) {
		t.Fatal(got)
	}
}

func TestLockMarkerParseFollowsTheSharedContract(t *testing.T) {
	path := lcMarker(t)
	other := lcOtherPID(t)
	now := time.Now().Unix()
	ct := *ProcessCreateTime(other)
	live := map[string]string{
		"bom":          fmt.Sprintf("\ufeff%d\n%d\nct:%.3f\n", other, now, ct),
		"crlf":         fmt.Sprintf("%d\r\n%d\r\nct:%.3f\r\n", other, now, ct),
		"bad-ct-fresh": fmt.Sprintf("%d\n%d\nct:abc\n", other, now),
	}
	dead := map[string]string{
		"bad-ct-aged":        fmt.Sprintf("%d\n%d\nct:abc\n", other, now-UpdateMarkerMaxAgeSeconds-300),
		"spaced-delegate":    fmt.Sprintf("%d\n%d\nct:1.000\ndelegate: %d ct:%.3f\n", lcDeadPID, now, other, ct),
		"delegate-on-line-3": fmt.Sprintf("%d\n%d\ndelegate:%d ct:%.3f\n", lcDeadPID, now, other, ct),
	}
	for _, set := range []map[string]string{live, dead} {
		for name, body := range set {
			os.WriteFile(path, []byte(body), 0o644)
			holder := ReadLiveUpdate(context.Background(), path, "")
			if _, want := live[name]; (holder != nil) != want {
				t.Fatalf("%s %+v", name, holder)
			}
		}
	}
}

func TestLockFailedExclusiveCreateLeavesNoTornClaim(t *testing.T) {
	path := lcMarker(t)
	func() {
		lkSwap(t, &linkMarker, lkNoHardLinks)
		lkSwap(t, &writeMarker, lkTornWrite(nil))
		if lcAcquire(t, NewUpdateLock(path, "", true)) {
			t.Fatal("a torn claim acquired")
		}
	}()
	linkMarker, writeMarker = os.Link, (*os.File).Write
	if lcExists(path) || ReadLiveUpdate(context.Background(), path, "") != nil {
		t.Fatal("a torn claim was left behind")
	}
}

func TestLockUnreadableCreationTimeGetsTheV1Ceiling(t *testing.T) {
	path := lcMarker(t)
	other := lcOtherPID(t)
	unreadable := func(int) *float64 { return nil }
	lcClaimV2(t, path, other, 0, 0)
	createTimeOf = unreadable
	t.Cleanup(func() { createTimeOf = ProcessCreateTime })
	if ReadLiveUpdate(context.Background(), path, "") == nil {
		t.Fatal("a fresh claim with an unreadable creation time is live")
	}
	createTimeOf = ProcessCreateTime
	lcClaimV2(t, path, other, time.Now().Unix()-UpdateMarkerMaxAgeSeconds-300, 0)
	if ReadLiveUpdate(context.Background(), path, "") == nil {
		t.Fatal("a matching creation time is live at any age")
	}
	createTimeOf = unreadable
	if ReadLiveUpdate(context.Background(), path, "") != nil || lcExists(path) {
		t.Fatal("an aged claim with an unreadable creation time survived the ceiling")
	}
}

func TestLockAnUnjudgeableMarkerNeverAdmitsAContenderPastAHeldCheckoutLock(t *testing.T) {
	tmp := t.TempDir()
	install := lkMkdir(t, filepath.Join(tmp, "checkout"))
	path := filepath.Join(tmp, "marker")
	holder := pcStart(t, "lock_hold", filepath.Join(tmp, "homeA", "m"), install)
	if line := holder.line(t); line != "held" {
		t.Fatal(line)
	}
	lcClaimV2(t, path, holder.cmd.Process.Pid, 0, 0)
	lkSwap(t, &markerReader, func(string) *marker { panic("an error the parse was never expected to raise") })
	if ReadLiveUpdate(context.Background(), path, install) != nil {
		t.Fatal("an unjudgeable marker read as live")
	}
	if in, _ := UpdateInProgress(context.Background(), install); !in {
		t.Fatal("an unjudgeable marker hid a held checkout lock")
	}
	if lcAcquire(t, NewUpdateLock(path, install, true)) {
		t.Fatal("a contender got past a held checkout lock")
	}
}

func TestLockGrandchildAdoptsOrchestratorMarkerWithoutPsutil(t *testing.T) {
	path := lcMarker(t)
	lcClaim(t, path, os.Getpid(), 0)
	child := pcStart(t, "lock_middle", path)
	out, code := child.rest(t)
	if code != 0 || !strings.Contains(out, "ADOPTED") {
		t.Fatalf("%d %q", code, out)
	}
	if !lcExists(path) {
		t.Fatal("the orchestrator still needs its marker after the leaf ends")
	}
}

func TestLockUnrelatedLiveHolderIsStillRefusedUnderTheWalk(t *testing.T) {
	path := lcMarker(t)
	sleeper := lcOtherPID(t)
	lcClaim(t, path, sleeper, 0)
	if ReadLiveUpdate(context.Background(), path, "") == nil {
		t.Fatal("the sibling's claim is live")
	}
	out, code := pcStart(t, "lock_leaf", path).rest(t)
	if code != 2 || !strings.Contains(out, "REFUSED") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestLockAcquireAcceptsTheOrchestratorBehindAnUnreadableInit(t *testing.T) {
	path := lcMarker(t)
	lkSwap(t, &aliveOf, func(int) bool { return true })
	chain := map[int]int{os.Getpid(): 2000, 2000: 1}
	lkSwap(t, &parentOf, func(pid int) (int, bool, error) {
		if pid == 1 {
			return 0, false, errors.New("access denied")
		}
		parent, ok := chain[pid]
		return parent, ok, nil
	})
	lcClaim(t, path, 2000, 0)
	lock := NewUpdateLock(path, "", true)
	if !lcAcquire(t, lock) || lock.Acquired {
		t.Fatalf("the hand-off child runs under 2000's claim %+v", lock.Holder)
	}
	lock.Release(context.Background())
	if !lcExists(path) {
		t.Fatal("the orchestrator still needs its marker")
	}
}

func TestLockWholeSecondCreationTimeInOurOwnSecondIsUs(t *testing.T) {
	own := *ProcessCreateTime(os.Getpid())
	if live := IncarnationLive(os.Getpid(), math.Floor(own)); live == nil || !*live {
		t.Fatal("our own whole second is not us")
	}
	parent := os.Getppid()
	body := fmt.Sprintf("%d\n%d\nct:%d.000\ndelegate:%d ct:%d.000\n", parent, time.Now().Unix(),
		int64(math.Floor(*ProcessCreateTime(parent))), os.Getpid(), int64(math.Floor(own)))
	if verdict, _, _ := JudgeMarker([]byte(body), nil); verdict != "ours" {
		t.Fatal(verdict)
	}
	if live := IncarnationLive(os.Getpid(), math.Floor(own)-1); live == nil || *live {
		t.Fatal("an earlier whole second is a reused pid")
	}
}

func TestLockMarkerMutationsWaitForTheMutexAnotherProcessHolds(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, MarkerName)
	os.WriteFile(path, []byte("0\n0\n"), 0o644)
	holder := pcStart(t, "lock_mutex_holder", path, filepath.Join(tmp, "held"))
	lkWaitFor(t, filepath.Join(tmp, "held"), holder)
	started := time.Now()
	lock := NewUpdateLock(path, "", true)
	if !lcAcquire(t, lock) || !lock.Acquired {
		t.Fatalf("%+v", lock.Holder)
	}
	if time.Since(started) < 900*time.Millisecond {
		t.Fatal("the reclaim ran inside another process's hold")
	}
	lock.Release(context.Background())
	if !lcExists(MarkerMutexPath(path)) {
		t.Fatal("the sidecar is never deleted")
	}
	holder.rest(t)
}

func TestLockALiveLegacyProfileClaimIsHonored(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	profile := lkMkdir(t, filepath.Join(root, "profiles", "work"))
	t.Setenv("ICEBATH_HOME", profile)
	legacy := lcOtherPID(t)
	lcClaim(t, filepath.Join(profile, MarkerName), legacy, 0)
	lock := NewUpdateLock(filepath.Join(root, MarkerName), "", true)
	if lcAcquire(t, lock) || lock.Holder == nil || lock.Holder.PID != legacy || lcExists(filepath.Join(root, MarkerName)) {
		t.Fatalf("%+v", lock.Holder)
	}
	os.WriteFile(filepath.Join(profile, MarkerName), []byte(fmt.Sprintf("%d\n%d\n", lcDeadPID, time.Now().Unix())), 0o644)
	lock = NewUpdateLock(filepath.Join(root, MarkerName), "", true)
	if !lcAcquire(t, lock) {
		t.Fatalf("%+v", lock.Holder)
	}
	lock.Release(context.Background())
}

func TestLockDeadMarkerIsKeptAndReportedHeldWhileTheCheckoutLockIsHeld(t *testing.T) {
	tmp := t.TempDir()
	install := lkMkdir(t, filepath.Join(tmp, "install"))
	path := filepath.Join(tmp, MarkerName)
	lkDeadMarker(t, path)
	holder := pcStart(t, "lock_checkout_holder", install, filepath.Join(tmp, "ready"))
	lkWaitFor(t, filepath.Join(tmp, "ready"), holder)
	verdict := ReadLiveUpdate(context.Background(), path, install)
	if !lcExists(path) {
		t.Fatal("a reader deleted the dead marker while the update tree still held the checkout")
	}
	if verdict == nil || !verdict.Held {
		t.Fatalf("expected held, got %+v", verdict)
	}
	holder.kill(t)
	if ReadLiveUpdate(context.Background(), path, install) != nil || lcExists(path) {
		t.Fatal("with the tree gone the dead marker is reclaimed")
	}
}

func TestLockAClaimerNeverReclaimsADeadMarkerWhileTheCheckoutLockIsHeld(t *testing.T) {
	tmp := t.TempDir()
	install := lkMkdir(t, filepath.Join(tmp, "install"))
	path := filepath.Join(tmp, MarkerName)
	dead := lkDeadMarker(t, path)
	holder := pcStart(t, "lock_checkout_holder", install, filepath.Join(tmp, "ready"))
	lkWaitFor(t, filepath.Join(tmp, "ready"), holder)
	claim := NewUpdateLock(path, install, true)
	if ok, err := claim.claimMarker(context.Background()); ok || err != nil {
		t.Fatal("claimed over a dead marker while the checkout lock is held")
	}
	if raw, _ := os.ReadFile(path); string(raw) != string(dead) || claim.Holder == nil || !claim.Holder.Held {
		t.Fatalf("the claim step replaced the held update's marker %+v", claim.Holder)
	}
	launch := NewUpdateLock(path, install, false)
	if lcAcquire(t, launch) {
		t.Fatal("the launch claimed over a held checkout")
	}
	launch.Release(context.Background())
	if launch.Holder == nil || !launch.Holder.Held || launch.Holder.PID != 0 {
		t.Fatalf("%+v", launch.Holder)
	}
	if raw, _ := os.ReadFile(path); string(raw) != string(dead) {
		t.Fatal("the dead marker changed")
	}
	holder.kill(t)
	launch = NewUpdateLock(path, install, false)
	if !lcAcquire(t, launch) || !launch.Acquired {
		t.Fatal("with the tree gone the dead marker is reclaimed and claimed")
	}
	launch.Release(context.Background())
	if lcExists(path) {
		t.Fatal("the released claim stayed")
	}
	if held, _ := CheckoutLockHeld(context.Background(), install); held {
		t.Fatal("the checkout stayed held")
	}
}

func TestLockOversizedMarkerNumbersNeverRaiseAndAreReclaimed(t *testing.T) {
	tmp := t.TempDir()
	install := lkMkdir(t, filepath.Join(tmp, "install"))
	path := filepath.Join(tmp, MarkerName)
	for _, text := range []string{
		strings.Repeat("1", 5000) + "\n1\nct:1.5\n",
		"4242\n" + strings.Repeat("9", 5000) + "\nct:1.5\n",
		"4242\n1\nct:1.5\ndelegate:" + strings.Repeat("7", 5000) + " ct:1.5\n",
	} {
		os.WriteFile(path, []byte(text), 0o644)
		if ReadLiveUpdate(context.Background(), path, install) != nil {
			t.Fatal("an oversized marker read live")
		}
		os.WriteFile(path, []byte(text), 0o644)
		lock := NewUpdateLock(path, install, true)
		if !lcAcquire(t, lock) {
			t.Fatalf("%+v", lock.Holder)
		}
		lock.Release(context.Background())
	}
}

// Readers paused while they hold their probe of the checkout lock never read each other as held
func TestLockConcurrentLockProbesNeverReadEachOtherAsHeld(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "install")
	lkMkdir(t, filepath.Join(root, ".git"))
	os.WriteFile(filepath.Join(root, ".git", GitCheckoutLockName), nil, 0o644)
	var readers []*pcProc
	var stdins []interface{ Write([]byte) (int, error) }
	for range 2 {
		cmd := pcCommand("lock_probe_paused", root)
		stdin, _ := cmd.StdinPipe()
		stdout, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		reader := &pcProc{cmd: cmd, out: bufio.NewReader(stdout)}
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				cmd.Process.Kill()
				cmd.Wait()
			}
		})
		if line := reader.line(t); line != "probing" {
			t.Fatal(line)
		}
		readers, stdins = append(readers, reader), append(stdins, stdin)
	}
	for _, stdin := range stdins {
		stdin.Write([]byte("go\n"))
	}
	for _, reader := range readers {
		if line := reader.line(t); line != "False" {
			t.Fatalf("a reader read the other reader's probe as a held lock %q", line)
		}
		if _, code := reader.rest(t); code != 0 {
			t.Fatal(code)
		}
	}
	cmd := pcCommand("lock_checkout_holder", root)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	holder := &pcProc{cmd: cmd, out: bufio.NewReader(stdout)}
	if line := holder.line(t); line != "held" {
		t.Fatal(line)
	}
	if held, _ := CheckoutLockHeld(context.Background(), root); !held {
		t.Fatal("a live holder of the checkout lock must read held")
	}
	stdin.Close()
	holder.rest(t)
	if held, _ := CheckoutLockHeld(context.Background(), root); held {
		t.Fatal("the lock outlived its holder")
	}
}
