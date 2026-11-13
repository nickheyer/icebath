package state

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Names of the WAL lock bytes of the shm file, UNIX_SHM_BASE at 120
var shmLockNames = map[int64]string{120: "WAL write", 121: "WAL checkpoint", 122: "WAL recover", 128: "shm dead-man switch"}

// Names of the rollback journal lock bytes of the main file
var mainLockNames = map[int64]string{pendingByte: "PENDING", pendingByte + 1: "RESERVED"}

// Lock kind a byte range on the main file or one sidecar encodes
func describeLockRange(sidecar string, start, end int64) string {
	if sidecar == shmSuffix {
		if name, ok := shmLockNames[start]; ok && end == start {
			return name
		}
		if start >= 123 && start <= 127 && end == start {
			return fmt.Sprintf("WAL read slot %d", start-123)
		}
		return fmt.Sprintf("shm bytes %d-%d", start, end)
	}
	if name, ok := mainLockNames[start]; ok && start == end {
		return name
	}
	if start == sharedFirst {
		return "SHARED range"
	}
	return fmt.Sprintf("db bytes %d-%d", start, end)
}

// Write locks of a /proc/locks listing on one of the inodes, blocked waiters and read locks dropped
func parseProcLocks(text string, inodes map[fileIdentity]string) []lockOwner {
	var found []lockOwner
	for line := range strings.Lines(text) {
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[1] == "->" || fields[3] != "WRITE" {
			continue
		}
		ident := strings.Split(fields[5], ":")
		if len(ident) != 3 {
			continue
		}
		major, err1 := strconv.ParseUint(ident[0], 16, 32)
		minor, err2 := strconv.ParseUint(ident[1], 16, 32)
		ino, err3 := strconv.ParseUint(ident[2], 10, 64)
		pid, err4 := strconv.Atoi(fields[4])
		start, err5 := strconv.ParseInt(fields[6], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
			continue
		}
		end := start
		if fields[7] != "EOF" {
			v, err := strconv.ParseInt(fields[7], 10, 64)
			if err != nil {
				continue
			}
			end = v
		}
		sidecar, ok := inodes[fileIdentity{dev: unix.Mkdev(uint32(major), uint32(minor)), ino: ino}]
		if !ok {
			continue
		}
		found = append(found, lockOwner{pid: pid, kind: describeLockRange(sidecar, start, end), sidecar: sidecar})
	}
	return found
}

// Write locks on the inodes, three reads united since lock churn can shift the host table mid read
func readLockOwners(inodes map[fileIdentity]string) ([]lockOwner, bool) {
	var held []lockOwner
	for range 3 {
		data, err := os.ReadFile("/proc/locks")
		if err != nil {
			return nil, false
		}
		for _, h := range parseProcLocks(string(data), inodes) {
			if !slices.Contains(held, h) {
				held = append(held, h)
			}
		}
	}
	return held, true
}
