//go:build !windows

package tools

import (
	"errors"
	"math"
	"math/big"
	"syscall"
)

// Version resources exist only on Windows
func platformPEVersion(path string) (observation[string], error) {
	return observation[string]{State: checkUnavailable, Detail: "pe_resource needs Windows"}, nil
}

// Uninstall entries exist only in the Windows registry
func platformUninstallEntries(prefix string) []map[string]string { return nil }

// pyOSError is an OSError a POSIX call raised, carrying its Python class name
type pyOSError struct {
	class string
	err   error
}

func (e *pyOSError) Error() string   { return e.err.Error() }
func (e *pyOSError) PyClass() string { return e.class }

// Signal zero liveness of a runtime file pid, permission denied meaning the process exists
func platformPIDAlive(pid *big.Int) (observation[bool], error) {
	if pid == nil || pid.Sign() <= 0 {
		return observation[bool]{State: checkUnavailable, Detail: "no pid in runtime file"}, nil
	}
	if !pid.IsInt64() || pid.Int64() > math.MaxInt32 {
		return observation[bool]{}, &pyOSError{class: "OverflowError", err: errors.New("signed integer is greater than maximum")}
	}
	t, f := true, false
	switch err := syscall.Kill(int(pid.Int64()), 0); {
	case err == nil, errors.Is(err, syscall.EPERM):
		return observation[bool]{State: checkPresent, Value: &t}, nil
	case errors.Is(err, syscall.ESRCH):
		return observation[bool]{State: checkAbsent, Value: &f}, nil
	default:
		return observation[bool]{}, &pyOSError{class: "OSError", err: err}
	}
}
