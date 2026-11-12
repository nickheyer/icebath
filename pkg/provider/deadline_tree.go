package provider

import (
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/treekill"
)

// KillProcessTree signals a process and every descendant, children first then the group, true when anything was signalled
func KillProcessTree(pid int, sig syscall.Signal) bool { return treekill.KillProcessTree(pid, sig) }
