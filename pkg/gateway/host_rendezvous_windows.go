//go:build windows

package gateway

import (
	"os"
	"os/signal"
	"syscall"
)

// Records live under the user's own state root on Windows, where mode bits set no ACLs
func hostRecordIsOwn(string) bool { return true }

func tightenHostStateDir(string) {}

var hostSignalInstalled bool

// Clears every registered role on a console close, logoff or shutdown before the OS ends the process
func installHostSignalCleanup() {
	if hostSignalInstalled {
		return
	}
	hostSignalInstalled = true
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	go func() {
		<-ch
		HostRendezvousAtExit()
		signal.Stop(ch)
	}()
}
