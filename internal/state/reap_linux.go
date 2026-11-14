package state

import (
	"context"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/proc"
)

// Wait for a signalled desktop backend to exit before the next signal
const orphanReapWait = 1500 * time.Millisecond

// Terminates parentless desktop port zero backends past minAge that hold the database with no client, returning the signalled pids
func (s *Store) reapInactiveOrphanDesktopHolders(ctx context.Context, holders []Holder, minAge time.Duration) []int {
	now := epochNow()
	var candidates []proc.Identity
	for _, pid := range concreteHolderPIDs(s.path, holders) {
		identity, err := proc.Identify(pid)
		if err != nil {
			continue
		}
		statuses, err := proc.InetStates(pid)
		if err != nil {
			continue
		}
		ppid, err := proc.ParentOf(pid)
		if err != nil {
			continue
		}
		started, err := proc.Started(pid)
		if err != nil {
			continue
		}
		argv, err := proc.Cmdline(pid)
		if err != nil {
			continue
		}
		if !inactiveOrphanDesktopHolder(ppid, now-started, minAge.Seconds(), ephemeralPortZeroBackend(argv), statuses) {
			continue
		}
		candidates = append(candidates, identity)
	}
	var signalled []int
	for _, identity := range candidates {
		if identity.Signal(syscall.SIGTERM) == nil {
			signalled = append(signalled, identity.PID)
		}
	}
	if len(signalled) == 0 {
		return nil
	}
	alive := awaitExit(ctx, candidates)
	for _, identity := range alive {
		_ = identity.Signal(syscall.SIGKILL)
	}
	if len(alive) > 0 {
		awaitExit(ctx, alive)
	}
	return signalled
}

// Incarnations still alive after the reap wait
func awaitExit(ctx context.Context, ids []proc.Identity) []proc.Identity {
	waitCtx, cancel := context.WithTimeout(ctx, orphanReapWait)
	defer cancel()
	return proc.Await(waitCtx, ids)
}

// Unique positive pids proven to hold the database or one of its sidecars, in holder order
func concreteHolderPIDs(dbPath string, holders []Holder) []int {
	canonical := normCase(absPath(dbPath))
	watched := map[string]bool{canonical: true, canonical + walSuffix: true, canonical + shmSuffix: true}
	seen := map[int]bool{}
	var out []int
	for _, h := range holders {
		if h.PID > 0 && !seen[h.PID] && watched[canonicalSQLitePath(h.Target)] {
			seen[h.PID] = true
			out = append(out, h.PID)
		}
	}
	return out
}

// True for an init parented process at least minAge old that is a port zero backend with no established socket
func inactiveOrphanDesktopHolder(ppid int, ageSeconds, minAgeSeconds float64, ephemeralBackend bool, statuses []string) bool {
	if (ppid != 0 && ppid != 1) || ageSeconds < minAgeSeconds || !ephemeralBackend {
		return false
	}
	for _, status := range statuses {
		if status == "ESTABLISHED" {
			return false
		}
	}
	return true
}

// True for a serve or dashboard command line that binds port zero
func ephemeralPortZeroBackend(argv []string) bool {
	if !containsString(argv, "serve") && !containsString(argv, "dashboard") {
		return false
	}
	for i, tok := range argv {
		if tok == "--port" && i+1 < len(argv) && argv[i+1] == "0" {
			return true
		}
		if value, ok := strings.CutPrefix(tok, "--port="); ok && strings.TrimSpace(value) == "0" {
			return true
		}
	}
	return false
}
