package install

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Marker path under a fresh profile tree root with its sibling install root
func markerHome(t *testing.T) (marker, installRoot string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	return filepath.Join(root, updaterecord.MarkerName), filepath.Join(root, InstallDirName)
}

// Live process that is not us, killed at cleanup
func foreignHolder(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), sleepEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	for i := 0; i < 100 && updaterecord.ProcessCreateTime(cmd.Process.Pid) == nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	return cmd.Process.Pid
}

// Pid of a process that has exited
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func v2Body(pid int, startedAt int64, ct float64) string {
	return fmt.Sprintf("%d\n%d\nct:%.3f\n", pid, startedAt, ct)
}

func ctOf(t *testing.T, pid int) float64 {
	t.Helper()
	ct := updaterecord.ProcessCreateTime(pid)
	if ct == nil {
		t.Skip("process creation time is unreadable on this host")
	}
	return *ct
}

func acquire(t *testing.T, marker, installRoot string) (*UpdateGuard, string) {
	t.Helper()
	g, refusal := AcquireUpdateGuard(context.Background(), marker, installRoot)
	if g != nil {
		t.Cleanup(func() { g.Complete(context.Background()) })
	}
	return g, refusal
}

func TestUpdateMarkerGuardWritesThenRemovesOnComplete(t *testing.T) {
	marker, root := markerHome(t)
	g, refusal := acquire(t, marker, root)
	if g == nil {
		t.Fatal(refusal)
	}
	raw, err := os.ReadFile(marker)
	if err != nil || !strings.HasPrefix(string(raw), itoa(os.Getpid())+"\n") {
		t.Fatalf("marker %q %v", raw, err)
	}
	g.Complete(context.Background())
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("marker must be removed on complete")
	}
}

func TestUpdateMarkerGuardCompleteIsQuietWhenAlreadyGone(t *testing.T) {
	marker, root := markerHome(t)
	g, refusal := acquire(t, marker, root)
	if g == nil {
		t.Fatal(refusal)
	}
	os.Remove(marker)
	g.Complete(context.Background())
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("complete recreated the marker")
	}
}

func TestAcquireRefusesWhileALiveUpdaterOwnsTheMarker(t *testing.T) {
	marker, root := markerHome(t)
	pid := foreignHolder(t)
	body := v2Body(pid, time.Now().Unix(), ctOf(t, pid))
	os.WriteFile(marker, []byte(body), 0o644)
	g, refusal := acquire(t, marker, root)
	if g != nil || !strings.Contains(refusal, "PID "+itoa(pid)) {
		t.Fatalf("guard=%v refusal=%q", g, refusal)
	}
	if raw, _ := os.ReadFile(marker); string(raw) != body {
		t.Fatal("a live foreign marker must never be touched")
	}
}

func TestOwnPidV1PrewriteIsAPreviousIncarnationAndReclaimed(t *testing.T) {
	marker, root := markerHome(t)
	ctOf(t, os.Getpid())
	os.WriteFile(marker, []byte(fmt.Sprintf("%d\n%d\n", os.Getpid(), time.Now().Unix())), 0o644)
	g, refusal := acquire(t, marker, root)
	if g == nil {
		t.Fatal(refusal)
	}
	if raw, _ := os.ReadFile(marker); !strings.Contains(string(raw), "\nct:") {
		t.Fatalf("reclaimed claim must carry our creation time: %q", raw)
	}
}

func TestOwnPidWithACreationTime6msOffIsNotUs(t *testing.T) {
	marker, root := markerHome(t)
	own := ctOf(t, os.Getpid())
	stale := v2Body(os.Getpid(), time.Now().Unix(), own+0.006)
	os.WriteFile(marker, []byte(stale), 0o644)
	g, refusal := acquire(t, marker, root)
	if g == nil {
		t.Fatal(refusal)
	}
	if raw, _ := os.ReadFile(marker); string(raw) == stale {
		t.Fatal("a 6ms creation time drift is a previous incarnation and must be replaced")
	}
}

func TestSelfOwnedMarkerPlusExit2Heals(t *testing.T) {
	marker, root := markerHome(t)
	if g, refusal := acquire(t, marker, root); g == nil {
		t.Fatal(refusal)
	}
	if !ShouldHealSelfMarkerRefusal(context.Background(), 2, marker, root) {
		t.Fatal("exit 2 over our own claim must heal")
	}
}

func TestForeignOwnedMarkerNeverHeals(t *testing.T) {
	marker, root := markerHome(t)
	pid := foreignHolder(t)
	os.WriteFile(marker, []byte(v2Body(pid, time.Now().Unix(), ctOf(t, pid))), 0o644)
	if ShouldHealSelfMarkerRefusal(context.Background(), 2, marker, root) {
		t.Fatal("a foreign owner's refusal is legitimate")
	}
}

func TestMissingOrGarbageMarkerNeverHeals(t *testing.T) {
	marker, root := markerHome(t)
	if ShouldHealSelfMarkerRefusal(context.Background(), 2, marker, root) {
		t.Fatal("missing marker healed")
	}
	for _, body := range []string{"", "garbage\n", "not-a-pid\n123\n", itoa(os.Getpid()) + "\n"} {
		os.WriteFile(marker, []byte(body), 0o644)
		if ShouldHealSelfMarkerRefusal(context.Background(), 2, marker, root) {
			t.Fatalf("garbage %q healed", body)
		}
	}
}

func TestNonExit2OutcomesNeverHeal(t *testing.T) {
	marker, root := markerHome(t)
	if g, refusal := acquire(t, marker, root); g == nil {
		t.Fatal(refusal)
	}
	for _, code := range []int{0, 1, 3, -1, 130} {
		if ShouldHealSelfMarkerRefusal(context.Background(), code, marker, root) {
			t.Fatalf("exit %d healed", code)
		}
	}
}

func TestHealEndToEndMarkerLifecycle(t *testing.T) {
	marker, root := markerHome(t)
	g, refusal := acquire(t, marker, root)
	if g == nil {
		t.Fatal(refusal)
	}
	if !ShouldHealSelfMarkerRefusal(context.Background(), 2, marker, root) {
		t.Fatal("heal expected")
	}
	g.Complete(context.Background())
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("heal must drop our claim")
	}
	if ShouldHealSelfMarkerRefusal(context.Background(), 2, marker, root) {
		t.Fatal("a second refusal with the marker gone must not heal again")
	}
	g.Complete(context.Background())
}

func TestAcquireReclaimsAMarkerOwnedByADeadPid(t *testing.T) {
	marker, root := markerHome(t)
	os.WriteFile(marker, []byte(v2Body(deadPID(t), time.Now().Unix(), 1.0)), 0o644)
	if g, refusal := acquire(t, marker, root); g == nil {
		t.Fatal(refusal)
	}
}

func TestAcquireReclaimsAMarkerPastTheAgeCeiling(t *testing.T) {
	marker, root := markerHome(t)
	pid := foreignHolder(t)
	os.WriteFile(marker, []byte(fmt.Sprintf("%d\n%d\n", pid, time.Now().Unix()-21*60)), 0o644)
	if g, refusal := acquire(t, marker, root); g == nil {
		t.Fatal(refusal)
	}
}

func TestCompletedUpdateReleasesMarkerBeforeGuardDrop(t *testing.T) {
	marker, root := markerHome(t)
	g, refusal := acquire(t, marker, root)
	if g == nil {
		t.Fatal(refusal)
	}
	g.Complete(context.Background())
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("complete must release before the host exits")
	}
	g2, refusal := acquire(t, marker, root)
	if g2 == nil {
		t.Fatalf("a completed update must not block the next one: %s", refusal)
	}
}

func TestV2LiveOwnerIsNeverAgedOut(t *testing.T) {
	marker, root := markerHome(t)
	pid := foreignHolder(t)
	os.WriteFile(marker, []byte(v2Body(pid, time.Now().Unix()-2*3600, ctOf(t, pid))), 0o644)
	if g, refusal := acquire(t, marker, root); g != nil || !strings.Contains(refusal, "PID "+itoa(pid)+", started 120m") {
		t.Fatalf("guard=%v refusal=%q", g, refusal)
	}
}

func TestV2MarkerWithMismatchedCreationTimeIsReclaimed(t *testing.T) {
	marker, root := markerHome(t)
	pid := foreignHolder(t)
	os.WriteFile(marker, []byte(v2Body(pid, time.Now().Unix(), ctOf(t, pid)-100)), 0o644)
	if g, refusal := acquire(t, marker, root); g == nil {
		t.Fatalf("a recycled pid must not hold the marker: %s", refusal)
	}
}

func TestMarkerUnderARegularFileIsUnwritable(t *testing.T) {
	_, root := markerHome(t)
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, []byte("x"), 0o644)
	g, refusal := acquire(t, filepath.Join(file, updaterecord.MarkerName), root)
	if g != nil || !strings.HasPrefix(refusal, "Cannot lock this install for the update: ") {
		t.Fatalf("guard=%v refusal=%q", g, refusal)
	}
}

func TestMarkerInAReadOnlyDirIsUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits do not bind here")
	}
	_, root := markerHome(t)
	dir := filepath.Join(t.TempDir(), "ro")
	os.MkdirAll(dir, 0o555)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	g, refusal := acquire(t, filepath.Join(dir, updaterecord.MarkerName), root)
	if g != nil || !strings.HasPrefix(refusal, "Cannot lock this install for the update: ") {
		t.Fatalf("guard=%v refusal=%q", g, refusal)
	}
}

func TestFreshEmptyMarkerIsAClaimInFlight(t *testing.T) {
	marker, root := markerHome(t)
	os.WriteFile(marker, nil, 0o644)
	g, refusal := acquire(t, marker, root)
	if g != nil || !strings.Contains(refusal, "starting right now") {
		t.Fatalf("guard=%v refusal=%q", g, refusal)
	}
}

func TestOldEmptyMarkerIsReclaimedAndClaimLeavesNoTmp(t *testing.T) {
	marker, root := markerHome(t)
	os.WriteFile(marker, nil, 0o644)
	old := time.Now().Add(-time.Minute)
	os.Chtimes(marker, old, old)
	if g, refusal := acquire(t, marker, root); g == nil {
		t.Fatal(refusal)
	}
	entries, _ := os.ReadDir(filepath.Dir(marker))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("claim left %s", e.Name())
		}
	}
}

func TestOwnExactV2ClaimIsAdoptedVerbatimAndReleased(t *testing.T) {
	marker, root := markerHome(t)
	body := fmt.Sprintf("%d\n%d\n%s\n", os.Getpid(), time.Now().Unix(), updaterecord.IdentityLine(os.Getpid()))
	if updaterecord.IdentityLine(os.Getpid()) == "" {
		t.Skip("own creation time unreadable")
	}
	os.WriteFile(marker, []byte(body), 0o644)
	g, refusal := acquire(t, marker, root)
	if g == nil {
		t.Fatal(refusal)
	}
	if raw, _ := os.ReadFile(marker); string(raw) != body {
		t.Fatalf("our exact claim must be adopted verbatim, now %q", raw)
	}
	g.Complete(context.Background())
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("adopted claim must be released")
	}
}

// Child process holding root's checkout lock until cleanup closes its stdin
func holdCheckoutLock(t *testing.T, root string) {
	t.Helper()
	os.MkdirAll(root, 0o755)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), holdCheckoutEnv+"="+root)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Wait() })
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("lock holder: %q", line)
	}
}

func TestDeadMarkerIsNeverReclaimedWhileTheCheckoutLockIsHeld(t *testing.T) {
	marker, root := markerHome(t)
	holdCheckoutLock(t, root)
	body := v2Body(deadPID(t), time.Now().Unix()-65, 1.0)
	os.WriteFile(marker, []byte(body), 0o644)
	g, refusal := acquire(t, marker, root)
	if g != nil || !strings.Contains(refusal, "still holds the checkout") {
		t.Fatalf("guard=%v refusal=%q", g, refusal)
	}
	if raw, _ := os.ReadFile(marker); string(raw) != body {
		t.Fatal("a held checkout keeps the dead marker")
	}
}

func TestSelfOwnedMarkerNeverHealsWhileTheCheckoutLockIsHeld(t *testing.T) {
	marker, root := markerHome(t)
	if g, refusal := acquire(t, marker, root); g == nil {
		t.Fatal(refusal)
	}
	holdCheckoutLock(t, root)
	if ShouldHealSelfMarkerRefusal(context.Background(), 2, marker, root) {
		t.Fatal("a held checkout lock makes the refusal legitimate")
	}
}

func TestBusyUpdateMessageWording(t *testing.T) {
	wait := "Wait for it to finish, or close the window or dashboard tab that started it, then try again."
	cases := []struct {
		holder *updaterecord.UpdateHolder
		want   string
	}{
		{&updaterecord.UpdateHolder{PID: 4242, AgeSeconds: 222}, "Another Icebath update is already running (PID 4242, started 3m 42s ago). " + wait},
		{&updaterecord.UpdateHolder{PID: 7, AgeSeconds: 9}, "Another Icebath update is already running (PID 7, started 9s ago). " + wait},
		{&updaterecord.UpdateHolder{}, "Another Icebath update is starting right now. " + wait},
		{nil, "Another Icebath update is starting right now. " + wait},
		{&updaterecord.UpdateHolder{Held: true, AgeSeconds: 65}, "Another Icebath update is already running (started 1m 5s ago; its owner exited but a process it started still holds the checkout). " + wait},
	}
	for _, c := range cases {
		if got := BusyUpdateMessage(c.holder); got != c.want {
			t.Fatalf("got %q want %q", got, c.want)
		}
	}
}
