package gateway

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// SystemdStopMarkEntry is the hidden argument the unit ExecStop runs to mark a supervisor stop as planned
const SystemdStopMarkEntry = "__icebath_systemd_stop_mark__"

var stopMarkWrite = WritePlannedStopMarker

// Arguments after the binary with a leading home pin applied to an unset ICEBATH_HOME and dropped
func pinnedEntryArgs(args []string) []string {
	if len(args) == 0 || (args[0] != pm.PinDefaultHomeFlag && !strings.HasPrefix(args[0], pm.PinDefaultHomeFlag+"=")) {
		return args
	}
	if value, ok := strings.CutPrefix(args[0], pm.PinDefaultHomeFlag+"="); ok && os.Getenv("ICEBATH_HOME") == "" {
		os.Setenv("ICEBATH_HOME", value)
	}
	return args[1:]
}

// Explicit argv pid or else systemd MAINPID, zero when unknown or not positive
func resolveStopMarkTargetPid(argv []string) int {
	raw, set := "", true
	if len(argv) > 1 && argv[1] != "" {
		raw = argv[1]
	} else {
		raw, set = os.LookupEnv("MAINPID")
	}
	if !set {
		return 0
	}
	n, ok := pystr.IntText(raw)
	if !ok || n.Sign() <= 0 || !n.IsInt64() || n.Int64() != int64(int(n.Int64())) {
		return 0
	}
	return int(n.Int64())
}

// Writes the planned stop marker for the supervised pid, exiting zero whatever happens
func SystemdStopMarkMain(ctx context.Context, argv []string) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	if pid := resolveStopMarkTargetPid(argv); pid > 0 {
		stopMarkWrite(ctx, pid)
	}
	return 0
}

// Runs the stop marker and exits when the process was started with the hidden entry argument
func DispatchSystemdStopMarkEntry(ctx context.Context) {
	args := pinnedEntryArgs(os.Args[1:])
	if len(args) > 0 && args[0] == SystemdStopMarkEntry {
		os.Exit(SystemdStopMarkMain(ctx, args))
	}
}
