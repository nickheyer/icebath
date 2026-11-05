package state

import (
	"fmt"
	"log/slog"
	"strings"
)

// Walks procfs descriptors, keeping holders found before a scan failure and adding the unknown holder for it
func scanOpenFiles(dbPath string, watched map[string]bool, watchedIDs map[FileIdentity]bool, dbDev uint64, hasDBDev bool, holders []Holder) []Holder {
	if err := procfsHolderScan(dbPath, watched, watchedIDs, dbDev, hasDBDev, &holders); err != nil {
		slog.Warn("Could not prove state.db has no foreign holders; deferring structural maintenance: " + pyOSErrorStr(err))
		holders = append(holders, Holder{-1, "open-file scan failed: " + pyOSErrorStr(err)})
	}
	return holders
}

// Appends every process whose descriptor names the watched family, or that may hold it uninspected
func procfsHolderScan(dbPath string, watched map[string]bool, watchedIDs map[FileIdentity]bool, dbDev uint64, hasDBDev bool, holders *[]Holder) error {
	ownPID := holderGetpid()
	entries, err := holdersSys.listdir("/proc")
	if err != nil {
		return err
	}
	uninspected := func(pid int, target string) error {
		argv, ok := readProcArgv(pid)
		if !ok {
			return nil
		}
		hit, err := icebathHolderArgv(argv, dbPath)
		if hit && err == nil {
			*holders = append(*holders, Holder{pid, target})
		}
		return err
	}
	for _, pidStr := range entries {
		if !pyIsDigits(pidStr) {
			continue
		}
		n, err := pyIntOf(pidStr)
		if err != nil {
			return err
		}
		pid := int(n.Int64())
		if pid == ownPID {
			continue
		}
		fdDir := fmt.Sprintf("/proc/%d/fd", pid)
		fds, err := holdersSys.listdir(fdDir)
		if err != nil {
			argv, ok := readProcArgv(pid)
			if !ok {
				continue
			}
			hit, err := icebathHolderArgv(argv, dbPath)
			if err != nil {
				return err
			}
			if hit {
				r := []rune(strings.Join(argv, " "))
				*holders = append(*holders, Holder{pid, "uninspectable holder: " + string(r[:min(len(r), 80)])})
			}
			continue
		}
		for _, fd := range fds {
			fdPath := fdDir + "/" + fd
			target, err := holdersSys.readlink(fdPath)
			if err != nil {
				if isVanishedErr(err) {
					continue
				}
				if err := uninspected(pid, "uninspectable descriptor: "+fdPath+": "+pyOSErrorStr(err)); err != nil {
					return err
				}
				continue
			}
			canonical, err := CanonicalSQLitePath(target)
			if err != nil {
				return err
			}
			targetIsWatched := watched[canonical]
			fdID, err := holdersSys.stat(fdPath)
			if err != nil {
				if isVanishedErr(err) {
					continue
				}
				description := "uninspectable descriptor: " + target + ": " + pyOSErrorStr(err)
				if targetIsWatched {
					*holders = append(*holders, Holder{pid, description})
				} else if err := uninspected(pid, description); err != nil {
					return err
				}
				continue
			}
			if watchedIDs[fdID] || (targetIsWatched && strings.HasSuffix(target, " (deleted)") && hasDBDev && fdID.Dev == dbDev) {
				*holders = append(*holders, Holder{pid, target})
			}
		}
	}
	return nil
}
