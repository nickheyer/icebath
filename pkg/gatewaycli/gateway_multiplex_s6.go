package gatewaycli

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/pkg/gatewayrun"
)

// Desired states that autostart, everything else waits for the operator
var AutostartStates = map[string]bool{"running": true}

// Named profiles whose autostart intent the root slot takes over and whether the root must be up
type FoldDecision struct {
	Folded          []string
	RootShouldStart bool
}

// Named profile and the desired state its slot recorded before the boot, nil when none
type NamedSlotState struct {
	Profile string
	Prior   *string
}

// Folds every running named slot intent into the root slot, which starts when it or any folded slot was running
func FoldNamedSlotIntent(defaultPriorState *string, namedStates []NamedSlotState) FoldDecision {
	folded := []string{}
	for _, entry := range namedStates {
		if entry.Prior != nil && AutostartStates[*entry.Prior] {
			folded = append(folded, entry.Profile)
		}
	}
	sort.Strings(folded)
	rootRunning := defaultPriorState != nil && AutostartStates[*defaultPriorState]
	return FoldDecision{Folded: folded, RootShouldStart: rootRunning || len(folded) > 0}
}

// s6 service directory name of a profile's slot
func NamedSlotName(profile string) string { return S6ServicePrefix + profile }

// Slot operations of the s6 supervisor the multiplex convergence drives
type s6SlotManager interface {
	IsRunning(name string) (bool, error)
	Stop(ctx context.Context, name string) error
	Start(name string) error
	Restart(name string) error
	SlotScandir() string
}

// Scan directory of the supervised slots
func (m *S6ServiceManager) SlotScandir() string { return m.Scandir }

// Supervisor of the container's dynamic scan directory
var s6SlotManagerFor = func() s6SlotManager { return NewS6ServiceManager(S6DynamicScandir) }

// Named profiles whose slot is up right now, an unprobeable slot counting as down
func RunningNamedSlots(profiles []string, manager s6SlotManager) []string {
	if manager == nil {
		manager = s6SlotManagerFor()
	}
	up := []string{}
	for _, profile := range profiles {
		if profile == "default" {
			continue
		}
		running, err := manager.IsRunning(NamedSlotName(profile))
		if err != nil {
			slog.Default().With("logger", "hermes_cli.gateway_multiplex_s6").Debug("could not probe s6 slot for "+profile, "error", err)
			continue
		}
		if running {
			up = append(up, profile)
		}
	}
	return up
}

func SlotIsUp(profile string, manager s6SlotManager) bool {
	return len(RunningNamedSlots([]string{profile}, manager)) > 0
}

// Stops a named slot and writes its down file so a supervisor restart does not revive it
func ParkNamedSlot(ctx context.Context, profile string, manager s6SlotManager) error {
	if manager == nil {
		manager = s6SlotManagerFor()
	}
	slot := NamedSlotName(profile)
	running, err := manager.IsRunning(slot)
	if err != nil {
		return err
	}
	if running {
		if err := manager.Stop(ctx, slot); err != nil {
			return err
		}
		s6WaitDown(filepath.Join(manager.SlotScandir(), slot), 15000)
	}
	f, err := os.OpenFile(filepath.Join(manager.SlotScandir(), slot, "down"), os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	return f.Close()
}

// Waits for the slot to report down, a failed wait only logged
var s6WaitDown = func(serviceDir string, timeoutMS int) {
	timeout := time.Duration(timeoutMS)*time.Millisecond + 5*time.Second
	if _, err := s6Run(timeout, false, "s6-svwait", "-d", "-t", strconv.Itoa(timeoutMS), serviceDir); err != nil {
		slog.Default().With("logger", "hermes_cli.gateway_multiplex_s6").Debug("s6-svwait failed for "+serviceDir, "error", err)
	}
}

// Starts the root slot or restarts it so it rereads its config, the one line description returned
func BringRootSlotUp(manager s6SlotManager) (string, error) {
	if manager == nil {
		manager = s6SlotManagerFor()
	}
	slot := NamedSlotName("default")
	if err := os.Remove(filepath.Join(manager.SlotScandir(), slot, "down")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	running, err := manager.IsRunning(slot)
	if err != nil {
		return "", err
	}
	if running {
		if err := manager.Restart(slot); err != nil {
			return "", err
		}
		return "restarted the root gateway slot (s6)", nil
	}
	if err := manager.Start(slot); err != nil {
		return "", err
	}
	return "started the root gateway slot (s6)", nil
}

// Installs the s6 legs the multiplex migration in pkg/gatewayrun drives
func InstallMigrateS6Hooks() {
	h := &gatewayrun.MigrateS6
	h.Running = runningUnderS6
	h.Scandir = func() string { return NewS6ServiceManager(S6DynamicScandir).Scandir }
	h.NamedSlotName = NamedSlotName
	h.SlotIsUp = func(profile string) bool { return SlotIsUp(profile, nil) }
	h.ParkNamedSlot = func(ctx context.Context, profile string) error { return ParkNamedSlot(ctx, profile, nil) }
	h.BringRootSlotUp = func() (string, error) { return BringRootSlotUp(nil) }
}
