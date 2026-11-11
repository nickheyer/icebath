package tuigateway

// Whether a live peer transport is attached to the session
func SessionHasLiveTransport(session *Session) bool { return sessionHasLiveTransport(session, nil) }

// Whether a transport is the module stdio transport
func IsStdioTransport(t any) bool { return t == Transport(stdioTransport) }

// Whether a transport is closed or its peer is gone
func TransportIsDead(transport Transport) bool { return transportIsDead(transport) }

// Attaches a transport to the session's peer set, true when it was not attached yet
func AttachSessionTransport(session *Session, transport Transport) bool {
	return attachSessionTransport(session, transport)
}
