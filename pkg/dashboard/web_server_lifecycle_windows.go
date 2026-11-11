//go:build windows

package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
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

var (
	signalSubsMu sync.Mutex
	signalSubs   = map[chan<- os.Signal][]os.Signal{}
)

// Registers a channel for process signals and for signals this process raises itself
func notifySignals(ch chan<- os.Signal, sigs ...os.Signal) {
	signalSubsMu.Lock()
	signalSubs[ch] = append(signalSubs[ch], sigs...)
	signalSubsMu.Unlock()
	signal.Notify(ch, sigs...)
}

// Unregisters a channel from process and raised signals
func stopSignals(ch chan<- os.Signal) {
	signalSubsMu.Lock()
	delete(signalSubs, ch)
	signalSubsMu.Unlock()
	signal.Stop(ch)
}

// Runs the handlers registered for the signal in this process, the C runtime default exit when none is
func raiseSignal(sig syscall.Signal) {
	signalSubsMu.Lock()
	var targets []chan<- os.Signal
	for ch, sigs := range signalSubs {
		for _, s := range sigs {
			if s == sig {
				targets = append(targets, ch)
				break
			}
		}
	}
	signalSubsMu.Unlock()
	if len(targets) == 0 {
		os.Exit(3)
	}
	for _, ch := range targets {
		select {
		case ch <- sig:
		default:
		}
	}
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
