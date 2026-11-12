package tuigateway

// Removes a transport from a session's peers and viewers, true when another live client keeps the session from parking
func DetachSessionTransport(session *Session, transport Transport) bool {
	return detachSessionTransport(session, transport)
}

// The stdio transport of the process, the real stdout peer
func ProcessStdioTransport() Transport { return stdioTransport() }
