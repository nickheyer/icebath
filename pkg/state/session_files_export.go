package state

// Unlinks the on disk transcript snapshots and request dumps of a session under its sessions dir
func RemoveSessionFiles(sessionsDir, sessionID string) { removeSessionFiles(sessionsDir, sessionID) }
