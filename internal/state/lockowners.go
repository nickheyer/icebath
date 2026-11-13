package state

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
)

// One write class lock on the store, pid -1 for an OFD lock whose owner the kernel hides
type lockOwner struct {
	pid     int
	kind    string
	sidecar string
}

// Operator lines naming the processes holding a write class lock on the store, empty where no lock table exists
func WriteLockHolders(path string) []string {
	base := resolvePath(path)
	inodes := map[fileIdentity]string{}
	for _, sidecar := range []string{"", walSuffix, shmSuffix} {
		if id, err := identityOf(base + sidecar); err == nil {
			inodes[id] = sidecar
		}
	}
	held, ok := readLockOwners(inodes)
	if !ok {
		return nil
	}
	lines := make([]string, 0, len(held))
	for _, h := range held {
		who := "OFD lock, owner pid not exported by the kernel"
		if h.pid > 0 {
			who = DescribeHolderPID(h.pid)
		}
		lines = append(lines, fmt.Sprintf("%s holds %s lock on %s", who, h.kind, filepath.Base(base+h.sidecar)))
	}
	return lines
}

// Logs one warning naming the write lock holders at the moment a writer gave up waiting
func logWriteLockHolders(path string, patience time.Duration) {
	detail := strings.Join(WriteLockHolders(path), " | ")
	if detail == "" {
		detail = "no write-class lock held at the deadline (the holder released just before it)"
	}
	slog.Warn(fmt.Sprintf("state.db write lock unavailable for %.0fs (%s): %s", patience.Seconds(), filepath.Base(path), detail))
}
