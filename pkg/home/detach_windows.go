package home

import "syscall"

const (
	createNewProcessGroup  = 0x00000200
	createNoWindow         = 0x08000000
	createBreakawayFromJob = 0x01000000
)

// New process group with a hidden console, breaking away from the launcher job when allowed
func detachAttr(breakaway bool) *syscall.SysProcAttr {
	flags := uint32(createNewProcessGroup | createNoWindow)
	if breakaway {
		flags |= createBreakawayFromJob
	}
	return &syscall.SysProcAttr{CreationFlags: flags}
}

// DetachAttr is the detached child attribute set, a hidden console process group on Windows
func DetachAttr(breakaway bool) *syscall.SysProcAttr { return detachAttr(breakaway) }
