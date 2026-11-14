//go:build !windows

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

func bootTimeOf(t *testing.T) float64 {
	t.Helper()
	boot, err := proc.BootTime()
	if err != nil {
		t.Fatal(err)
	}
	return boot
}

func TestTwoHomesTakeTheirOwnPerHomeLockButOnlyOneHostLock(t *testing.T) {
	dir := hostDir(t)
	homeA, homeB := filepath.Join(dir, "home_a"), filepath.Join(dir, "home_b")
	os.Mkdir(homeA, 0o700)
	os.Mkdir(homeB, 0o700)
	child := startHostChild(t, "runtime-lock", "ICEBATH_HOME="+homeA)
	var first map[string]bool
	if err := json.Unmarshal([]byte(child.readLine(t)), &first); err != nil || !first["per_home"] || !first["host"] {
		t.Fatalf("child %v %v", first, err)
	}
	t.Setenv("ICEBATH_HOME", homeB)
	defer ReleaseGatewayRuntimeLock()
	if !AcquireGatewayRuntimeLock(context.Background()) {
		t.Fatal("second home must get its OWN lock")
	}
	if o, _ := ClaimHostLock(context.Background(), RoleGateway); o != HostLockHeldByOther {
		t.Fatal("host lock is not per-home")
	}
}

func TestLiveHostRecordSurvivesWallClockCreateTimeDrift(t *testing.T) {
	dir := hostDir(t)
	r := publishGateway(t, dir)
	drifted := *r
	drifted.CreateTime = f64(*r.CreateTime - 5)
	if HostRecordIsStale(&drifted) || !HostLivenessIsProven(&drifted) {
		t.Fatal("create time drift staled a live owner")
	}
	if r.StartTime == nil {
		t.Fatal("no start fingerprint published")
	}
	prev := hostProcessStartTime
	hostProcessStartTime = func(int) any { return *r.StartTime + StartTimeDriftTolerance + 1 }
	t.Cleanup(func() { hostProcessStartTime = prev })
	if !HostRecordIsStale(r) {
		t.Fatal("a changed start fingerprint is not pid reuse")
	}
}

func TestReusedEarlyPidAfterRebootIsNotTheGateway(t *testing.T) {
	hostDir(t)
	child := startHostChild(t, "sleep")
	start := GetProcessStartTime(child.pid())
	n, ok := jsonPyInt(start)
	if !ok {
		t.Fatalf("start %v", start)
	}
	boot := bootTimeOf(t)
	r := gatewayRecord(child.pid(), f64(boot-3600), n.Int64()+10, "")
	if HostLivenessIsProven(r) || !HostRecordIsStale(r) || exists(HostLockPath(RoleGateway)) {
		t.Fatal("a pre-boot record with a free lock vouched for a reused pid")
	}
	drifted := *r
	drifted.CreateTime = f64(boot + 1)
	if !HostLivenessIsProven(&drifted) || HostRecordIsStale(&drifted) {
		t.Fatal("a record dated inside this boot is not live")
	}
	unprovable := *r
	unprovable.CreateTime, unprovable.StartTime = nil, nil
	if HostLivenessIsProven(&unprovable) || !HostRecordIsStale(&unprovable) {
		t.Fatal("a record without incarnation proof vouched for its pid")
	}
}

func TestLiveLegacyOwnerSurvivesAClockStepPastItsCreateTime(t *testing.T) {
	dir := hostDir(t)
	child := startHostChild(t, "runtime-lock", "ICEBATH_HOME="+filepath.Join(dir, "home"))
	var first map[string]bool
	if err := json.Unmarshal([]byte(child.readLine(t)), &first); err != nil || !first["host"] {
		t.Fatalf("child %v %v", first, err)
	}
	r := gatewayRecord(child.pid(), f64(bootTimeOf(t)-3600), GetProcessStartTime(child.pid()), "")
	if r.StartTime == nil || !HostLivenessIsProven(r) || HostRecordIsStale(r) {
		t.Fatal("a lock holding legacy owner went stale after a clock step")
	}
}

func TestEmptyBootEpochDoesNotStaleALiveOwner(t *testing.T) {
	dir := hostDir(t)
	empty := *publishGateway(t, dir)
	empty.BootEpoch = ""
	if !HostLivenessIsProven(&empty) || HostRecordIsStale(&empty) {
		t.Fatal("an empty epoch staled a live owner")
	}
}

func init() {
	platformHostChildModes["sigterm-gateway-loop"] = func(hold func()) int {
		if outcome, _ := ClaimHostLock(context.Background(), RoleGateway); outcome != HostLockAcquired {
			return 1
		}
		PublishHostRecord(context.Background(), RoleGateway, HostPublishOptions{Profiles: []string{}, Home: os.Getenv("ICEBATH_HOME")})
		HostCleanupOnExit(RoleGateway)
		SupersedeHostSignalCleanup()
		loop := make(chan os.Signal, 1)
		signal.Notify(loop, syscall.SIGTERM)
		fmt.Println("published")
		<-loop
		time.Sleep(300 * time.Millisecond)
		os.Exit(0)
		return 0
	}
}

func TestGatewayLoopSigtermLeavesTheHostRecordOfItsHardExit(t *testing.T) {
	hostDir(t)
	child := startHostChild(t, "sigterm-gateway-loop", "ICEBATH_HOME="+t.TempDir())
	if child.readLine(t) != "published" || !exists(HostRecordPath(RoleGateway)) {
		t.Fatal("child did not publish")
	}
	child.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { child.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("child outlived SIGTERM")
	}
	if code := child.cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("the loop handler exits 0, got %d after a second SIGTERM", code)
	}
	raw, err := os.ReadFile(HostRecordPath(RoleGateway))
	if err != nil {
		t.Fatal("the superseded cleanup removed the record the gateway hard exit leaves")
	}
	if !strings.HasPrefix(string(raw), "{\n  \"role\": \"gateway\",\n  \"home\": ") {
		t.Fatalf("record is not the two space indented document of atomic_json_write: %q", raw)
	}
}

func TestSigtermRemovesTheRecordAndItsLiveSessionToken(t *testing.T) {
	hostDir(t)
	child := startHostChild(t, "sigterm-serve")
	if child.readLine(t) != "published" || !exists(HostRecordPath(RoleServe)) || !exists(HostTokenPath(RoleServe)) {
		t.Fatal("child did not publish")
	}
	child.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { child.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("child outlived SIGTERM")
	}
	if exists(HostRecordPath(RoleServe)) {
		t.Fatal("record outlived its process")
	}
	if exists(HostTokenPath(RoleServe)) {
		t.Fatal("live session token outlived its process")
	}
	if o, _ := ClaimHostLock(context.Background(), RoleServe); o != HostLockAcquired {
		t.Fatal("host lock not released")
	}
}

func TestAnotherProcessHoldingTheHostLockIsObservedAndItsExitFreesTheFile(t *testing.T) {
	dir := hostDir(t)
	sleeper := startHostChild(t, "sleep")
	holder := startHostChild(t, "holder")
	if holder.readLine(t) != "held" {
		t.Fatal("holder did not claim")
	}
	writeHostRecord(t, gatewayRecord(sleeper.pid(), HostProcessCreateTime(sleeper.pid()), GetProcessStartTime(sleeper.pid()), dir))
	if held := HostLockIsHeld(RoleGateway); OwnsHostLock(RoleGateway) || held == nil || !*held {
		t.Fatal("another process's lock is not observed")
	}
	if DiscardDeadHostRecord(RoleGateway) || !exists(HostRecordPath(RoleGateway)) {
		t.Fatal("record retracted under a held lock")
	}
	holder.kill()
	if held := HostLockIsHeld(RoleGateway); !exists(HostLockPath(RoleGateway)) || held == nil || *held {
		t.Fatal("the dead holder's lock does not read free")
	}
	if !DiscardDeadHostRecord(RoleGateway) || exists(HostRecordPath(RoleGateway)) {
		t.Fatal("record kept after the holder died")
	}
}

func TestRestartCleanupNeverDeletesAGatewayThatClaimedTheLockMidCleanup(t *testing.T) {
	dir := hostDir(t)
	sleeper := startHostChild(t, "sleep")
	claimer := startHostChild(t, "claimer")
	if claimer.readLine(t) != "ready" {
		t.Fatal("claimer not ready")
	}
	claim := func() bool {
		claimer.stdin.Write([]byte("claim\n"))
		return claimer.readLine(t) == "true"
	}
	writeHostRecord(t, gatewayRecord(sleeper.pid(), HostProcessCreateTime(sleeper.pid()), GetProcessStartTime(sleeper.pid()), dir))
	var wonInGap []bool
	prev := hostUnlink
	hostUnlink = func(path string) error {
		if len(wonInGap) == 0 {
			wonInGap = append(wonInGap, claim())
		}
		return prev(path)
	}
	DiscardDeadHostRecord(RoleGateway)
	hostUnlink = prev
	if len(wonInGap) == 0 {
		t.Fatal("cleanup never reached the unlink")
	}
	if !wonInGap[0] && !claim() {
		t.Fatal("the cleanup must release the host lock it took")
	}
	current := ReadHostRecord(RoleGateway, true)
	if current == nil || current.PID != claimer.pid() || !exists(HostTokenPath(RoleGateway)) {
		t.Fatalf("new owner's record or token deleted: %+v", current)
	}
}

func TestAForeignRecordIsNotARecord(t *testing.T) {
	dir := hostDir(t)
	owner := startHostChild(t, "sleep")
	publishRaw(t, owner.pid(), filepath.Join(dir, "root"), []string{"default", "other"})
	realUID := os.Getuid()
	prev := hostGetuid
	hostGetuid = func() int { return realUID + 1 }
	t.Cleanup(func() { hostGetuid = prev })
	if ReadHostRecord(RoleGateway, false) != nil || hostGateway() != nil {
		t.Fatal("a foreign record decided the lifecycle")
	}
}
