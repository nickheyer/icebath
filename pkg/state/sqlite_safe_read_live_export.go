package state

// Connections this process holds open to the database at path
func LiveConnectionCount(path string) int {
	liveMu.Lock()
	defer liveMu.Unlock()
	return liveConnections[liveKey(path)]
}

// Whether the handle still holds its writer connection
func (c *sessionCore) HasWriterConn() bool { return c.hasWriterConn() }
