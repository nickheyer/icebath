package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const hostChildEnv = "ICEBATH_TEST_HOST_CHILD"

// Child modes the rendezvous tests run in a re-executed test binary
func runHostRendezvousChild(mode string) int {
	hold := func() {
		done := make(chan struct{})
		go func() { io.Copy(io.Discard, os.Stdin); close(done) }()
		select {
		case <-done:
		case <-time.After(120 * time.Second):
		}
	}
	switch mode {
	case "sleep":
		hold()
	case "runtime-lock":
		perHome := AcquireGatewayRuntimeLock(context.Background())
		outcome, _ := ClaimHostLock(context.Background(), RoleGateway)
		line, _ := json.Marshal(map[string]bool{"per_home": perHome, "host": outcome == HostLockAcquired})
		fmt.Println(string(line))
		hold()
	case "sigterm-cleanup":
		if outcome, _ := ClaimHostLock(context.Background(), RoleGateway); outcome != HostLockAcquired {
			return 1
		}
		PublishHostRecord(context.Background(), RoleGateway, HostPublishOptions{Profiles: []string{"default"}})
		HostCleanupOnExit(RoleGateway)
		fmt.Println("published")
		hold()
	case "holder":
		if outcome, _ := ClaimHostLock(context.Background(), RoleGateway); outcome != HostLockAcquired {
			return 1
		}
		fmt.Println("held")
		hold()
	case "claimer":
		fmt.Println("ready")
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			outcome, _ := ClaimHostLock(context.Background(), RoleGateway)
			won := outcome == HostLockAcquired
			if won {
				PublishHostRecord(context.Background(), RoleGateway, HostPublishOptions{Profiles: []string{"default"}})
			}
			fmt.Println(won)
		}
	default:
		if run, ok := platformHostChildModes[mode]; ok {
			return run(hold)
		}
		return 2
	}
	return 0
}

// Child modes only one platform builds, registered by its test file
var platformHostChildModes = map[string]func(hold func()) int{}

type hostChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

// Starts the test binary in a host child mode with extra environment, killed at cleanup
func startHostChild(t *testing.T, mode string, env ...string) *hostChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(append(os.Environ(), hostChildEnv+"="+mode), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &hostChild{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(out)}
	t.Cleanup(c.kill)
	return c
}

func (c *hostChild) pid() int { return c.cmd.Process.Pid }

func (c *hostChild) kill() {
	if c.cmd.ProcessState == nil {
		c.cmd.Process.Kill()
		c.cmd.Wait()
	}
}

func (c *hostChild) readLine(t *testing.T) string {
	t.Helper()
	line, err := c.stdout.ReadString('\n')
	if err != nil {
		t.Fatalf("child output: %v", err)
	}
	return line[:len(line)-1]
}

// Points the lock directory at a temp dir and releases both role locks afterwards
func hostDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(dir, "locks"))
	invalidateHostGatewayCache()
	t.Cleanup(func() {
		ReleaseHostLock(RoleGateway)
		invalidateHostGatewayCache()
	})
	return dir
}

func i64(v int64) *int64 { return &v }

func gatewayRecord(pid int, createTime *float64, startTime any, home string) *HostRecord {
	r := &HostRecord{Role: RoleGateway, PID: pid, CreateTime: createTime, ProtocolVersion: HostProtocolVersion,
		Profiles: []string{"default"}, UpdatedAt: "2026-01-01T00:00:00+00:00", Home: home}
	if n, ok := jsonPyInt(startTime); ok {
		r.StartTime = i64(n.Int64())
	}
	return r
}

func writeHostRecord(t *testing.T, r *HostRecord) {
	t.Helper()
	if _, err := EnsureHostStateDir(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(r.ToJSON())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HostRecordPath(r.Role), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func publishGateway(t *testing.T, home string) *HostRecord {
	t.Helper()
	r := PublishHostRecord(context.Background(), RoleGateway, HostPublishOptions{Profiles: []string{"default"}, Home: home})
	if r == nil {
		t.Fatal("publish failed")
	}
	return r
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestStaleRecordIsNeverAttachable(t *testing.T) {
	for _, tc := range []struct {
		name string
		pid  int
	}{{"dead-pid", 1<<22 - 1}, {"creation-time-mismatch", os.Getpid()}} {
		t.Run(tc.name, func(t *testing.T) {
			hostDir(t)
			r := &HostRecord{Role: RoleGateway, PID: tc.pid, CreateTime: f64(1.0),
				ProtocolVersion: HostProtocolVersion, Profiles: []string{"default"}, UpdatedAt: "2026-01-01T00:00:00+00:00"}
			writeHostRecord(t, r)
			if !HostRecordIsStale(r) || ReadHostRecord(RoleGateway, false) != nil || ReadHostRecord(RoleGateway, true) == nil {
				t.Fatal("a dead or recycled pid record is attachable")
			}
		})
	}
}

func TestUnopenableLockDirIsNotReportedAsAnotherOwner(t *testing.T) {
	dir := hostDir(t)
	blocked := filepath.Join(dir, "blocked")
	os.WriteFile(blocked, []byte("not a directory"), 0o600)
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(blocked, "locks"))
	outcome, err := ClaimHostLock(context.Background(), RoleGateway)
	var errno syscall.Errno
	if outcome != HostLockCouldNotOpen || !errors.As(err, &errno) {
		t.Fatalf("outcome %v err %v", outcome, err)
	}
}

func TestLockCacheFollowsTheLockDirectory(t *testing.T) {
	hostDir(t)
	if o, _ := ClaimHostLock(context.Background(), RoleGateway); o != HostLockAcquired {
		t.Fatal(o)
	}
	moved := filepath.Join(t.TempDir(), "other-locks")
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", moved)
	if OwnsHostLock(RoleGateway) {
		t.Fatal("ownership followed the role, not the lock path")
	}
	if o, _ := ClaimHostLock(context.Background(), RoleGateway); o != HostLockAcquired || !exists(filepath.Join(moved, "host-gateway.lock")) {
		t.Fatal("claim in the moved directory")
	}
	ReleaseHostLock(RoleGateway)
}

func TestRelativeXdgStateHomeIsIgnored(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", "")
	os.Unsetenv("ICEBATH_GATEWAY_LOCK_DIR")
	t.Setenv("XDG_STATE_HOME", "relative/state")
	t.Setenv("HOME", tmp)
	got := GatewayLockDir()
	if !filepath.IsAbs(got) || got != filepath.Join(tmp, ".local", "state", "icebath", locksDirname) {
		t.Fatal(got)
	}
}

func TestMissingIncarnationFieldsDoNotProveALivePid(t *testing.T) {
	dir := hostDir(t)
	if HostLivenessIsProven(gatewayRecord(os.Getpid(), nil, nil, dir)) {
		t.Fatal("a record without createTime or startTime proved a live pid")
	}
}

func TestFreeHostLockMeansAttachDoesNotTreatARecycledPidAsOwner(t *testing.T) {
	dir := hostDir(t)
	child := startHostChild(t, "sleep")
	home := filepath.Join(dir, "home")
	os.Mkdir(home, 0o700)
	writeHostRecord(t, gatewayRecord(child.pid(), HostProcessCreateTime(child.pid()), GetProcessStartTime(child.pid()), home))
	prev := hostIdentifyGateway
	hostIdentifyGateway = func(string, time.Duration) *pyjson.Object { return nil }
	t.Cleanup(func() { hostIdentifyGateway = prev })
	invalidateHostGatewayCache()
	d, err := DecideHostAttach(t.Context(), home, false)
	if err != nil || d.Outcome != HostAttachStart || d.Owner != nil {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestHoldingTheHostLockKeepsAMatchingIncarnationLive(t *testing.T) {
	dir := hostDir(t)
	if o, _ := ClaimHostLock(context.Background(), RoleGateway); o != HostLockAcquired {
		t.Fatal(o)
	}
	r := publishGateway(t, dir)
	if held := HostLockIsHeld(RoleGateway); held == nil || !*held || !HostLivenessIsProven(r) {
		t.Fatal("the lock holder's own record is not live")
	}
}

func TestNullCreateTimeRecordOnARecycledPidStartsInsteadOfRefusingForever(t *testing.T) {
	dir := hostDir(t)
	child := startHostChild(t, "sleep")
	home := filepath.Join(dir, "home")
	os.Mkdir(home, 0o700)
	r := gatewayRecord(child.pid(), nil, nil, home)
	r.UpdatedAt = "2020-01-01T00:00:00+00:00"
	writeHostRecord(t, r)
	prev := hostIdentifyGateway
	hostIdentifyGateway = func(string, time.Duration) *pyjson.Object { return nil }
	t.Cleanup(func() { hostIdentifyGateway = prev })
	invalidateHostGatewayCache()
	if d, err := DecideHostAttach(t.Context(), home, false); err != nil || d.Outcome != HostAttachStart {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestDiscardDeadRecordRetractsALiveLookingPidOnlyOnceTheHostLockIsFree(t *testing.T) {
	dir := hostDir(t)
	child := startHostChild(t, "sleep")
	writeHostRecord(t, gatewayRecord(child.pid(), HostProcessCreateTime(child.pid()), GetProcessStartTime(child.pid()), dir))
	if o, _ := ClaimHostLock(context.Background(), RoleGateway); o != HostLockAcquired {
		t.Fatal(o)
	}
	if DiscardDeadHostRecord(RoleGateway) || !exists(HostRecordPath(RoleGateway)) {
		t.Fatal("record retracted while the lock is held")
	}
	ReleaseHostLock(RoleGateway)
	if !DiscardDeadHostRecord(RoleGateway) || exists(HostRecordPath(RoleGateway)) {
		t.Fatal("record kept after the lock went free")
	}
}
