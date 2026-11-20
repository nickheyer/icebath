package provider

// Clears every hidden provider endpoint
func auxResetAuxUnhealthyCache() {
	auxUnhealthyMu.Lock()
	defer auxUnhealthyMu.Unlock()
	clear(auxUnhealthyUntil)
	clear(auxUnhealthyLoggedAt)
	clear(auxUnhealthyReason)
}
