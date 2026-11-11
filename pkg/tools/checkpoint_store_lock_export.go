package tools

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// Lock file beside the resolved checkpoint base whose inode survives clearing the base
func CheckpointStoreLockPath(base string) string { return checkpointStoreLockPath(base) }

// Takes the whole checkpoint store lock without waiting and returns its release, a busy store errors
func CheckpointStoreLock(base string) (func(), error) { return checkpointStoreLock(base) }

// Spawn ledger rows of path, empty for a missing or blank file and not ok for a corrupt one
func ReadSpawnLedger(path string) ([]*pyjson.Object, bool) { return readLedger(path) }
