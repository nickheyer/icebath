package tuigateway

import "slices"

// Transports of every connected peer a session less broadcast reaches, in registration order
func LiveTransports() []Transport {
	liveTransportsMu.Lock()
	defer liveTransportsMu.Unlock()
	return slices.Clone(liveTransports)
}
