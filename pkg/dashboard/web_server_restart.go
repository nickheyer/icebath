package dashboard

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Spawn time, handle and argv of the last gateway restart, kept after the child exits
type gatewayRestartRecord struct {
	spawnedAt float64
	proc      actionProc
	command   []string
}

var (
	lastGatewayRestartMu sync.Mutex
	lastGatewayRestart   *gatewayRestartRecord
)

var processClockStart = time.Now()

// Seconds on the monotonic clock, the time.monotonic of the restart cooldown
var restartMonotonic = func() float64 { return time.Since(processClockStart).Seconds() }

// Argv builder and spawner the restart goes through, the module attributes hermes tests patch
var (
	restartGatewaySubcommand = gatewaySubcommand
	restartSpawnAction       = func(ctx context.Context, subcommand []string, name string, env map[string]string) (actionProc, error) {
		return spawnHermesAction(ctx, subcommand, name, env)
	}
)

func init() {
	actionMu.Lock()
	defer actionMu.Unlock()
	if _, ok := actionLogFiles["computer-use-grant"]; !ok {
		actionLogFiles["computer-use-grant"] = "action-computer-use-grant.log"
	}
}

// Last gateway restart record, nil when this process started none
func lastGatewayRestartRecord() *gatewayRestartRecord {
	lastGatewayRestartMu.Lock()
	defer lastGatewayRestartMu.Unlock()
	return lastGatewayRestart
}

// Spawns gateway restart for the profile, reusing a live child or one started within the cooldown
func spawnGatewayRestart(ctx context.Context, profile string, interrupt bool) (actionProc, bool, error) {
	subcommand, err := restartGatewaySubcommand(ctx, profile, "restart")
	if err != nil {
		return nil, false, err
	}
	if interrupt {
		subcommand = append(subcommand, "--interrupt")
	}
	actionMu.Lock()
	existing := actionProcs["gateway-restart"]
	existingCommand, hasCommand := actionCommands["gateway-restart"]
	actionMu.Unlock()
	if existing != nil && existing.Poll() == nil {
		if !hasCommand || slices.Equal(existingCommand, subcommand) {
			return existing, true, nil
		}
		return nil, false, errors.New("gateway restart already in progress for another profile")
	}
	lastGatewayRestartMu.Lock()
	defer lastGatewayRestartMu.Unlock()
	if recent := lastGatewayRestart; recent != nil && slices.Equal(recent.command, subcommand) {
		age := restartMonotonic() - recent.spawnedAt
		if age < GatewayRestartCooldownSeconds {
			webServerLog().Info(fmt.Sprintf("Coalescing gateway restart: one was started %.1fs ago "+
				"(pid %d) and the gateway may still be coming back; not "+
				"spawning another (#89034).", age, recent.proc.Pid()))
			return recent.proc, true, nil
		}
	}
	proc, err := restartSpawnAction(ctx, subcommand, "gateway-restart", nil)
	if err != nil {
		return nil, false, err
	}
	lastGatewayRestart = &gatewayRestartRecord{spawnedAt: restartMonotonic(), proc: proc, command: slices.Clone(subcommand)}
	return proc, false, nil
}
