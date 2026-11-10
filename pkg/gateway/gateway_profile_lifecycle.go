package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
)

// ReportProfileStopFailure prints absence when proven, else the unconfirmed stop error and host hint with exit code 1
func ReportProfileStopFailure(ctx context.Context, w io.Writer) int {
	absent := false
	if _, running, err := statusGetRunningPid("", true); err != nil {
		slog.Debug(fmt.Sprintf("Could not confirm gateway absence after stop: %v", err))
	} else if !running {
		pids, err := FindGatewayPIDsFor(ctx, nil, false)
		if err != nil {
			slog.Debug(fmt.Sprintf("Could not confirm gateway absence after stop: %v", err))
		}
		absent = err == nil && len(pids) == 0
	}
	if absent {
		fmt.Fprintln(w, "✗ No gateway running for this profile")
		return 0
	}
	serviceConsole(w).PrintError("Gateway stop was not confirmed; a gateway may still be running. " +
		"Check gateway status and process ownership.")
	printHostHint(w)
	return 1
}

// Names the refused pid when it is the host gateway serving this profile
func printHostHint(w io.Writer) {
	pid, running, err := statusGetRunningPid("", true)
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not read the host record for the stop hint: %v", err))
		return
	}
	record := hostReadRecord(roleGateway, false)
	if running && record != nil && record.PID == pid {
		fmt.Fprintf(w, "  PID %d is the host gateway serving this profile; stopping it would take every "+
			"served profile offline. Stop the host instead: icebath gateway stop --all\n", pid)
	}
}
