package state

import "github.com/nickheyer/icebath/pkg/internal/proc"

// psutil.pid_exists through the process handle probe
func psutilPIDExists(pid int) bool {
	if pid < 0 {
		return false
	}
	return pid == 0 || proc.Exists(pid)
}
