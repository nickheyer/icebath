package proc

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// The root followed by its descendants, or the root alone with an ErrIncomplete error when the table cannot be read
func treeMembers(root Identity) ([]Identity, error) {
	table, err := ReadTable()
	if err != nil {
		return []Identity{root}, fmt.Errorf("descendants of pid %d: %w: %w", root.PID, ErrIncomplete, err)
	}
	return append([]Identity{root}, table.Descendants(root)...), nil
}

// Terminates every member, children before parents, ignoring members already gone
func terminateAll(members []Identity) error {
	var failed []error
	for i := len(members) - 1; i >= 0; i-- {
		err := terminateIdentity(members[i], 1)
		if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrReused) {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// Terminates root and every descendant it started, children before parents, as taskkill /T /F does, sig ignored
func SignalTree(root Identity, sig syscall.Signal) error {
	if !root.Running() {
		return fmt.Errorf("pid %d: %w", root.PID, ErrNotFound)
	}
	members, listErr := treeMembers(root)
	return errors.Join(listErr, terminateAll(members))
}

// Sends WM_CLOSE to tree windows as taskkill does, waits grace for the root if any took one, then terminates
func KillTree(ctx context.Context, root Identity, grace time.Duration) error {
	if !root.Running() {
		return fmt.Errorf("pid %d: %w", root.PID, ErrNotFound)
	}
	members, listErr := treeMembers(root)
	if grace > 0 {
		var handles []windows.Handle
		for _, m := range members {
			if h, err := openIdentity(m, 0); err == nil {
				handles = append(handles, h)
			}
		}
		if closeWindows(handles) {
			wait, cancel := context.WithTimeout(ctx, grace)
			root.Wait(wait)
			cancel()
		}
		for _, h := range handles {
			windows.CloseHandle(h)
		}
		if root.Running() {
			var fresh []Identity
			fresh, listErr = treeMembers(root)
			members = mergeIdentities(members, fresh)
		}
	}
	return errors.Join(listErr, terminateAll(members))
}

// Identities of a followed by those of b it lacks
func mergeIdentities(a, b []Identity) []Identity {
	seen := map[Identity]bool{}
	for _, id := range a {
		seen[id] = true
	}
	for _, id := range b {
		if !seen[id] {
			seen[id] = true
			a = append(a, id)
		}
	}
	return a
}
