package state

import "sync"

// SQLite unix VFS lock geometry, the pending byte page of the main file and the shm dead-man byte
const (
	pendingByte = 0x40000000
	sharedFirst = pendingByte + 2
	sharedSize  = 510
	shmDMSByte  = 128
)

// Byte range one guarded inode is locked over
type lockRange struct {
	start  int64
	length int64
}

// Inodes one store guards mapped to the range it locked on each
type guardHeld map[fileIdentity]lockRange

// Stores per guarded inode in this process, the last one on an inode lifting its guard
type guardCounts struct {
	mu      sync.Mutex
	handles map[fileIdentity]int
}
