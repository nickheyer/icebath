//go:build !windows

package terminal

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/term"
)

// Processes of this host have groups a whole tree is signalled through
const groups = true

const noFollow = syscall.O_NOFOLLOW

// Signal that ended the process, zero when it exited
func signalOf(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return int(status.Signal())
	}
	return 0
}

func brokenPipe(err error) bool { return errors.Is(err, syscall.EPIPE) }

// One chunk read within the timeout: ready false on an idle pipe, an error at its end
func readChunk(f *os.File, buf []byte, timeout time.Duration) (int, bool, error) {
	if err := f.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		n, err := f.Read(buf)
		if n > 0 {
			return n, true, nil
		}
		return 0, false, cmp.Or(err, io.EOF)
	}
	n, err := f.Read(buf)
	if n > 0 {
		return n, true, nil
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return 0, false, nil
	}
	return 0, false, cmp.Or(err, io.EOF)
}

// Starts the command as the leader of a session and group of its own
func NewSession(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

func groupOf(pid int) (int, error) { return syscall.Getpgid(pid) }

// Start time of a process, kernel ticks on Linux else ps's lstart in centiseconds
func StartTime(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		fields := statFields(string(data))
		if len(fields) > 19 {
			if ticks, err := strconv.ParseInt(fields[19], 10, 64); err == nil {
				return ticks, true
			}
		}
		return 0, false
	}
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	text := strings.TrimSpace(string(out))
	if err != nil || text == "" {
		return 0, false
	}
	for _, layout := range []string{"Mon Jan _2 15:04:05 2006", "Mon _2 Jan 15:04:05 2006", "Mon Jan _2 15:04:05 MST 2006"} {
		if t, err := time.ParseInLocation(layout, text, time.Local); err == nil {
			return t.UnixNano() / 1e7, true
		}
	}
	return 0, false
}

// Fields of /proc/<pid>/stat after the command name, the state first
func statFields(stat string) []string {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return nil
	}
	return strings.Fields(stat[end+1:])
}

// Whether the process exists, though it may be a zombie
func Running(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Whether the process exited and waits for its parent to reap it
func zombie(pid int) bool {
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		fields := statFields(string(data))
		return len(fields) > 0 && fields[0] == "Z"
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func terminate(pid int) { signal(pid, syscall.SIGTERM) }
func kill(pid int)      { signal(pid, syscall.SIGKILL) }

func signal(pid int, sig syscall.Signal) {
	if pid > 0 {
		syscall.Kill(pid, sig)
	}
}

// Whether no member of the group is left, an unclear answer keeping it covered; no pid or this process's own group is never one to wait on
func GroupGone(pgid int) bool {
	if pgid <= 0 || pgid == syscall.Getpgrp() {
		return true
	}
	return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

// TERM, or KILL when forced, to the whole group; ErrGone when none of it is left, this process's own group never signalled
func SignalGroup(pgid int, force bool) error {
	if pgid <= 0 || pgid == syscall.Getpgrp() {
		return ErrGone
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	if err := syscall.Kill(-pgid, sig); errors.Is(err, syscall.ESRCH) {
		return ErrGone
	} else {
		return err
	}
}

// No process of the group or pid is left
var ErrGone = errors.New("no such process")

// Whether pid leads a group of its own, never this process's
func leadsGroup(pid int) bool {
	pgid, err := syscall.Getpgid(pid)
	return err == nil && pgid == pid && pgid != syscall.Getpgrp()
}

// Descendant and the start time it had when seen, so a recycled pid is never signalled
type descendant struct {
	pid   int
	start int64
	known bool
}

func snapshot(pid int) []descendant {
	var out []descendant
	for _, d := range term.Descendants(pid) {
		start, ok := StartTime(d)
		out = append(out, descendant{pid: d, start: start, known: ok})
	}
	return out
}

func (d descendant) running() bool {
	if !Running(d.pid) {
		return false
	}
	if !d.known {
		return true
	}
	current, ok := StartTime(d.pid)
	return !ok || SameStart(d.start, current)
}

// Waits until the group is gone, polling the leader so a dead one is reaped
func waitGroupExit(p *HostProc, pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		p.Poll()
		if GroupGone(pgid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// KILLs descendants seen before the kill that left the group through setsid
func sweep(descendants []descendant, pgid int) {
	for _, d := range descendants {
		if !d.running() {
			continue
		}
		if g, err := syscall.Getpgid(d.pid); err == nil && g == pgid {
			continue
		}
		kill(d.pid)
	}
}

// Whether the group's leader is still the process that was started, before the whole group is signalled
func leaderIsOurs(pgid int, start int64) bool {
	if pgid <= 0 {
		return false
	}
	if start == 0 {
		return true
	}
	current, ok := StartTime(pgid)
	return !ok || SameStart(start, current)
}

func killKnown(p *HostProc, descendants []descendant) {
	p.Kill()
	for _, d := range descendants {
		if d.running() {
			kill(d.pid)
		}
	}
}

// TERM to the group, a second, KILL, then a sweep of setsid escapees, a recycled pid never signalled
func killTree(p *HostProc) error {
	pid := p.Pid()
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		if p.pgid == 0 {
			return err
		}
		pgid = p.pgid
	}
	if !leaderIsOurs(pgid, p.pgidStart) {
		sweep(snapshot(pid), pgid)
		return nil
	}
	descendants := snapshot(pid)
	if pgid == syscall.Getpgrp() {
		killKnown(p, descendants)
	} else {
		switch err := syscall.Kill(-pgid, syscall.SIGTERM); {
		case err == nil:
			if !waitGroupExit(p, pgid, time.Second) {
				if !leaderIsOurs(pgid, p.pgidStart) {
					return nil
				}
				syscall.Kill(-pgid, syscall.SIGKILL)
				waitGroupExit(p, pgid, 2*time.Second)
				p.Wait(200 * time.Millisecond)
			}
		case errors.Is(err, syscall.ESRCH):
		case errors.Is(err, syscall.EPERM):
			killKnown(p, descendants)
		default:
			return err
		}
	}
	sweep(descendants, pgid)
	return nil
}

// KILL to the whole group with no grace and no wait
func forceKillTree(p *HostProc) {
	pgid := p.pgid
	if pgid == 0 {
		if g, err := syscall.Getpgid(p.Pid()); err == nil {
			pgid = g
		}
	}
	if pgid != 0 && pgid != syscall.Getpgrp() && leaderIsOurs(pgid, p.pgidStart) {
		syscall.Kill(-pgid, syscall.SIGKILL)
	}
	p.Kill()
}

// Inode and change time of the file at path
func fileIdentity(path string) (uint64, int64) {
	var st unix.Stat_t
	if unix.Stat(path, &st) != nil {
		return 0, 0
	}
	return uint64(st.Ino), st.Ctim.Nano()
}

func linkCount(st os.FileInfo) uint64 {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(sys.Nlink)
	}
	return 1
}

// Whether this process may enter the folder
func Searchable(path string) bool { return unix.Access(path, unix.X_OK) == nil }

// Whether this process may write in and enter the folder
func Writable(path string) bool { return unix.Access(path, unix.W_OK|unix.X_OK) == nil }

// Whether this host has pseudo terminals
func ptyAvailable() bool {
	_, err := os.Stat("/dev/ptmx")
	return err == nil
}
