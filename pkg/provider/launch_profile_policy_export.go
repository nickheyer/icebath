package provider

// Drops the frozen launch env so the next capture reads the process env again
func ResetLaunchEnvSnapshotForTests() {
	launchEnvSnapshot.Lock()
	launchEnvSnapshot.env = nil
	launchEnvSnapshot.Unlock()
}
