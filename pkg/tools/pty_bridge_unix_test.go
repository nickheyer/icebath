//go:build !windows

package tools

import (
	"bufio"
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func ptbEnv() map[string]string {
	return map[string]string{"PATH": os.Getenv("PATH"), "HOME": os.Getenv("HOME")}
}

func ptbSpawn(t *testing.T, argv []string, o PtySpawnOptions) *PtyBridge {
	t.Helper()
	if o.Env == nil {
		o.Env = ptbEnv()
	}
	tb, err := SpawnPtyBridge(context.Background(), nil, argv, o)
	if err != nil {
		t.Fatal(err)
	}
	return tb.(*PtyBridge)
}

func ptbReadUntil(b *PtyBridge, needle []byte, timeout time.Duration) []byte {
	deadline := time.Now().Add(timeout)
	var buf []byte
	for time.Now().Before(deadline) {
		chunk, _ := b.Read(200 * time.Millisecond)
		if chunk == nil {
			break
		}
		buf = append(buf, chunk...)
		if bytes.Contains(buf, needle) {
			break
		}
	}
	return buf
}

func ptbGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func TestPtyBridgeSpawnRaisesOnMissingArgv0(t *testing.T) {
	if _, err := SpawnPtyBridge(context.Background(), nil, []string{filepath.Join(t.TempDir(), "definitely-not-a-real-binary")}, PtySpawnOptions{Env: ptbEnv()}); err == nil {
		t.Fatal("missing binary spawned")
	}
}

func TestPtyBridgeSpawnMarksChildAsDashboardHosted(t *testing.T) {
	b := ptbSpawn(t, []string{"/bin/sh", "-c", `printf "%s" "$` + PtyHostEnv + `"`}, PtySpawnOptions{})
	defer b.Close()
	if out := ptbReadUntil(b, []byte(PtyHostDashboard), 5*time.Second); !bytes.Contains(out, []byte(PtyHostDashboard)) {
		t.Fatalf("output %q", out)
	}
}

func TestPtyBridgeWriteSendsToChildStdin(t *testing.T) {
	b := ptbSpawn(t, []string{"cat"}, PtySpawnOptions{})
	defer b.Close()
	ok, err := b.Write(context.Background(), []byte("hello-pty\n"), PtyWriteTimeout, nil)
	if !ok || err != nil {
		t.Fatalf("write %v %v", ok, err)
	}
	if out := ptbReadUntil(b, []byte("hello-pty"), 5*time.Second); !bytes.Contains(out, []byte("hello-pty")) {
		t.Fatalf("output %q", out)
	}
}

func TestPtyBridgeWriteYieldsWhileInputIsBackpressured(t *testing.T) {
	b := &PtyBridge{}
	waitStarted, release := make(chan struct{}), make(chan struct{})
	calls := 0
	b.sysWrite = func(p []byte) (int, error) {
		calls++
		if calls == 1 {
			return 0, syscall.EAGAIN
		}
		return len(p), nil
	}
	b.waitWritable = func(ctx context.Context, timeout time.Duration) bool {
		if timeout <= 0 {
			t.Errorf("timeout %v", timeout)
		}
		close(waitStarted)
		<-release
		return true
	}
	result := make(chan bool)
	go func() {
		ok, _ := b.Write(context.Background(), []byte("queued input"), PtyWriteTimeout, nil)
		result <- ok
	}()
	<-waitStarted
	heartbeat := make(chan bool)
	go func() { heartbeat <- true }()
	if !<-heartbeat {
		t.Fatal("heartbeat starved")
	}
	close(release)
	if !<-result || calls != 2 {
		t.Fatalf("calls %d", calls)
	}
}

func TestPtyBridgeWriteReportsSustainedBackpressure(t *testing.T) {
	b := &PtyBridge{}
	b.sysWrite = func([]byte) (int, error) { return 0, syscall.EAGAIN }
	b.waitWritable = func(context.Context, time.Duration) bool { return false }
	if ok, err := b.Write(context.Background(), []byte("queued input"), PtyWriteTimeout, nil); ok || err != nil {
		t.Fatalf("backpressure %v %v", ok, err)
	}
}

func TestPtyBridgeWriteRunsEveryAttemptInsideTheFence(t *testing.T) {
	b := &PtyBridge{}
	var mu sync.Mutex
	held := 0
	b.sysWrite = func(p []byte) (int, error) {
		if !mu.TryLock() {
			mu.Unlock()
		} else {
			t.Error("write ran outside the fence")
			mu.Unlock()
		}
		return 1, nil
	}
	fence := func(attempt PtyWriteAttempt) (int, bool, error) {
		mu.Lock()
		held++
		return attempt()
	}
	if ok, err := b.Write(context.Background(), []byte("abc"), PtyWriteTimeout, fence); !ok || err != nil || held != 3 {
		t.Fatalf("fence %v %v %d", ok, err, held)
	}
}

func TestPtyBridgeReadTreatsNonblockingReadRaceAsIdle(t *testing.T) {
	b := &PtyBridge{}
	b.sysRead = func([]byte, time.Duration) (int, error) { return 0, syscall.EAGAIN }
	if got, err := b.Read(10 * time.Millisecond); got == nil || len(got) != 0 || err != nil {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestPtyBridgeReadReturnsNoneAfterChildExits(t *testing.T) {
	b := ptbSpawn(t, []string{"/bin/sh", "-c", "printf done"}, PtySpawnOptions{})
	defer b.Close()
	ptbReadUntil(b, []byte("done"), 5*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for b.IsAlive() && time.Now().Before(deadline) {
		b.Read(100 * time.Millisecond)
	}
	for i := 0; i < 10; i++ {
		if got, _ := b.Read(100 * time.Millisecond); got == nil {
			return
		}
	}
	t.Fatal("read did not return nil after child EOF")
}

func TestPtyBridgeResizeUpdatesChildWinsize(t *testing.T) {
	b := ptbSpawn(t, []string{"/bin/sh", "-c", "sleep 0.3; stty size"}, PtySpawnOptions{Cols: 80, Rows: 24})
	defer b.Close()
	b.Resize(123, 45)
	if cols, rows, ok := b.Size(); !ok || cols != 123 || rows != 45 {
		t.Fatalf("size %d %d %v", cols, rows, ok)
	}
	if out := ptbReadUntil(b, []byte("45 123"), 5*time.Second); !bytes.Contains(out, []byte("45 123")) {
		t.Fatalf("output %q", out)
	}
}

func TestClampDimension(t *testing.T) {
	if clampDimension(131072, ptyMaxCols) != ptyMaxCols || clampDimension(131072, ptyMaxRows) != ptyMaxRows {
		t.Fatal("above max")
	}
	for _, v := range []any{nil, math.NaN(), math.Inf(1), "abc"} {
		if clampDimension(v, ptyMaxCols) != 1 {
			t.Fatalf("%v", v)
		}
	}
	if clampDimension(" 12 ", ptyMaxCols) != 12 || clampDimension(-5, ptyMaxCols) != 1 || clampDimension(37.9, ptyMaxCols) != 37 {
		t.Fatal("int coercion")
	}
	cols, rows := clampDimension(131072, ptyMaxCols), clampDimension(1, ptyMaxRows)
	if cols > math.MaxUint16 || rows > math.MaxUint16 {
		t.Fatal("does not pack as unsigned short")
	}
}

func TestPtyBridgeCloseIsIdempotent(t *testing.T) {
	b := ptbSpawn(t, []string{"/bin/sh", "-c", "sleep 30"}, PtySpawnOptions{})
	b.Close()
	b.Close()
	if b.IsAlive() {
		t.Fatal("alive after close")
	}
}

func TestPtyBridgeCloseTerminatesLongRunningChild(t *testing.T) {
	b := ptbSpawn(t, []string{"/bin/sh", "-c", "sleep 30"}, PtySpawnOptions{})
	pid := b.Pid()
	b.Close()
	if !ptbGone(pid, 3*time.Second) {
		t.Fatalf("pid %d still running after close", pid)
	}
}

type ptbFakeChild struct {
	pid    int
	alive  bool
	kills  []syscall.Signal
	onKill func(sig syscall.Signal)
	closed bool
}

func (f *ptbFakeChild) Pid() int      { return f.pid }
func (f *ptbFakeChild) IsAlive() bool { return f.alive }
func (f *ptbFakeChild) Kill(sig syscall.Signal) error {
	f.kills = append(f.kills, sig)
	if f.onKill != nil {
		f.onKill(sig)
	}
	return nil
}
func (f *ptbFakeChild) CloseForce() error { f.closed = true; return nil }

func ptbStubKillpg(t *testing.T, fn func(pgid int, sig syscall.Signal) error) {
	prev := ptyKillpg
	ptyKillpg = fn
	t.Cleanup(func() { ptyKillpg = prev })
}

func TestPtyBridgeCloseSignalsChildProcessGroup(t *testing.T) {
	fake := &ptbFakeChild{pid: 12345, alive: true}
	fake.onKill = func(sig syscall.Signal) { t.Fatalf("single process kill used %v", sig) }
	type sent struct {
		pgid int
		sig  syscall.Signal
	}
	var got []sent
	ptbStubKillpg(t, func(pgid int, sig syscall.Signal) error {
		if !fake.alive {
			return syscall.ESRCH
		}
		got = append(got, sent{pgid, sig})
		fake.alive = false
		return nil
	})
	b := &PtyBridge{child: fake, pgid: 12345, hasPgid: true}
	b.Close()
	if len(got) != 1 || got[0] != (sent{12345, syscall.SIGHUP}) || !b.closed.Load() || !fake.closed {
		t.Fatalf("sent %v", got)
	}
}

func TestPtyBridgeCloseNeverKillpgsASharedGroup(t *testing.T) {
	fake := &ptbFakeChild{pid: 12345, alive: true}
	fake.onKill = func(syscall.Signal) { fake.alive = false }
	var groupSignals int
	ptbStubKillpg(t, func(int, syscall.Signal) error { groupSignals++; return nil })
	b := &PtyBridge{child: fake, pgid: 67890, hasPgid: true}
	b.Close()
	if groupSignals != 0 || len(fake.kills) != 1 || fake.kills[0] != syscall.SIGHUP || !b.closed.Load() {
		t.Fatalf("group %d direct %v", groupSignals, fake.kills)
	}
}

func TestPtyBridgeCloseEndsHelpersThatOutliveADeadLeader(t *testing.T) {
	b := ptbSpawn(t, []string{"/bin/sh", "-c", "trap '' HUP; sleep 60 & echo helper=$!; exec sleep 60"}, PtySpawnOptions{})
	out := ptbReadUntil(b, []byte("\n"), 5*time.Second)
	parts := strings.SplitN(string(out), "helper=", 2)
	if len(parts) != 2 {
		t.Fatalf("output %q", out)
	}
	helper, err := strconv.Atoi(strings.Fields(parts[1])[0])
	if err != nil {
		t.Fatal(err)
	}
	syscall.Kill(b.Pid(), syscall.SIGKILL)
	deadline := time.Now().Add(3 * time.Second)
	for b.IsAlive() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	b.Close()
	if !ptbGone(helper, 3*time.Second) {
		syscall.Kill(helper, syscall.SIGKILL)
		t.Fatalf("helper pid %d survived close after its leader died", helper)
	}
}

type ptbCmdChild struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (c *ptbCmdChild) Pid() int { return c.cmd.Process.Pid }
func (c *ptbCmdChild) IsAlive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}
func (c *ptbCmdChild) Kill(sig syscall.Signal) error { return c.cmd.Process.Signal(sig) }
func (c *ptbCmdChild) CloseForce() error             { return nil }

func TestPtyBridgeCloseSweepsHelpersOfANonLeaderChild(t *testing.T) {
	script := `/bin/sh -c 'trap "" HUP; while :; do sleep 0.05; done' & echo helper=$!; while :; do sleep 0.05; done`
	cmd := exec.Command("/bin/sh", "-c", script)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &ptbCmdChild{cmd: cmd, done: make(chan struct{})}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || !strings.Contains(line, "helper=") {
		cmd.Process.Kill()
		t.Fatalf("leader line %q %v", line, err)
	}
	helper, _ := strconv.Atoi(strings.TrimSpace(strings.SplitN(line, "helper=", 2)[1]))
	go func() { cmd.Wait(); close(child.done) }()
	pgid, _ := syscall.Getpgid(cmd.Process.Pid)
	b := &PtyBridge{child: child, pgid: pgid, hasPgid: true}
	b.Close()
	if child.IsAlive() {
		cmd.Process.Kill()
	}
	<-child.done
	if !ptbGone(helper, 3*time.Second) {
		syscall.Kill(helper, syscall.SIGKILL)
		t.Fatalf("helper pid %d survived close of its non leader parent", helper)
	}
}

func TestPtyBridgeCloseLetsAHelperFinishItsSighupShutdown(t *testing.T) {
	dir := t.TempDir()
	marker, script := filepath.Join(dir, "saved"), filepath.Join(dir, "helper.sh")
	os.WriteFile(script, []byte("trap 'trap \"\" HUP; sleep 0.6; echo ok > "+marker+"; exit 0' HUP\necho armed\nwhile :; do sleep 0.05; done\n"), 0o644)
	b := ptbSpawn(t, []string{"/bin/sh", "-c", "/bin/sh " + script + " & exec sleep 60"}, PtySpawnOptions{})
	ptbReadUntil(b, []byte("armed"), 10*time.Second)
	b.Close()
	if data, _ := os.ReadFile(marker); strings.TrimSpace(string(data)) != "ok" {
		t.Fatalf("marker %q", data)
	}
}

func TestPtyBridgeCwdIsRespected(t *testing.T) {
	dir := t.TempDir()
	b := ptbSpawn(t, []string{"/bin/sh", "-c", "pwd"}, PtySpawnOptions{Cwd: dir})
	defer b.Close()
	if out := ptbReadUntil(b, []byte(dir), 5*time.Second); !bytes.Contains(out, []byte(dir)) {
		t.Fatalf("output %q", out)
	}
}

func TestPtyBridgeReadsAndWaitsAboveFdSetsize(t *testing.T) {
	var held []*os.File
	defer func() {
		for _, f := range held {
			f.Close()
		}
	}()
	for {
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatalf("fd limit too low to reproduce %v", err)
		}
		held = append(held, f)
		if rc, _ := f.SyscallConn(); rc != nil {
			var fd uintptr
			rc.Control(func(d uintptr) { fd = d })
			if fd >= 1100 {
				break
			}
		}
	}
	b := ptbSpawn(t, []string{"/bin/sh", "-c", "echo pty-high-fd; exec sleep 30"}, PtySpawnOptions{})
	defer b.Close()
	rc, _ := b.master.SyscallConn()
	var fd uintptr
	rc.Control(func(d uintptr) { fd = d })
	if fd < 1024 {
		t.Fatalf("master fd %d", fd)
	}
	var buf []byte
	deadline := time.Now().Add(5 * time.Second)
	for !bytes.Contains(buf, []byte("pty-high-fd")) && time.Now().Before(deadline) {
		chunk, _ := b.Read(200 * time.Millisecond)
		if chunk == nil {
			t.Fatalf("EOF before output %q", buf)
		}
		buf = append(buf, chunk...)
	}
	if !bytes.Contains(buf, []byte("pty-high-fd")) {
		t.Fatalf("output %q", buf)
	}
	b.Read(200 * time.Millisecond)
	started := time.Now()
	if got, _ := b.Read(500 * time.Millisecond); got == nil || len(got) != 0 {
		t.Fatalf("idle read %q", got)
	}
	if time.Since(started) < 300*time.Millisecond {
		t.Fatal("idle read returned without waiting")
	}
}

func TestPlatformPtyBridgeIsThePosixBridge(t *testing.T) {
	if !PtyBridgeAvailable() {
		t.Fatal("posix bridge unavailable")
	}
	b := ptbSpawn(t, []string{"/bin/sh", "-c", "true"}, PtySpawnOptions{})
	b.Close()
	var err error = &PtyUnavailableError{Msg: "x"}
	if _, ok := err.(*PtyUnavailableError); !ok {
		t.Fatal("error type")
	}
}

func TestWinPtySpawnRaisesUnavailableOffWindows(t *testing.T) {
	_, err := SpawnWinPtyBridge(context.Background(), nil, []string{"true"}, PtySpawnOptions{})
	if _, ok := err.(*PtyUnavailableError); !ok || WinPtyBridgeAvailable() || !TerminalBridgeAvailable() {
		t.Fatalf("error %v", err)
	}
}
