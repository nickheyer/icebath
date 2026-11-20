package gateway

import "os"

// Takes the exclusive non blocking lock of an open file, false when another holder has it
func TryAcquireFileLock(f *os.File) bool { return tryAcquireFileLock(f) }

// Releases the lock of an open file, failures ignored
func ReleaseFileLock(f *os.File) { releaseFileLock(f) }
