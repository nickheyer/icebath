//go:build !windows

package proc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Whether a process cannot run, and so cannot fork, until it is continued
func frozen(st Status) bool { return st == Stopped || st == TracingStop }

// Descendants of root, each hard kill target stopped before its children are read, false when a fork may escape
func treeSnapshot(root Identity, hardKill bool) (descendants, stopped []Identity, complete bool) {
	table, err := ReadTable()
	if err != nil {
		return nil, nil, false
	}
	descendants = table.Descendants(root)
	if !hardKill {
		return descendants, nil, true
	}
	known := map[int]bool{}
	for _, d := range descendants {
		known[d.PID] = true
	}
	seen := map[int]bool{os.Getpid(): true}
	pending := []Identity{root}
	deadline := time.Now().Add(time.Second)
	for len(pending) > 0 {
		for _, p := range pending {
			if seen[p.PID] {
				continue
			}
			seen[p.PID] = true
			if !time.Now().Before(deadline) {
				return descendants, stopped, false
			}
			st, err := StatusOf(p.PID)
			if err != nil || st.Exited() {
				continue
			}
			if !frozen(st) {
				if err := p.Signal(syscall.SIGSTOP); err != nil {
					if errors.Is(err, ErrNotFound) || errors.Is(err, ErrReused) {
						continue
					}
					return descendants, stopped, false
				}
				stopped = append(stopped, p)
			}
			for {
				st, err := StatusOf(p.PID)
				if err != nil || frozen(st) || st.Exited() {
					break
				}
				if !time.Now().Before(deadline) {
					return descendants, stopped, false
				}
				time.Sleep(time.Millisecond)
			}
		}
		if table, err = ReadTable(); err != nil {
			return descendants, stopped, false
		}
		for _, c := range table.Descendants(root) {
			if !known[c.PID] {
				known[c.PID] = true
				descendants = append(descendants, c)
			}
		}
		pending = pending[:0]
		for _, d := range descendants {
			if !seen[d.PID] {
				pending = append(pending, d)
			}
		}
	}
	return descendants, stopped, true
}

// Whether root leads a process group this process may signal
func leadsGroup(root Identity) bool {
	pgid, err := unix.Getpgid(root.PID)
	return err == nil && pgid == root.PID && signallableGroup(pgid) == nil
}

// Signals root and every descendant, children before parents and the group root leads last, freezing the tree for SIGKILL
func SignalTree(root Identity, sig syscall.Signal) error {
	if !root.Running() {
		return fmt.Errorf("pid %d: %w", root.PID, ErrNotFound)
	}
	descendants, stopped, complete := treeSnapshot(root, sig == syscall.SIGKILL)
	defer func() {
		for _, p := range stopped {
			if err := p.Signal(syscall.SIGCONT); err != nil {
				slog.Debug("tree kill resume skipped", "pid", p.PID, "err", err)
			}
		}
	}()
	signalled := false
	for i := len(descendants) - 1; i >= 0; i-- {
		if descendants[i].Signal(sig) == nil {
			signalled = true
		}
	}
	var err error
	if leadsGroup(root) {
		err = signalLedGroup(root, sig)
	} else {
		err = root.Signal(sig)
	}
	switch {
	case err != nil && (!signalled || !errors.Is(err, ErrNotFound)):
		return err
	case !complete:
		return fmt.Errorf("pid %d: %w", root.PID, ErrIncomplete)
	}
	return nil
}

// Kills what is left of the group an exited root led, through the root while it is still uncollected
func endLeftovers(root Identity) error {
	if root.Running() {
		return signalLedGroup(root, syscall.SIGKILL)
	}
	return groupError(root.PID, unix.Kill(-root.PID, syscall.SIGKILL))
}

// Ends root and its tree, SIGTERM first with grace for the root to exit, then SIGKILL for whatever remains
func KillTree(ctx context.Context, root Identity, grace time.Duration) error {
	if grace > 0 && root.Running() {
		leads := leadsGroup(root)
		if err := SignalTree(root, syscall.SIGTERM); err == nil || errors.Is(err, ErrIncomplete) {
			wait, cancel := context.WithTimeout(ctx, grace)
			exited := root.Wait(wait) == nil
			cancel()
			if exited {
				if !leads {
					return nil
				}
				if err := endLeftovers(root); err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrReused) {
					return err
				}
				return nil
			}
		}
	}
	return SignalTree(root, syscall.SIGKILL)
}
