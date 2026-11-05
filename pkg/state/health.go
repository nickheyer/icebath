package state

import (
	"fmt"
	"log/slog"
	"sync"
)

// StorageOK and StorageCorrupt are the two storage health states of a database path
const (
	StorageOK      = "ok"
	StorageCorrupt = "corrupt"
)

var (
	storageHealthMu sync.Mutex
	storageCorrupt  = map[string]string{}
)

// Registry key of a database path, user expanded and symlink resolved
func storageKey(dbPath string) string {
	return realpathLax(expandUser(dbPath))
}

// Reports corruption outside the FTS index and the repairable malformed schema case
func IsStructuralCorruptionError(err error) bool {
	return isDatabaseError(err) && !IsFTSScopedCorruptionError(err) && !IsMalformedSchemaError(err) &&
		ClassifyPersistenceError(err) == "corrupt"
}

// Latches a database path as corrupt for the life of the process
func MarkStorageCorrupt(dbPath string, reason any) {
	key := storageKey(dbPath)
	storageHealthMu.Lock()
	if _, seen := storageCorrupt[key]; seen {
		storageHealthMu.Unlock()
		return
	}
	text := errText(reason)
	storageCorrupt[key] = text
	storageHealthMu.Unlock()
	slog.Error(fmt.Sprintf("state.db at %s is structurally corrupt (%s); session storage is reported as corrupt "+
		"until icebath restarts on a recovered or restored file. Stop icebath, then run "+
		"`icebath sessions recover --source %s --inspect-only` or restore a snapshot.", dbPath, text, dbPath))
}

// Latches the path when the error is structural corruption and reports whether it was
func NoteStorageError(dbPath string, err error) bool {
	if !IsStructuralCorruptionError(err) {
		return false
	}
	MarkStorageCorrupt(dbPath, err)
	return true
}

// Corrupt once this process saw structural corruption on the path, else ok
func StorageState(dbPath string) string {
	key := storageKey(dbPath)
	storageHealthMu.Lock()
	defer storageHealthMu.Unlock()
	if _, ok := storageCorrupt[key]; ok {
		return StorageCorrupt
	}
	return StorageOK
}

// The first error text latched for the path, false when none
func StorageCorruptReason(dbPath string) (string, bool) {
	key := storageKey(dbPath)
	storageHealthMu.Lock()
	defer storageHealthMu.Unlock()
	reason, ok := storageCorrupt[key]
	return reason, ok
}

// Forgets the latch for the path, or for every path when the path is empty
func ResetStorageState(dbPath string) {
	storageHealthMu.Lock()
	defer storageHealthMu.Unlock()
	if dbPath == "" {
		clear(storageCorrupt)
		return
	}
	delete(storageCorrupt, storageKey(dbPath))
}
