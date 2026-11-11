package selfupdate

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

const (
	systemctlRestartTimeoutMax = (1<<31-1)/1000 - 1
	unitJobHardCapS            = 900.0
	unitJobStillRunning        = 124
)

var (
	unitTransitionalStates = map[string]bool{"activating": true, "deactivating": true, "reloading": true, "refreshing": true}
	restartSecUnits        = []struct {
		suffix string
		mult   float64
	}{{"ms", 0.001}, {"us", 0.000001}, {"min", 60.0}, {"s", 1.0}}
)

// Stop and start timeouts of a unit in seconds, 90 for each one that is unknown or infinite
func unitStopStartBudgets(ctx context.Context, scopeCmd []string, svcName string) (float64, float64, error) {
	budgets := map[string]float64{"TimeoutStopUSec": 90.0, "TimeoutStartUSec": 90.0}
	show, err := systemctl(ctx, fleetArgv(scopeCmd, "show", svcName, "--property=TimeoutStopUSec,TimeoutStartUSec"), 5)
	if err != nil {
		if isCommandMissing(err) || asCommandTimeout(err) != nil {
			return 90.0, 90.0, nil
		}
		return 0, 0, err
	}
	if show.Returncode == 0 {
		for _, line := range pystr.Splitlines(completedStdout(show), false) {
			key, raw, _ := strings.Cut(line, "=")
			if _, known := budgets[key]; !known {
				continue
			}
			raw = pystr.Strip(raw)
			var duration *big.Int
			if raw != "" && strings.Trim(raw, "0123456789") == "" {
				duration, _ = new(big.Int).SetString(raw, 10)
			} else {
				duration = gateway.ParseSystemdDurationToUS(raw)
			}
			if duration == nil || duration.Sign() <= 0 {
				continue
			}
			seconds, _ := new(big.Float).Quo(new(big.Float).SetInt(duration), big.NewFloat(1_000_000)).Float64()
			if !math.IsInf(seconds, 0) {
				budgets[key] = seconds
			}
		}
	}
	return budgets["TimeoutStopUSec"], budgets["TimeoutStartUSec"], nil
}

// Client timeout that outwaits the unit's stop and start budgets, the start budget alone when startOnly
func systemdRestartTimeout(ctx context.Context, scopeCmd []string, svcName string, startOnly bool) (float64, error) {
	stop, start, err := unitStopStartBudgets(ctx, scopeCmd, svcName)
	if err != nil {
		return 0, err
	}
	if startOnly {
		stop = 0.0
	}
	return math.Min(stop+start+15.0, systemctlRestartTimeoutMax), nil
}

// Seconds systemd may take to finish a verb on the unit, capped at fifteen minutes
func unitJobBudget(ctx context.Context, scopeCmd []string, svcName, verb string) (float64, error) {
	stop, start, err := unitStopStartBudgets(ctx, scopeCmd, svcName)
	if err != nil {
		return 0, err
	}
	budget := 2*stop + 30.0
	if verb == "start" || verb == "restart" {
		budget = 2*stop + start + 30.0
	}
	return math.Min(budget, unitJobHardCapS), nil
}

// ActiveState, SubState, MainPID and Job of the unit, empty when unreadable
func unitJobState(ctx context.Context, scopeCmd []string, svcName string) (map[string]string, error) {
	show, err := systemctl(ctx, fleetArgv(scopeCmd, "show", svcName, "--property=ActiveState,SubState,MainPID,Job"), 10)
	if err != nil {
		if isCommandMissing(err) || asCommandTimeout(err) != nil {
			return map[string]string{}, nil
		}
		return nil, err
	}
	state := map[string]string{}
	if show.Returncode != 0 {
		return state, nil
	}
	for _, line := range pystr.Splitlines(completedStdout(show), false) {
		if key, value, found := strings.Cut(line, "="); found {
			state[key] = value
		}
	}
	return state, nil
}

// True once the unit left every transitional state and systemd holds no job for it
func settled(state map[string]string) bool {
	active := state["ActiveState"]
	return active != "" && !unitTransitionalStates[active] && state["Job"] == ""
}

// Runs a systemctl verb on a unit for the pause, printing to the pause stream
func runUnitJob(ctx context.Context, manageCmd, scopeCmd []string, verb, svcName string, timeout float64) (*pysubprocess.Completed, error) {
	return runUnitJobTo(ctx, posixPauseOut, manageCmd, scopeCmd, verb, svcName, timeout)
}

// Runs a systemctl verb on a unit, a client timeout answered by waiting for the unit's job to settle
func runUnitJobTo(ctx context.Context, w io.Writer, manageCmd, scopeCmd []string, verb, svcName string, timeout float64) (*pysubprocess.Completed, error) {
	started := fleetMonotonic()
	result, err := systemctl(ctx, fleetArgv(manageCmd, verb, svcName), timeout)
	expired := asCommandTimeout(err)
	if expired == nil {
		return result, err
	}
	argv := expired.Cmd
	budget, err := unitJobBudget(ctx, scopeCmd, svcName, verb)
	if err != nil {
		return nil, err
	}
	deadline := started.Add(time.Duration(math.Max(budget, timeout) * float64(time.Second)))
	state, err := unitJobState(ctx, scopeCmd, svcName)
	if err != nil {
		return nil, err
	}
	for !settled(state) && fleetMonotonic().Before(deadline) {
		fleetSleep(time.Second)
		if state, err = unitJobState(ctx, scopeCmd, svcName); err != nil {
			return nil, err
		}
	}
	waited := fleetMonotonic().Sub(started).Seconds()
	active, known := state["ActiveState"]
	if !known {
		active = "unknown"
	}
	if !settled(state) {
		sub, known := state["SubState"]
		if !known {
			sub = "?"
		}
		fmt.Fprintf(w, "  ⏳ systemctl %s %s timed out after %.0fs; systemd's job is still running after %.0fs (%s/%s)\n", verb, svcName, timeout, waited, active, sub)
		return &pysubprocess.Completed{Args: argv, Returncode: unitJobStillRunning, Stdout: []byte{}, Stderr: []byte(fmt.Sprintf("systemd's %s job still running", verb))}, nil
	}
	reached := active == "inactive" || active == "failed"
	if verb == "start" || verb == "restart" {
		reached = active == "active"
	}
	fmt.Fprintf(w, "  ⏳ systemctl %s %s timed out after %.0fs; systemd finished the job after %.0fs (%s)\n", verb, svcName, timeout, waited, active)
	if reached {
		return &pysubprocess.Completed{Args: argv, Returncode: 0, Stdout: []byte{}, Stderr: []byte{}}, nil
	}
	return &pysubprocess.Completed{Args: argv, Returncode: 1, Stdout: []byte{}, Stderr: []byte("unit settled " + active)}, nil
}

// RestartUSec of the unit in seconds, the fallback when it is unset, infinite or unreadable
func serviceRestartSec(ctx context.Context, scopeCmd []string, svcName string, fallback float64) (float64, error) {
	show, err := systemctl(ctx, fleetArgv(scopeCmd, "show", svcName, "--property=RestartUSec", "--value"), 5)
	if err != nil {
		if isCommandMissing(err) || asCommandTimeout(err) != nil {
			return fallback, nil
		}
		return 0, err
	}
	raw := pystr.Strip(completedStdout(show))
	if raw == "" || raw == "infinity" {
		return fallback, nil
	}
	total, matched := 0.0, false
	for _, part := range pystr.SplitWhitespace(raw, -1) {
		for _, unit := range restartSecUnits {
			if !strings.HasSuffix(part, unit.suffix) {
				continue
			}
			if value, err := pyint.ParseFloat(strings.TrimSuffix(part, unit.suffix)); err == nil {
				total += value * unit.mult
				matched = true
			}
			break
		}
	}
	if matched {
		return total, nil
	}
	return fallback, nil
}
