//go:build !windows

package tools

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"golang.org/x/sys/unix"
)

// Child seen by the bridge, the ptyprocess surface close and the liveness checks use
type ptyBridgeChild interface {
	Pid() int
	IsAlive() bool
	Kill(sig syscall.Signal) error
	CloseForce() error
}

type ptyProcessChild struct{ p *ptyProcess }

func (c ptyProcessChild) Pid() int      { return c.p.Pid() }
func (c ptyProcessChild) IsAlive() bool { return c.p.IsAlive() }
func (c ptyProcessChild) Kill(sig syscall.Signal) error {
	c.p.kill(sig)
	return nil
}
func (c ptyProcessChild) CloseForce() error { return c.p.Close() }

// Group signal seam, the os.killpg call
var ptyKillpg = func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }

// PtyBridge streams bytes between a child behind a pseudo terminal and the dashboard terminal
type PtyBridge struct {
	child   ptyBridgeChild
	master  *os.File
	pgid    int
	hasPgid bool
	closed  atomic.Bool
	fdLock  sync.Mutex
	// One non blocking write of p to the master, the os.write seam
	sysWrite func(p []byte) (int, error)
	// Read of up to len(p) bytes waiting at most timeout, the waiter plus os.read seam
	sysRead func(p []byte, timeout time.Duration) (int, error)
	// Waits until the master accepts input, false on timeout, close or cancel
	waitWritable func(ctx context.Context, timeout time.Duration) bool
}

// Reports whether a process group still has a member
func ptyProcessGroupExists(pgid int) bool {
	return ptyKillpg(pgid, 0) == nil
}

// Descendant snapshot entry identified by pid and birth time like a psutil Process
type ptyDescendant struct {
	pid   int
	birth float64
}

// Alive when the pid still names the same process and it is not a zombie, never failing
func ptyPsutilAlive(d ptyDescendant) bool {
	if !proc.Exists(d.pid) {
		return false
	}
	if b, err := proc.Birth(d.pid); err != nil || b != d.birth {
		return false
	}
	return !proc.Zombie(d.pid)
}

// Recursive children of pid with their birth times
func ptyDescendants(pid int) []ptyDescendant {
	table, err := proc.Snapshot()
	if err != nil {
		return nil
	}
	var out []ptyDescendant
	for _, k := range table.Descendants(pid) {
		if b, err := proc.Birth(k); err == nil {
			out = append(out, ptyDescendant{pid: k, birth: b})
		}
	}
	return out
}

// Reports whether a pseudo terminal can be spawned on this platform
func PtyBridgeAvailable() bool { return true }

// Spawns argv behind a new pseudo terminal marked as dashboard hosted
func SpawnPtyBridge(ctx context.Context, env *Env, argv []string, o PtySpawnOptions) (TerminalBridge, error) {
	if len(argv) == 0 {
		return nil, &os.PathError{Op: "exec", Path: "", Err: os.ErrNotExist}
	}
	var spawnEnv map[string]string
	if o.Env == nil {
		built, err := BuildSubprocessEnv(ctx, env, SubprocessEnvOptions{ScrubSecrets: false, InheritProfileHome: false})
		if err != nil {
			return nil, err
		}
		spawnEnv = built
	} else {
		spawnEnv = make(map[string]string, len(o.Env)+2)
		for k, v := range o.Env {
			spawnEnv[k] = v
		}
	}
	if spawnEnv["TERM"] == "" {
		spawnEnv["TERM"] = "xterm-256color"
	}
	spawnEnv[PtyHostEnv] = PtyHostDashboard
	cols, rows := o.Cols, o.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	p, err := spawnPty(argv, o.Cwd, spawnEnv, rows, cols)
	if err != nil {
		return nil, err
	}
	b, err := newPtyBridge(p)
	if err != nil {
		p.Close()
		return nil, err
	}
	return b, nil
}

// Wraps a spawned child, switching the master to a non blocking poller registered descriptor
func newPtyBridge(p *ptyProcess) (*PtyBridge, error) {
	fd, err := unix.FcntlInt(p.master.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	old := p.master
	p.master = os.NewFile(uintptr(fd), old.Name())
	old.Close()
	b := &PtyBridge{child: ptyProcessChild{p}, master: p.master}
	if pgid, err := syscall.Getpgid(p.Pid()); err == nil {
		b.pgid, b.hasPgid = pgid, true
	}
	b.sysWrite = b.masterWrite
	b.sysRead = b.masterRead
	b.waitWritable = b.masterWaitWritable
	return b, nil
}

func (b *PtyBridge) masterWrite(p []byte) (int, error) {
	rc, err := b.master.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n int
	var werr error
	if cerr := rc.Control(func(fd uintptr) { n, werr = unix.Write(int(fd), p) }); cerr != nil {
		return 0, cerr
	}
	if n < 0 {
		n = 0
	}
	return n, werr
}

func (b *PtyBridge) masterRead(p []byte, timeout time.Duration) (int, error) {
	if timeout <= 0 {
		rc, err := b.master.SyscallConn()
		if err != nil {
			return 0, err
		}
		var n int
		var rerr error
		if cerr := rc.Control(func(fd uintptr) { n, rerr = unix.Read(int(fd), p) }); cerr != nil {
			return 0, cerr
		}
		if n < 0 {
			n = 0
		}
		return n, rerr
	}
	if err := b.master.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}
	return b.master.Read(p)
}

func (b *PtyBridge) masterWaitWritable(ctx context.Context, timeout time.Duration) bool {
	if b.closed.Load() || (timeout != PtyNoTimeout && timeout <= 0) {
		return false
	}
	var deadline time.Time
	if timeout != PtyNoTimeout {
		deadline = time.Now().Add(timeout)
	}
	if err := b.master.SetWriteDeadline(deadline); err != nil {
		return false
	}
	stop := context.AfterFunc(ctx, func() { b.master.SetWriteDeadline(time.Now()) })
	defer stop()
	rc, err := b.master.SyscallConn()
	if err != nil {
		return false
	}
	first := true
	err = rc.Write(func(uintptr) bool {
		if first {
			first = false
			return false
		}
		return true
	})
	return err == nil && ctx.Err() == nil && !b.closed.Load()
}

func (b *PtyBridge) Pid() int { return b.child.Pid() }

func (b *PtyBridge) IsAlive() bool { return !b.closed.Load() && b.child.IsAlive() }

// Reads up to 64 KiB within timeout, empty when nothing arrived and nil at end of file or after close
func (b *PtyBridge) Read(timeout time.Duration) ([]byte, error) {
	if b.closed.Load() || b.sysRead == nil {
		return nil, nil
	}
	buf := make([]byte, 65536)
	n, err := b.sysRead(buf, timeout)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, syscall.EAGAIN), errors.Is(err, syscall.EWOULDBLOCK):
		return []byte{}, nil
	case errors.Is(err, syscall.EIO), errors.Is(err, syscall.EBADF), errors.Is(err, os.ErrClosed), errors.Is(err, io.EOF):
		return nil, nil
	default:
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	return buf[:n], nil
}

// One non blocking write, zero while the child buffer is full and false once closed
func (b *PtyBridge) writeSome(view []byte) (int, bool, error) {
	b.fdLock.Lock()
	defer b.fdLock.Unlock()
	if b.closed.Load() {
		return 0, false, nil
	}
	n, err := b.sysWrite(view)
	switch {
	case err == nil:
		return n, true, nil
	case errors.Is(err, syscall.EIO), errors.Is(err, syscall.EBADF), errors.Is(err, syscall.EPIPE), errors.Is(err, os.ErrClosed):
		return 0, false, nil
	case errors.Is(err, syscall.EAGAIN), errors.Is(err, syscall.EWOULDBLOCK):
		return 0, true, nil
	}
	return 0, false, err
}

// Writes every byte without blocking other goroutines, false when closed or input stays full past timeout
func (b *PtyBridge) Write(ctx context.Context, data []byte, timeout time.Duration, fence PtyWriteFence) (bool, error) {
	if b.closed.Load() {
		return false, nil
	}
	if len(data) == 0 {
		return true, nil
	}
	var deadline time.Time
	if timeout != PtyNoTimeout {
		deadline = time.Now().Add(max(0, timeout))
	}
	view := data
	for len(view) > 0 {
		if ctx.Err() != nil {
			return false, nil
		}
		chunk := view
		var n int
		var open bool
		var err error
		if fence == nil {
			n, open, err = b.writeSome(chunk)
		} else {
			n, open, err = fence(func() (int, bool, error) { return b.writeSome(chunk) })
		}
		if err != nil {
			return false, err
		}
		if !open {
			return false, nil
		}
		if n > 0 {
			view = view[n:]
			continue
		}
		remaining := PtyNoTimeout
		if timeout != PtyNoTimeout {
			remaining = time.Until(deadline)
			if remaining <= 0 {
				return false, nil
			}
		}
		if !b.waitWritable(ctx, remaining) {
			return false, nil
		}
	}
	return true, nil
}

// Forwards a clamped terminal resize to the child, ignoring a failing ioctl
func (b *PtyBridge) Resize(cols, rows int) {
	if b.closed.Load() || b.master == nil {
		return
	}
	ws := &unix.Winsize{Row: uint16(clampDimension(rows, ptyMaxRows)), Col: uint16(clampDimension(cols, ptyMaxCols))}
	if rc, err := b.master.SyscallConn(); err == nil {
		rc.Control(func(fd uintptr) { unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, ws) })
	}
}

// Child columns and rows, false once the terminal is closed or the query fails
func (b *PtyBridge) Size() (int, int, bool) {
	if b.closed.Load() || b.master == nil {
		return 0, 0, false
	}
	rc, err := b.master.SyscallConn()
	if err != nil {
		return 0, 0, false
	}
	var ws *unix.Winsize
	var werr error
	if cerr := rc.Control(func(fd uintptr) { ws, werr = unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ) }); cerr != nil || werr != nil {
		return 0, 0, false
	}
	return int(ws.Col), int(ws.Row), true
}

// Drains pending output for up to timeout, sleeping when nothing was read so a dead descriptor never spins
func (b *PtyBridge) discardOutput(timeout time.Duration) {
	if b.sysRead != nil && b.master != nil {
		buf := make([]byte, 65536)
		if n, err := b.sysRead(buf, timeout); err == nil && n > 0 {
			return
		}
	}
	time.Sleep(timeout)
}

func (b *PtyBridge) waitForGroupExit(pgid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for ptyProcessGroupExists(pgid) && time.Now().Before(deadline) {
		b.discardOutput(20 * time.Millisecond)
	}
}

// Ends the child process group HUP then TERM then KILL, sweeps surviving helpers, reaps and closes
func (b *PtyBridge) Close() error {
	b.fdLock.Lock()
	if b.closed.Load() {
		b.fdLock.Unlock()
		return nil
	}
	b.closed.Store(true)
	b.fdLock.Unlock()
	pgid, hasPgid := b.pgid, b.hasPgid
	leaderWasAlive := b.child.IsAlive()
	var nonLeader []ptyDescendant
	if hasPgid && pgid != b.child.Pid() {
		if leaderWasAlive {
			nonLeader = ptyDescendants(b.child.Pid())
		}
		hasPgid = false
	}
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM, syscall.SIGKILL} {
		if !b.child.IsAlive() {
			break
		}
		if hasPgid {
			ptyKillpg(pgid, sig)
		} else {
			b.child.Kill(sig)
		}
		deadline := time.Now().Add(500 * time.Millisecond)
		for b.child.IsAlive() && time.Now().Before(deadline) {
			b.discardOutput(20 * time.Millisecond)
		}
	}
	grace := time.Duration(ptyHelperShutdownGrace() * float64(time.Second))
	if hasPgid {
		sweep := []syscall.Signal{syscall.SIGHUP, syscall.SIGKILL}
		if leaderWasAlive {
			sweep = []syscall.Signal{syscall.SIGKILL}
			b.waitForGroupExit(pgid, grace)
		}
		for _, sig := range sweep {
			if ptyKillpg(pgid, sig) != nil {
				break
			}
			if sig == syscall.SIGHUP {
				b.waitForGroupExit(pgid, grace)
			} else {
				b.waitForGroupExit(pgid, 500*time.Millisecond)
			}
		}
	} else if len(nonLeader) > 0 {
		for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGKILL} {
			for _, d := range nonLeader {
				if ptyPsutilAlive(d) {
					syscall.Kill(d.pid, sig)
				}
			}
			if sig == syscall.SIGHUP {
				deadline := time.Now().Add(grace)
				for time.Now().Before(deadline) && ptyAnyAlive(nonLeader) {
					b.discardOutput(20 * time.Millisecond)
				}
			}
		}
	}
	b.child.CloseForce()
	return nil
}

func ptyAnyAlive(ds []ptyDescendant) bool {
	for _, d := range ds {
		if ptyPsutilAlive(d) {
			return true
		}
	}
	return false
}
