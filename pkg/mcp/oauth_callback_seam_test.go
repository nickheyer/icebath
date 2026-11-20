package mcp

// Reports whether a port is parked
func isReserved(port int) bool {
	reservedMu.Lock()
	defer reservedMu.Unlock()
	_, ok := reservedListeners[port]
	return ok
}
