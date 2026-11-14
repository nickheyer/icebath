package pywebsockets

// Whether the connection has not ended yet
func (c *Conn) Open() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}
