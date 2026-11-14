package proc

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"slices"
	"syscall"
	"time"
)

const (
	orphanGrace = 2 * time.Second
	reapMinAge  = 180.0
)

var backendOwnerPurposes = []string{"serve", "dashboard", "gateway"}

func Reapable(purpose string) bool {
	return purpose == "mcp-helper" || slices.Contains(backendOwnerPurposes, purpose)
}

func terminateThenKill(ctx context.Context, id Identity) bool {
	if id.Signal(syscall.SIGTERM) != nil {
		return false
	}
	wait, cancel := context.WithTimeout(ctx, orphanGrace)
	err := id.Wait(wait)
	cancel()
	if err == nil {
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	return id.Signal(syscall.SIGKILL) == nil
}

func (l *Ledger) ReapOrphanedBackendOwner(ctx context.Context, pid int, created *float64, sshClaimed func() map[int]bool) bool {
	if pid <= 0 || pid == os.Getpid() {
		return false
	}
	var entry *LedgerEntry
	for _, e := range l.Entries(LedgerFilter{}) {
		if e.PID != pid || !slices.Contains(backendOwnerPurposes, e.Purpose) {
			continue
		}
		recorded, set, ok := recordedNumber(e.fields["create_time"])
		if set && created != nil {
			if !ok {
				return false
			}
			if math.Abs(recorded-*created) >= epochSlack {
				continue
			}
		}
		entry = &e
		break
	}
	if entry == nil {
		return false
	}
	if dead, known := entry.SpawnerDead(); !(known && dead) && !(entry.fields["spawner_pid"] == nil && reparentedOrphan(pid, sshClaimed)) {
		return false
	}
	if sameBirth(pid, entry.fields["create_time"], entryBoot(entry.fields, "boot_start")) != provenAlive {
		return false
	}
	id, err := Identify(pid)
	if err != nil || !terminateThenKill(ctx, id) {
		return false
	}
	slog.Info(fmt.Sprintf("reaped orphaned %s backend owner pid %d", entry.Purpose, pid))
	return true
}

func (l *Ledger) ReapOrphanedMCPHelpers(ctx context.Context) []int {
	var reaped []int
	self := os.Getpid()
	for _, e := range l.Entries(LedgerFilter{}) {
		if e.Purpose != "mcp-helper" || e.PID <= 0 || e.PID == self {
			continue
		}
		if dead, known := e.SpawnerDead(); !known || !dead {
			continue
		}
		if sameBirth(e.PID, e.fields["create_time"], entryBoot(e.fields, "boot_start")) != provenAlive {
			continue
		}
		id, err := Identify(e.PID)
		if err != nil || !stopHelper(ctx, id) {
			slog.Debug("mcp-helper orphan reap failed", "pid", e.PID, "err", err)
			continue
		}
		reaped = append(reaped, e.PID)
	}
	if len(reaped) > 0 {
		slog.Info(fmt.Sprintf("reaped %d orphaned stdio MCP helper(s): %v", len(reaped), reaped))
	}
	return reaped
}

func IsDesktopSSHBackendArgv(argv []string) bool {
	return slices.Contains(argv, "--ssh-session-token-file")
}

func IsDesktopOwnedBackend(argv []string) bool {
	if os.Getenv("ICEBATH_DESKTOP") != "1" {
		return false
	}
	if os.Getenv("ICEBATH_DASHBOARD_SESSION_TOKEN") != "" {
		return true
	}
	return IsDesktopSSHBackendArgv(argv)
}

type servingLocalDesktopKey struct{}

func ServingLocalDesktop(ctx context.Context, active bool) context.Context {
	return context.WithValue(ctx, servingLocalDesktopKey{}, active || ServesLocalDesktop(ctx))
}

func ServesLocalDesktop(ctx context.Context) bool {
	active, _ := ctx.Value(servingLocalDesktopKey{}).(bool)
	return active
}

func LocalDesktopClient(declared bool, provider string, nativeBootstrap bool) bool {
	return declared && provider == "local" && nativeBootstrap
}
