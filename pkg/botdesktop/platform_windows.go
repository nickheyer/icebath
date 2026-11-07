package botdesktop

import (
	"errors"
	"os"
	"syscall"
)

var errUnsupported = errors.New("Bot Desktop runs on Linux gateway hosts only")

// True when the path exists, Windows grants execute to every readable file
func accessExec(path string) bool { _, err := os.Stat(path); return err == nil }

// Opens the lock file, Windows has no flock and hermes runs the screen on Linux only
func flockPath(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	return func() { f.Close() }, nil
}

func getpgid(pid int) (int, error) { return 0, errUnsupported }

func killpg(pgid int, sig syscall.Signal) error { return errUnsupported }

// Kills pid, the only signal Windows delivers
func killPid(pid int, sig syscall.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

func waitpidNoHang(pid int) {}

func detachSession() {}

func newSessionAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

func isESRCH(err error) bool { return errors.Is(err, os.ErrProcessDone) }

func isEPERM(err error) bool { return errors.Is(err, os.ErrPermission) }
