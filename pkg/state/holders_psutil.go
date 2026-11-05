//go:build unix && !linux

package state

import (
	"errors"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Matches every process's open files against the watched family, adding the unknown holder on a failed scan
func scanOpenFiles(dbPath string, watched map[string]bool, watchedIDs map[FileIdentity]bool, dbDev uint64, hasDBDev bool, holders []Holder) []Holder {
	if !proc.OpenFilesAvailable {
		return []Holder{{-1, "open-file scan unavailable"}}
	}
	if err := openFilesHolderScan(watched, &holders); err != nil {
		slog.Warn("Could not prove state.db has no foreign holders; deferring structural maintenance: " + pyOSErrorStr(err))
		holders = append(holders, Holder{-1, "open-file scan failed: " + pyOSErrorStr(err)})
	}
	return holders
}

// Appends every other process holding a watched path, skipping vanished and access denied processes
func openFilesHolderScan(watched map[string]bool, holders *[]Holder) error {
	pids, err := proc.PIDs()
	if err != nil {
		return err
	}
	for _, pid := range pids {
		files, err := proc.OpenFiles(pid)
		switch {
		case errors.Is(err, proc.ErrNoSuchProcess):
			continue
		case errors.Is(err, proc.ErrAccessDenied):
			files = nil
		case err != nil:
			return err
		}
		if pid == holderGetpid() {
			continue
		}
		for _, opened := range files {
			if opened.Path == "" {
				continue
			}
			resolved, err := pyRealpath(opened.Path)
			if err != nil {
				return err
			}
			canonical, err := CanonicalSQLitePath(resolved)
			if err != nil {
				return err
			}
			if watched[canonical] {
				*holders = append(*holders, Holder{pid, opened.Path})
			}
		}
	}
	return nil
}
