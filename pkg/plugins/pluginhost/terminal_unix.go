//go:build !windows

package pluginhost

import (
	"strconv"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
)

// File descriptor the parent passed for one standard stream
func (rt *runtime) terminalFD(_ py, _ int, raw string) any {
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		throw(&pyhost.Exception{Type: "RuntimeError", Message: "the plugin host terminal stream " + raw + " is not a file descriptor"})
	}
	return fd
}

// Standard streams follow the descriptors on this platform, so nothing besides dup2 moves
func swapStdHandles([]string) func() { return func() {} }
