package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Liveness probe that never signals the target, zombies reported dead
func pidExistsLive(pid int) bool { return proc.Exists(pid) }

// Start time fingerprint from /proc stat field 22, else process birth in centiseconds, nil when unreadable
func processStartTimeLive(pid int) any {
	if data, err := statusProcRead(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if end := bytes.LastIndexByte(data, ')'); end >= 0 {
			if fields := bytes.Fields(data[end+1:]); len(fields) > 19 {
				if n, err := strconv.ParseInt(string(fields[19]), 10, 64); err == nil {
					return n
				}
			}
		}
	}
	birth, err := tools.ProcessBirth(pid)
	if err != nil {
		return nil
	}
	return int64(pyRound(birth * 100))
}

// Whether err is what Python raises as OSError or one of its subclasses
func isOSError(err error) bool {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var sysErr *os.SyscallError
	var errno syscall.Errno
	return errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &sysErr) ||
		errors.As(err, &errno) || errors.Is(err, os.ErrDeadlineExceeded)
}
