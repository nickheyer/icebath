package state

// Reports whether close released the writer connection
func (c *sessionCore) closedForTest() bool { return !c.hasConn.Load() }
