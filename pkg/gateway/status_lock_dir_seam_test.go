package gateway

// Forgets every unusable lock directory report
func ResetUnusableLockDirReports() {
	unusableLockDirMu.Lock()
	defer unusableLockDirMu.Unlock()
	clear(unusableLockDirWarned)
}

// Whether lockDir was reported unusable by this process
func UnusableLockDirReported(lockDir string) bool {
	unusableLockDirMu.Lock()
	defer unusableLockDirMu.Unlock()
	return unusableLockDirWarned[lockDir]
}
