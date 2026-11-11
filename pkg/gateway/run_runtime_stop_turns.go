package gateway

import (
	"errors"
	"log/slog"
	"reflect"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Seconds a stopped turn gets to settle durably before shutdown stops waiting on it
const TurnSettleSeconds = 45.0

// Share of the stop watchdog leash kept for the phases after the settle
const StopTailReserveSeconds = 10.0

// Side agent holder exposing the agent its first slot holds
type sideAgentInterruptible interface {
	HolderAgent() any
}

func runRuntimeLog() *slog.Logger { return slog.Default().With("logger", "gateway.run_runtime") }

// Hard stop of any agent value with a message and tool reason, false when it has no interrupt
func requestHardInterruptWith(agent any, message, toolReason string) bool {
	if agent == nil {
		return false
	}
	if value := reflect.ValueOf(agent); value.Kind() == reflect.Pointer && value.IsNil() {
		return false
	}
	switch target := agent.(type) {
	case hardInterruptWithReason:
		target.HardInterrupt(message, toolReason)
	case hardInterruptMessageOnly:
		target.HardInterrupt(message)
	case interruptWithReason:
		target.Interrupt(message, toolReason)
	case interruptMessageOnly:
		target.Interrupt(message)
	default:
		return false
	}
	return true
}

// Generation of a decoded generation value, false when it is not an integer
func generationOf(v any) (int64, bool) {
	g := rowInt(pyjson.ObjectOf("g", v), "g")
	if g == nil {
		return 0, false
	}
	return *g, true
}

// Cooperative stop of every turn the authority executes, recording reason on each signalled worker
func StopAuthorityTurns(runner any, a *SessionAuthority, inProcess bool, reason *string) int {
	signalled := 0
	a.mu.Lock()
	workers := make(map[string]managedWorkerHandle, len(a.managedWorkers))
	for sid, w := range a.managedWorkers {
		workers[sid] = w
	}
	a.mu.Unlock()
	for _, handle := range workers {
		worker, ok := handle.(*managedWorker)
		if !ok || worker.Closed() || worker.stopped() {
			continue
		}
		worker.setCause(reason)
		if err := worker.control(pyjson.ObjectOf("type", "stop")); err != nil {
			var refusal *state.RuntimeStoreError
			if !errors.As(err, &refusal) {
				panic(err)
			}
			runRuntimeLog().Debug("managed Stop not queued (" + refusal.Reason + "); the latch escalates it")
		}
		signalled++
	}
	signalled += stopSpawningManaged(a, workers, reason)
	if !inProcess {
		return signalled
	}
	running, _ := runner.(runningAgentsRunner)
	a.mu.Lock()
	sessions := make(map[string]*LiveSession, len(a.Sessions))
	for sid, live := range a.Sessions {
		sessions[sid] = live
	}
	a.mu.Unlock()
	for sid, live := range sessions {
		if live == nil || live.EventStream == nil {
			continue
		}
		raw, _ := live.EventStream.Execution.Get("execution_generation")
		generation, ok := generationOf(raw)
		if raw == nil || !ok {
			continue
		}
		a.mu.Lock()
		_, isWorker := workers[sid]
		_, spawning := a.managedSpawning[sid]
		adopted, hasAdopted := a.adopted[sid]
		a.mu.Unlock()
		if isWorker || spawning {
			continue
		}
		agent := adopted.agent
		pending := false
		if !hasAdopted || adopted.generation != generation {
			agent = nil
			if running != nil {
				agent, pending = running.RunningAgent(live.Route)
			}
		}
		if agent == nil || pending {
			a.mu.Lock()
			if a.pendingStops == nil {
				a.pendingStops = map[string]int64{}
			}
			a.pendingStops[sid] = generation
			a.mu.Unlock()
		} else {
			requestHardInterruptWith(agent, "Profile stopping", "gateway shutdown")
		}
		signalled++
	}
	return signalled
}

// Latches the stop and its cause for every managed turn whose worker is still being spawned
func stopSpawningManaged(a *SessionAuthority, workers map[string]managedWorkerHandle, reason *string) int {
	stopped := 0
	a.mu.Lock()
	defer a.mu.Unlock()
	for sid, raw := range a.managedSpawning {
		if _, ok := workers[sid]; ok {
			continue
		}
		generation, _ := generationOf(raw)
		if a.pendingStops == nil {
			a.pendingStops = map[string]int64{}
		}
		latched, has := a.pendingStops[sid]
		if reason != nil && *reason != "" && (!has || latched != generation) {
			if a.pendingStopCauses == nil {
				a.pendingStopCauses = map[string]stopCause{}
			}
			a.pendingStopCauses[sid] = stopCause{generation: generation, reason: *reason}
		}
		a.pendingStops[sid] = generation
		stopped++
	}
	return stopped
}

// Cooperative stop of every turn across every served authority, the whole runtime shutdown
func StopManagedTurns(runner any, reason *string) int {
	total := 0
	for _, a := range allAuthorities(runner) {
		total += StopAuthorityTurns(runner, a, false, reason)
	}
	return total
}

// Hard interrupts the agent of every counted side agent across served authorities, returning how many
func InterruptSideAgents(runner any, reason string) int {
	count := 0
	for _, a := range allAuthorities(runner) {
		for _, holder := range sideAgents(a) {
			interruptible, ok := holder.(sideAgentInterruptible)
			if !ok {
				continue
			}
			if agent := interruptible.HolderAgent(); agent != nil && requestHardInterruptWith(agent, reason, "") {
				count++
			}
		}
	}
	return count
}
