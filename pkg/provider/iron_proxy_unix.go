//go:build !windows

package provider

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// Opens the path with O_NOFOLLOW so a planted symlink fails with ELOOP
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
}

// Owner uid of the open file and whether it equals the current uid
func fdOwner(f *os.File) (string, bool, error) {
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

// Starts the daemon in a new session so it outlives the terminal that launched it
func detachDaemon(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// Sends SIGTERM like Popen.terminate on POSIX
func terminateChild(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
