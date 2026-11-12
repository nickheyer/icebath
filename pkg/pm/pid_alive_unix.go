//go:build !windows

package pm

import (
	"errors"
	"strings"
	"syscall"
)

// Reports pid answering a null signal, another user's process live and an unreaped zombie dead
func pidAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return errors.Is(err, syscall.EPERM)
	}
	state, known := processState(pid)
	return !known || !strings.HasPrefix(strings.ToUpper(state), "Z")
}
