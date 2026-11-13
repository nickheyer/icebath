package state

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Reports a descriptor race where the descriptor or its whole process went away mid scan
func goneMidScan(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ESRCH)
}

// Numeric entries of /proc, every process the kernel lists
func procPIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(entries))
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil && digitsOnly(e.Name()) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// Foreign holders found through /proc descriptor links and their device and inode identity
func foreignHolders(path string) []Holder {
	db := resolvePath(path)
	watched := map[string]bool{}
	ids := map[fileIdentity]bool{}
	var holders []Holder
	dbDev, haveDev := uint64(0), false
	for _, candidate := range sqliteFamily(db) {
		watched[canonicalSQLitePath(candidate)] = true
		id, err := identityOf(candidate)
		if err != nil {
			if !goneMidScan(err) {
				holders = append(holders, Holder{Target: fmt.Sprintf("watched-file stat failed: %s: %v", candidate, err)})
			}
			continue
		}
		ids[id] = true
		if candidate == db {
			dbDev, haveDev = id.dev, true
		}
	}
	pids, err := procPIDs()
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not prove state.db has no foreign holders; deferring structural maintenance: %v", err))
		return append(holders, Holder{Target: fmt.Sprintf("open-file scan failed: %v", err)})
	}
	own := os.Getpid()
	for _, pid := range pids {
		if pid == own {
			continue
		}
		dir := fmt.Sprintf("/proc/%d/fd", pid)
		fds, err := os.ReadDir(dir)
		if err != nil {
			if cmdline, ok := suspectHolder(pid, path); ok {
				holders = append(holders, Holder{PID: pid, Target: "uninspectable holder: " + headRunes(cmdline, 80)})
			}
			continue
		}
		for _, fd := range fds {
			fdPath := dir + "/" + fd.Name()
			target, err := os.Readlink(fdPath)
			if err != nil {
				if goneMidScan(err) {
					continue
				}
				if _, ok := suspectHolder(pid, path); ok {
					holders = append(holders, Holder{PID: pid, Target: fmt.Sprintf("uninspectable descriptor: %s: %v", fdPath, err)})
				}
				continue
			}
			isWatched := watched[canonicalSQLitePath(target)]
			id, err := identityOf(fdPath)
			if err != nil {
				if goneMidScan(err) {
					continue
				}
				if _, suspect := suspectHolder(pid, path); isWatched || suspect {
					holders = append(holders, Holder{PID: pid, Target: fmt.Sprintf("uninspectable descriptor: %s: %v", target, err)})
				}
				continue
			}
			if ids[id] || (isWatched && strings.HasSuffix(target, deletedSuffix) && haveDev && id.dev == dbDev) {
				holders = append(holders, Holder{PID: pid, Target: target})
			}
		}
	}
	return holders
}

// Distinct processes whose descriptor links name the resolved store, this one included
func countHolders(path string) (int, bool) {
	target := resolvePath(path)
	pids, err := procPIDs()
	if err != nil {
		return 0, false
	}
	count := 0
	for _, pid := range pids {
		dir := fmt.Sprintf("/proc/%d/fd", pid)
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if link, err := os.Readlink(dir + "/" + fd.Name()); err == nil && link == target {
				count++
				break
			}
		}
	}
	return count, true
}

// Holders of a deleted WAL or SHM generation of the store from /proc, the deleted suffix filtering and identity deciding
func deletedSidecarHolders(path string) []Holder {
	watched := watchedSidecars(path)
	pids, err := procPIDs()
	if err != nil {
		slog.Debug(fmt.Sprintf("deleted-WAL holder scan failed for %s: %v", path, err))
		return nil
	}
	var holders []Holder
	for _, pid := range pids {
		dir := fmt.Sprintf("/proc/%d/fd", pid)
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			fdPath := dir + "/" + fd.Name()
			target, err := os.Readlink(fdPath)
			if err != nil {
				continue
			}
			literal, ok := watched[canonicalSQLitePath(target)]
			if strings.Contains(target, deletedSuffix) && ok && fdTrulyUnlinked(fdPath, literal) {
				holders = append(holders, Holder{PID: pid, Target: target})
			}
		}
	}
	return holders
}

// Descriptors of this process whose links name a deleted watched sidecar generation that is truly unlinked
func ownDeletedSidecar(path string) (bool, error) {
	watched := watchedSidecars(path)
	dir := fmt.Sprintf("/proc/%d/fd", os.Getpid())
	fds, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, fd := range fds {
		fdPath := dir + "/" + fd.Name()
		target, err := os.Readlink(fdPath)
		if err != nil {
			continue
		}
		if literal, ok := watched[canonicalSQLitePath(target)]; strings.Contains(target, deletedSuffix) && ok && fdTrulyUnlinked(fdPath, literal) {
			return true, nil
		}
	}
	return false, nil
}

// Reports whether a deleted descriptor names a generation the watched path no longer holds, comparing identity never link count
func fdTrulyUnlinked(fdPath, watchedPath string) bool {
	id, err := identityOf(fdPath)
	if err != nil {
		return !goneMidScan(err)
	}
	return identityUnlinked(id, watchedPath)
}
