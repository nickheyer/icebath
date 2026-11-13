//go:build windows

package connections

import (
	"errors"
	"os"
	"syscall"
)

// Exit code of a finished child, Windows has no signal deaths
func processExit(state *os.ProcessState) (*int, string) {
	if state == nil {
		return nil, ""
	}
	code := state.ExitCode()
	return &code, ""
}

// TerminateProcess the way Node child.kill ends a Windows child
func terminateChild(p *os.Process) error {
	return p.Kill()
}

var windowsErrnoCodes = map[syscall.Errno]string{
	syscall.ERROR_FILE_NOT_FOUND: "ENOENT",
	syscall.ERROR_PATH_NOT_FOUND: "ENOENT",
	syscall.ERROR_ACCESS_DENIED:  "EPERM",
	syscall.Errno(32):            "EBUSY",
	syscall.ERROR_DIR_NOT_EMPTY:  "ENOTEMPTY",
	syscall.ERROR_ALREADY_EXISTS: "EEXIST",
	syscall.ERROR_FILE_EXISTS:    "EEXIST",
}

// Node style errno code libuv maps a Windows error to
func errnoCode(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if code, ok := windowsErrnoCodes[errno]; ok {
			return code
		}
	}
	return "UNKNOWN"
}

// Windows has no AF_UNIX path budget, the short control dir is never used
func checkShortControlParent() error { return nil }

// Windows control dirs take the inherited ACL as Node leaves them
func secureControlDir(string) error { return nil }
