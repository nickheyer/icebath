package updaterecord

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

const lcDeadPID = 4294967294

func lcMarker(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), MarkerName)
}

func lcOtherPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "120")
	if err := cmd.Start(); err != nil {
		t.Skipf("no sleep binary %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd.Process.Pid
}

func lcClaim(t *testing.T, path string, pid int, startedAt int64) {
	t.Helper()
	if startedAt == 0 {
		startedAt = time.Now().Unix()
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n%d\n", pid, startedAt)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func lcClaimV2(t *testing.T, path string, pid int, startedAt int64, ctOffset float64) {
	t.Helper()
	if startedAt == 0 {
		startedAt = time.Now().Unix()
	}
	ct := ProcessCreateTime(pid)
	if ct == nil {
		t.Fatalf("no creation time for %d", pid)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n%d\nct:%.3f\n", pid, startedAt, *ct+ctOffset)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func lcLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(strings.TrimPrefix(string(data), "\ufeff"), "\n"), "\n")
}

func lcAcquire(t *testing.T, lock *UpdateLock) bool {
	t.Helper()
	ok, err := lock.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func lcExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestLockAcquireWritesPIDAndStartTimeAndReleases(t *testing.T) {
	path := lcMarker(t)
	lock := NewUpdateLock(path, "", true)
	if !lcAcquire(t, lock) || !lock.Acquired {
		t.Fatal("not acquired")
	}
	lines := lcLines(t, path)
	started, _ := strconv.ParseInt(lines[1], 10, 64)
	ct, _ := strconv.ParseFloat(strings.TrimPrefix(lines[2], "ct:"), 64)
	if len(lines) != 3 || lines[0] != strconv.Itoa(os.Getpid()) || time.Now().Unix()-started > 5 || *ProcessCreateTime(os.Getpid())-ct > 0.01 {
		t.Fatalf("got %q", lines)
	}
	func() {
		defer func() { recover() }()
		defer lock.Release(context.Background())
		panic("raised inside the claim")
	}()
	if lcExists(path) {
		t.Fatal("release left the marker")
	}
}

func TestLockSecondAcquireRefusedAndNeverDeletesTheLiveMarker(t *testing.T) {
	path, other := lcMarker(t), lcOtherPID(t)
	lcClaim(t, path, other, 0)
	second := NewUpdateLock(path, "", true)
	if lcAcquire(t, second) || second.Holder == nil || second.Holder.PID != other || second.Acquired {
		t.Fatalf("%+v", second.Holder)
	}
	second.Release(context.Background())
	if !lcExists(path) {
		t.Fatal("a refused claimant cleared the live owner's lock")
	}
}

func TestLockMarkerOwnedByAZombieSelfHeals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix zombies")
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	defer cmd.Wait()
	zombie := false
	for range 40 {
		if proc.Zombie(cmd.Process.Pid) {
			zombie = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !zombie {
		t.Fatal("child never reached zombie state")
	}
	path := lcMarker(t)
	lcClaim(t, path, cmd.Process.Pid, 0)
	if ReadLiveUpdate(context.Background(), path, "") != nil || lcExists(path) {
		t.Fatal("a zombie read as a live update")
	}
}

func TestLockMarkerNamingOurOwnPIDIsAdoptedAndClaimedFresh(t *testing.T) {
	path := lcMarker(t)
	lcClaim(t, path, os.Getpid(), time.Now().Unix()-UpdateMarkerMaxAgeSeconds+5)
	lock := NewUpdateLock(path, "", true)
	if !lcAcquire(t, lock) || !lock.Acquired {
		t.Fatal("not adopted")
	}
	if started, _ := strconv.ParseInt(lcLines(t, path)[1], 10, 64); time.Now().Unix()-started > 5 {
		t.Fatal("the nearly expired claim was kept")
	}
	if holder := ReadLiveUpdate(context.Background(), path, ""); holder == nil || holder.PID != os.Getpid() {
		t.Fatalf("%+v", holder)
	}
	lock.Release(context.Background())
	if lcExists(path) {
		t.Fatal("marker kept")
	}
}

func TestLockNearbyCreationTimeOfOurPIDIsAKilledUpdate(t *testing.T) {
	path := lcMarker(t)
	lcClaimV2(t, path, os.Getpid(), 0, -1.5)
	if ReadLiveUpdate(context.Background(), path, "") != nil {
		t.Fatal("a dead update's claim read as live")
	}
	lcClaimV2(t, path, os.Getpid(), 0, -1.5)
	lock := NewUpdateLock(path, "", true)
	if !lcAcquire(t, lock) || !lock.Acquired || lcLines(t, path)[2] != IdentityLine(os.Getpid()) {
		t.Fatalf("%q", lcLines(t, path))
	}
	lock.Release(context.Background())
}

func TestLockOneIncarnationRuleWhenOurCreationTimeIsUnreadable(t *testing.T) {
	ownCT.Lock()
	savedPID, savedCT := ownCT.pid, ownCT.ct
	ownCT.pid, ownCT.ct = os.Getpid(), nil
	ownCT.Unlock()
	t.Cleanup(func() { ownCT.Lock(); ownCT.pid, ownCT.ct = savedPID, savedCT; ownCT.Unlock() })
	path := lcMarker(t)
	now := time.Now().Unix()
	for _, tc := range []struct {
		text string
		ours bool
	}{{fmt.Sprintf("%d\n%d\n", os.Getpid(), now), true}, {fmt.Sprintf("%d\n%d\nct:%d.000\n", os.Getpid(), now, now-1), false}} {
		os.WriteFile(path, []byte(tc.text), 0o644)
		raw, _ := os.ReadFile(path)
		parsed := parseMarker(raw, nil)
		live := incarnation(os.Getpid(), parsed.createTime, realWorld())
		if parsed.ownerLive(realWorld()) != tc.ours || live == nil || *live != tc.ours || lockHolder(path).Held == tc.ours {
			t.Fatalf("%q", tc.text)
		}
	}
}

func TestLockReleaseLeavesAMarkerAHandoffPartnerNowOwns(t *testing.T) {
	path := lcMarker(t)
	lock := NewUpdateLock(path, "", true)
	lcAcquire(t, lock)
	os.WriteFile(path, []byte(fmt.Sprintf("%d\n%d\n", lcDeadPID, time.Now().Unix())), 0o644)
	lock.Release(context.Background())
	if !lcExists(path) {
		t.Fatal("the partner's marker was removed")
	}
}

func TestLockDeadReusedAndAgedOwnersAreReclaimedLiveV2IsNot(t *testing.T) {
	path, other := lcMarker(t), lcOtherPID(t)
	reclaim := func(name string) {
		lock := NewUpdateLock(path, "", true)
		if !lcAcquire(t, lock) || !lock.Acquired || lcLines(t, path)[0] != strconv.Itoa(os.Getpid()) {
			t.Fatalf("%s not reclaimed", name)
		}
		lock.Release(context.Background())
	}
	os.WriteFile(path, []byte(fmt.Sprintf("%d\n%d\n", lcDeadPID, time.Now().Unix())), 0o644)
	reclaim("dead owner")
	lcClaim(t, path, other, time.Now().Unix()-UpdateMarkerMaxAgeSeconds-60)
	reclaim("v1 past the ceiling")
	lcClaimV2(t, path, other, 0, -100)
	reclaim("reused pid")
	lcClaimV2(t, path, other, time.Now().Unix()-25*60, 0)
	before, _ := os.ReadFile(path)
	lock := NewUpdateLock(path, "", true)
	if lcAcquire(t, lock) || lock.Holder == nil || lock.Holder.PID != other || ReadLiveUpdate(context.Background(), path, "") == nil {
		t.Fatal("a live v2 owner was reclaimed by age")
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a refused claimant rewrote the owner's marker")
	}
}

func TestLockDelegateKeepsTheClaimLiveAfterThePartnerDies(t *testing.T) {
	path, other := lcMarker(t), lcOtherPID(t)
	lcClaimV2(t, path, other, 0, 0)
	base, _ := os.ReadFile(path)
	t.Setenv(HandoffPIDEnv, strconv.Itoa(other))
	lock := NewUpdateLock(path, "", true)
	if !lcAcquire(t, lock) || lock.Acquired {
		t.Fatal("handoff not adopted")
	}
	lines := lcLines(t, path)
	now, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(now), string(base)) || !strings.HasPrefix(lines[3], fmt.Sprintf("delegate:%d ct:", os.Getpid())) {
		t.Fatalf("%q", lines)
	}
	os.WriteFile(path, []byte(fmt.Sprintf("%d\n%d\nct:1.000\n%s\n", lcDeadPID, time.Now().Unix(), lines[3])), 0o644)
	if holder := ReadLiveUpdate(context.Background(), path, ""); holder == nil || holder.PID != os.Getpid() {
		t.Fatalf("%+v", holder)
	}
	os.WriteFile(path, append(append([]byte{}, base...), lines[3]+"\n"...), 0o644)
	lock.Release(context.Background())
	if after, _ := os.ReadFile(path); string(after) != string(base) {
		t.Fatalf("partner claim not restored %q", after)
	}
}

func TestLockMalformedMarkersNeverBlockAnUpdate(t *testing.T) {
	other := lcOtherPID(t)
	now := time.Now().Unix()
	for _, body := range []string{"", "not-a-pid\n123\n", "\n\n", "12345", fmt.Sprintf("%d\n%d.5\n", other, now), fmt.Sprintf("1_0\n%d\n", now), fmt.Sprintf("%d\nsoon\n", other)} {
		path := lcMarker(t)
		os.WriteFile(path, []byte(body), 0o644)
		old := time.Now().Add(-60 * time.Second)
		os.Chtimes(path, old, old)
		lock := NewUpdateLock(path, "", true)
		if ReadLiveUpdate(context.Background(), path, "") != nil || !lcAcquire(t, lock) {
			t.Fatalf("%q blocked", body)
		}
		lock.Release(context.Background())
	}
}

func TestLockFreshEmptyMarkerIsAClaimInFlight(t *testing.T) {
	path := lcMarker(t)
	os.WriteFile(path, nil, 0o644)
	holder := ReadLiveUpdate(context.Background(), path, "")
	lock := NewUpdateLock(path, "", true)
	if holder == nil || holder.PID != 0 || lcAcquire(t, lock) || !lcExists(path) {
		t.Fatalf("%+v", holder)
	}
}

func TestLockClaimPublishesWholeAndReclaimsDeadTmpSiblings(t *testing.T) {
	path, other := lcMarker(t), lcOtherPID(t)
	dead := fmt.Sprintf("%s.%d.abcd.tmp", path, lcDeadPID)
	live := fmt.Sprintf("%s.%d.abcd.tmp", path, other)
	stale := fmt.Sprintf("%s.%d.0000.tmp", path, os.Getpid())
	fresh := fmt.Sprintf("%s.%d.1111.tmp", path, os.Getpid())
	lookalike := path + ".١٢.tmp"
	huge := path + ".99999999999999999999.tmp"
	for _, name := range []string{dead, live, stale, fresh, lookalike, huge} {
		os.WriteFile(name, []byte("x"), 0o644)
	}
	old := time.Now().Add(-2 * time.Minute)
	os.Chtimes(stale, old, old)
	lock := NewUpdateLock(path, "", true)
	if !lcAcquire(t, lock) {
		t.Fatal("not acquired")
	}
	defer lock.Release(context.Background())
	for name, want := range map[string]bool{dead: false, live: true, stale: false, fresh: true, lookalike: true, huge: false} {
		if lcExists(name) != want {
			t.Fatalf("%s exists=%v", filepath.Base(name), !want)
		}
	}
	if len(lcLines(t, path)) != 3 {
		t.Fatal("torn claim")
	}
}

func TestLockAbsentAndStaleMarkers(t *testing.T) {
	path := lcMarker(t)
	if ReadLiveUpdate(context.Background(), path, "") != nil {
		t.Fatal("absent marker read live")
	}
	os.WriteFile(path, []byte(fmt.Sprintf("%d\n%d\n", lcDeadPID, time.Now().Unix())), 0o644)
	if ReadLiveUpdate(context.Background(), path, "") != nil || lcExists(path) {
		t.Fatal("stale marker kept")
	}
}

func TestLockDescribeHolder(t *testing.T) {
	text := DescribeHolder(&UpdateHolder{PID: 4242, AgeSeconds: 125})
	if !strings.Contains(text, "started 2m 5s ago, process 4242") || !strings.Contains(text, "icebath update") {
		t.Fatalf("%q", text)
	}
	if text := DescribeHolder(&UpdateHolder{AgeSeconds: 7, Held: true}); !strings.Contains(text, "started 7s ago; its owner exited but a process it started still holds the checkout") {
		t.Fatalf("%q", text)
	}
	if text := DescribeHolder(nil); !strings.Contains(text, "(started 0s ago)") {
		t.Fatalf("%q", text)
	}
	if text := DescribeHolder(&UpdateHolder{Reason: "x is not writable"}); !strings.HasPrefix(text, "✗ Cannot lock this install for the update: x is not writable.\n") {
		t.Fatalf("%q", text)
	}
}

func TestLockUnwritableMarkerLocationRefusesAndSaysWhy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o555)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	lock := NewUpdateLock(filepath.Join(dir, "sub", MarkerName), "", true)
	if lcAcquire(t, lock) || lock.Holder == nil {
		t.Fatal("ran unlocked")
	}
	text := DescribeHolder(lock.Holder)
	if !strings.Contains(text, "is not writable ([Errno 13] Permission denied") || !strings.Contains(text, "the update records itself there so the Desktop app, gateways, installers and other updaters wait for it") || !strings.Contains(text, "Make that path\n  writable") {
		t.Fatalf("%q", text)
	}
}

func TestLockCheckoutLockExcludesAnUpdateFromAnotherHome(t *testing.T) {
	root := t.TempDir()
	lockPath, _ := CheckoutLockPath(root)
	other := lcOtherPID(t)
	os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n%d\n%s\n", other, time.Now().Unix(), IdentityLine(other))), 0o644)
	holderFile, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := tryLock(holderFile); !ok || err != nil {
		t.Fatal("could not hold the checkout lock")
	}
	held, err := CheckoutLockHeld(context.Background(), root)
	if err != nil || !held {
		t.Fatal("held lock read free")
	}
	lock := NewUpdateLock(lcMarker(t), root, true)
	start := time.Now()
	if lcAcquire(t, lock) || lock.Holder == nil || lock.Holder.PID != other || time.Since(start) < 2*time.Second {
		t.Fatalf("%+v", lock.Holder)
	}
	holderFile.Close()
	if held, _ := CheckoutLockHeld(context.Background(), root); held {
		t.Fatal("free lock read held")
	}
	lock = NewUpdateLock(lcMarker(t), root, true)
	if !lcAcquire(t, lock) {
		t.Fatalf("%+v", lock.Holder)
	}
	if holds, _ := HoldsCheckoutLock(root); !holds || len(CustodySpawnFiles()) != 1 {
		t.Fatal("hold not recorded")
	}
	if lines := lcLines(t, lockPath); lines[0] != strconv.Itoa(os.Getpid()) {
		t.Fatalf("record %q", lines)
	}
	lock.Release(context.Background())
	if holds, _ := HoldsCheckoutLock(root); holds {
		t.Fatal("hold kept after release")
	}
}

func TestLockGitCheckoutLockLivesInTheCommonGitDir(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	lock := NewUpdateLock(lcMarker(t), root, true)
	if !lcAcquire(t, lock) {
		t.Fatalf("%+v", lock.Holder)
	}
	defer lock.Release(context.Background())
	if !lcExists(filepath.Join(root, ".git", GitCheckoutLockName)) || lcExists(filepath.Join(root, CheckoutLockName)) {
		t.Fatal("lock dirtied the tree")
	}
}

func TestLockUnwritableInstallRootRefusesAndLinkedLockFilesAreRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	root := t.TempDir()
	os.Chmod(root, 0o555)
	t.Cleanup(func() { os.Chmod(root, 0o755) })
	lock := NewUpdateLock(lcMarker(t), root, true)
	if lcAcquire(t, lock) || lock.Holder == nil || !strings.Contains(lock.Holder.Reason, "is not writable (Permission denied); the update locks the checkout there") {
		t.Fatalf("%+v", lock.Holder)
	}
	linked := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("keep"), 0o644)
	os.Link(outside, filepath.Join(linked, CheckoutLockName))
	lock = NewUpdateLock(lcMarker(t), linked, true)
	if lcAcquire(t, lock) || !strings.Contains(lock.Holder.Reason, "is not a regular single-link file; delete it") {
		t.Fatalf("%+v", lock.Holder)
	}
	symlinked := t.TempDir()
	os.Symlink(outside, filepath.Join(symlinked, CheckoutLockName))
	lock = NewUpdateLock(lcMarker(t), symlinked, true)
	if lcAcquire(t, lock) || lock.Holder == nil || lock.Holder.Reason == "" {
		t.Fatalf("followed a planted link %+v", lock.Holder)
	}
	if data, _ := os.ReadFile(outside); string(data) != "keep" {
		t.Fatal("wrote through a link")
	}
}

func TestLockHandoffFromOrchestratingUpdater(t *testing.T) {
	other := lcOtherPID(t)
	path := lcMarker(t)
	lcClaimV2(t, path, other, 0, 0)
	t.Setenv(HandoffPIDEnv, strconv.Itoa(other))
	lock := NewUpdateLock(path, "", true)
	if !lcAcquire(t, lock) || lock.Acquired {
		t.Fatal("child refused under the parent's claim")
	}
	lock.Release(context.Background())
	lcClaimV2(t, path, other, 0, 0)
	match := func([]string) bool { return true }
	SetRuntimeHostMatcher(match)
	t.Cleanup(func() { runtimeHostMatcher.Store(nil) })
	lock = NewUpdateLock(path, "", true)
	if lcAcquire(t, lock) || lock.Holder == nil || lock.Holder.PID != other {
		t.Fatal("handoff below a runtime host adopted")
	}
	runtimeHostMatcher.Store(nil)
	third := lcOtherPID(t)
	for _, value := range []string{strconv.Itoa(third), "abc", "-5", "0", "", " "} {
		t.Setenv(HandoffPIDEnv, value)
		lock = NewUpdateLock(path, "", true)
		if lcAcquire(t, lock) {
			t.Fatalf("handoff %q granted", value)
		}
	}
	os.Remove(path)
	t.Setenv(HandoffPIDEnv, strconv.Itoa(other))
	lock = NewUpdateLock(path, "", true)
	if !lcAcquire(t, lock) || !lock.Acquired {
		t.Fatal("no marker did not claim normally")
	}
	lock.Release(context.Background())
}

func TestLockAncestryHandoff(t *testing.T) {
	path := lcMarker(t)
	lcClaimV2(t, path, os.Getppid(), 0, 0)
	lock := NewUpdateLock(path, "", true)
	if !lcAcquire(t, lock) || lock.Acquired {
		t.Fatalf("parent's claim refused %+v", lock.Holder)
	}
	lock.Release(context.Background())
	saved := parentOf
	t.Cleanup(func() { parentOf = saved })
	chain := map[int]int{os.Getpid(): 900001, 900001: 900002, 900002: 900003}
	unreadable := map[int]bool{}
	parentOf = func(pid int) (int, bool, error) {
		if unreadable[pid] {
			return 0, false, errors.New("access denied")
		}
		parent, ok := chain[pid]
		return parent, ok, nil
	}
	unreadable[900003] = true
	if !isAncestorPID(900002) || !isAncestorPID(900003) {
		t.Fatal("ancestor below an unreadable process missed")
	}
	unreadable = map[int]bool{900001: true}
	if isAncestorPID(900002) {
		t.Fatal("an unreadable process below the match admitted")
	}
	unreadable = map[int]bool{}
	if isAncestorPID(900099) || isAncestorPID(os.Getpid()) || isAncestorPID(0) {
		t.Fatal("unrelated or own pid admitted")
	}
	calls := 0
	parentOf = func(pid int) (int, bool, error) { calls++; return pid + 1, true, nil }
	if isAncestorPID(1) || calls != maxAncestryDepth {
		t.Fatalf("walk unbounded %d", calls)
	}
}

func TestLockMarkerMutexIsReentrantAlongTheContextAndBusyElsewhere(t *testing.T) {
	path := lcMarker(t)
	ctx, release, err := MarkerMutex(context.Background(), path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, inner, err := MarkerMutex(ctx, path, time.Second); err != nil {
		t.Fatal(err)
	} else {
		inner()
	}
	var busy *MarkerBusy
	if _, _, err := MarkerMutex(context.Background(), path, 50*time.Millisecond); !errors.As(err, &busy) {
		t.Fatalf("second holder got %v", err)
	}
	release()
	if !lcExists(MarkerMutexPath(path)) {
		t.Fatal("sidecar removed")
	}
}

func TestLockCheckoutLockOutlivesItsOwnerWhileAnInheritingChildRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX descriptor inheritance")
	}
	root := t.TempDir()
	lock := NewUpdateLock(lcMarker(t), root, true)
	if !lcAcquire(t, lock) {
		t.Fatalf("%+v", lock.Holder)
	}
	files, err := CheckoutLockFiles(root)
	if err != nil || len(files) != 1 {
		t.Fatalf("%v %v", files, err)
	}
	child := exec.Command("sleep", "120")
	child.ExtraFiles = CustodySpawnFiles()
	if err := child.Start(); err != nil {
		t.Skip(err)
	}
	lock.Release(context.Background())
	if held, _ := CheckoutLockHeld(context.Background(), root); !held {
		t.Fatal("the lock died with its owner while the inheriting child runs")
	}
	child.Process.Kill()
	child.Wait()
	if held, _ := CheckoutLockHeld(context.Background(), root); held {
		t.Fatal("the lock outlived the last inheriting child")
	}
}

func TestLockUnwritableLockFileStillLocksTheCheckout(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("needs a read only file the user cannot write")
	}
	root := t.TempDir()
	lockPath, _ := CheckoutLockPath(root)
	os.WriteFile(lockPath, []byte("1\n2\n"), 0o444)
	lock := NewUpdateLock(lcMarker(t), root, true)
	if !lcAcquire(t, lock) {
		t.Fatalf("%+v", lock.Holder)
	}
	defer lock.Release(context.Background())
	if held, _ := CheckoutLockHeld(context.Background(), root); !held {
		t.Fatal("read only lock file left the checkout unlocked")
	}
	if data, _ := os.ReadFile(lockPath); string(data) != "1\n2\n" {
		t.Fatal("read only record rewritten")
	}
}

func TestLockDeadMarkerOverAHeldCheckoutLockIsHeldNeverReclaimed(t *testing.T) {
	root := t.TempDir()
	lockPath, _ := CheckoutLockPath(root)
	holderFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer holderFile.Close()
	if ok, _ := tryLock(holderFile); !ok {
		t.Fatal("could not hold the checkout lock")
	}
	path := lcMarker(t)
	os.WriteFile(path, []byte(fmt.Sprintf("%d\n%d\n", lcDeadPID, time.Now().Unix()-30)), 0o644)
	holder := ReadLiveUpdate(context.Background(), path, root)
	if holder == nil || !holder.Held || holder.PID != 0 || holder.AgeSeconds < 29 || !lcExists(path) {
		t.Fatalf("%+v", holder)
	}
	if in, _ := UpdateInProgress(context.Background(), root); !in {
		t.Fatal("update not in progress")
	}
	lock := NewUpdateLock(path, root, false)
	if lcAcquire(t, lock) || lock.Holder == nil || !lock.Holder.Held || !lcExists(path) {
		t.Fatalf("%+v", lock.Holder)
	}
}

func TestAcquireAndReleaseCheckoutExports(t *testing.T) {
	root := t.TempDir()
	refused, err := AcquireCheckout(root)
	if err != nil || refused != nil {
		t.Fatalf("acquire %v %v", refused, err)
	}
	held, err := HoldsCheckoutLock(root)
	if err != nil || !held {
		t.Fatalf("held %v %v", held, err)
	}
	ReleaseCheckout()
	if held, _ := HoldsCheckoutLock(root); held {
		t.Fatal("lock still held after release")
	}
}

func TestPsutilParentNamesTheParentOfThisProcess(t *testing.T) {
	parent, ok, err := PsutilParent(os.Getpid())
	if err != nil || !ok || parent != os.Getppid() {
		t.Fatalf("parent %d %v %v want %d", parent, ok, err, os.Getppid())
	}
}
