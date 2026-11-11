package selfupdate

import (
	"context"
	"fmt"
	"io"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var staleSurvivorsLog = updateLog.With("logger", "hermes_cli.update_cmd_stale_survivors")

// Process id of a fleet row when it is a positive integer
func fleetRowPID(row any) (int, bool) {
	if !isRowDict(row) {
		return 0, false
	}
	pid, ok := pyIntValue(rowValue(row, "pid"))
	return int(pid), ok && pid > 0
}

// Live pids the fleet matrix stamped stale that the restart phase never touched
func staleFleetSurvivorPids(fleet []any, alreadySignalled map[int]bool) []int {
	pids := []int{}
	for _, row := range fleet {
		pid, ok := fleetRowPID(row)
		if pyrepr.Equal(rowValue(row, "state"), "stale") && ok && !alreadySignalled[pid] {
			pids = append(pids, pid)
		}
	}
	return pids
}

// Drain first restarts every proven stale gateway and returns the pids signalled, never failing verification
func signalStaleFleetSurvivors(ctx context.Context, w io.Writer, fleet []any, restart *gatewayRestartOutcome, drainBudget float64) []int {
	pids := staleFleetSurvivorPids(fleet, restart.KilledPIDs)
	if len(pids) == 0 {
		return []int{}
	}
	servicePIDs := map[int]bool{}
	if listed, err := gateway.GetServicePIDs(ctx, true); err == nil {
		for _, pid := range listed {
			servicePIDs[pid] = true
		}
	}
	labels := map[int]string{}
	for _, row := range fleet {
		if !isRowDict(row) {
			continue
		}
		pid, ok := pyIntValue(rowValue(row, "pid"))
		if !ok {
			continue
		}
		label := "gateway"
		if profile := rowValue(row, "profile"); pyrepr.Truthy(profile) {
			label = pyrepr.Str(profile)
		}
		labels[int(pid)] = label
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  ⚠ %d gateway process(es) still run the pre-update code — requesting a restart\n", len(pids))
	signalled := []int{}
	var manual []int
	for _, pid := range pids {
		name, ok := labels[pid]
		if !ok {
			name = "gateway"
		}
		label := fmt.Sprintf("%s (PID %d)", name, pid)
		sent, err := drainOrSignalGatewayForUpdate(ctx, w, pid, drainBudget, label)
		if err != nil {
			staleSurvivorsLog.Warn(fmt.Sprintf("Could not signal stale gateway PID %d: %s", pid, launchErrorText(err)))
			fmt.Fprintf(w, "  ⚠ %s: could not be signalled (%s) — restart it by hand\n", label, launchErrorText(err))
			continue
		}
		if sent {
			signalled = append(signalled, pid)
			restart.KilledPIDs[pid] = true
			if !servicePIDs[pid] {
				manual = append(manual, pid)
			}
		}
	}
	if len(manual) > 0 {
		if restart.StoppedUnmappedPIDs != nil {
			for _, pid := range manual {
				restart.StoppedUnmappedPIDs[pid] = true
			}
		}
		fmt.Fprintf(w, "  → Stopped %d manual gateway process(es) that had no supervisor to respawn them\n", len(manual))
		fmt.Fprintln(w, "    Restart manually: icebath gateway run")
		if len(manual) > 1 {
			fmt.Fprintln(w, "    (or: icebath -p <profile> gateway run  for each profile)")
		}
	}
	return signalled
}
