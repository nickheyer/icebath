//go:build !windows

package terminal

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// Opens the path with O_NOFOLLOW, so a link planted there fails
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
}

// Owner of the open file and whether it is this user
func owner(f *os.File) (string, bool, error) {
	info, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false, errors.New("fstat returned no owner")
	}
	return strconv.FormatUint(uint64(st.Uid), 10), int(st.Uid) == os.Getuid(), nil
}

// Starts the daemon in a session of its own, so it outlives the terminal that started it
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func terminate(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}

// Whether a process of the pid is there, one of another user counting
func exists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Sends the process a terminate, or a kill
func signal(pid int, kill bool) error {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(pid, sig)
}

// Whether a signal failed for the process being gone
func gone(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}
