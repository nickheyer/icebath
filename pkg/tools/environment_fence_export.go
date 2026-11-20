package tools

// Lowers the foreground exit fence a hard exit kill raised, for tests that drive the exit path
func ResetForegroundExitFenceForTests() {
	liveForeground.mu.Lock()
	liveForeground.fenced = false
	liveForeground.mu.Unlock()
}
