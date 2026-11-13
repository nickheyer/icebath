package state

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/internal/home"
)

// Storage health of one database file as this process has observed it
type Health string

// Health values readiness and the session list report, corrupt being the one reason string consumers key on
const (
	HealthOK      Health = "ok"
	HealthCorrupt Health = "corrupt"
)

// Process wide latch of database files that showed structural corruption, keyed by resolved path
var corruptLatch = struct {
	sync.Mutex
	first map[string]string
}{first: map[string]string{}}

// Resolved form of a database path every latch lookup shares
func healthKey(path string) string { return resolvePath(home.ExpandUser(path)) }

// Latches path as corrupt for the life of the process, keeping the first reason
func MarkCorrupt(path string, reason error) {
	key := healthKey(path)
	corruptLatch.Lock()
	if _, seen := corruptLatch.first[key]; seen {
		corruptLatch.Unlock()
		return
	}
	corruptLatch.first[key] = errText(reason)
	corruptLatch.Unlock()
	slog.Error(fmt.Sprintf("state.db at %s is structurally corrupt (%s); session storage is reported as corrupt "+
		"until icebath restarts on a recovered or restored file. Stop icebath, then run "+
		"`icebath sessions recover --source %s --inspect-only` or restore a snapshot.", path, errText(reason), path))
}

// Latches path when err is structural corruption and reports whether it was
func NoteStorageError(path string, err error) bool {
	if !isStructuralCorruption(err) {
		return false
	}
	MarkCorrupt(path, err)
	return true
}

// Corrupt once this process saw structural corruption on path, ok otherwise
func StorageHealth(path string) Health {
	key := healthKey(path)
	corruptLatch.Lock()
	defer corruptLatch.Unlock()
	if _, seen := corruptLatch.first[key]; seen {
		return HealthCorrupt
	}
	return HealthOK
}

// First error text latched for path, for logs and diagnostics, false when the file is not latched
func CorruptReason(path string) (string, bool) {
	key := healthKey(path)
	corruptLatch.Lock()
	defer corruptLatch.Unlock()
	reason, seen := corruptLatch.first[key]
	return reason, seen
}
