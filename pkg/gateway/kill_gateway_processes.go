package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

var (
	killFindGatewayPIDs = FindGatewayPIDsFor
	killCaptureArgv     = captureGatewayArgv
)

// KillGatewayProcesses signals the running gateways, force kills only after a live argv recheck, and counts the kills
func KillGatewayProcesses(ctx context.Context, w io.Writer, force bool, exclude map[int]bool, allProfiles bool) (int, error) {
	pids, err := killFindGatewayPIDs(ctx, exclude, allProfiles)
	if err != nil {
		return 0, err
	}
	if allProfiles {
		owned, foreign := PartitionGatewayPidsByScope(ctx, pids, nil)
		pids = owned
		for _, f := range foreign {
			where := "an unreadable home"
			if f.Home != "" {
				where = "another Icebath home (" + f.Home + ")"
			}
			fmt.Fprintf(w, "↷ PID %d: gateway of %s — left alone\n", f.PID, where)
		}
	}
	killed := 0
	for _, pid := range pids {
		var expected any
		if force {
			if killCaptureArgv(pid) == nil {
				continue
			}
			expected = GetProcessStartTime(pid)
		}
		err := statusTerminatePid(pid, force, expected)
		switch {
		case err == nil:
			killed++
		case errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH):
		case errors.Is(err, fs.ErrPermission):
			fmt.Fprintf(w, "⚠ Permission denied to kill PID %d\n", pid)
		case errors.Is(err, context.DeadlineExceeded):
			return killed, err
		default:
			fmt.Fprintf(w, "Failed to kill PID %d: %s\n", pid, killErrorText(err))
		}
	}
	return killed, nil
}

// Live argv of a running gateway, nil when the process is gone, denied or not a gateway command
func captureGatewayArgv(pid int) []string {
	if pid <= 1 {
		return nil
	}
	argv, err := proc.Cmdline(pid)
	if err != nil || len(argv) == 0 {
		return nil
	}
	if !LooksLikeGatewayCommandLine(strings.Join(argv, " ")) {
		return nil
	}
	return argv
}

// Python str of the OSError a refused or failed kill raised
func killErrorText(err error) string {
	if errors.Is(err, ErrRefusedKill) {
		return strings.TrimPrefix(err.Error(), ErrRefusedKill.Error()+": ")
	}
	if text, ok := pypath.ErrnoText(err); ok {
		return text
	}
	return err.Error()
}
