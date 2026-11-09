package tools

// SnapshotTerminalBackend returns the plugin backend registered globally under the exact name, nil when the slot is empty
func SnapshotTerminalBackend(name string) BackendProvider {
	return terminalEnvRegistry.SnapshotRegistration(name, "")
}

// RestoreTerminalBackend puts previous back under name only while current still holds the global slot
func RestoreTerminalBackend(name string, current, previous BackendProvider) bool {
	return terminalEnvRegistry.RestoreRegistration(name, current, previous, "")
}
