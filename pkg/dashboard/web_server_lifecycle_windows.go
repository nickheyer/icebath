//go:build windows

package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

const (
	processQueryLimitedInformation = 0x1000
	filetimeToDotnetTicks          = 504911232000000000
	soExclusiveAddrUse             = ^4
)

// Incarnation marker of a pid from its creation FILETIME, ProcessLookupError when absent
func processStartMarker(pid int) (string, error) {
	handle, err := windows.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) && (errno == 87 || errno == 1168) {
			return "", &ProcessLookupError{PID: pid}
		}
		return "", fmt.Errorf("OpenProcess failed for PID %d: %w", pid, err)
	}
	defer windows.CloseHandle(handle)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return "", fmt.Errorf("GetProcessTimes failed for PID %d: %w", pid, err)
	}
	filetime := uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	return fmt.Sprintf("win:%d", filetime+filetimeToDotnetTicks), nil
}

// Whether an exclusive address bind of host and port fails with address in use
func probeBindConflict(network, host string, port int) bool {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var setErr error
		if err := c.Control(func(fd uintptr) {
			setErr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, soExclusiveAddrUse, 1)
		}); err != nil {
			return err
		}
		return setErr
	}}
	ln, err := lc.Listen(context.Background(), network, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return isAddrInUseError(err)
	}
	ln.Close()
	return false
}
