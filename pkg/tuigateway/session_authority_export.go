package tuigateway

import "slices"

// Module stdio transport, the sink of a request no transport was bound for
func ModuleStdioTransport() Transport { return stdioTransport }

// Parked sink a disconnected WebSocket session holds until it is resumed or reaped
func DetachedWSTransport() Transport { return detachedWSTransport }

// Live record under sid the transport is attached to, nil when absent, replaced by another record or detached
func SteerAuthoritySession(sid string, transport Transport, expected *Session) *Session {
	sessionsMu.RLock()
	defer sessionsMu.RUnlock()
	session := sessions[sid]
	if session == nil || (expected != nil && session != expected) || !sessionTransportContains(session, transport) {
		return nil
	}
	return session
}

// Whether a live transport is the session transport or a peer of its fanout
func SessionTransportContains(session *Session, transport Transport) bool {
	return sessionTransportContains(session, transport)
}

// Whether a transport is a real attached client, not a sink or a closed peer
func TransportIsLivePeer(transport Transport) bool { return transportIsLivePeer(transport) }

// Live client transports attached to a session
func SessionLiveTransports(session *Session) []Transport { return sessionLiveTransports(session) }

// Removes and returns the live record of sid in one registry critical section, nil when absent
func PopSessionRecord(sid string) *Session {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	session, known := sessions[sid]
	if !known {
		return nil
	}
	delete(sessions, sid)
	sessionOrder = slices.DeleteFunc(sessionOrder, func(id string) bool { return id == sid })
	return session
}
