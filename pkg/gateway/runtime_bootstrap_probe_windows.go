//go:build windows

package gateway

import (
	"os"

	"github.com/nickheyer/icebath/pkg/home"
	"golang.org/x/sys/windows"
)

// Logon session of pid, nil when it cannot be read
func sessionOf(pid uint32) *uint32 {
	var session uint32
	if err := windows.ProcessIdToSessionId(pid, &session); err != nil {
		return nil
	}
	return &session
}

// Reason code and remedy naming why pid's gateway refused this client, ok false when no boundary is proven
func probeRefusedOwner(pid int) (reason, remedy string, ok bool, err error) {
	ownSession, ownerSession := sessionOf(uint32(os.Getpid())), sessionOf(uint32(pid))
	target := uint32(pid)
	var ownerElevated *bool
	if elevated, known := home.ProcessElevation(&target); known {
		ownerElevated = &elevated
	}
	reason = ClassifyRefusedOwner(ownSession, ownerSession, home.ProcessIsElevated(), ownerElevated)
	if reason == "" {
		return "", "", false, nil
	}
	return reason, RefusedOwnerRemedy(reason, pid, ownerSession), true, nil
}
