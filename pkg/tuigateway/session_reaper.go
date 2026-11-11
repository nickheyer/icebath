package tuigateway

// Whether a transport can never deliver again, a fanout being dead exactly when none of its peers is alive
func transportIsDead(transport Transport) bool {
	if transport == detachedWSTransport {
		return true
	}
	if fanout, ok := transport.(*FanoutTransport); ok {
		for _, peer := range fanout.Transports() {
			if !transportIsDead(peer) {
				return false
			}
		}
		return true
	}
	closing, ok := transport.(interface{ Closed() bool })
	return ok && closing.Closed()
}
