package gateway

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Hidden entry of the install binary that runs a gateway restart once the old pid exits or the wait runs out
const GatewayDetachedRestartWatcherEntry = "gateway-detached-restart-watcher"

// Runs the detached restart watcher entry and exits when this process was launched as one
func DispatchDetachedRestartWatcherEntry(ctx context.Context) {
	if len(os.Args) > 1 && os.Args[1] == GatewayDetachedRestartWatcherEntry {
		os.Exit(DetachedRestartWatcherMain(ctx, os.Args[2:], os.Stderr))
	}
}

// Waits up to the bound for the pid to exit, then spawns the command detached with its output discarded
func DetachedRestartWatcherMain(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "IndexError: list index out of range")
		return 1
	}
	pid, err := strconv.Atoi(strings.TrimSpace(args[0]))
	if err != nil {
		fmt.Fprintf(stderr, "ValueError: invalid literal for int() with base 10: %s\n", pyrepr.Repr(args[0]))
		return 1
	}
	restartAfterS, err := pyint.ParseFloat(args[1])
	if err != nil {
		fmt.Fprintf(stderr, "ValueError: could not convert string to float: %s\n", pyrepr.Repr(args[1]))
		return 1
	}
	command := args[2:]
	deadline := time.Now().Add(time.Duration(restartAfterS * float64(time.Second)))
	for time.Now().Before(deadline) {
		if !proc.Exists(pid) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(command) == 0 {
		fmt.Fprintln(stderr, "IndexError: list index out of range")
		return 1
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.SysProcAttr = home.DetachAttr(false)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	_ = cmd.Process.Release()
	return 0
}
